package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/blobs"
	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/engine/objectstore/objectstoretest"
	"github.com/frankbardon/nexus/pkg/events"
)

// The predicate is the whole safety argument for immutable-skip, so it is
// tested as a predicate rather than only through the snapshot: every false
// positive here is a file that silently stops being uploaded.
func TestObjectStoreImmutable(t *testing.T) {
	// A syntactically valid sha256: 64 lowercase hex characters.
	const sha = "aabb1122334455667788990011223344556677889900112233445566778899aa"

	cases := []struct {
		path string
		want bool
		why  string
	}{
		{"journal/events-001.jsonl.zst", true, "sealed rotated segment"},
		{"journal/events-12345.jsonl.zst", true, "rotation index is 3+ digits, unbounded above"},
		{"journal/events.jsonl", false, "the active segment is rewritten and truncated"},
		{"journal/header.json", false, "not claimed by this predicate"},
		{"journal/events-01.jsonl.zst", false, "fewer than three digits is not a rotation name"},
		{"journal/events-001.jsonl", false, "uncompressed: not a name rotation ever produces"},
		{"journal/cache/events-001.jsonl.zst", false, "the tool-result cache is ordinary mutable data"},
		{"blobs/" + sha[:2] + "/" + sha + ".bin", true, "content-addressed blob"},
		{"blobs/" + sha[:2] + "/" + sha + ".meta", true, "written once beside the blob, never rewritten"},
		{"blobs/zz/" + sha + ".bin", false, "shard directory must be the sha's own first two chars"},
		{"blobs/" + sha[:2] + "/" + sha + ".bin.tmp-123", false, "writeAtomic's in-flight temp file"},
		{"blobs/" + sha[:2] + "/" + strings.ToUpper(sha) + ".bin", false, "uppercase hex is not a name blobs.Store produces"},
		{"blobs/" + sha[:2] + "/" + sha[:63] + ".bin", false, "short sha"},
		{"blobs/" + sha[:2] + "/" + sha + "zz.bin", false, "long sha"},
		{"blobs/" + sha[:2] + "/nested/" + sha + ".bin", false, "blobs are exactly two levels deep"},
		{"files/report.md", false, "ordinary session output"},
		{"metadata/session.json", false, "rewritten every turn"},
		{"context/conversation.jsonl", false, "appended to constantly"},
	}
	for _, tc := range cases {
		if got := objectStoreImmutable(tc.path); got != tc.want {
			t.Errorf("objectStoreImmutable(%q) = %v, want %v (%s)", tc.path, got, tc.want, tc.why)
		}
	}
}

// seedImmutableTree plants one rotated journal segment and two blobs in the
// live session tree, and returns their session-relative paths.
//
// The segment is written straight into the journal directory rather than
// driven through a real rotation: journal.Writer.Snapshot reports every
// top-level file that is not the active segment, so this is exactly the shape
// a rotation leaves behind, without having to push megabytes through the
// writer to trigger one.
func seedImmutableTree(t *testing.T, eng *Engine) []string {
	t.Helper()

	segment := filepath.Join(eng.Session.RootDir, "journal", "events-001.jsonl.zst")
	if err := os.MkdirAll(filepath.Dir(segment), 0o755); err != nil {
		t.Fatalf("mkdir journal: %v", err)
	}
	if err := os.WriteFile(segment, []byte("rotated-segment-bytes"), 0o644); err != nil {
		t.Fatalf("seed rotated segment: %v", err)
	}

	store, err := blobs.New(eng.Session.BlobsDir(), 0)
	if err != nil {
		t.Fatalf("blobs.New: %v", err)
	}
	out := []string{"journal/events-001.jsonl.zst"}
	for i := 0; i < 2; i++ {
		h, err := store.Put([]byte(fmt.Sprintf("blob-payload-%d", i)), "text/plain")
		if err != nil {
			t.Fatalf("blobs.Put: %v", err)
		}
		rel, relErr := filepath.Rel(eng.Session.RootDir, h.Path)
		if relErr != nil {
			t.Fatalf("relativising blob path: %v", relErr)
		}
		slash := filepath.ToSlash(rel)
		out = append(out, slash, strings.TrimSuffix(slash, ".bin")+".meta")
	}
	sort.Strings(out)
	return out
}

// captureSnapshotResults records every session.snapshot.result for later
// assertion on the uploaded/skipped split.
func captureSnapshotResults(eng *Engine) (*[]events.SessionSnapshotResult, func()) {
	var got []events.SessionSnapshotResult
	unsub := eng.Bus.Subscribe("session.snapshot.result", func(ev Event[any]) {
		if r, ok := ev.Payload.(events.SessionSnapshotResult); ok {
			got = append(got, r)
		}
	})
	return &got, unsub
}

// The headline of the added scope: the second turn does not re-upload files
// that cannot have changed. Before this, a session whose bytes were mostly
// blobs and rotated segments paid for all of them on every single turn.
func TestSnapshotSkipsImmutableFilesAfterFirstUpload(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	immutable := seedImmutableTree(t, eng)
	results, unsub := captureSnapshotResults(eng)
	defer unsub()

	endTurn(t, eng, "turn-1")
	firstPuts := backend.Counts().Puts
	keysAfterFirst := sessionKeys(backend, eng.Session.ID)

	endTurn(t, eng, "turn-2")
	secondPuts := backend.Counts().Puts - firstPuts

	if len(*results) != 2 {
		t.Fatalf("expected 2 snapshot results, got %d", len(*results))
	}
	first, second := (*results)[0], (*results)[1]

	if first.ObjectsSkipped != 0 {
		t.Errorf("first snapshot skipped %d objects; nothing was in the store yet", first.ObjectsSkipped)
	}
	if second.ObjectsSkipped != len(immutable) {
		t.Errorf("second snapshot skipped %d objects, want %d (%v)",
			second.ObjectsSkipped, len(immutable), immutable)
	}
	if second.BytesSkipped <= 0 {
		t.Errorf("second snapshot reported %d skipped bytes, want > 0", second.BytesSkipped)
	}

	// Objects/Bytes describe the committed set, so they must NOT shrink when
	// files stop being uploaded. This is the property a later per-object
	// manifest depends on: skipped objects are still part of the session.
	if second.Objects != first.Objects {
		t.Errorf("committed object count changed across turns: %d then %d", first.Objects, second.Objects)
	}
	if second.Objects != second.ObjectsUploaded+second.ObjectsSkipped {
		t.Errorf("objects (%d) != uploaded (%d) + skipped (%d)",
			second.Objects, second.ObjectsUploaded, second.ObjectsSkipped)
	}
	if second.Bytes != second.BytesUploaded+second.BytesSkipped {
		t.Errorf("bytes (%d) != uploaded (%d) + skipped (%d)",
			second.Bytes, second.BytesUploaded, second.BytesSkipped)
	}

	if secondPuts >= firstPuts {
		t.Errorf("second turn issued %d Puts vs %d on the first; the skip saved nothing", secondPuts, firstPuts)
	}

	// And the store still holds every object: skipping is not deleting.
	if got, want := sessionKeys(backend, eng.Session.ID), keysAfterFirst; len(got) != len(want) {
		t.Errorf("object set changed across turns: %v then %v", want, got)
	}
	for _, rel := range immutable {
		if _, ok := backend.Get("sessions/" + eng.Session.ID + "/" + rel); !ok {
			t.Errorf("immutable object %q missing from the store after the skip", rel)
		}
	}

	// The commit marker is the restore contract and must still name the whole
	// set, not just this turn's uploads.
	if marker := readMarker(t, backend, eng.Session.ID); marker.Objects != second.Objects {
		t.Errorf("marker objects = %d, want the committed set size %d", marker.Objects, second.Objects)
	}
}

// A skip must never turn a gap into a permanent gap. If the object is not
// there — dropped upload, lifecycle rule, someone with a console — the next
// snapshot uploads it like anything else.
func TestSnapshotReuploadsImmutableObjectMissingRemotely(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	immutable := seedImmutableTree(t, eng)
	endTurn(t, eng, "turn-1")

	// Second turn establishes the steady state where everything is skipped, so
	// the third turn's re-upload is unambiguously caused by the deletion.
	endTurn(t, eng, "turn-2")

	victim := immutable[0]
	key := "sessions/" + eng.Session.ID + "/" + victim
	if err := backend.Delete(context.Background(), key); err != nil {
		t.Fatalf("Delete %s: %v", key, err)
	}

	results, unsub := captureSnapshotResults(eng)
	defer unsub()
	endTurn(t, eng, "turn-3")

	if _, ok := backend.Get(key); !ok {
		t.Fatalf("object %q was skipped even though the store no longer had it", victim)
	}
	if len(*results) != 1 {
		t.Fatalf("expected 1 snapshot result, got %d", len(*results))
	}
	if (*results)[0].ObjectsSkipped != len(immutable)-1 {
		t.Errorf("skipped %d objects, want %d — the missing one must be re-uploaded",
			(*results)[0].ObjectsSkipped, len(immutable)-1)
	}
}

// A remote object of the wrong length under a content-addressed key is a
// truncated or interrupted upload, and the name that made it skippable is
// exactly what proves it is wrong. Repairing it is the only safe reading.
func TestSnapshotReuploadsImmutableObjectWithWrongRemoteSize(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	immutable := seedImmutableTree(t, eng)
	endTurn(t, eng, "turn-1")

	victim := immutable[0]
	key := "sessions/" + eng.Session.ID + "/" + victim
	want, ok := backend.Get(key)
	if !ok {
		t.Fatalf("object %q not uploaded on the first turn", victim)
	}
	if err := backend.Seed(key, want[:len(want)/2]); err != nil {
		t.Fatalf("Seed truncated object: %v", err)
	}

	endTurn(t, eng, "turn-2")

	got, ok := backend.Get(key)
	if !ok {
		t.Fatalf("object %q vanished", victim)
	}
	if len(got) != len(want) {
		t.Fatalf("truncated object %q was skipped rather than repaired: %d bytes, want %d",
			victim, len(got), len(want))
	}
}

// The saving has to survive a restart, which is the case that forced the
// listing: a resumed session's blobs and segments were hydrated FROM the
// store, so this process never uploaded them and cannot know they are there
// without asking.
func TestSnapshotSkipsImmutableObjectsHydratedFromStore(t *testing.T) {
	backendName := "memory-" + t.Name()
	objectstoretest.RegisterMemory(t, backendName, nil)

	first := resumeEngine(t, backendName, t.TempDir())
	if err := first.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	sessionID := first.Session.ID
	immutable := seedImmutableTree(t, first)
	endTurn(t, first, "turn-1")
	if err := first.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// A second engine on a completely separate data root, resuming the same
	// session — the container-restart case. Nothing it skips can have been
	// uploaded by this process.
	second := resumeEngine(t, backendName, t.TempDir())
	second.RecallSessionID = sessionID
	if err := second.Boot(context.Background()); err != nil {
		t.Fatalf("resumed Boot: %v", err)
	}
	t.Cleanup(func() { _ = second.Stop(context.Background()) })
	if second.Session.ID != sessionID {
		t.Fatalf("resumed session id = %q, want %q", second.Session.ID, sessionID)
	}

	results, unsub := captureSnapshotResults(second)
	defer unsub()
	endTurn(t, second, "turn-2")

	if len(*results) != 1 {
		t.Fatalf("expected 1 snapshot result, got %d", len(*results))
	}
	// The very first snapshot of the resumed run skips, because it listed the
	// store rather than assuming it had uploaded nothing.
	if got := (*results)[0].ObjectsSkipped; got != len(immutable) {
		t.Errorf("first snapshot after resume skipped %d objects, want %d (%v)", got, len(immutable), immutable)
	}
}

// A backend that cannot list must not be allowed to make the engine guess.
// Nothing is skipped on the strength of "we uploaded it earlier", because Put
// may complete asynchronously and only Flush promises durability — so the
// engine falls all the way back to uploading everything, and repairs itself as
// soon as listing works again.
func TestSnapshotUploadsEverythingWhenListingFails(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	immutable := seedImmutableTree(t, eng)
	backend.SetListError(fmt.Errorf("listing is down"))

	results, unsub := captureSnapshotResults(eng)
	defer unsub()
	endTurn(t, eng, "turn-1")
	endTurn(t, eng, "turn-2")

	if len(*results) != 2 {
		t.Fatalf("expected 2 snapshot results, got %d", len(*results))
	}
	for i, r := range *results {
		if r.ObjectsSkipped != 0 {
			t.Errorf("snapshot %d skipped %d objects with no successful listing, want 0", i+1, r.ObjectsSkipped)
		}
	}

	// Listing recovers; the skip comes back without a restart.
	backend.SetListError(nil)
	endTurn(t, eng, "turn-3")
	if len(*results) != 3 {
		t.Fatalf("expected 3 snapshot results, got %d", len(*results))
	}
	if got := (*results)[2].ObjectsSkipped; got != len(immutable) {
		t.Errorf("snapshot after listing recovered skipped %d objects, want %d", got, len(immutable))
	}
}

// A young session has no blobs and no rotated segments. Paying a listing round
// trip every turn to rediscover that would be a cost this story exists to
// remove.
func TestSnapshotDoesNotListWhenNothingIsImmutable(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	// Boot lists once per shared root to discover which app- and agent-scope
	// plugin stores the bucket holds (see shared_objectstore.go). That is a
	// boot cost, not a per-turn one, so the baseline is taken after Boot and
	// this stays a claim about the *snapshot*.
	baseline := backend.Counts().Lists

	if err := eng.Session.WriteFile("files/report.md", []byte("hello")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	endTurn(t, eng, "turn-1")
	endTurn(t, eng, "turn-2")

	if got := backend.Counts().Lists - baseline; got != 0 {
		t.Errorf("backend listed %d times during snapshots with no immutable files in the tree, want 0", got)
	}
}

// Compile-time reminder that the double really is the seam. objectstoretest is
// the reference implementation the S3 and GCS modules are held to, so a test
// that reached past it would prove less than it looks.
var _ objectstore.Backend = (*objectstoretest.Memory)(nil)
