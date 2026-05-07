package storage

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestOpen_RejectsEmptyPluginID(t *testing.T) {
	mgr := NewManager(t.TempDir(), "", nil, nil)
	t.Cleanup(func() { _ = mgr.Close() })
	if _, err := mgr.Open(ScopeApp, ""); err == nil {
		t.Fatal("expected error for empty pluginID")
	}
}

func TestOpen_UnknownScope(t *testing.T) {
	mgr := NewManager(t.TempDir(), "", nil, nil)
	t.Cleanup(func() { _ = mgr.Close() })
	if _, err := mgr.Open(Scope(99), "nexus.test"); err == nil {
		t.Fatal("expected error for unknown scope")
	}
}

func TestClose_Idempotent(t *testing.T) {
	root := t.TempDir()
	mgr := NewManager(root, "", nil, nil)
	if _, err := mgr.Open(ScopeApp, "nexus.test"); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := mgr.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := mgr.Close(); err != nil {
		t.Fatalf("second Close should be a no-op, got %v", err)
	}
}

func TestAttachSessionResolver_LiveSwap(t *testing.T) {
	root := t.TempDir()
	mgr := NewManager(root, "", nil, nil)
	t.Cleanup(func() { _ = mgr.Close() })

	// Without resolver, ScopeSession fails.
	if _, err := mgr.Open(ScopeSession, "nexus.test"); err == nil {
		t.Fatal("expected error before resolver attached")
	}

	sessionDir := filepath.Join(root, "sessions", "live")
	mgr.AttachSessionResolver(func() string { return sessionDir })

	st, err := mgr.Open(ScopeSession, "nexus.test")
	if err != nil {
		t.Fatalf("after AttachSessionResolver: %v", err)
	}
	if err := st.Put("k", []byte("v")); err != nil {
		t.Fatalf("put: %v", err)
	}

	// File must land under the resolver-supplied session dir.
	if _, err := os.Stat(filepath.Join(sessionDir, "plugins", "nexus.test", "store.db")); err != nil {
		t.Errorf("expected store.db under session dir, got %v", err)
	}
}

func TestDirFor_AgentScopePath(t *testing.T) {
	root := t.TempDir()
	mgr := NewManager(root, "researcher", nil, nil)
	t.Cleanup(func() { _ = mgr.Close() })

	st, err := mgr.Open(ScopeAgent, "nexus.test")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Put("k", []byte("v")); err != nil {
		t.Fatalf("put: %v", err)
	}

	want := filepath.Join(root, "agents", "researcher", "plugins", "nexus.test", "store.db")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected store.db at %s, got %v", want, err)
	}
}

func TestStorage_FTS5VirtualTable(t *testing.T) {
	mgr := NewManager(t.TempDir(), "", nil, nil)
	t.Cleanup(func() { _ = mgr.Close() })

	st, err := mgr.Open(ScopeApp, "nexus.fts.test")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Create FTS5 virtual table — exercises that the modernc.org/sqlite
	// build has FTS5 enabled.
	if _, err := st.DB().Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS docs USING fts5(content)`); err != nil {
		t.Fatalf("CREATE VIRTUAL TABLE fts5: %v", err)
	}
	for _, doc := range []string{
		"the quick brown fox",
		"jumps over the lazy dog",
		"a new fox in town",
	} {
		if _, err := st.DB().Exec(`INSERT INTO docs(content) VALUES (?)`, doc); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	rows, err := st.DB().Query(`SELECT content FROM docs WHERE docs MATCH ? ORDER BY rank`, "fox")
	if err != nil {
		t.Fatalf("MATCH query: %v", err)
	}
	defer rows.Close()

	var hits []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		hits = append(hits, c)
	}
	if len(hits) != 2 {
		t.Errorf("FTS5 MATCH 'fox' returned %d rows, want 2 (got %v)", len(hits), hits)
	}
	for _, h := range hits {
		if !strings.Contains(h, "fox") {
			t.Errorf("FTS5 hit %q missing match keyword", h)
		}
	}
}

// TestOpen_ConcurrentSameKeyShareDB verifies the pool returns one *sql.DB
// even under racing Opens for the same (scope, pluginID). Run under
// `go test -race`.
func TestOpen_ConcurrentSameKeyShareDB(t *testing.T) {
	mgr := NewManager(t.TempDir(), "", nil, nil)
	t.Cleanup(func() { _ = mgr.Close() })

	const goroutines = 16
	var (
		wg       sync.WaitGroup
		dbsMu    sync.Mutex
		dbHandle = map[any]struct{}{}
	)
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			st, err := mgr.Open(ScopeApp, "nexus.race.test")
			if err != nil {
				t.Errorf("open: %v", err)
				return
			}
			dbsMu.Lock()
			dbHandle[st.DB()] = struct{}{}
			dbsMu.Unlock()
		}()
	}
	wg.Wait()

	if len(dbHandle) != 1 {
		t.Errorf("concurrent Opens produced %d distinct *sql.DB, want exactly 1", len(dbHandle))
	}
}
