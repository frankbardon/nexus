package blobs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T, budget int64) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir, budget)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestPutGet_RoundTrip(t *testing.T) {
	s := newTestStore(t, 0)
	data := []byte("hello world")

	h, err := s.Put(data, "text/plain")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	want := sha256.Sum256(data)
	if h.SHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("SHA256: got %s want %s", h.SHA256, hex.EncodeToString(want[:]))
	}
	if h.MediaType != "text/plain" {
		t.Errorf("MediaType: got %q want %q", h.MediaType, "text/plain")
	}
	if h.Size != int64(len(data)) {
		t.Errorf("Size: got %d want %d", h.Size, len(data))
	}

	got, mt, err := s.Get(h.SHA256)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("Get bytes: got %q want %q", got, data)
	}
	if mt != "text/plain" {
		t.Errorf("Get media: got %q want %q", mt, "text/plain")
	}
}

func TestPut_Idempotent(t *testing.T) {
	s := newTestStore(t, 0)
	data := []byte("idempotent payload")

	h1, err := s.Put(data, "image/png")
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}

	h2, err := s.Put(data, "image/png")
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}

	if h1.SHA256 != h2.SHA256 {
		t.Errorf("idempotent Put yielded different hashes: %s vs %s", h1.SHA256, h2.SHA256)
	}
	if h1.Path != h2.Path {
		t.Errorf("idempotent Put yielded different paths: %s vs %s", h1.Path, h2.Path)
	}
}

func TestPut_DifferentContentDifferentSHA(t *testing.T) {
	s := newTestStore(t, 0)
	h1, _ := s.Put([]byte("a"), "text/plain")
	h2, _ := s.Put([]byte("b"), "text/plain")
	if h1.SHA256 == h2.SHA256 {
		t.Errorf("different content collided: %s", h1.SHA256)
	}
}

func TestGet_Missing(t *testing.T) {
	s := newTestStore(t, 0)
	if _, _, err := s.Get("0000000000000000000000000000000000000000000000000000000000000000"); !os.IsNotExist(err) {
		t.Errorf("expected ErrNotExist; got %v", err)
	}
}

func TestStat_NoLoad(t *testing.T) {
	s := newTestStore(t, 0)
	data := []byte("stat me")
	h, _ := s.Put(data, "application/octet-stream")

	st, err := s.Stat(h.SHA256)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Size != int64(len(data)) {
		t.Errorf("Stat.Size: got %d want %d", st.Size, len(data))
	}
	if st.MediaType != "application/octet-stream" {
		t.Errorf("Stat.MediaType: got %q want octet-stream", st.MediaType)
	}
}

func TestDelete(t *testing.T) {
	s := newTestStore(t, 0)
	h, _ := s.Put([]byte("delete me"), "text/plain")

	if err := s.Delete(h.SHA256); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := s.Get(h.SHA256); !os.IsNotExist(err) {
		t.Errorf("expected ErrNotExist after Delete; got %v", err)
	}
}

func TestDelete_Missing_NoError(t *testing.T) {
	s := newTestStore(t, 0)
	if err := s.Delete("0000000000000000000000000000000000000000000000000000000000000000"); err != nil {
		t.Errorf("Delete on missing should not error; got %v", err)
	}
}

func TestSweep_NoBudget_NoOp(t *testing.T) {
	s := newTestStore(t, 0)
	for i := range 10 {
		_, _ = s.Put([]byte{byte(i)}, "application/octet-stream")
	}
	evicted, freed, err := s.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if evicted != 0 || freed != 0 {
		t.Errorf("zero-budget Sweep should be no-op; got evicted=%d freed=%d", evicted, freed)
	}
}

func TestSweep_EvictsLRU(t *testing.T) {
	dir := t.TempDir()
	// Budget = 30 bytes; we'll put three 20-byte blobs so the oldest two get evicted.
	s, err := New(dir, 30)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	bigA := bytes.Repeat([]byte("A"), 20)
	bigB := bytes.Repeat([]byte("B"), 20)
	bigC := bytes.Repeat([]byte("C"), 20)

	// Put A, advance time so mtime ordering is deterministic across filesystems.
	hA, _ := s.Put(bigA, "")
	mustChtime(t, hA.Path, time.Now().Add(-3*time.Second))

	hB, _ := s.Put(bigB, "")
	mustChtime(t, hB.Path, time.Now().Add(-2*time.Second))

	hC, _ := s.Put(bigC, "")
	mustChtime(t, hC.Path, time.Now().Add(-1*time.Second))

	evicted, freed, err := s.Sweep()
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if evicted != 2 {
		t.Errorf("evicted count: got %d want 2", evicted)
	}
	if freed != 40 {
		t.Errorf("freed bytes: got %d want 40", freed)
	}

	// Oldest two (A, B) should be gone; C survives.
	if _, _, err := s.Get(hA.SHA256); !os.IsNotExist(err) {
		t.Errorf("A should be evicted; got err=%v", err)
	}
	if _, _, err := s.Get(hB.SHA256); !os.IsNotExist(err) {
		t.Errorf("B should be evicted; got err=%v", err)
	}
	if _, _, err := s.Get(hC.SHA256); err != nil {
		t.Errorf("C should survive; got err=%v", err)
	}
}

func TestSweep_GetTouchesMtime(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir, 30)

	bigA := bytes.Repeat([]byte("A"), 20)
	bigB := bytes.Repeat([]byte("B"), 20)
	bigC := bytes.Repeat([]byte("C"), 20)

	hA, _ := s.Put(bigA, "")
	mustChtime(t, hA.Path, time.Now().Add(-3*time.Second))

	hB, _ := s.Put(bigB, "")
	mustChtime(t, hB.Path, time.Now().Add(-2*time.Second))

	// Read A — should bump A's mtime to now, pushing B to the oldest slot.
	if _, _, err := s.Get(hA.SHA256); err != nil {
		t.Fatalf("Get A: %v", err)
	}

	hC, _ := s.Put(bigC, "")
	mustChtime(t, hC.Path, time.Now().Add(-1*time.Second))

	// Now bump A again so it's clearly newer than C.
	mustChtime(t, hA.Path, time.Now())

	if _, _, err := s.Sweep(); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// B is the oldest, should be evicted; A survived because Get touched it.
	if _, _, err := s.Get(hA.SHA256); err != nil {
		t.Errorf("A should survive due to recent Get; got err=%v", err)
	}
	if _, _, err := s.Get(hB.SHA256); !os.IsNotExist(err) {
		t.Errorf("B should be evicted; got err=%v", err)
	}
}

func TestTotalBytes(t *testing.T) {
	s := newTestStore(t, 0)
	_, _ = s.Put([]byte("aaaaa"), "")
	_, _ = s.Put([]byte("bbbbbbbb"), "")
	got, err := s.TotalBytes()
	if err != nil {
		t.Fatalf("TotalBytes: %v", err)
	}
	if got != 13 {
		t.Errorf("TotalBytes: got %d want 13", got)
	}
}

func TestURI_RoundTrip(t *testing.T) {
	h := Handle{SHA256: "abc123"}
	uri := h.URI()
	if got := SHAFromURI(uri); got != "abc123" {
		t.Errorf("SHAFromURI: got %q want %q", got, "abc123")
	}
	if SHAFromURI("https://example.com/foo") != "" {
		t.Errorf("non-blob URI should yield empty sha")
	}
}

func TestPut_ConcurrentSameContent(t *testing.T) {
	s := newTestStore(t, 0)
	data := []byte("concurrent content")

	var wg sync.WaitGroup
	results := make([]Handle, 10)
	for i := range 10 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h, err := s.Put(data, "application/octet-stream")
			if err != nil {
				t.Errorf("concurrent Put %d: %v", i, err)
				return
			}
			results[i] = h
		}(i)
	}
	wg.Wait()

	first := results[0]
	for i, h := range results {
		if h.SHA256 != first.SHA256 {
			t.Errorf("concurrent Put %d returned different sha: %s vs %s", i, h.SHA256, first.SHA256)
		}
	}
}

// mustChtime sets both atime and mtime on path. Used to synthesize
// deterministic LRU ordering in tests since Put on a fast filesystem can
// land multiple blobs within the mtime resolution window.
func mustChtime(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("Chtimes %s: %v", filepath.Base(path), err)
	}
}
