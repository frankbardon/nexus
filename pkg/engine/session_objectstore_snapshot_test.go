package engine

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/blobs"
	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/engine/objectstore/objectstoretest"
	"github.com/frankbardon/nexus/pkg/engine/storage"
	"github.com/frankbardon/nexus/pkg/events"

	_ "modernc.org/sqlite"
)

// newMemoryObjectStoreEngine wires an engine over the shared in-memory backend
// from objectstoretest, which is the reference implementation the contract
// suite holds every real backend to. Using it rather than another local double
// means these tests exercise the same key validation and prefix semantics a GCS
// or S3 module will.
func newMemoryObjectStoreEngine(t *testing.T, policy objectstore.FailurePolicy) (*Engine, *objectstoretest.Memory) {
	t.Helper()
	root := t.TempDir()
	backend := objectstoretest.NewMemory()
	name := "memory-" + t.Name()
	objectstore.Register(name, func(context.Context, objectstore.Config) (objectstore.Backend, error) {
		return backend, nil
	})
	t.Cleanup(func() { objectstore.Unregister(name) })

	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = filepath.Join(root, "sessions")
	cfg.Core.Storage.Root = root
	if policy == "" {
		policy = objectstore.FailurePolicyDegrade
	}
	cfg.Core.ObjectStore = objectstore.Config{
		BackendName:   name,
		Bucket:        "test-bucket",
		FailurePolicy: policy,
	}
	return newFromConfig(cfg), backend
}

// endTurn emits the turn boundary the engine snapshots on. Synchronous
// dispatch means the snapshot has completed by the time this returns, which is
// the property the whole design rests on.
func endTurn(t *testing.T, eng *Engine, turnID string) {
	t.Helper()
	if err := eng.Bus.Emit("agent.turn.end", events.TurnInfo{
		SchemaVersion: events.TurnInfoVersion,
		TurnID:        turnID,
	}); err != nil {
		t.Fatalf("emit agent.turn.end: %v", err)
	}
}

func sessionKeys(backend *objectstoretest.Memory, sessionID string) []string {
	prefix := "sessions/" + sessionID + "/"
	var out []string
	for _, k := range backend.Keys() {
		if strings.HasPrefix(k, prefix) {
			out = append(out, strings.TrimPrefix(k, prefix))
		}
	}
	return out
}

func readMarker(t *testing.T, backend *objectstoretest.Memory, sessionID string) sessionSnapshotMarker {
	t.Helper()
	body, ok := backend.Get(sessionSnapshotMarkerKey(sessionID))
	if !ok {
		t.Fatalf("no commit marker at %q; keys = %v", sessionSnapshotMarkerKey(sessionID), backend.Keys())
	}
	var m sessionSnapshotMarker
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal commit marker: %v", err)
	}
	return m
}

// The headline behaviour: a completed turn leaves the whole tree in the store,
// under the session's key prefix, with a commit marker naming it.
func TestTurnBoundarySnapshotUploadsTree(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	sessionID := eng.Session.ID

	if err := eng.Session.WriteFile("files/report.md", []byte("hello")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// The owner marker is the one object Boot writes ahead of a turn boundary
	// (session_owner.go). It is a diagnostic sibling of the tree, not session
	// state, so nothing the session itself produced may be in the store yet.
	if got := backend.Keys(); len(got) != 1 || got[0] != sessionOwnerMarkerKey(sessionID) {
		t.Fatalf("objects uploaded before any turn boundary: %v", got)
	}

	var result events.SessionSnapshotResult
	unsub := eng.Bus.Subscribe("session.snapshot.result", func(ev Event[any]) {
		result, _ = ev.Payload.(events.SessionSnapshotResult)
	})
	defer unsub()

	endTurn(t, eng, "turn-1")

	keys := sessionKeys(backend, sessionID)
	if len(keys) == 0 {
		t.Fatalf("turn boundary uploaded nothing; all keys = %v", backend.Keys())
	}
	want := map[string]bool{
		"metadata/session.json": false,
		"files/report.md":       false,
		"journal/header.json":   false,
		"journal/events.jsonl":  false,
	}
	for _, k := range keys {
		if _, ok := want[k]; ok {
			want[k] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("%q not uploaded; got %v", k, keys)
		}
	}
	if got, _ := backend.Get("sessions/" + sessionID + "/files/report.md"); string(got) != "hello" {
		t.Errorf("uploaded report.md = %q, want %q", got, "hello")
	}

	marker := readMarker(t, backend, sessionID)
	if marker.SessionID != sessionID {
		t.Errorf("marker session = %q, want %q", marker.SessionID, sessionID)
	}
	if marker.Sequence != 1 {
		t.Errorf("marker sequence = %d, want 1", marker.Sequence)
	}
	if marker.Trigger != snapshotTriggerTurn || marker.TurnID != "turn-1" {
		t.Errorf("marker trigger/turn = %q/%q, want %q/%q", marker.Trigger, marker.TurnID, snapshotTriggerTurn, "turn-1")
	}
	if marker.Objects != len(keys) {
		t.Errorf("marker objects = %d, want %d", marker.Objects, len(keys))
	}

	// The result event is the cost measurement; zeros here would mean the
	// numbers an operator watches are fiction.
	if !result.OK || result.Trigger != snapshotTriggerTurn || result.TurnID != "turn-1" {
		t.Errorf("snapshot result = %+v", result)
	}
	if result.Objects != marker.Objects || result.Bytes != marker.Bytes {
		t.Errorf("result objects/bytes = %d/%d, marker says %d/%d",
			result.Objects, result.Bytes, marker.Objects, marker.Bytes)
	}
	if result.Bytes <= 0 {
		t.Errorf("result bytes = %d, want the uploaded total", result.Bytes)
	}
	if result.DurationMs <= 0 {
		t.Errorf("result duration = %v ms, want a measured value", result.DurationMs)
	}
	if result.Sequence != 1 {
		t.Errorf("result sequence = %d, want 1", result.Sequence)
	}
}

// The commit marker is deliberately a *sibling* of the session tree, not a
// member of it: it must never hydrate back into the session and become an input
// to the next snapshot. That works only because prefixes match whole segments.
func TestSnapshotMarkerIsOutsideTheSessionPrefix(t *testing.T) {
	key := sessionSnapshotMarkerKey("sess-1")
	if err := objectstore.ValidateKey(key); err != nil {
		t.Fatalf("marker key %q is not a valid object key: %v", key, err)
	}
	if _, under := objectstore.TrimKeyPrefix(key, sessionObjectKeyPrefix("sess-1")); under {
		t.Errorf("marker key %q is under the session prefix; it would hydrate into the tree", key)
	}
}

// FR-13's exclusion half, end to end: the lock never leaves, and neither do the
// SQLite sidecars.
func TestSnapshotExcludesLockAndSidecars(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	// The lock is written by Boot; assert the premise rather than trusting it.
	if _, err := os.Stat(filepath.Join(eng.Session.RootDir, sessionLockFilename)); err != nil {
		t.Fatalf("expected a session lock on disk: %v", err)
	}
	// A session-scope store with enough writes to guarantee a -wal on disk.
	st, err := eng.Storage.Open(storage.ScopeSession, "nexus.test.snapshot")
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	for i := 0; i < 500; i++ {
		if err := st.Put(fmt.Sprintf("k%04d", i), make([]byte, 256)); err != nil {
			t.Fatal(err)
		}
	}
	walPath := filepath.Join(eng.Session.RootDir, "plugins", "nexus.test.snapshot", "store.db-wal")
	if _, err := os.Stat(walPath); err != nil {
		t.Fatalf("expected a -wal beside the session store: %v", err)
	}

	endTurn(t, eng, "turn-1")

	for _, k := range sessionKeys(backend, eng.Session.ID) {
		if k == sessionLockFilename {
			t.Error("session.lock was uploaded; every rehydrated session would look locked")
		}
		if strings.HasSuffix(k, "-wal") || strings.HasSuffix(k, "-shm") {
			t.Errorf("SQLite sidecar %q was uploaded", k)
		}
	}
}

// The story's central correctness claim: the uploaded store.db opens and reads
// on a host that has never seen its sidecars.
func TestSnapshotUploadsRestorableStoreDB(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	st, err := eng.Storage.Open(storage.ScopeSession, "nexus.test.snapshot")
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	const rows = 1000
	for i := 0; i < rows; i++ {
		if err := st.Put(fmt.Sprintf("k%05d", i), make([]byte, 256)); err != nil {
			t.Fatal(err)
		}
	}

	endTurn(t, eng, "turn-1")

	key := "sessions/" + eng.Session.ID + "/plugins/nexus.test.snapshot/store.db"
	body, ok := backend.Get(key)
	if !ok {
		t.Fatalf("store.db not uploaded; keys = %v", sessionKeys(backend, eng.Session.ID))
	}

	// Restore it the way a hydration would: the file alone, nothing beside it.
	restored := filepath.Join(t.TempDir(), "store.db")
	if err := os.WriteFile(restored, body, 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+restored+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	defer db.Close()

	var check string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if check != "ok" {
		t.Fatalf("integrity_check = %q on the restored database", check)
	}
	var got int
	if err := db.QueryRow(`SELECT count(*) FROM kv`).Scan(&got); err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != rows {
		t.Errorf("restored database holds %d rows, want %d — the WAL checkpoint did not run", got, rows)
	}
}

// FR-18: a failed upload must leave the previous good state restorable. The
// commit marker is what makes "previous good state" a thing a reader can
// identify, so it must not advance past a snapshot that did not complete.
func TestFailedSnapshotDoesNotAdvanceCommitMarker(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	if err := eng.Session.WriteFile("files/good.md", []byte("first turn")); err != nil {
		t.Fatal(err)
	}
	endTurn(t, eng, "turn-1")
	first := readMarker(t, backend, eng.Session.ID)
	if first.Sequence != 1 {
		t.Fatalf("first marker sequence = %d, want 1", first.Sequence)
	}

	var results []events.SessionSnapshotResult
	unsub := eng.Bus.Subscribe("session.snapshot.result", func(ev Event[any]) {
		if r, ok := ev.Payload.(events.SessionSnapshotResult); ok {
			results = append(results, r)
		}
	})
	defer unsub()

	backend.SetPutError(errors.New("bucket unreachable"))
	if err := eng.Session.WriteFile("files/lost.md", []byte("second turn")); err != nil {
		t.Fatal(err)
	}
	endTurn(t, eng, "turn-2")
	backend.SetPutError(nil)

	if len(results) != 1 || results[0].OK {
		t.Fatalf("snapshot results = %+v, want exactly one failure", results)
	}
	after := readMarker(t, backend, eng.Session.ID)
	if after.Sequence != first.Sequence || after.TurnID != first.TurnID {
		t.Errorf("commit marker advanced past a failed snapshot: %+v", after)
	}
	if _, ok := backend.Get("sessions/" + eng.Session.ID + "/files/good.md"); !ok {
		t.Error("the previous good snapshot's content was removed by the failed one")
	}
}

// Under degrade a failed snapshot must not stop the run; under strict it must
// be loud. Neither policy may panic or block the turn boundary.
func TestSnapshotFailurePolicy(t *testing.T) {
	for _, tc := range []struct {
		policy    objectstore.FailurePolicy
		wantError bool
	}{
		{objectstore.FailurePolicyDegrade, false},
		{objectstore.FailurePolicyStrict, true},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			eng, backend := newMemoryObjectStoreEngine(t, tc.policy)
			if err := eng.Boot(context.Background()); err != nil {
				t.Fatalf("Boot: %v", err)
			}
			t.Cleanup(func() { _ = eng.Stop(context.Background()) })

			var coreErrors int
			unsub := eng.Bus.Subscribe("core.error", func(ev Event[any]) {
				if info, ok := ev.Payload.(events.ErrorInfo); ok && info.Source == "nexus.engine.objectstore" {
					coreErrors++
				}
			})
			defer unsub()

			backend.SetFlushError(errors.New("bucket unreachable"))
			endTurn(t, eng, "turn-1")
			backend.SetFlushError(nil)

			if tc.wantError && coreErrors == 0 {
				t.Error("strict policy swallowed a failed snapshot")
			}
			if !tc.wantError && coreErrors != 0 {
				t.Errorf("degrade policy raised %d core.error events, want 0", coreErrors)
			}
		})
	}
}

// session.snapshot.request is the documented escape hatch for callers that do
// not emit agent.turn.end.
func TestSnapshotOnRequestEvent(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	if err := eng.Bus.Emit("session.snapshot.request", events.SessionSnapshotRequest{
		SchemaVersion: events.SessionSnapshotRequestVersion,
		Reason:        "embedder checkpoint",
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	marker := readMarker(t, backend, eng.Session.ID)
	if marker.Trigger != snapshotTriggerRequest {
		t.Errorf("marker trigger = %q, want %q", marker.Trigger, snapshotTriggerRequest)
	}
}

// Clean shutdown takes a final snapshot, so a session that ended between turns
// is still restorable — and one that ran no turns at all is in the bucket at
// all.
func TestShutdownSnapshotsTheTree(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	sessionID := eng.Session.ID
	if err := eng.Session.WriteFile("files/final.md", []byte("after the last turn")); err != nil {
		t.Fatal(err)
	}
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got, ok := backend.Get("sessions/" + sessionID + "/files/final.md"); !ok || string(got) != "after the last turn" {
		t.Errorf("shutdown snapshot missed files/final.md (ok=%v, content=%q)", ok, got)
	}
	marker := readMarker(t, backend, sessionID)
	if marker.Trigger != snapshotTriggerShutdown {
		t.Errorf("marker trigger = %q, want %q", marker.Trigger, snapshotTriggerShutdown)
	}
}

// A snapshot leaves no staging directory behind, on success or on failure.
func TestSnapshotCleansUpStaging(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	sessionsRoot := ExpandPath(eng.Config.Core.Sessions.Root)

	endTurn(t, eng, "turn-1")
	backend.SetPutError(errors.New("nope"))
	endTurn(t, eng, "turn-2")
	backend.SetPutError(nil)

	entries, err := os.ReadDir(sessionsRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), snapshotStagingPrefix) {
			t.Errorf("snapshot staging directory left behind: %s", e.Name())
		}
	}
}

// Zero-impact default: with no backend configured nothing subscribes, so
// agent.turn.end costs exactly what it did before this code existed.
func TestNoObjectStoreInstallsNoSnapshotHandlers(t *testing.T) {
	subs := func(t *testing.T, eng *Engine) (typed int, wildcards int) {
		t.Helper()
		if err := eng.Boot(context.Background()); err != nil {
			t.Fatalf("Boot: %v", err)
		}
		t.Cleanup(func() { _ = eng.Stop(context.Background()) })
		bus, ok := eng.Bus.(*eventBus)
		if !ok {
			t.Skip("bus is not the default implementation")
		}
		bus.mu.RLock()
		defer bus.mu.RUnlock()
		return len(bus.handlers["session.snapshot.request"]), len(bus.wildcards)
	}

	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = filepath.Join(root, "sessions")
	cfg.Core.Storage.Root = root
	plain := newFromConfig(cfg)
	plainTyped, plainWildcards := subs(t, plain)

	if plainTyped != 0 {
		t.Errorf("session.snapshot.request has %d handlers with no backend configured, want 0", plainTyped)
	}
	// The request event is inert rather than an error.
	if err := plain.Bus.Emit("session.snapshot.request", events.SessionSnapshotRequest{
		SchemaVersion: events.SessionSnapshotRequestVersion,
	}); err != nil {
		t.Errorf("emitting a snapshot request with no backend = %v, want nil", err)
	}

	withStore, _ := newMemoryObjectStoreEngine(t, "")
	storeTyped, storeWildcards := subs(t, withStore)
	if storeTyped != 1 {
		t.Errorf("session.snapshot.request has %d handlers with a backend, want 1", storeTyped)
	}
	if storeWildcards != plainWildcards+1 {
		t.Errorf("wildcard handlers: %d with a backend vs %d without, want exactly one more",
			storeWildcards, plainWildcards)
	}
}

// The snapshot must capture the agent.turn.end envelope of the very turn it is
// reacting to. Without it, journal.Coordinator reads the restored journal as an
// unfinished turn and crash resume re-runs a turn that already completed — on
// every single resume.
func TestSnapshotCapturesItsOwnTurnEnd(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	endTurn(t, eng, "turn-1")

	body, ok := backend.Get("sessions/" + eng.Session.ID + "/journal/events.jsonl")
	if !ok {
		t.Fatalf("active journal segment not uploaded; keys = %v", sessionKeys(backend, eng.Session.ID))
	}
	found := false
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" {
			continue
		}
		var env struct {
			Type    string `json:"type"`
			Payload struct {
				TurnID string `json:"TurnID"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("unmarshal journal line: %v", err)
		}
		if env.Type == "agent.turn.end" && env.Payload.TurnID == "turn-1" {
			found = true
		}
	}
	if !found {
		t.Error("the uploaded journal does not contain the agent.turn.end that triggered the snapshot")
	}
}

// The snapshot runs after every other agent.turn.end subscriber, so it captures
// the writes that boundary produces rather than the state before them.
func TestSnapshotRunsAfterOtherTurnEndHandlers(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	// A plugin-shaped subscriber writing at the boundary, at a priority far
	// higher than any shipped plugin uses.
	unsub := eng.Bus.Subscribe("agent.turn.end", func(Event[any]) {
		_ = eng.Session.WriteFile("files/written-at-turn-end.md", []byte("late"))
	}, WithPriority(1<<20))
	defer unsub()

	endTurn(t, eng, "turn-1")

	if _, ok := backend.Get("sessions/" + eng.Session.ID + "/files/written-at-turn-end.md"); !ok {
		t.Error("the snapshot ran before another turn-end handler's write")
	}
}

// Concurrent snapshots must serialise rather than interleave, or two markers
// would describe half-overwritten trees.
func TestConcurrentSnapshotsSerialise(t *testing.T) {
	eng, backend := newMemoryObjectStoreEngine(t, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	const n = 4
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			_ = eng.Bus.Emit("session.snapshot.request", events.SessionSnapshotRequest{
				SchemaVersion: events.SessionSnapshotRequestVersion,
				Reason:        fmt.Sprintf("racer-%d", i),
			})
		}(i)
	}
	deadline := time.After(30 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-done:
		case <-deadline:
			t.Fatal("concurrent snapshots did not finish")
		}
	}

	if marker := readMarker(t, backend, eng.Session.ID); marker.Sequence != n {
		t.Errorf("marker sequence = %d after %d snapshots, want %d", marker.Sequence, n, n)
	}
}

// BenchmarkSessionSnapshot measures the whole-turn snapshot cost against the
// in-memory backend, which isolates the engine's own work (staging, the WAL
// checkpoint, the tree walk, the per-object handoff) from network time.
//
// E1-S4 required this measurement and the shape it revealed was the point: the
// cost was O(tree size), paid in full on every turn however little changed.
// E2-S2 added the immutable-skip, so the "immutable" cases below are the
// before/after: they hold the same bytes as their "files" siblings but as
// content-addressed blobs, which cannot change once written and are therefore
// uploaded exactly once per session rather than once per turn.
//
// The "files" cases are unchanged by immutable-skip and are kept precisely for
// that reason — ordinary session output is still O(tree size) per turn, and
// pretending otherwise by only benchmarking the improved shape would be the
// dishonest version of this measurement. Delta upload for mutable files and a
// size-dependent cadence remain deliberately undesigned; see the notes on
// E1-S4.
//
// puts/op is the headline: how many objects one turn actually transfers.
//
//	go test ./pkg/engine/ -run '^$' -bench BenchmarkSessionSnapshot -benchtime 10x
func BenchmarkSessionSnapshot(b *testing.B) {
	cases := []struct {
		name    string
		files   int
		size    int
		dbRows  int
		dbValue int
		// immutable writes the payload as content-addressed blobs rather than
		// as ordinary files under files/, which is the composition
		// immutable-skip applies to.
		immutable bool
	}{
		{name: "small/10files-1KB/db-100rows", files: 10, size: 1 << 10, dbRows: 100, dbValue: 256},
		{name: "medium/200files-16KB/db-5krows", files: 200, size: 16 << 10, dbRows: 5_000, dbValue: 512},
		{name: "large/1000files-64KB/db-50krows", files: 1000, size: 64 << 10, dbRows: 50_000, dbValue: 512},
		{name: "large-immutable/1000blobs-64KB/db-50krows", files: 1000, size: 64 << 10, dbRows: 50_000, dbValue: 512, immutable: true},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			root := b.TempDir()
			backend := objectstoretest.NewMemory()
			name := "bench-" + tc.name
			objectstore.Register(name, func(context.Context, objectstore.Config) (objectstore.Backend, error) {
				return backend, nil
			})
			defer objectstore.Unregister(name)

			cfg := DefaultConfig()
			cfg.Core.Sessions.Root = filepath.Join(root, "sessions")
			cfg.Core.Storage.Root = root
			cfg.Core.ObjectStore = objectstore.Config{
				BackendName:   name,
				Bucket:        "bench",
				FailurePolicy: objectstore.FailurePolicyDegrade,
			}
			eng := newFromConfig(cfg)
			if err := eng.Boot(context.Background()); err != nil {
				b.Fatalf("Boot: %v", err)
			}
			defer func() { _ = eng.Stop(context.Background()) }()

			if tc.immutable {
				blobStore, err := blobs.New(eng.Session.BlobsDir(), 0)
				if err != nil {
					b.Fatal(err)
				}
				payload := make([]byte, tc.size)
				for i := 0; i < tc.files; i++ {
					// Distinct bytes per blob, or every Put would collapse onto
					// one sha and the tree would hold a single object.
					binary.LittleEndian.PutUint64(payload, uint64(i))
					if _, err := blobStore.Put(payload, "application/octet-stream"); err != nil {
						b.Fatal(err)
					}
				}
			} else {
				blob := make([]byte, tc.size)
				for i := 0; i < tc.files; i++ {
					if err := eng.Session.WriteFile(fmt.Sprintf("files/f%05d.bin", i), blob); err != nil {
						b.Fatal(err)
					}
				}
			}
			st, err := eng.Storage.Open(storage.ScopeSession, "nexus.bench.store")
			if err != nil {
				b.Fatal(err)
			}
			value := make([]byte, tc.dbValue)
			for i := 0; i < tc.dbRows; i++ {
				if err := st.Put(fmt.Sprintf("k%08d", i), value); err != nil {
					b.Fatal(err)
				}
			}

			// Warm the numbers up once so the reported per-op figure is a
			// steady-state turn rather than a first-write.
			endTurnB(b, eng, "warmup")
			var bytes int64
			for _, k := range backend.Keys() {
				if v, ok := backend.Get(k); ok {
					bytes += int64(len(v))
				}
			}
			b.SetBytes(bytes)
			putsBefore := backend.Counts().Puts

			// Bytes actually transferred per turn is the number that decides
			// what a turn costs on a real link; the in-memory backend makes an
			// upload almost free, so ns/op understates the saving badly.
			var uploaded int64
			unsub := eng.Bus.Subscribe("session.snapshot.result", func(ev Event[any]) {
				if r, ok := ev.Payload.(events.SessionSnapshotResult); ok {
					uploaded += r.BytesUploaded
				}
			})
			defer unsub()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				endTurnB(b, eng, fmt.Sprintf("turn-%d", i))
			}
			b.StopTimer()
			puts := backend.Counts().Puts - putsBefore
			// Reported after the loop because ResetTimer deletes
			// user-reported metrics.
			b.ReportMetric(float64(len(backend.Keys())), "objects")
			b.ReportMetric(float64(bytes)/(1<<20), "tree_MiB")
			b.ReportMetric(float64(puts)/float64(b.N), "puts/op")
			b.ReportMetric(float64(uploaded)/float64(b.N)/(1<<20), "upload_MiB/op")
			// The cost E1-S4's marker design refused and E3-S5 accepted: the
			// per-object manifest is re-uploaded whole on every snapshot, so
			// its size is a per-turn cost that grows with the object count.
			// Reported beside the numbers it has to be judged against.
			if body, ok := backend.Get(sessionManifestKey(eng.Session.ID)); ok {
				b.ReportMetric(float64(len(body))/(1<<10), "manifest_KiB")
			}
		})
	}
}

func endTurnB(b *testing.B, eng *Engine, turnID string) {
	b.Helper()
	if err := eng.Bus.Emit("agent.turn.end", events.TurnInfo{
		SchemaVersion: events.TurnInfoVersion,
		TurnID:        turnID,
	}); err != nil {
		b.Fatalf("emit agent.turn.end: %v", err)
	}
}
