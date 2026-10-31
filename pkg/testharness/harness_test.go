package testharness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
)

// homeRootsConfig is a mock-mode config (no API key, no network) that, like
// every configs/test-*.yaml, points the sessions root at ~/.nexus. The
// storage root is set explicitly so the test proves it is redirected too.
const homeRootsConfig = `
core:
  log_level: warn
  tick_interval: 5s
  models:
    default: mock
    mock:
      provider: nexus.llm.anthropic
      model: mock
      max_tokens: 1024
  sessions:
    root: ~/.nexus/test-sessions
    retention: 30d
    id_format: datetime_short
  storage:
    root: ~/.nexus

plugins:
  active:
    - nexus.io.test
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.memory.capped

  nexus.io.test:
    inputs:
      - "hello"
    timeout: 10s
    mock_responses:
      - content: "hi"

  nexus.llm.anthropic:
    api_key: "sk-mock-not-used"

  nexus.memory.capped:
    max_messages: 10
    persist: false
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// TestNew_RedirectsRootsToTempDir pins the default: whatever the config says,
// a harness-driven run writes its session tree and app/agent storage under a
// per-test temp dir, never under ~/.nexus, and that dir is removed at test end.
func TestNew_RedirectsRootsToTempDir(t *testing.T) {
	cfgPath := writeConfig(t, homeRootsConfig)

	var tempRoot string
	t.Run("run", func(t *testing.T) {
		h := New(t, cfgPath, WithTimeout(20*time.Second), WithKeepSession())

		tempRoot = h.TempRoot()
		if tempRoot == "" {
			t.Fatal("TempRoot() empty; harness did not redirect roots")
		}
		cfg := h.eng.Config.Core
		if got, want := cfg.Sessions.Root, filepath.Join(tempRoot, "sessions"); got != want {
			t.Errorf("Sessions.Root = %q, want %q", got, want)
		}
		if got, want := cfg.Storage.Root, filepath.Join(tempRoot, "data"); got != want {
			t.Errorf("Storage.Root = %q, want %q", got, want)
		}
		// Keys the override must not disturb.
		if cfg.Sessions.IDFormat != "datetime_short" || cfg.Sessions.Retention != "30d" {
			t.Errorf("sibling sessions keys changed: %+v", cfg.Sessions)
		}
		if h.eng.ConfigPath != cfgPath {
			t.Errorf("ConfigPath = %q, want %q", h.eng.ConfigPath, cfgPath)
		}

		h.Run()
		h.AssertEventEmitted("io.session.end")

		dir := h.SessionDir()
		if !within(dir, filepath.Join(tempRoot, "sessions")) {
			t.Errorf("session dir %q is not under the temp sessions root %q", dir, tempRoot)
		}
		if home, err := os.UserHomeDir(); err == nil && within(dir, filepath.Join(home, ".nexus")) {
			t.Errorf("session dir %q is under ~/.nexus", dir)
		}
		h.AssertSessionArtifact("metadata/session.json")
	})

	if tempRoot == "" {
		return
	}
	if _, err := os.Stat(tempRoot); !os.IsNotExist(err) {
		t.Errorf("temp root %q still exists after the test ended (stat err %v)", tempRoot, err)
	}
}

// TestNew_WithConfiguredRoots pins the opt-out: the config's roots are used
// verbatim and no temp root is created.
func TestNew_WithConfiguredRoots(t *testing.T) {
	sessionsRoot := t.TempDir()
	body := strings.Replace(homeRootsConfig, "root: ~/.nexus/test-sessions", "root: "+sessionsRoot, 1)
	h := New(t, writeConfig(t, body), WithConfiguredRoots())

	if h.TempRoot() != "" {
		t.Errorf("TempRoot() = %q, want empty under WithConfiguredRoots", h.TempRoot())
	}
	if got := h.eng.Config.Core.Sessions.Root; got != sessionsRoot {
		t.Errorf("Sessions.Root = %q, want %q", got, sessionsRoot)
	}
	if got := h.eng.Config.Core.Storage.Root; got != "~/.nexus" {
		t.Errorf("Storage.Root = %q, want ~/.nexus", got)
	}
}

func TestOverrideRoots_CreatesMissingKeys(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"no core":     "plugins:\n  active: []\n",
		"flow style":  "core: {log_level: warn, sessions: {root: ~/x, id_format: timestamp}}\n",
		"scalar core": "core: nope\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := overrideRoots([]byte(in), "/s", "/d")
			if err != nil {
				t.Fatalf("overrideRoots: %v", err)
			}
			cfg, err := engine.LoadConfigFromBytes(out)
			if err != nil {
				t.Fatalf("LoadConfigFromBytes: %v\n%s", err, out)
			}
			if cfg.Core.Sessions.Root != "/s" || cfg.Core.Storage.Root != "/d" {
				t.Errorf("roots = %q, %q; want /s, /d\n%s", cfg.Core.Sessions.Root, cfg.Core.Storage.Root, out)
			}
			if name == "flow style" {
				if cfg.Core.Sessions.IDFormat != "timestamp" || cfg.Core.LogLevel != "warn" {
					t.Errorf("sibling keys lost: %s", out)
				}
			}
		})
	}
}

// TestNewEngine_RedirectsRoots pins that the non-Run constructor, used by
// tests driven over HTTP rather than scripted io.test inputs, applies the
// same redirect as New and registers the built-in plugins.
func TestNewEngine_RedirectsRoots(t *testing.T) {
	eng := NewEngine(t, writeConfig(t, homeRootsConfig))
	root := eng.Config.Core.Sessions.Root
	if strings.HasPrefix(root, "~") || filepath.Base(root) != "sessions" {
		t.Fatalf("Sessions.Root = %q, want <tmp>/sessions", root)
	}
	if got, want := eng.Config.Core.Storage.Root, filepath.Join(filepath.Dir(root), "data"); got != want {
		t.Errorf("Storage.Root = %q, want %q", got, want)
	}
	if _, ok := eng.Registry.Get("nexus.io.test"); !ok {
		t.Error("nexus.io.test not registered; NewEngine must register the built-in plugins")
	}
}

// TestWithConfigRewrite pins that rewrites run on the raw config bytes, in
// order, for New and NewEngine alike, under both root modes.
func TestWithConfigRewrite(t *testing.T) {
	cfgPath := writeConfig(t, homeRootsConfig)
	replace := func(old, new string) Option {
		return WithConfigRewrite(func(b []byte) []byte {
			return []byte(strings.ReplaceAll(string(b), old, new))
		})
	}
	// The second rewrite only matches what the first produced, so order matters.
	opts := []Option{replace("max_messages: 10", "max_messages: 7"), replace("max_messages: 7", "max_messages: 3")}
	maxMessages := func(t *testing.T, eng *engine.Engine) {
		t.Helper()
		if got := eng.Config.Plugins.Configs["nexus.memory.capped"]["max_messages"]; got != 3 {
			t.Errorf("max_messages = %v, want 3 (rewrites not applied in order)", got)
		}
	}

	t.Run("New", func(t *testing.T) { maxMessages(t, New(t, cfgPath, opts...).eng) })
	t.Run("NewEngine", func(t *testing.T) { maxMessages(t, NewEngine(t, cfgPath, opts...)) })
	t.Run("ConfiguredRoots", func(t *testing.T) {
		eng := NewEngine(t, cfgPath, append(opts, WithConfiguredRoots())...)
		maxMessages(t, eng)
		if eng.ConfigPath != cfgPath {
			t.Errorf("ConfigPath = %q, want %q", eng.ConfigPath, cfgPath)
		}
	})
}
