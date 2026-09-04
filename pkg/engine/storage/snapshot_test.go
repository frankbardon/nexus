package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

// fillKV writes n rows of payload bytes so the WAL is genuinely non-trivial.
// A snapshot of a database whose WAL happened to be empty proves nothing about
// whether the checkpoint ran.
func fillKV(t *testing.T, st *sqliteStore, n int, payload int) {
	t.Helper()
	value := make([]byte, payload)
	for i := range value {
		value[i] = byte('a' + i%26)
	}
	for i := 0; i < n; i++ {
		if err := st.Put(fmt.Sprintf("k%06d", i), value); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
}

// openStandalone opens path with no sidecars available, which is the whole
// point of the exercise: a restored snapshot arrives on a host that has never
// seen the -wal or -shm files.
func openStandalone(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func countKV(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM kv`).Scan(&n); err != nil {
		t.Fatalf("count kv: %v", err)
	}
	return n
}

// The load-bearing case: everything committed into the WAL must survive into a
// snapshot that is then read with no sidecars beside it, and the checkpoint
// must leave the live file in the same condition.
func TestSnapshotIsRestorableWithoutSidecars(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "store.db")
	st, err := openSQLite(live, DefaultSQLiteOptions())
	if err != nil {
		t.Fatalf("openSQLite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const rows = 2000
	fillKV(t, st, rows, 512)

	// Prove the premise: without a checkpoint, a large share of the data is in
	// the WAL and not in store.db at all.
	walInfo, err := os.Stat(live + "-wal")
	if err != nil {
		t.Fatalf("expected a -wal beside a WAL-mode database: %v", err)
	}
	if walInfo.Size() == 0 {
		t.Fatal("WAL is empty; this test would not prove the checkpoint runs")
	}

	dest := filepath.Join(t.TempDir(), "snap.db")
	ck, err := st.snapshotTo(dest)
	if err != nil {
		t.Fatalf("snapshotTo: %v", err)
	}
	if ck.Busy {
		t.Errorf("checkpoint reported busy with no concurrent reader")
	}

	// The snapshot must stand alone.
	for _, sidecar := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(dest + sidecar); err == nil {
			t.Errorf("snapshot produced a %s sidecar; the uploaded file would not be self-contained", sidecar)
		}
	}

	if got := countKV(t, openStandalone(t, dest)); got != rows {
		t.Errorf("snapshot holds %d rows, want %d — WAL contents were lost", got, rows)
	}

	// The checkpoint also has to leave the *live* database self-contained,
	// which is what bounds -wal growth over a long session.
	if walInfo, err := os.Stat(live + "-wal"); err == nil && walInfo.Size() != 0 {
		t.Errorf("live WAL is %d bytes after a TRUNCATE checkpoint, want 0", walInfo.Size())
	}
}

// Demonstrates why the checkpoint is not optional: the naive approach — copy
// store.db and upload it — loses every transaction still in the WAL, and does
// so silently, producing a database that opens cleanly and is simply missing
// data. This is the failure mode the checkpoint exists to prevent.
func TestUncheckpointedCopyLosesCommittedRows(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "store.db")
	st, err := openSQLite(live, DefaultSQLiteOptions())
	if err != nil {
		t.Fatalf("openSQLite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const rows = 2000
	fillKV(t, st, rows, 512)

	// Byte-for-byte copy of the main database file only, exactly what an
	// implementation that skipped the checkpoint would upload.
	raw, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	naive := filepath.Join(t.TempDir(), "naive.db")
	if err := os.WriteFile(naive, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	db := openStandalone(t, naive)
	var got int
	if err := db.QueryRow(`SELECT count(*) FROM kv`).Scan(&got); err != nil {
		// An empty main file with no kv table at all is the same failure, just
		// louder. Either way it is not the database that was committed.
		got = -1
	}
	if got == rows {
		t.Skip("this filesystem checkpointed the WAL on its own; the premise cannot be demonstrated here")
	}
	t.Logf("uncheckpointed copy holds %d of %d committed rows", got, rows)

	// And the checkpointed snapshot of the very same handle holds all of them.
	dest := filepath.Join(t.TempDir(), "snap.db")
	if _, err := st.snapshotTo(dest); err != nil {
		t.Fatalf("snapshotTo: %v", err)
	}
	if n := countKV(t, openStandalone(t, dest)); n != rows {
		t.Errorf("checkpointed snapshot holds %d rows, want %d", n, rows)
	}
}

// A plugin committing while the snapshot runs must not tear it. This is why the
// snapshot is a VACUUM INTO rather than a file copy taken after the checkpoint:
// with the WAL just truncated, a torn copy is corrupt rather than merely stale.
func TestSnapshotUnderConcurrentWriter(t *testing.T) {
	dir := t.TempDir()
	st, err := openSQLite(filepath.Join(dir, "store.db"), DefaultSQLiteOptions())
	if err != nil {
		t.Fatalf("openSQLite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fillKV(t, st, 500, 256)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = st.Put(fmt.Sprintf("live%06d", i), []byte("churn"))
		}
	}()

	dest := filepath.Join(t.TempDir(), "snap.db")
	_, snapErr := st.snapshotTo(dest)
	close(stop)
	wg.Wait()
	if snapErr != nil {
		t.Fatalf("snapshotTo under a concurrent writer: %v", snapErr)
	}

	db := openStandalone(t, dest)
	var res string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&res); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if res != "ok" {
		t.Errorf("integrity_check = %q, want %q", res, "ok")
	}
	if got := countKV(t, db); got < 500 {
		t.Errorf("snapshot holds %d rows, want at least the 500 written before it started", got)
	}
}

// VACUUM INTO refuses an existing destination, which is the property that makes
// snapshotting over a live database impossible by construction.
func TestSnapshotRefusesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	st, err := openSQLite(filepath.Join(dir, "store.db"), DefaultSQLiteOptions())
	if err != nil {
		t.Fatalf("openSQLite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	dest := filepath.Join(dir, "snap.db")
	if err := os.WriteFile(dest, []byte("occupied"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.snapshotTo(dest); err == nil {
		t.Error("snapshotTo overwrote an existing file, want an error")
	}
}

// Manager.Snapshot covers exactly the handles at the requested scope, and lays
// them out one directory per plugin so a caller can map each back onto its
// place in the tree.
func TestManagerSnapshotScoping(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "sessions", "sess-1")
	m := NewManager(root, "", func() string { return sessionDir }, nil)
	t.Cleanup(func() { _ = m.Close() })

	sessionStore, err := m.Open(ScopeSession, "nexus.test.session")
	if err != nil {
		t.Fatalf("open session scope: %v", err)
	}
	if err := sessionStore.Put("a", []byte("1")); err != nil {
		t.Fatal(err)
	}
	appStore, err := m.Open(ScopeApp, "nexus.test.app")
	if err != nil {
		t.Fatalf("open app scope: %v", err)
	}
	if err := appStore.Put("b", []byte("2")); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "stage")
	snaps, err := m.Snapshot(ScopeSession, dest)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("Snapshot returned %d entries, want only the session-scope handle", len(snaps))
	}
	got := snaps[0]
	if got.PluginID != "nexus.test.session" {
		t.Errorf("PluginID = %q", got.PluginID)
	}
	wantLive := filepath.Join(sessionDir, "plugins", "nexus.test.session", "store.db")
	if got.LivePath != wantLive {
		t.Errorf("LivePath = %q, want %q", got.LivePath, wantLive)
	}
	if want := filepath.Join(dest, "nexus.test.session", "store.db"); got.Path != want {
		t.Errorf("Path = %q, want %q", got.Path, want)
	}
	if got.Bytes <= 0 {
		t.Errorf("Bytes = %d, want a real size", got.Bytes)
	}
	if got.Duration <= 0 {
		t.Errorf("Duration = %v, want a measured cost", got.Duration)
	}
	if _, err := os.Stat(got.Path); err != nil {
		t.Errorf("snapshot file missing: %v", err)
	}

	// App scope is untouched by a session-scope snapshot.
	if _, err := os.Stat(filepath.Join(dest, "nexus.test.app")); err == nil {
		t.Error("session-scope snapshot also wrote the app-scope handle")
	}
}

// ScopeAgent collapses to ScopeApp on an engine with no agent ID, exactly as
// Open does — otherwise Snapshot(ScopeAgent) would quietly find nothing.
func TestManagerSnapshotAgentScopeCollapse(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root, "", nil, nil)
	t.Cleanup(func() { _ = m.Close() })

	st, err := m.Open(ScopeAgent, "nexus.test.agent")
	if err != nil {
		t.Fatalf("open agent scope: %v", err)
	}
	if err := st.Put("a", []byte("1")); err != nil {
		t.Fatal(err)
	}

	snaps, err := m.Snapshot(ScopeAgent, filepath.Join(t.TempDir(), "stage"))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("Snapshot(ScopeAgent) returned %d entries, want 1 after the collapse to app scope", len(snaps))
	}
	if snaps[0].Scope != ScopeApp {
		t.Errorf("Scope = %v, want the collapsed ScopeApp", snaps[0].Scope)
	}
}

// Checkpoint is usable on its own, and reports per plugin.
func TestManagerCheckpoint(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root, "", nil, nil)
	t.Cleanup(func() { _ = m.Close() })

	st, err := m.Open(ScopeApp, "nexus.test.app")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < 200; i++ {
		if err := st.Put(fmt.Sprintf("k%03d", i), make([]byte, 256)); err != nil {
			t.Fatal(err)
		}
	}
	res, err := m.Checkpoint(ScopeApp)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if _, ok := res["nexus.test.app"]; !ok {
		t.Fatalf("Checkpoint result missing the open handle: %v", res)
	}
	wal := filepath.Join(root, "plugins", "nexus.test.app", "store.db-wal")
	if info, err := os.Stat(wal); err == nil && info.Size() != 0 {
		t.Errorf("WAL is %d bytes after Checkpoint, want 0", info.Size())
	}
}

func TestSnapshotRequiresDestination(t *testing.T) {
	m := NewManager(t.TempDir(), "", nil, nil)
	t.Cleanup(func() { _ = m.Close() })
	if _, err := m.Snapshot(ScopeApp, ""); err == nil {
		t.Error("Snapshot with no destination = nil, want an error")
	}
}

// BenchmarkSnapshot measures what a turn-boundary snapshot of one store.db
// costs, as a function of database size. This is the story's required
// measurement: the cost is O(database size) and is paid on every turn
// regardless of how little changed.
//
//	go test ./pkg/engine/storage/ -run '^$' -bench BenchmarkSnapshot -benchmem
func BenchmarkSnapshot(b *testing.B) {
	for _, rows := range []int{1_000, 10_000, 50_000, 200_000} {
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			dir := b.TempDir()
			live := filepath.Join(dir, "store.db")
			st, err := openSQLite(live, DefaultSQLiteOptions())
			if err != nil {
				b.Fatal(err)
			}
			defer st.Close()

			value := make([]byte, 512)
			for i := 0; i < rows; i++ {
				if err := st.Put(fmt.Sprintf("k%08d", i), value); err != nil {
					b.Fatal(err)
				}
			}
			if _, err := st.checkpoint(); err != nil {
				b.Fatal(err)
			}
			info, err := os.Stat(live)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(info.Size())
			b.ReportMetric(float64(info.Size()), "db_bytes")

			stage := b.TempDir()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dest := filepath.Join(stage, fmt.Sprintf("snap-%d.db", i))
				if _, err := st.snapshotTo(dest); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				_ = os.Remove(dest)
				b.StartTimer()
			}
		})
	}
}
