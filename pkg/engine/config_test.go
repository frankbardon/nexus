package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultConfig_KnownDefaults(t *testing.T) {
	c := DefaultConfig()
	if c == nil {
		t.Fatal("DefaultConfig returned nil")
	}
	if c.Core.LogLevel != "info" {
		t.Errorf("Core.LogLevel = %q, want info", c.Core.LogLevel)
	}
	if c.Core.TickInterval != 1*time.Second {
		t.Errorf("Core.TickInterval = %v, want 1s", c.Core.TickInterval)
	}
	if c.Core.MaxConcurrentEvents != 100 {
		t.Errorf("Core.MaxConcurrentEvents = %d, want 100", c.Core.MaxConcurrentEvents)
	}
	if c.Core.Sessions.Root != "~/.nexus/sessions" {
		t.Errorf("Sessions.Root = %q, want ~/.nexus/sessions", c.Core.Sessions.Root)
	}
	if c.Core.Sessions.Retention != "30d" {
		t.Errorf("Sessions.Retention = %q, want 30d", c.Core.Sessions.Retention)
	}
	if c.Capabilities == nil {
		t.Error("Capabilities should be initialized to empty map, not nil")
	}
	if c.Plugins.Configs == nil {
		t.Error("Plugins.Configs should be initialized to empty map, not nil")
	}
	if c.Journal.Fsync != "turn-boundary" {
		t.Errorf("Journal.Fsync = %q, want turn-boundary", c.Journal.Fsync)
	}
	if c.Journal.RetainDays != 30 {
		t.Errorf("Journal.RetainDays = %d, want 30", c.Journal.RetainDays)
	}
	if c.Journal.RotateSizeMB != 4 {
		t.Errorf("Journal.RotateSizeMB = %d, want 4", c.Journal.RotateSizeMB)
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	if _, err := LoadConfig(missing); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoadConfig_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minimal.yaml")
	if err := os.WriteFile(path, []byte("core:\n  log_level: debug\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Core.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.Core.LogLevel)
	}
	if len(cfg.Raw) == 0 {
		t.Error("Raw bytes not stashed")
	}
}

func TestLoadConfigFromBytes_MalformedYAML(t *testing.T) {
	if _, err := LoadConfigFromBytes([]byte("core:\n  log_level: : :\n")); err == nil {
		t.Fatal("expected parse error for malformed YAML, got nil")
	}
}

func TestLoadConfigFromBytes_EmptyMergedOverDefaults(t *testing.T) {
	cfg, err := LoadConfigFromBytes(nil)
	if err != nil {
		t.Fatalf("LoadConfigFromBytes(nil): %v", err)
	}
	// Defaults should still be present.
	if cfg.Core.LogLevel != "info" {
		t.Errorf("expected default LogLevel preserved, got %q", cfg.Core.LogLevel)
	}
}

func TestLoadConfigFromBytes_ExtractsPluginConfigs(t *testing.T) {
	yaml := `
plugins:
  active:
    - nexus.test.alpha
  nexus.test.alpha:
    timeout_ms: 500
    nested:
      key: value
  nexus.test.beta:
    flag: true
`
	cfg, err := LoadConfigFromBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}

	alpha, ok := cfg.Plugins.Configs["nexus.test.alpha"]
	if !ok {
		t.Fatal("missing nexus.test.alpha config")
	}
	if alpha["timeout_ms"] != 500 {
		t.Errorf("alpha.timeout_ms = %v, want 500", alpha["timeout_ms"])
	}
	nested, ok := alpha["nested"].(map[string]any)
	if !ok {
		t.Fatalf("alpha.nested wrong type: %T", alpha["nested"])
	}
	if nested["key"] != "value" {
		t.Errorf("alpha.nested.key = %v, want value", nested["key"])
	}

	beta, ok := cfg.Plugins.Configs["nexus.test.beta"]
	if !ok {
		t.Fatal("missing nexus.test.beta config")
	}
	if beta["flag"] != true {
		t.Errorf("beta.flag = %v, want true", beta["flag"])
	}

	// "active" is not a plugin config, must not appear in Configs map.
	if _, leaked := cfg.Plugins.Configs["active"]; leaked {
		t.Error("'active' key leaked into Plugins.Configs")
	}

	if got := cfg.Plugins.Active; len(got) != 1 || got[0] != "nexus.test.alpha" {
		t.Errorf("Active = %v, want [nexus.test.alpha]", got)
	}
}

func TestLoadConfigFromBytes_ExtractsCoreModels(t *testing.T) {
	yaml := `
core:
  models:
    fast:
      provider: openai
      model: gpt-4o-mini
    slow:
      provider: anthropic
      model: claude-3-5-sonnet
`
	cfg, err := LoadConfigFromBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if cfg.Core.ModelsRaw == nil {
		t.Fatal("Core.ModelsRaw is nil")
	}
	fast, ok := cfg.Core.ModelsRaw["fast"].(map[string]any)
	if !ok {
		t.Fatalf("ModelsRaw[fast] wrong type: %T", cfg.Core.ModelsRaw["fast"])
	}
	if fast["model"] != "gpt-4o-mini" {
		t.Errorf("fast.model = %v, want gpt-4o-mini", fast["model"])
	}
}

func TestLoadConfigFromBytes_StashesRawBytes(t *testing.T) {
	src := []byte("core:\n  log_level: warn\n")
	cfg, err := LoadConfigFromBytes(src)
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if string(cfg.Raw) != string(src) {
		t.Errorf("Raw mismatch: got %q, want %q", cfg.Raw, src)
	}
	// Mutating the input must not affect Raw — Raw should be a copy.
	src[0] = 'X'
	if cfg.Raw[0] == 'X' {
		t.Error("Raw bytes should be copied, not aliased to caller's slice")
	}
}

func TestConfig_Validate_RejectsBootstrapStderrWithVisualPlugin(t *testing.T) {
	cases := []string{
		"nexus.io.tui",
		"nexus.io.browser",
		"nexus.io.wails",
		"nexus.io.tui/instance-2", // instance suffix should still match base ID
	}
	for _, active := range cases {
		c := DefaultConfig()
		c.Core.Logging.BootstrapStderr = true
		c.Plugins.Active = []string{active}
		if err := c.validate(); err == nil {
			t.Errorf("validate() should reject bootstrap_stderr with active=%q", active)
		} else if !strings.Contains(err.Error(), "bootstrap_stderr") {
			t.Errorf("error should mention bootstrap_stderr, got %q", err.Error())
		}
	}
}

func TestConfig_Validate_AllowsBootstrapStderrWithoutVisualPlugin(t *testing.T) {
	c := DefaultConfig()
	c.Core.Logging.BootstrapStderr = true
	c.Plugins.Active = []string{"nexus.tool.shell", "nexus.memory.simple"}
	if err := c.validate(); err != nil {
		t.Errorf("validate() should allow bootstrap_stderr with non-visual plugins: %v", err)
	}
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no user home dir on this system")
	}

	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"~", home},
		{"~/foo", filepath.Join(home, "foo")},
		{"~/a/b/c", filepath.Join(home, "a", "b", "c")},
		{"/absolute", "/absolute"},
		{"relative/path", "relative/path"},
		{"~user/foo", "~user/foo"}, // only "~" or "~/" expand; "~user" left alone
		{"./local", "./local"},
	}
	for _, c := range cases {
		if got := ExpandPath(c.in); got != c.want {
			t.Errorf("ExpandPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
