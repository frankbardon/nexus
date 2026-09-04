package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/objectstore"
)

// scriptedBackend is a local test double for the seam. It is deliberately NOT
// the shared in-memory backend (that lands with its own contract suite) — it
// only records what the engine asked for and replays whatever the test told it
// to produce, which is the house pattern the fake commandRunner in
// cmd/nexus-broker established.
type scriptedBackend struct {
	// hydrate, when set, is run with the staging directory the engine chose.
	// A test uses it to materialise a tree, to fail, or to fail *after*
	// materialising a tree (the partial-hydration case).
	hydrate func(keyPrefix string, destDir string) error
	// flushErr is returned by every Flush.
	flushErr error

	mu            sync.Mutex
	hydratePrefix []string
	flushes       int
	closes        int
}

func (b *scriptedBackend) Hydrate(_ context.Context, keyPrefix string, destDir string) error {
	b.mu.Lock()
	b.hydratePrefix = append(b.hydratePrefix, keyPrefix)
	b.mu.Unlock()
	if b.hydrate == nil {
		return nil
	}
	return b.hydrate(keyPrefix, destDir)
}

func (b *scriptedBackend) Put(context.Context, string, string) error { return nil }
func (b *scriptedBackend) Delete(context.Context, string) error      { return nil }
func (b *scriptedBackend) List(context.Context, string) ([]objectstore.Object, error) {
	return nil, nil
}

func (b *scriptedBackend) Flush(context.Context) error {
	b.mu.Lock()
	b.flushes++
	b.mu.Unlock()
	return b.flushErr
}

// Close proves the engine honours the optional io.Closer rather than requiring
// a Close method on objectstore.Backend itself.
func (b *scriptedBackend) Close() error {
	b.mu.Lock()
	b.closes++
	b.mu.Unlock()
	return nil
}

func (b *scriptedBackend) counts() (hydrates, flushes, closes int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.hydratePrefix), b.flushes, b.closes
}

func (b *scriptedBackend) prefixes() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.hydratePrefix...)
}

// newObjectStoreEngine wires an engine whose session root is a temp dir and
// whose object store is the supplied double. Returns the sessions root so
// tests can assert on the tree the engine produced.
func newObjectStoreEngine(t *testing.T, b objectstore.Backend) (*Engine, string) {
	t.Helper()
	root := t.TempDir()
	sessionsRoot := filepath.Join(root, "sessions")

	// The registry is process-global and Register panics on a duplicate, so the
	// name is derived from the test and removed on cleanup. objectstore
	// deliberately exports Unregister only for this: leaking the name would
	// make a second run of the same test in one binary panic.
	name := "scripted-" + t.Name()
	objectstore.Register(name, func(context.Context, objectstore.Config) (objectstore.Backend, error) {
		return b, nil
	})
	t.Cleanup(func() { objectstore.Unregister(name) })

	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = sessionsRoot
	cfg.Core.Storage.Root = root
	cfg.Core.ObjectStore = objectstore.Config{
		BackendName:   name,
		Bucket:        "test-bucket",
		FailurePolicy: objectstore.FailurePolicyDegrade,
	}
	return newFromConfig(cfg), sessionsRoot
}

// writeSessionTree materialises a minimally valid session tree at dir, the way
// a real backend would after pulling objects down.
func writeSessionTree(t *testing.T, dir string, id string, extra map[string]string) {
	t.Helper()
	for _, sub := range []string{"context", "files", "plugins", "metadata"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	meta, err := json.Marshal(SessionMeta{ID: id, Status: "completed"})
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata", "session.json"), meta, 0o644); err != nil {
		t.Fatalf("write session.json: %v", err)
	}
	for rel, content := range extra {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

// stagingLeftovers returns any hydration staging directories still present.
// A failed hydration must leave none.
func stagingLeftovers(t *testing.T, sessionsRoot string) []string {
	t.Helper()
	entries, err := os.ReadDir(sessionsRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("reading sessions root: %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), hydrateStagingPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestObjectStoreExcludesSessionLock(t *testing.T) {
	if !objectStoreExcluded(sessionLockFilename) {
		t.Errorf("objectStoreExcluded(%q) = false; a synced lock makes every rehydrated session look locked", sessionLockFilename)
	}
	for _, keep := range []string{
		"metadata/session.json",
		"context/conversation.jsonl",
		"files/report.md",
		"plugins/nexus.scene/scene.jsonl",
		"journal/000001.jsonl",
		"plugins/nexus.gate.token_budget/notes.db",
		"files/wal-analysis.md",
	} {
		if objectStoreExcluded(keep) {
			t.Errorf("objectStoreExcluded(%q) = true; session data must cross the seam", keep)
		}
	}
}

// SQLite sidecars describe a machine, not a session. A -wal hydrated beside a
// store.db that was checkpointed and snapshotted separately would roll the
// database back to a state neither file ever held.
func TestObjectStoreExcludesSQLiteSidecars(t *testing.T) {
	for _, excluded := range []string{
		"plugins/nexus.gate.token_budget/store.db-wal",
		"plugins/nexus.gate.token_budget/store.db-shm",
		"plugins/nexus.gate.token_budget/store.db-journal",
	} {
		if !objectStoreExcluded(excluded) {
			t.Errorf("objectStoreExcluded(%q) = false; SQLite sidecars must never cross the seam", excluded)
		}
	}
	if objectStoreExcluded("plugins/nexus.gate.token_budget/store.db") {
		t.Error("objectStoreExcluded excluded store.db itself; the snapshot is what must be uploaded")
	}
}

func TestSessionObjectKeyPrefixIsStoreRelative(t *testing.T) {
	got := sessionObjectKeyPrefix("abc123")
	if got != "sessions/abc123" {
		t.Errorf("sessionObjectKeyPrefix = %q, want %q", got, "sessions/abc123")
	}
	if strings.HasPrefix(got, "/") || strings.HasSuffix(got, "/") {
		t.Errorf("key prefix %q is not store-relative", got)
	}
}

// The zero-impact default: no backend named means no handle, no hydration and
// no flush — the engine behaves exactly as it did before the seam existed.
func TestBootWithoutObjectStoreLeavesSeamInert(t *testing.T) {
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = filepath.Join(root, "sessions")
	cfg.Core.Storage.Root = root
	eng := newFromConfig(cfg)

	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if eng.objectStore != nil {
		t.Error("objectStore handle installed for a config that names no backend")
	}
	if eng.Session == nil {
		t.Fatal("Boot produced no session")
	}
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if eng.objectStore != nil {
		t.Error("objectStore handle present after Stop")
	}
}

// Hydration runs before the workspace is opened, so an entirely absent local
// tree is indistinguishable from one that never left.
func TestBootRecallHydratesBeforeSessionOpens(t *testing.T) {
	const sessionID = "sess-hydrate"
	backend := &scriptedBackend{
		hydrate: func(_ string, destDir string) error {
			writeSessionTree(t, destDir, sessionID, map[string]string{
				"files/report.md": "restored",
			})
			return nil
		},
	}
	eng, sessionsRoot := newObjectStoreEngine(t, backend)
	eng.RecallSessionID = sessionID

	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	if eng.Session == nil || eng.Session.ID != sessionID {
		t.Fatalf("session = %+v, want ID %q", eng.Session, sessionID)
	}
	if want := filepath.Join(sessionsRoot, sessionID); eng.Session.RootDir != want {
		t.Errorf("RootDir = %q, want %q", eng.Session.RootDir, want)
	}
	// The hydrated artifact must be readable through the ordinary local read
	// path — no faulting, no lazy fetch.
	data, err := os.ReadFile(filepath.Join(eng.Session.RootDir, "files", "report.md"))
	if err != nil {
		t.Fatalf("reading hydrated file: %v", err)
	}
	if string(data) != "restored" {
		t.Errorf("hydrated file = %q, want %q", data, "restored")
	}
	// Three hydrations, in this order: the session tree, the committed object
	// manifest that hydration prunes the tree against, then the owner marker
	// that split-brain detection reads before claiming the session. The tree
	// must come first, and the manifest must come before the tree is committed
	// to its real path — an object from an interrupted snapshot must never be
	// observable at the session root, even for an instant. The owner marker is
	// last because that read happens after the workspace is open and must never
	// be what a resume waits on.
	wantPrefixes := []string{
		sessionObjectKeyPrefix(sessionID),
		sessionManifestKeyPrefix(sessionID),
		sessionOwnerKeyPrefix(sessionID),
	}
	if got := backend.prefixes(); !slices.Equal(got, wantPrefixes) {
		t.Errorf("Hydrate called with %v, want %v", got, wantPrefixes)
	}
	if left := stagingLeftovers(t, sessionsRoot); len(left) != 0 {
		t.Errorf("staging directories left behind: %v", left)
	}
}

// An unknown session ID is a brand-new session that happens to have been named
// by the caller, not an error.
func TestBootRecallUnknownSessionYieldsEmptySession(t *testing.T) {
	const sessionID = "sess-unknown"
	backend := &scriptedBackend{} // hydrates nothing
	eng, sessionsRoot := newObjectStoreEngine(t, backend)
	eng.RecallSessionID = sessionID

	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	if eng.Session == nil || eng.Session.ID != sessionID {
		t.Fatalf("session = %+v, want ID %q", eng.Session, sessionID)
	}

	// Identical in shape to a session created locally from scratch.
	local, err := NewSessionWorkspace(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}
	for _, sub := range []string{"context", "files", "plugins", "metadata"} {
		if _, err := os.Stat(filepath.Join(local.RootDir, sub)); err != nil {
			t.Fatalf("local reference session missing %s: %v", sub, err)
		}
		if _, err := os.Stat(filepath.Join(eng.Session.RootDir, sub)); err != nil {
			t.Errorf("hydrated-empty session missing %s: %v", sub, err)
		}
	}
	meta, err := eng.Session.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	if meta.ID != sessionID {
		t.Errorf("metadata ID = %q, want %q", meta.ID, sessionID)
	}
	if left := stagingLeftovers(t, sessionsRoot); len(left) != 0 {
		t.Errorf("staging directories left behind: %v", left)
	}
}

// A hydration that dies partway must leave nothing the engine could mistake
// for a complete session.
func TestBootRecallDiscardsPartialHydration(t *testing.T) {
	const sessionID = "sess-partial"
	wantErr := errors.New("connection reset mid-stream")
	backend := &scriptedBackend{
		hydrate: func(_ string, destDir string) error {
			// Get some of the tree down, then fail — the exact shape that
			// would be catastrophic if it were published.
			writeSessionTree(t, destDir, sessionID, map[string]string{
				"files/half.md": "truncated",
			})
			return wantErr
		},
	}
	eng, sessionsRoot := newObjectStoreEngine(t, backend)
	eng.RecallSessionID = sessionID

	err := eng.Boot(context.Background())
	if err == nil {
		t.Fatal("Boot succeeded after a failed hydration")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Boot error = %v, want it to wrap %v", err, wantErr)
	}
	if _, statErr := os.Stat(filepath.Join(sessionsRoot, sessionID)); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("session directory exists after a failed hydration (stat err = %v)", statErr)
	}
	if left := stagingLeftovers(t, sessionsRoot); len(left) != 0 {
		t.Errorf("partial hydration left staging directories behind: %v", left)
	}
	// A failed boot must not leave the backend handle installed.
	if eng.objectStore != nil {
		t.Error("objectStore handle still installed after a failed Boot")
	}
	if _, _, closes := backend.counts(); closes != 1 {
		t.Errorf("backend Close calls = %d, want 1 after a failed Boot", closes)
	}
}

// A lock file that made it into the store must never be adopted: it names a
// PID on a machine this process has never met.
func TestBootRecallNeverAdoptsHydratedSessionLock(t *testing.T) {
	const sessionID = "sess-locked"
	backend := &scriptedBackend{
		hydrate: func(_ string, destDir string) error {
			writeSessionTree(t, destDir, sessionID, nil)
			// A lock naming *this* live process. If the engine adopted it,
			// acquireSessionLock would see a non-stale lock and refuse to boot.
			return WriteSessionLock(destDir, os.Getpid())
		},
	}
	eng, _ := newObjectStoreEngine(t, backend)
	eng.RecallSessionID = sessionID

	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot refused a session whose hydrated lock should have been scrubbed: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	// The lock present now is the one this engine wrote for itself.
	lock, err := ReadSessionLock(eng.Session.RootDir)
	if err != nil {
		t.Fatalf("ReadSessionLock: %v", err)
	}
	if lock.PID != os.Getpid() {
		t.Errorf("session lock PID = %d, want this process (%d)", lock.PID, os.Getpid())
	}
}

// A tree already on local disk is the live working copy and is not overwritten.
func TestHydrateSessionTreeSkipsWhenLocalTreeExists(t *testing.T) {
	const sessionID = "sess-local"
	backend := &scriptedBackend{
		hydrate: func(string, string) error {
			t.Error("Hydrate called even though a local tree was present")
			return nil
		},
	}
	eng, sessionsRoot := newObjectStoreEngine(t, backend)
	if err := eng.openObjectStore(context.Background()); err != nil {
		t.Fatalf("openObjectStore: %v", err)
	}
	dest := filepath.Join(sessionsRoot, sessionID)
	writeSessionTree(t, dest, sessionID, map[string]string{"files/local.md": "keep me"})

	if err := eng.hydrateSessionTree(context.Background(), sessionID); err != nil {
		t.Fatalf("hydrateSessionTree: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dest, "files", "local.md"))
	if err != nil || string(data) != "keep me" {
		t.Errorf("local file = %q (err %v), want it untouched", data, err)
	}
	if hydrates, _, _ := backend.counts(); hydrates != 0 {
		t.Errorf("Hydrate calls = %d, want 0", hydrates)
	}
}

func TestHydrateSessionTreeIsNoOpWithoutBackend(t *testing.T) {
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = filepath.Join(root, "sessions")
	eng := newFromConfig(cfg)

	if err := eng.hydrateSessionTree(context.Background(), "anything"); err != nil {
		t.Fatalf("hydrateSessionTree with no backend = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sessions")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("hydrate created the sessions root with no backend configured (stat err = %v)", err)
	}
}

// Clean shutdown flushes the backend and then releases it.
func TestStopFlushesAndClosesBackend(t *testing.T) {
	backend := &scriptedBackend{}
	eng, _ := newObjectStoreEngine(t, backend)

	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	// One: claiming the owner marker flushes it, because an unflushed
	// heartbeat is invisible to the host that has to read it. Nothing else in
	// Boot touches the store.
	if _, flushes, _ := backend.counts(); flushes != 1 {
		t.Fatalf("Flush called %d times during Boot, want 1 (the owner marker claim)", flushes)
	}
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Six: the owner claim above, then the shutdown snapshot flushing the
	// session objects, flushing again after the per-object manifest and again
	// after the commit marker, then the owner marker's removal, then
	// finalizeObjectStore's final barrier. The manifest's and the marker's own
	// flushes are the load-bearing ones, in that order — they are what make a
	// manifest unable to describe objects that are not yet durable, and a
	// marker unable to name a manifest that is not yet durable.
	_, flushes, closes := backend.counts()
	if flushes != 6 {
		t.Errorf("Flush calls = %d, want 6 on clean shutdown (owner claim, objects, manifest, marker, owner release, finalize)", flushes)
	}
	if closes != 1 {
		t.Errorf("Close calls = %d, want 1 (io.Closer honoured without widening Backend)", closes)
	}
	if eng.objectStore != nil {
		t.Error("objectStore handle still installed after Stop")
	}
}

// Stop must survive an already-cancelled context: hosts routinely hand it one,
// and the final flush is exactly the work that still has to happen.
func TestStopFlushesUnderCancelledContext(t *testing.T) {
	backend := &scriptedBackend{}
	eng, _ := newObjectStoreEngine(t, backend)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := eng.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// As in TestStopFlushesAndClosesBackend: the Boot-time owner claim, the
	// shutdown snapshot (objects + manifest + commit marker), the owner
	// marker's removal and the finalize barrier. Every one of them runs on its
	// own context, which is the point of the assertion.
	if _, flushes, _ := backend.counts(); flushes != 6 {
		t.Errorf("Flush calls = %d, want 6 even with a cancelled Stop context", flushes)
	}
}

func TestFinalizeObjectStoreFailurePolicy(t *testing.T) {
	flushErr := errors.New("bucket unreachable")

	t.Run("degrade keeps shutdown clean", func(t *testing.T) {
		backend := &scriptedBackend{flushErr: flushErr}
		eng, _ := newObjectStoreEngine(t, backend)
		if err := eng.openObjectStore(context.Background()); err != nil {
			t.Fatalf("openObjectStore: %v", err)
		}
		if err := eng.finalizeObjectStore(); err != nil {
			t.Errorf("finalizeObjectStore under degrade = %v, want nil", err)
		}
		if _, _, closes := backend.counts(); closes != 1 {
			t.Errorf("Close calls = %d, want 1 even when the flush failed", closes)
		}
	})

	t.Run("strict surfaces the failure", func(t *testing.T) {
		backend := &scriptedBackend{flushErr: flushErr}
		eng, _ := newObjectStoreEngine(t, backend)
		eng.Config.Core.ObjectStore.FailurePolicy = objectstore.FailurePolicyStrict
		if err := eng.openObjectStore(context.Background()); err != nil {
			t.Fatalf("openObjectStore: %v", err)
		}
		err := eng.finalizeObjectStore()
		if !errors.Is(err, flushErr) {
			t.Errorf("finalizeObjectStore under strict = %v, want it to wrap %v", err, flushErr)
		}
		if _, _, closes := backend.counts(); closes != 1 {
			t.Errorf("Close calls = %d, want 1 even when the flush failed", closes)
		}
	})
}

func TestScrubObjectStoreExcludedRemovesOnlyTheLock(t *testing.T) {
	dir := t.TempDir()
	writeSessionTree(t, dir, "id", map[string]string{
		"files/keep.md":                "keep",
		"plugins/nexus.x/session.lock": "a plugin file that merely shares a name",
	})
	if err := WriteSessionLock(dir, 4242); err != nil {
		t.Fatalf("WriteSessionLock: %v", err)
	}

	if err := scrubObjectStoreExcluded(dir); err != nil {
		t.Fatalf("scrubObjectStoreExcluded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, sessionLockFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("root session.lock survived the scrub (stat err = %v)", err)
	}
	// Only the session-relative root path is excluded; a same-named file
	// deeper in the tree is ordinary session state.
	if _, err := os.Stat(filepath.Join(dir, "plugins", "nexus.x", "session.lock")); err != nil {
		t.Errorf("nested file removed by the scrub: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "files", "keep.md")); err != nil {
		t.Errorf("ordinary file removed by the scrub: %v", err)
	}
}
