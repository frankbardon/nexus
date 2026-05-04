package engine

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/storage"
)

// storageProbePlugin records the PluginContext storage-related fields and
// opens a per-scope handle so tests can verify both the path conventions and
// that PluginContext.Storage actually returns a working handle.
type storageProbePlugin struct {
	id           string
	pluginID     string
	dataDir      string
	appDataDir   string
	agentDataDir string
	openErr      error
	wrote        bool
}

func (p *storageProbePlugin) ID() string                         { return p.id }
func (p *storageProbePlugin) Name() string                       { return p.id }
func (p *storageProbePlugin) Version() string                    { return "test" }
func (p *storageProbePlugin) Dependencies() []string             { return nil }
func (p *storageProbePlugin) Requires() []Requirement            { return nil }
func (p *storageProbePlugin) Capabilities() []Capability         { return nil }
func (p *storageProbePlugin) Subscriptions() []EventSubscription { return nil }
func (p *storageProbePlugin) Emissions() []string                { return nil }
func (p *storageProbePlugin) Ready() error                       { return nil }
func (p *storageProbePlugin) Shutdown(_ context.Context) error   { return nil }

func (p *storageProbePlugin) Init(ctx PluginContext) error {
	p.pluginID = ctx.PluginID
	p.dataDir = ctx.DataDir
	p.appDataDir = ctx.AppDataDir
	p.agentDataDir = ctx.AgentDataDir
	if ctx.Storage == nil {
		p.openErr = nil
		return nil
	}
	st, err := ctx.Storage(storage.ScopeApp)
	if err != nil {
		p.openErr = err
		return nil
	}
	if err := st.Put("hello", []byte("world")); err != nil {
		p.openErr = err
		return nil
	}
	p.wrote = true
	return nil
}

func TestPluginContextStorageWiring(t *testing.T) {
	root := t.TempDir()
	probe := &storageProbePlugin{id: "nexus.test.probe"}

	reg := NewPluginRegistry()
	reg.Register(probe.id, func() Plugin { return probe })

	cfg := &Config{
		Core: CoreConfig{
			Sessions: SessionsConfig{Root: filepath.Join(root, "sessions")},
			Storage:  StorageConfig{Root: root},
		},
		Plugins: PluginsConfig{Active: []string{probe.id}},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := NewEventBus()
	lm := NewLifecycleManager(reg, bus, cfg, logger, nil, nil, nil, nil)

	mgr := storage.NewManager(root, "", nil, nil)
	lm.storage = mgr

	if err := lm.Boot(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	if probe.pluginID != "nexus.test.probe" {
		t.Errorf("PluginID = %q, want nexus.test.probe", probe.pluginID)
	}

	wantApp := filepath.Join(root, "plugins", "nexus.test.probe")
	if probe.appDataDir != wantApp {
		t.Errorf("AppDataDir = %q, want %q", probe.appDataDir, wantApp)
	}
	if probe.agentDataDir != wantApp {
		t.Errorf("AgentDataDir collapse mismatch: got %q, want %q", probe.agentDataDir, wantApp)
	}
	if probe.openErr != nil {
		t.Errorf("storage open from Init: %v", probe.openErr)
	}
	if !probe.wrote {
		t.Errorf("expected probe to write through ctx.Storage")
	}

	dbPath := filepath.Join(wantApp, "store.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("expected db at %s: %v", dbPath, err)
	}
}

func TestPluginContextStorageWiring_AgentScopeDistinct(t *testing.T) {
	root := t.TempDir()
	probe := &storageProbePlugin{id: "nexus.test.probe"}

	reg := NewPluginRegistry()
	reg.Register(probe.id, func() Plugin { return probe })

	cfg := &Config{
		Core: CoreConfig{
			AgentID:  "researcher",
			Sessions: SessionsConfig{Root: filepath.Join(root, "sessions")},
			Storage:  StorageConfig{Root: root},
		},
		Plugins: PluginsConfig{Active: []string{probe.id}},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := NewEventBus()
	lm := NewLifecycleManager(reg, bus, cfg, logger, nil, nil, nil, nil)

	mgr := storage.NewManager(root, "researcher", nil, nil)
	lm.storage = mgr

	if err := lm.Boot(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	wantApp := filepath.Join(root, "plugins", "nexus.test.probe")
	wantAgent := filepath.Join(root, "agents", "researcher", "plugins", "nexus.test.probe")
	if probe.appDataDir != wantApp {
		t.Errorf("AppDataDir = %q, want %q", probe.appDataDir, wantApp)
	}
	if probe.agentDataDir != wantAgent {
		t.Errorf("AgentDataDir = %q, want %q", probe.agentDataDir, wantAgent)
	}
}
