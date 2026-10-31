package tooltimeout

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// initPlugin runs the real factory + Init against cfg, as the engine does for
// an auto-activated gate.
func initPlugin(t *testing.T, cfg map[string]any) (*Plugin, error) {
	t.Helper()
	p := New().(*Plugin)
	err := p.Init(engine.PluginContext{Config: cfg, Bus: engine.NewEventBus(), Logger: slog.Default()})
	return p, err
}

// TestDefaults_NoConfig pins the default-on backstop: with no config at all
// the gate applies a 10m deadline to ordinary tools and none to the tools
// whose wait is human-bound or self-budgeted.
func TestDefaults_NoConfig(t *testing.T) {
	p, err := initPlugin(t, nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	for name, want := range map[string]time.Duration{
		"shell":                  10 * time.Minute,
		"run_code":               10 * time.Minute,
		"web_fetch":              10 * time.Minute,
		"ask_user":               0,
		"delegate":               0,
		"spawn_subagent":         0,
		"spawn_researcher":       0,
		"delegate_a2a_billing":   0,
		"delegate_agui_frontend": 0,
	} {
		if got := p.resolveTimeout(name); got != want {
			t.Errorf("resolveTimeout(%q) = %s, want %s", name, got, want)
		}
	}
}

// TestDefaults_UserKeyBeatsBuiltin: any user per_tool key, even a short glob,
// is consulted before the built-in exemptions.
func TestDefaults_UserKeyBeatsBuiltin(t *testing.T) {
	p, err := initPlugin(t, map[string]any{
		"per_tool": map[string]any{"ask_user": "15m", "*": "2m"},
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if got := p.resolveTimeout("ask_user"); got != 15*time.Minute {
		t.Errorf("ask_user = %s, want 15m", got)
	}
	if got := p.resolveTimeout("delegate"); got != 2*time.Minute {
		t.Errorf("delegate = %s, want 2m (user glob beats builtin exemption)", got)
	}
}

// TestDefaults_ZeroMeansNoDeadline: 0 is accepted for both keys and disables
// the deadline it names.
func TestDefaults_ZeroMeansNoDeadline(t *testing.T) {
	p, err := initPlugin(t, map[string]any{
		"default_timeout": "0s",
		"per_tool":        map[string]any{"shell": "0", "web_fetch": "45s"},
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if got := p.resolveTimeout("anything"); got != 0 {
		t.Errorf("default = %s, want 0", got)
	}
	if got := p.resolveTimeout("shell"); got != 0 {
		t.Errorf("shell = %s, want 0", got)
	}
	if got := p.resolveTimeout("web_fetch"); got != 45*time.Second {
		t.Errorf("web_fetch = %s, want 45s", got)
	}
}

func TestDefaults_EnabledFalse(t *testing.T) {
	p, err := initPlugin(t, map[string]any{
		"enabled":  false,
		"per_tool": map[string]any{"shell": "1s"},
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	for _, name := range []string{"shell", "run_code", "ask_user"} {
		if got := p.resolveTimeout(name); got != 0 {
			t.Errorf("enabled:false resolveTimeout(%q) = %s, want 0", name, got)
		}
	}
}

func TestDefaults_NegativeRejected(t *testing.T) {
	if _, err := initPlugin(t, map[string]any{"default_timeout": "-1s"}); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("negative default_timeout: err = %v", err)
	}
	if _, err := initPlugin(t, map[string]any{"per_tool": map[string]any{"shell": "-5s"}}); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("negative per_tool: err = %v", err)
	}
}

// TestDefaults_AwaitsHumanTagExempts: a tool whose definition carries
// events.ToolTagAwaitsHuman — learned from tool.register or from a finished
// tool.catalog.query snapshot (per-run tools such as AG-UI client tools) —
// gets no deadline; an untagged redefinition restores the default; and an
// operator per_tool key still bounds a tagged tool.
func TestDefaults_AwaitsHumanTagExempts(t *testing.T) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Config: map[string]any{"per_tool": map[string]any{"bounded_*": "3m"}},
		Bus:    bus, Logger: slog.Default(),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	human := []string{events.ToolTagAwaitsHuman}

	_ = bus.Emit("tool.register", events.ToolDef{Name: "memory_write", Tags: human})
	_ = bus.Emit("tool.register", events.ToolDef{Name: "memory_read"})
	_ = bus.Emit("tool.register", events.ToolDef{Name: "bounded_approval", Tags: human})

	// A catalog handler appends a per-run client tool; the gate observes the
	// finished snapshot.
	bus.Subscribe("tool.catalog.query", func(e engine.Event[any]) {
		q := e.Payload.(*events.ToolCatalogQuery)
		q.Tools = append(q.Tools, events.ToolDef{Name: "pick_color", Tags: human})
	}, engine.WithPriority(60))
	_ = bus.Emit("tool.catalog.query", &events.ToolCatalogQuery{})

	for name, want := range map[string]time.Duration{
		"memory_write":     0,
		"pick_color":       0,
		"memory_read":      10 * time.Minute,
		"bounded_approval": 3 * time.Minute,
	} {
		if got := p.resolveTimeout(name); got != want {
			t.Errorf("resolveTimeout(%q) = %s, want %s", name, got, want)
		}
	}

	// Re-registered without the tag (e.g. approval turned off): default again.
	_ = bus.Emit("tool.register", events.ToolDef{Name: "memory_write"})
	if got := p.resolveTimeout("memory_write"); got != 10*time.Minute {
		t.Errorf("untagged re-register: memory_write = %s, want 10m", got)
	}
}

// TestDefaults_AwaitsHumanNoSyntheticTimeout: end to end, an exempt tagged
// tool gets no timer, so no tool.timeout fires however long the human takes.
func TestDefaults_AwaitsHumanNoSyntheticTimeout(t *testing.T) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Config: map[string]any{"default_timeout": "20ms"},
		Bus:    bus, Logger: slog.Default(),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	_ = bus.Emit("tool.register", events.ToolDef{Name: "memory_write", Tags: []string{events.ToolTagAwaitsHuman}})

	var fired atomic.Int32
	bus.Subscribe("tool.timeout", func(engine.Event[any]) { fired.Add(1) })
	_ = bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "c1", Name: "memory_write"})
	_ = bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "c2", Name: "shell"})
	time.Sleep(100 * time.Millisecond)
	if got := fired.Load(); got != 1 {
		t.Fatalf("tool.timeout fired %d times, want 1 (shell only)", got)
	}
}
