package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	sessionDir := filepath.Join(root, "sessions", "sess-test")
	return NewManager(root, "", func() string { return sessionDir }, nil)
}

func TestKVRoundtrip(t *testing.T) {
	mgr := newTestManager(t)
	t.Cleanup(func() { _ = mgr.Close() })

	st, err := mgr.Open(ScopeApp, "nexus.test")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := st.Put("alpha", []byte("one")); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, err := st.Get("alpha")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok || string(got) != "one" {
		t.Fatalf("get returned %v / %q, want true / %q", ok, string(got), "one")
	}

	if err := st.Put("alpha", []byte("two")); err != nil {
		t.Fatalf("put overwrite: %v", err)
	}
	got, _, _ = st.Get("alpha")
	if string(got) != "two" {
		t.Fatalf("after overwrite got %q, want two", string(got))
	}

	if err := st.Delete("alpha"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, ok, _ = st.Get("alpha")
	if ok {
		t.Fatalf("get after delete returned ok=true")
	}
}

func TestListPrefix(t *testing.T) {
	mgr := newTestManager(t)
	t.Cleanup(func() { _ = mgr.Close() })

	st, _ := mgr.Open(ScopeApp, "nexus.test")
	for _, k := range []string{"a:1", "a:2", "b:1", "a%special", "c:x"} {
		if err := st.Put(k, []byte("v")); err != nil {
			t.Fatalf("put %q: %v", k, err)
		}
	}

	got, err := st.List("a:")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"a:1", "a:2"}
	if !equalStrings(got, want) {
		t.Fatalf("list a: got %v, want %v", got, want)
	}

	got, _ = st.List("a%")
	if len(got) != 1 || got[0] != "a%special" {
		t.Fatalf("list a%% got %v, want [a%%special]", got)
	}
}

func TestScopeIsolation(t *testing.T) {
	mgr := newTestManager(t)
	t.Cleanup(func() { _ = mgr.Close() })

	app, _ := mgr.Open(ScopeApp, "nexus.test")
	sess, _ := mgr.Open(ScopeSession, "nexus.test")
	_ = app.Put("k", []byte("app-value"))
	_ = sess.Put("k", []byte("session-value"))

	got, _, _ := app.Get("k")
	if string(got) != "app-value" {
		t.Fatalf("app got %q, want app-value", got)
	}
	got, _, _ = sess.Get("k")
	if string(got) != "session-value" {
		t.Fatalf("session got %q, want session-value", got)
	}
}

func TestPluginIsolation(t *testing.T) {
	mgr := newTestManager(t)
	t.Cleanup(func() { _ = mgr.Close() })

	a, _ := mgr.Open(ScopeApp, "nexus.plugin.a")
	b, _ := mgr.Open(ScopeApp, "nexus.plugin.b")
	_ = a.Put("x", []byte("from-a"))
	_ = b.Put("x", []byte("from-b"))

	got, _, _ := a.Get("x")
	if string(got) != "from-a" {
		t.Fatalf("a got %q", got)
	}
	got, _, _ = b.Get("x")
	if string(got) != "from-b" {
		t.Fatalf("b got %q", got)
	}
}

func TestHandlePoolingReturnsSameDB(t *testing.T) {
	mgr := newTestManager(t)
	t.Cleanup(func() { _ = mgr.Close() })

	first, _ := mgr.Open(ScopeApp, "nexus.test")
	second, _ := mgr.Open(ScopeApp, "nexus.test")
	if first.DB() != second.DB() {
		t.Fatalf("repeated Open returned different *sql.DB pointers")
	}
}

func TestTxRollback(t *testing.T) {
	mgr := newTestManager(t)
	t.Cleanup(func() { _ = mgr.Close() })

	st, _ := mgr.Open(ScopeApp, "nexus.test")
	_ = st.Put("k", []byte("initial"))

	sentinel := errors.New("rollback me")
	err := st.Tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE kv SET v = ? WHERE k = ?`, []byte("modified"), "k"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}

	got, _, _ := st.Get("k")
	if string(got) != "initial" {
		t.Fatalf("after rollback got %q, want initial", got)
	}
}

func TestTxCommit(t *testing.T) {
	mgr := newTestManager(t)
	t.Cleanup(func() { _ = mgr.Close() })

	st, _ := mgr.Open(ScopeApp, "nexus.test")
	_ = st.Put("k", []byte("initial"))

	err := st.Tx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE kv SET v = ? WHERE k = ?`, []byte("committed"), "k")
		return err
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
	got, _, _ := st.Get("k")
	if string(got) != "committed" {
		t.Fatalf("got %q, want committed", got)
	}
}

func TestSessionScopeUnavailable(t *testing.T) {
	root := t.TempDir()
	mgr := NewManager(root, "", func() string { return "" }, nil)
	t.Cleanup(func() { _ = mgr.Close() })

	_, err := mgr.Open(ScopeSession, "nexus.test")
	if err == nil {
		t.Fatalf("expected error when session resolver returns empty")
	}
	if !strings.Contains(err.Error(), "session scope unavailable") {
		t.Fatalf("error should mention session scope, got %v", err)
	}
}

func TestAgentScopeCollapsesWhenAgentIDEmpty(t *testing.T) {
	root := t.TempDir()
	mgr := NewManager(root, "", nil, nil)
	t.Cleanup(func() { _ = mgr.Close() })

	agent, err := mgr.Open(ScopeAgent, "nexus.test")
	if err != nil {
		t.Fatalf("open agent: %v", err)
	}
	app, err := mgr.Open(ScopeApp, "nexus.test")
	if err != nil {
		t.Fatalf("open app: %v", err)
	}
	if app.DB() != agent.DB() {
		t.Fatalf("agent and app scope produced different handles when AgentID empty")
	}
}

func TestAgentScopeDistinctWithAgentID(t *testing.T) {
	root := t.TempDir()
	mgr := NewManager(root, "researcher", nil, nil)
	t.Cleanup(func() { _ = mgr.Close() })

	agent, _ := mgr.Open(ScopeAgent, "nexus.test")
	app, _ := mgr.Open(ScopeApp, "nexus.test")
	if app.DB() == agent.DB() {
		t.Fatalf("agent and app scope share handle when AgentID is set")
	}
}

func TestConcurrentPuts(t *testing.T) {
	mgr := newTestManager(t)
	t.Cleanup(func() { _ = mgr.Close() })

	st, _ := mgr.Open(ScopeApp, "nexus.test")

	const workers = 8
	const perWorker = 50
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				key := fmt.Sprintf("k:%d:%d", w, i)
				if err := st.Put(key, []byte("v")); err != nil {
					t.Errorf("put %q: %v", key, err)
				}
			}
		}(w)
	}
	wg.Wait()

	keys, err := st.List("")
	if err != nil {
		t.Fatal(err)
	}
	if want := workers * perWorker; len(keys) != want {
		t.Fatalf("got %d keys, want %d", len(keys), want)
	}
}

func TestRawSQLAndKVCoexist(t *testing.T) {
	mgr := newTestManager(t)
	t.Cleanup(func() { _ = mgr.Close() })

	st, _ := mgr.Open(ScopeApp, "nexus.test")
	if _, err := st.DB().Exec(`CREATE TABLE custom (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatalf("create custom: %v", err)
	}
	if _, err := st.DB().Exec(`INSERT INTO custom(name) VALUES('a'),('b')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.Put("kv-key", []byte("kv-val")); err != nil {
		t.Fatalf("put: %v", err)
	}

	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM custom`).Scan(&n); err != nil {
		t.Fatalf("count custom: %v", err)
	}
	if n != 2 {
		t.Fatalf("custom count %d, want 2", n)
	}
	got, _, _ := st.Get("kv-key")
	if string(got) != "kv-val" {
		t.Fatalf("kv get %q, want kv-val", got)
	}
}

func TestPersistenceAcrossManagerRestart(t *testing.T) {
	root := t.TempDir()
	mgr := NewManager(root, "", nil, nil)
	st, _ := mgr.Open(ScopeApp, "nexus.test")
	_ = st.Put("survives", []byte("yes"))
	_ = mgr.Close()

	mgr2 := NewManager(root, "", nil, nil)
	t.Cleanup(func() { _ = mgr2.Close() })
	st2, _ := mgr2.Open(ScopeApp, "nexus.test")
	got, ok, _ := st2.Get("survives")
	if !ok || string(got) != "yes" {
		t.Fatalf("got %v / %q, want true / yes", ok, string(got))
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
