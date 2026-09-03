package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/blobs"
	"github.com/frankbardon/nexus/pkg/engine/objectstore/objectstoretest"
)

// waitForKey polls the backend for a key. Write-through is asynchronous by
// design — the whole point is not to block the tool goroutine that produced
// the blob — so a test that asserts immediately would be asserting the wrong
// thing. The deadline is generous because the assertion is "this happens
// without a turn boundary", not "this happens within N milliseconds".
func waitForKey(t *testing.T, backend *objectstoretest.Memory, key string) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if body, ok := backend.Get(key); ok {
			return body
		}
		if time.Now().After(deadline) {
			t.Fatalf("object %q never appeared; keys: %v", key, backend.Keys())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// The headline: a blob reaches the store without any turn boundary. Both files
// land, because a .bin restored without its .meta is a blob with no media
// type.
func TestBlobWriteThroughUploadsWithoutATurnBoundary(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	store, err := eng.Session.BlobStore(0)
	if err != nil {
		t.Fatalf("BlobStore: %v", err)
	}
	data := []byte("a screenshot, notionally")
	h, err := store.Put(data, "image/png")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	prefix := "sessions/" + eng.Session.ID + "/blobs/" + h.SHA256[:2] + "/" + h.SHA256
	body := waitForKey(t, backend, prefix+".bin")
	if string(body) != string(data) {
		t.Errorf("stored bin = %q, want %q", body, data)
	}
	meta := waitForKey(t, backend, prefix+".meta")
	if len(meta) == 0 {
		t.Error("stored meta is empty; the media type would be lost on hydrate")
	}

	// No agent.turn.end was ever emitted, so nothing above came from a
	// snapshot. Asserted rather than assumed, because the snapshot would
	// produce an identical bucket and hide a broken hook.
	if eng.objectStore.snapshotSeq != 0 {
		t.Errorf("snapshotSeq = %d, want 0 — the blob must have arrived by "+
			"write-through, not by snapshot", eng.objectStore.snapshotSeq)
	}
}

// Re-Putting the same content writes nothing locally, so it must upload
// nothing. The key is derived from the bytes, so a duplicate upload would be
// harmless — but paying for it every time a tool re-reads the same image is
// exactly the cost this design does not have.
func TestBlobWriteThroughDoesNotRepushIdenticalContent(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	store, err := eng.Session.BlobStore(0)
	if err != nil {
		t.Fatalf("BlobStore: %v", err)
	}
	data := []byte("the same bytes, twice")
	h, err := store.Put(data, "application/pdf")
	if err != nil {
		t.Fatalf("Put 1: %v", err)
	}
	prefix := "sessions/" + eng.Session.ID + "/blobs/" + h.SHA256[:2] + "/" + h.SHA256
	waitForKey(t, backend, prefix+".bin")
	waitForKey(t, backend, prefix+".meta")
	after := backend.Counts().Puts

	if _, err := store.Put(data, "application/pdf"); err != nil {
		t.Fatalf("Put 2: %v", err)
	}
	// Give a spurious push time to arrive before concluding none happened.
	time.Sleep(50 * time.Millisecond)
	if got := backend.Counts().Puts; got != after {
		t.Errorf("Puts = %d after re-Putting identical content, want %d", got, after)
	}
}

// The eviction decision, asserted rather than only documented. The LRU sweep
// bounds local disk; a bucket has no such bound, and deleting remotely to
// match would destroy data the operator is paying to keep — and would do it to
// content a resumed session may still reference by nexus-blob: URI.
func TestBlobLocalEvictionLeavesTheRemoteObjectAlone(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	// A budget small enough that the second blob evicts the first.
	store, err := eng.Session.BlobStore(16)
	if err != nil {
		t.Fatalf("BlobStore: %v", err)
	}
	first, err := store.Put([]byte("0123456789abcdef"), "application/octet-stream")
	if err != nil {
		t.Fatalf("Put 1: %v", err)
	}
	prefix := "sessions/" + eng.Session.ID + "/blobs/"
	waitForKey(t, backend, prefix+first.SHA256[:2]+"/"+first.SHA256+".bin")

	// mtime resolution on some filesystems is coarse enough that two blobs
	// written in the same instant sort arbitrarily, and the sweep is by mtime.
	time.Sleep(20 * time.Millisecond)
	second, err := store.Put([]byte("fedcba9876543210"), "application/octet-stream")
	if err != nil {
		t.Fatalf("Put 2: %v", err)
	}
	waitForKey(t, backend, prefix+second.SHA256[:2]+"/"+second.SHA256+".bin")

	evicted, freed, err := store.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if evicted == 0 {
		t.Fatalf("Sweep evicted nothing (freed %d); the test needs an eviction to be meaningful", freed)
	}
	if _, err := os.Stat(first.Path); !os.IsNotExist(err) {
		t.Fatalf("the oldest blob survived locally; Sweep did not evict what this test assumes")
	}

	for _, key := range []string{
		prefix + first.SHA256[:2] + "/" + first.SHA256 + ".bin",
		prefix + first.SHA256[:2] + "/" + first.SHA256 + ".meta",
	} {
		if _, ok := backend.Get(key); !ok {
			t.Errorf("%q was deleted remotely to match a local LRU eviction; "+
				"the sweep bounds disk, which is not a constraint the bucket has", key)
		}
	}
	if backend.Counts().Deletes != 0 {
		t.Errorf("backend Deletes = %d, want 0 — nothing on the blob path may delete",
			backend.Counts().Deletes)
	}
}

// Zero-impact default. With no backend named there is no worker goroutine, no
// queue, and no sink on the workspace, so a blob Put costs exactly one atomic
// load more than it did before write-through existed.
func TestBlobWriteThroughInertWithoutABackend(t *testing.T) {
	cfg := DefaultConfig()
	root := t.TempDir()
	cfg.Core.Sessions.Root = filepath.Join(root, "sessions")
	cfg.Core.Storage.Root = root
	eng := newFromConfig(cfg)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	if eng.blobPushes != nil {
		t.Error("a write-through worker was started with no object store configured")
	}
	if eng.Session.blobPush.Load() != nil {
		t.Error("a write-through sink was installed with no object store configured")
	}

	store, err := eng.Session.BlobStore(0)
	if err != nil {
		t.Fatalf("BlobStore: %v", err)
	}
	if _, err := store.Put([]byte("local only"), "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

// Opening a blob store must not create the directory, and neither must Boot:
// a session whose tools never produce a blob has no blobs/ directory at all,
// so nothing empty is carried into a synced tree.
func TestBlobsDirIsCreatedOnFirstPutNotAtBoot(t *testing.T) {
	eng, _ := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	dir := eng.Session.BlobsDir()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("Boot created %s", dir)
	}
	store, err := eng.Session.BlobStore(0)
	if err != nil {
		t.Fatalf("BlobStore: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("opening the store created %s; it must appear on first Put", dir)
	}
	if _, err := store.Put([]byte("now it exists"), "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("first Put did not create %s: %v", dir, err)
	}
}

// A store opened by hand still works and still syncs at the turn boundary; it
// simply does not get the write-through. Pinned so the difference between the
// two doors stays a documented choice rather than an accident.
func TestBlobsNewWithoutTheWorkspaceDoorSkipsWriteThrough(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	store, err := blobs.New(eng.Session.BlobsDir(), 0)
	if err != nil {
		t.Fatalf("blobs.New: %v", err)
	}
	h, err := store.Put([]byte("hand-rolled store"), "text/plain")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	key := "sessions/" + eng.Session.ID + "/blobs/" + h.SHA256[:2] + "/" + h.SHA256 + ".bin"

	time.Sleep(50 * time.Millisecond)
	if _, ok := backend.Get(key); ok {
		t.Fatal("a store opened with blobs.New pushed on write; only " +
			"SessionWorkspace.BlobStore installs the hook")
	}

	endTurn(t, eng, "turn-1")
	if _, ok := backend.Get(key); !ok {
		t.Errorf("object %q missing after the turn boundary; the snapshot is the "+
			"authority whether or not write-through ran", key)
	}
}

// Shutdown drains the queue before the final snapshot, and detaches the sink so
// nothing can Put into a backend the finalizer is about to close.
func TestStopDrainsBlobWriteThroughAndDetachesTheSink(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	store, err := eng.Session.BlobStore(0)
	if err != nil {
		t.Fatalf("BlobStore: %v", err)
	}
	h, err := store.Put([]byte("written just before shutdown"), "text/plain")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	session := eng.Session

	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if eng.blobPushes != nil {
		t.Error("Stop left the write-through worker installed")
	}
	if session.blobPush.Load() != nil {
		t.Error("Stop left the write-through sink attached; a late blob could Put " +
			"into a closed backend")
	}
	key := "sessions/" + session.ID + "/blobs/" + h.SHA256[:2] + "/" + h.SHA256 + ".bin"
	if _, ok := backend.Get(key); !ok {
		t.Errorf("object %q missing after Stop", key)
	}
}
