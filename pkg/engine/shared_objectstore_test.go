package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/engine/objectstore/objectstoretest"
	"github.com/frankbardon/nexus/pkg/engine/storage"
)

// registerSharedMemoryBackend registers one in-memory backend under a
// test-scoped name and returns both. Separate from
// newMemoryObjectStoreEngine because the shared-root tests need *two* engines
// with different data roots pointed at the *same* backend — which is what a
// second host resuming a machine-wide store actually looks like.
func registerSharedMemoryBackend(t *testing.T) (string, *objectstoretest.Memory) {
	t.Helper()
	backend := objectstoretest.NewMemory()
	name := "memory-shared-" + t.Name()
	objectstore.Register(name, func(context.Context, objectstore.Config) (objectstore.Backend, error) {
		return backend, nil
	})
	t.Cleanup(func() { objectstore.Unregister(name) })
	return name, backend
}

// newSharedRootEngine builds an engine whose data root is dataRoot and whose
// sessions live beneath it, mirroring the default layout where
// core.storage.root and core.sessions.root are both ~/.nexus.
func newSharedRootEngine(t *testing.T, backendName, dataRoot, agentID string) *Engine {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = filepath.Join(dataRoot, "sessions")
	cfg.Core.Storage.Root = dataRoot
	cfg.Core.AgentID = agentID
	cfg.Core.Sessions.ObjectStore = objectstore.Config{
		BackendName:   backendName,
		Bucket:        "test-bucket",
		FailurePolicy: objectstore.FailurePolicyDegrade,
	}
	return newFromConfig(cfg)
}

func TestSharedStoreKeyMirrorsTheLocalLayout(t *testing.T) {
	root := filepath.FromSlash("/data/nexus")
	cases := []struct {
		name string
		live string
		want string
	}{
		{
			name: "app scope",
			live: filepath.Join(root, "plugins", "nexus.gate.token_budget", "store.db"),
			want: "plugins/nexus.gate.token_budget/store.db",
		},
		{
			name: "agent scope",
			live: filepath.Join(root, "agents", "agent-a", "plugins", "nexus.vectorstore.sqlite_fts", "store.db"),
			want: "agents/agent-a/plugins/nexus.vectorstore.sqlite_fts/store.db",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sharedStoreKey(root, tc.live)
			if err != nil {
				t.Fatalf("sharedStoreKey: %v", err)
			}
			if got != tc.want {
				t.Errorf("key = %q, want %q", got, tc.want)
			}
			if err := objectstore.ValidateKey(got); err != nil {
				t.Errorf("key %q is not a legal store key: %v", got, err)
			}
			// The whole point of the layout: shared state is a sibling of the
			// session trees, never a member of one.
			if _, under := objectstore.TrimKeyPrefix(got, sessionsKeyRoot); under {
				t.Errorf("key %q is under %q", got, sessionsKeyRoot)
			}
		})
	}
}

// The lifetime invariant, at the one place it can be enforced mechanically: a
// key under sessions/ would give every session its own copy of a machine-wide
// store, so it is refused rather than produced.
func TestSharedStoreKeyRefusesSessionPrefixAndEscapes(t *testing.T) {
	root := filepath.FromSlash("/data/nexus")
	for _, live := range []string{
		filepath.Join(root, "sessions", "sess-1", "plugins", "p", "store.db"),
		filepath.Join(filepath.FromSlash("/elsewhere"), "plugins", "p", "store.db"),
	} {
		if got, err := sharedStoreKey(root, live); err == nil {
			t.Errorf("sharedStoreKey(%q) = %q, want an error", live, got)
		}
	}
}

// FR-24's headline: an app-scope store round-trips through the seam and keeps
// the machine-wide lifetime documented in docs/src/architecture/storage.md.
//
// Host B is a second data root with a second session ID against the same
// bucket — a fresh container resuming nothing in particular. If the store had
// been keyed under the session that flushed it, host B would open an empty
// database here and the tenant total would silently reset to zero.
func TestAppScopeStoreSurvivesIntoADifferentSessionOnADifferentRoot(t *testing.T) {
	name, backend := registerSharedMemoryBackend(t)
	const pluginID = "nexus.test.appscope"

	hostA := newSharedRootEngine(t, name, t.TempDir(), "")
	if err := hostA.Boot(context.Background()); err != nil {
		t.Fatalf("host A Boot: %v", err)
	}
	stA, err := hostA.Storage.Open(storage.ScopeApp, pluginID)
	if err != nil {
		t.Fatalf("open app storage: %v", err)
	}
	if err := stA.Put("tenant.acme.total", []byte("4242")); err != nil {
		t.Fatalf("put: %v", err)
	}
	endTurn(t, hostA, "turn-1")
	sessionA := hostA.Session.ID
	if err := hostA.Stop(context.Background()); err != nil {
		t.Fatalf("host A Stop: %v", err)
	}

	wantKey := "plugins/" + pluginID + "/store.db"
	if _, ok := backend.Get(wantKey); !ok {
		t.Fatalf("app-scope store not uploaded at %q; keys = %v", wantKey, backend.Keys())
	}
	// Not a member of the session tree, and therefore not in the session's
	// commit marker either.
	for _, k := range backend.Keys() {
		if strings.Contains(k, pluginID) && strings.HasPrefix(k, sessionsKeyRoot+"/") {
			t.Errorf("app-scope store also stored under a session prefix: %q", k)
		}
	}
	for _, k := range sessionKeys(backend, sessionA) {
		if strings.Contains(k, pluginID) {
			t.Errorf("app-scope store leaked into the session tree at %q", k)
		}
	}

	hostB := newSharedRootEngine(t, name, t.TempDir(), "")
	if err := hostB.Boot(context.Background()); err != nil {
		t.Fatalf("host B Boot: %v", err)
	}
	t.Cleanup(func() { _ = hostB.Stop(context.Background()) })
	if hostB.Session.ID == sessionA {
		t.Fatalf("host B reused session %q; the test proves nothing", sessionA)
	}
	stB, err := hostB.Storage.Open(storage.ScopeApp, pluginID)
	if err != nil {
		t.Fatalf("host B open app storage: %v", err)
	}
	got, ok, err := stB.Get("tenant.acme.total")
	if err != nil {
		t.Fatalf("host B get: %v", err)
	}
	if !ok || string(got) != "4242" {
		t.Fatalf("host B read %q (present=%v), want %q — the machine-wide store became per-session",
			got, ok, "4242")
	}
}

func TestAgentScopeStoreIsKeyedByAgent(t *testing.T) {
	name, backend := registerSharedMemoryBackend(t)
	const pluginID = "nexus.test.agentscope"

	eng := newSharedRootEngine(t, name, t.TempDir(), "agent-a")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	if _, err := eng.Storage.Open(storage.ScopeAgent, pluginID); err != nil {
		t.Fatalf("open agent storage: %v", err)
	}
	endTurn(t, eng, "turn-1")

	wantKey := "agents/agent-a/plugins/" + pluginID + "/store.db"
	if _, ok := backend.Get(wantKey); !ok {
		t.Fatalf("agent-scope store not uploaded at %q; keys = %v", wantKey, backend.Keys())
	}
}

// plugins/vectorstore/sqlite_fts asks for ScopeAgent and gets ScopeApp on an
// engine with no core.agent_id. The seam has to follow that collapse exactly:
// keying it under agents/<empty> would be malformed, and snapshotting both
// scopes would upload the same database twice.
func TestAgentScopeCollapsesToAppWithoutAnAgentID(t *testing.T) {
	name, backend := registerSharedMemoryBackend(t)
	const pluginID = "nexus.test.collapse"

	eng := newSharedRootEngine(t, name, t.TempDir(), "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	if _, err := eng.Storage.Open(storage.ScopeAgent, pluginID); err != nil {
		t.Fatalf("open agent storage: %v", err)
	}
	endTurn(t, eng, "turn-1")

	var hits []string
	for _, k := range backend.Keys() {
		if strings.Contains(k, pluginID) {
			hits = append(hits, k)
		}
	}
	if len(hits) != 1 || hits[0] != "plugins/"+pluginID+"/store.db" {
		t.Fatalf("collapsed agent-scope store stored at %v, want exactly [plugins/%s/store.db]", hits, pluginID)
	}
}

// A shared store may be open in this process or another one on the same
// machine. Overwriting it under a live SQLite handle produces a corrupt
// database, not a stale one, so a directory that exists locally is never
// touched — the same "local working copy wins" rule the session tree gets.
func TestSharedStoreHydrationNeverOverwritesALocalDirectory(t *testing.T) {
	name, backend := registerSharedMemoryBackend(t)
	root := t.TempDir()

	local := filepath.Join(root, "plugins", "nexus.test.local")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "store.db"), []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := backend.Seed("plugins/nexus.test.local/store.db", []byte("remote")); err != nil {
		t.Fatal(err)
	}
	if err := backend.Seed("plugins/nexus.test.fresh/store.db", []byte("remote-fresh")); err != nil {
		t.Fatal(err)
	}

	eng := newSharedRootEngine(t, name, root, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	got, err := os.ReadFile(filepath.Join(local, "store.db"))
	if err != nil {
		t.Fatalf("read local store: %v", err)
	}
	if string(got) != "local" {
		t.Errorf("local store.db = %q, want %q — hydration overwrote a live database", got, "local")
	}

	// A plugin directory with no local copy is still hydrated: whole-root
	// granularity would have skipped this one too.
	fresh, err := os.ReadFile(filepath.Join(root, "plugins", "nexus.test.fresh", "store.db"))
	if err != nil {
		t.Fatalf("fresh store not hydrated: %v", err)
	}
	if string(fresh) != "remote-fresh" {
		t.Errorf("hydrated store.db = %q, want %q", fresh, "remote-fresh")
	}
}

// The SQLite sidecars describe the machine that produced them. A hydrated
// -wal beside a fresh store.db rolls the database back to a state neither file
// ever held, so the scrub that protects the session tree protects these too.
func TestSharedStoreHydrationDropsSQLiteSidecars(t *testing.T) {
	name, backend := registerSharedMemoryBackend(t)
	root := t.TempDir()

	if err := backend.Seed("plugins/nexus.test.sidecar/store.db", []byte("db")); err != nil {
		t.Fatal(err)
	}
	if err := backend.Seed("plugins/nexus.test.sidecar/store.db-wal", []byte("wal")); err != nil {
		t.Fatal(err)
	}

	eng := newSharedRootEngine(t, name, root, "")
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	dir := filepath.Join(root, "plugins", "nexus.test.sidecar")
	if _, err := os.Stat(filepath.Join(dir, "store.db")); err != nil {
		t.Fatalf("store.db not hydrated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "store.db-wal")); err == nil {
		t.Error("store.db-wal was hydrated; it describes the host that wrote it")
	}
}

// Zero-impact default: with no backend named, nothing under the data root is
// listed, created or uploaded.
func TestSharedRootsAreInertWithoutABackend(t *testing.T) {
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = filepath.Join(root, "sessions")
	cfg.Core.Storage.Root = root
	eng := newFromConfig(cfg)

	if err := eng.hydrateSharedRoots(context.Background()); err != nil {
		t.Fatalf("hydrateSharedRoots without a backend: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "plugins")); err == nil {
		t.Error("hydration created <root>/plugins with no backend configured")
	}
	stats, err := eng.snapshotSharedStores(context.Background(), nil, root)
	if err != nil {
		t.Fatalf("snapshotSharedStores with no open handles: %v", err)
	}
	if stats.Objects != 0 || stats.Bytes != 0 {
		t.Errorf("stats = %+v, want zero", stats)
	}
}

func TestPublishTreeUploadsUnderPrefixAndHonoursSkip(t *testing.T) {
	name, backend := registerSharedMemoryBackend(t)
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "report.json"), []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "_sessions", "sess-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "_sessions", "sess-1", "big.bin"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := objectstore.Config{BackendName: name, Bucket: "test-bucket"}
	objects, bytes, err := PublishTree(context.Background(), cfg, EvalObjectKeyPrefix("run-1"), dir,
		func(rel string, isDir bool) bool { return isDir && rel == "_sessions" })
	if err != nil {
		t.Fatalf("PublishTree: %v", err)
	}
	if objects != 1 || bytes != int64(len(`{"ok":true}`)) {
		t.Errorf("published %d objects / %d bytes, want 1 / %d", objects, bytes, len(`{"ok":true}`))
	}
	if got, ok := backend.Get("eval/run-1/report.json"); !ok || string(got) != `{"ok":true}` {
		t.Errorf("eval/run-1/report.json = %q (present=%v)", got, ok)
	}
	for _, k := range backend.Keys() {
		if strings.Contains(k, "_sessions") {
			t.Errorf("pruned directory was published at %q", k)
		}
	}

	// A disabled config is a no-op, so a caller never branches on config
	// internals.
	if n, b, err := PublishTree(context.Background(), objectstore.Config{}, "eval/run-2", dir, nil); err != nil || n != 0 || b != 0 {
		t.Errorf("PublishTree with a disabled config = %d/%d/%v, want 0/0/nil", n, b, err)
	}
}

func TestEvalObjectKeyPrefixIsAStoreRelativeSibling(t *testing.T) {
	key := EvalObjectKeyPrefix("20260903T101500Z") + "/report.json"
	if err := objectstore.ValidateKey(key); err != nil {
		t.Fatalf("eval key %q is not store-relative: %v", key, err)
	}
	if _, under := objectstore.TrimKeyPrefix(key, sessionsKeyRoot); under {
		t.Errorf("eval key %q is under %q", key, sessionsKeyRoot)
	}
}
