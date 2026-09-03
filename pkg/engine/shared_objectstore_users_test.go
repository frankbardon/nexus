package engine_test

// The acceptance criterion this file exists for: app- and agent-scope coverage
// is verified against the *real* users of those scopes, not against synthetic
// plugin IDs in pkg/engine's own tests.
//
//   - plugins/gates/token_budget opens ScopeApp for a tenant token ceiling.
//     That ceiling is machine-wide on purpose. If syncing it made it
//     per-session, the gate would keep running and keep passing every request:
//     nothing errors, the budget simply stops being a budget. It is the canary,
//     so it gets a test that reads the number back from a *different* session
//     on a *different* data root.
//   - plugins/vectorstore/sqlite_fts opens ScopeAgent and relies on the
//     manager's collapse to ScopeApp when core.agent_id is empty. Both halves
//     are asserted, because the collapse is where a naive "agents/<id>/..." key
//     scheme would produce agents//plugins/... and fail silently.
//
// This is an external test package (engine_test) so it can import the plugins,
// which import pkg/engine. pkg/engine's in-package tests cannot.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/allplugins"
	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/engine/objectstore/objectstoretest"
)

// bootWithMemoryStore boots an engine from YAML with the in-memory backend
// wired in, returning the engine and the backend. Config-as-bytes rather than
// mutating an engine.Config by hand: it is the embedder-facing path, and it
// exercises the same YAML parsing an operator writes.
func bootWithMemoryStore(t *testing.T, backendName, dataRoot, pluginsYAML string) *engine.Engine {
	t.Helper()
	cfg := fmt.Sprintf(`
core:
  log_level: error
  agent_id: %q
  sessions:
    root: %s
    object_store:
      backend: %s
      bucket: test-bucket
  storage:
    root: %s
%s
`, "", filepath.Join(dataRoot, "sessions"), backendName, dataRoot, pluginsYAML)
	return bootYAML(t, cfg)
}

func bootYAML(t *testing.T, cfg string) *engine.Engine {
	t.Helper()
	eng, err := engine.NewFromBytes([]byte(cfg))
	if err != nil {
		t.Fatalf("NewFromBytes: %v\n%s", err, cfg)
	}
	allplugins.RegisterAll(eng.Registry)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v\n%s", err, cfg)
	}
	return eng
}

func registerMemory(t *testing.T) (string, *objectstoretest.Memory) {
	t.Helper()
	backend := objectstoretest.NewMemory()
	name := "memory-users-" + strings.ReplaceAll(t.Name(), "/", "-")
	objectstore.Register(name, func(context.Context, objectstore.Config) (objectstore.Backend, error) {
		return backend, nil
	})
	t.Cleanup(func() { objectstore.Unregister(name) })
	return name, backend
}

const tokenBudgetPluginsYAML = `plugins:
  active:
    - nexus.gate.token_budget
  nexus.gate.token_budget:
    ceilings:
      - dimension: tenant
        window: day
        max_total_tokens: 1000000
`

// The canary. A tenant ceiling is machine-wide; this proves the seam keeps it
// that way across a session boundary and a data-root boundary.
func TestTokenBudgetAppScopeCeilingStaysMachineWide(t *testing.T) {
	name, backend := registerMemory(t)
	const key = "plugins/nexus.gate.token_budget/store.db"

	hostA := bootWithMemoryStore(t, name, t.TempDir(), tokenBudgetPluginsYAML)
	sessionA := hostA.Session.ID
	if err := hostA.Stop(context.Background()); err != nil {
		t.Fatalf("host A Stop: %v", err)
	}

	if _, ok := backend.Get(key); !ok {
		t.Fatalf("token_budget app-scope store not uploaded at %q; keys = %v", key, backend.Keys())
	}
	for _, k := range backend.Keys() {
		if strings.Contains(k, "nexus.gate.token_budget") && strings.HasPrefix(k, "sessions/") {
			t.Fatalf("the machine-wide token ceiling was stored per session at %q — the gate would silently reset every session", k)
		}
	}

	// A second host, a second session, one bucket. The store has to arrive.
	rootB := t.TempDir()
	hostB := bootWithMemoryStore(t, name, rootB, tokenBudgetPluginsYAML)
	t.Cleanup(func() { _ = hostB.Stop(context.Background()) })
	if hostB.Session.ID == sessionA {
		t.Fatalf("host B reused session %q; the test proves nothing", sessionA)
	}
	local := filepath.Join(rootB, "plugins", "nexus.gate.token_budget", "store.db")
	if _, err := os.Stat(local); err != nil {
		t.Fatalf("token_budget store not hydrated onto host B at %s: %v", local, err)
	}
}

// sqlite_fts asks for ScopeAgent. With core.agent_id set it gets its own
// partition; with it empty the manager collapses it onto ScopeApp and the key
// has to collapse with it.
func TestSQLiteFTSAgentScopeFollowsTheAgentIDCollapse(t *testing.T) {
	const pluginsYAML = `plugins:
  active:
    - nexus.vectorstore.sqlite_fts
  nexus.vectorstore.sqlite_fts:
    scope: agent
`
	for _, tc := range []struct {
		name    string
		agentID string
		want    string
	}{
		{name: "collapsed to app scope", agentID: "", want: "plugins/nexus.vectorstore.sqlite_fts/store.db"},
		{name: "partitioned by agent", agentID: "agent-a", want: "agents/agent-a/plugins/nexus.vectorstore.sqlite_fts/store.db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, backend := registerMemory(t)
			root := t.TempDir()
			cfg := fmt.Sprintf(`
core:
  log_level: error
  agent_id: %q
  sessions:
    root: %s
    object_store:
      backend: %s
      bucket: test-bucket
  storage:
    root: %s
%s
`, tc.agentID, filepath.Join(root, "sessions"), name, root, pluginsYAML)

			eng := bootYAML(t, cfg)
			if err := eng.Stop(context.Background()); err != nil {
				t.Fatalf("Stop: %v", err)
			}

			var hits []string
			for _, k := range backend.Keys() {
				if strings.Contains(k, "nexus.vectorstore.sqlite_fts") {
					hits = append(hits, k)
				}
			}
			if len(hits) != 1 || hits[0] != tc.want {
				t.Fatalf("sqlite_fts store keys = %v, want exactly [%s]", hits, tc.want)
			}
		})
	}
}
