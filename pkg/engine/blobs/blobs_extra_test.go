package blobs

import (
	"fmt"
	"sync"
	"testing"
)

// TestPut_ConcurrentDifferentContent races many distinct payloads into the
// store at once and verifies each lands at its own SHA-keyed file. Run
// under `go test -race`.
func TestPut_ConcurrentDifferentContent(t *testing.T) {
	s := newTestStore(t, 0)

	const n = 64
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		shas   = map[string]string{}
		errsCh = make(chan error, n)
	)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			data := fmt.Appendf(nil, "payload-%d-with-some-padding-to-keep-it-unique", i)
			h, err := s.Put(data, "application/octet-stream")
			if err != nil {
				errsCh <- err
				return
			}
			mu.Lock()
			shas[h.SHA256] = string(data)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	close(errsCh)

	for err := range errsCh {
		if err != nil {
			t.Errorf("concurrent Put error: %v", err)
		}
	}

	if len(shas) != n {
		t.Errorf("expected %d unique SHAs, got %d", n, len(shas))
	}

	for sha, want := range shas {
		got, _, err := s.Get(sha)
		if err != nil {
			t.Errorf("Get %s: %v", sha, err)
			continue
		}
		if string(got) != want {
			t.Errorf("Get %s = %q, want %q", sha, got, want)
		}
	}
}

func TestGet_MalformedSHA(t *testing.T) {
	s := newTestStore(t, 0)
	// Empty SHA, traversal-like input, normal-but-missing.
	for _, sha := range []string{"", "../etc/passwd", "00", "ffff", "not-a-hex"} {
		if _, _, err := s.Get(sha); err == nil {
			t.Errorf("Get(%q) returned no error for non-existent / malformed SHA", sha)
		}
	}
}
