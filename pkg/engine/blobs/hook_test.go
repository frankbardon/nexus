package blobs

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The hook is the entire write-through mechanism, so the properties the engine
// relies on are pinned here rather than only through the engine that consumes
// them.

func TestPutHook_FiresOncePerNewBlobWithBothFiles(t *testing.T) {
	dir := t.TempDir()

	type call struct {
		h    Handle
		meta string
	}
	var mu sync.Mutex
	var calls []call

	s, err := New(dir, 0, WithPutHook(func(h Handle, metaPath string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, call{h: h, meta: metaPath})
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h, err := s.Put([]byte("write-through payload"), "image/png")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	mu.Lock()
	got := append([]call(nil), calls...)
	mu.Unlock()

	if len(got) != 1 {
		t.Fatalf("hook fired %d times, want 1", len(got))
	}
	if got[0].h.SHA256 != h.SHA256 {
		t.Errorf("hook sha = %q, want %q", got[0].h.SHA256, h.SHA256)
	}
	// A blob is two files. A consumer handed only the .bin stores a blob with
	// no media type, which reads back as an untyped payload after a resume.
	if got[0].h.Path != h.Path || !strings.HasSuffix(got[0].h.Path, ".bin") {
		t.Errorf("hook bin path = %q, want the handle's .bin path %q", got[0].h.Path, h.Path)
	}
	wantMeta := strings.TrimSuffix(h.Path, ".bin") + ".meta"
	if got[0].meta != wantMeta {
		t.Errorf("hook meta path = %q, want %q", got[0].meta, wantMeta)
	}
	for _, p := range []string{got[0].h.Path, got[0].meta} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("hook fired before %s existed: %v", p, err)
		}
	}
}

// A second Put of the same content writes nothing, so a second notification
// would describe a write that did not happen — and would make an engine
// re-upload an object it already stored under a key that can only ever hold
// those bytes.
func TestPutHook_SilentOnDuplicateContent(t *testing.T) {
	var fired int
	s, err := New(t.TempDir(), 0, WithPutHook(func(Handle, string) { fired++ }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	data := []byte("same bytes twice")
	if _, err := s.Put(data, "text/plain"); err != nil {
		t.Fatalf("Put 1: %v", err)
	}
	if _, err := s.Put(data, "text/plain"); err != nil {
		t.Fatalf("Put 2: %v", err)
	}
	if fired != 1 {
		t.Errorf("hook fired %d times for one blob, want 1", fired)
	}
}

// The hook uploads, so it must not hold the store mutex: a network round trip
// under the lock would serialise every other Put behind it, and a hook that
// re-entered the store would deadlock outright. Proved by having the hook take
// the lock, with a watchdog so the failure is a message rather than a hang.
func TestPutHook_RunsOutsideTheStoreMutex(t *testing.T) {
	var s *Store
	done := make(chan struct{})
	var err error
	s, err = New(t.TempDir(), 1, WithPutHook(func(Handle, string) {
		// Sweep takes s.mu. So does Delete. Either would block forever if the
		// hook ran inside the critical section.
		_, _, _ = s.Sweep()
		s.SetByteBudget(0)
		close(done)
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	putErr := make(chan error, 1)
	go func() {
		_, perr := s.Put([]byte("re-entrant"), "text/plain")
		putErr <- perr
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hook did not complete; it is being run while the store mutex is held")
	}
	if err := <-putErr; err != nil {
		t.Fatalf("Put: %v", err)
	}
}

// Concurrent Puts of the same content must produce exactly one notification:
// only the goroutine that actually wrote the files reports a write.
func TestPutHook_ConcurrentPutsOfOneBlobNotifyOnce(t *testing.T) {
	var mu sync.Mutex
	fired := 0
	s, err := New(t.TempDir(), 0, WithPutHook(func(Handle, string) {
		mu.Lock()
		fired++
		mu.Unlock()
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	data := []byte("contended content")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, perr := s.Put(data, "application/octet-stream"); perr != nil {
				t.Errorf("Put: %v", perr)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if fired != 1 {
		t.Errorf("hook fired %d times across 16 racing Puts of one blob, want 1", fired)
	}
}

// A nil hook is ignored rather than stored, so a caller that may or may not
// have one does not have to branch — and never panics on the first Put.
func TestWithPutHook_NilIsIgnored(t *testing.T) {
	s, err := New(t.TempDir(), 0, WithPutHook(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Put([]byte("no hook"), "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

// New must not create the root: a session whose tools never produce a blob
// must not grow an empty blobs/ directory, which is what
// SessionWorkspace.BlobsDir documents and what keeps an empty subtree out of
// every synced session.
func TestNew_DoesNotCreateRootUntilFirstPut(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blobs")
	s, err := New(root, 8)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("New created %s; the directory must appear on first Put", root)
	}

	// The read and accounting paths must tolerate that state rather than
	// reporting a walk error for a store nobody has written to.
	if n, err := s.TotalBytes(); err != nil || n != 0 {
		t.Errorf("TotalBytes on an unwritten store = (%d, %v), want (0, nil)", n, err)
	}
	if evicted, freed, err := s.Sweep(); err != nil || evicted != 0 || freed != 0 {
		t.Errorf("Sweep on an unwritten store = (%d, %d, %v), want (0, 0, nil)", evicted, freed, err)
	}

	if _, err := s.Put([]byte("first"), "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("first Put did not create %s: %v", root, err)
	}
}
