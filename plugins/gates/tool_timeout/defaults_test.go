package tooltimeout

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
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
