package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The bug this whole file exists for. Before checkUnknownConfigKeys, this
// booted clean with object storage silently disabled: yaml.v3 dropped the
// misspelled block, and the schema validator that guards plugin blocks never
// sees core because it rebuilds core from the already-decoded typed config.
//
// A run in that state looks completely healthy. Every turn succeeds, nothing is
// ever uploaded, and the first symptom is an empty bucket after the host is
// replaced — which is the exact failure the object-store seam exists to
// prevent.
func TestLoadConfig_MisspelledObjectStoreBlockFailsTheBoot(t *testing.T) {
	_, err := LoadConfigFromBytes([]byte(`
core:
  object_stor:
    backend: s3
    bucket: nexus-prod
`))
	if err == nil {
		t.Fatal("a misspelled core.object_store block loaded without error; " +
			"object storage would be silently disabled at runtime")
	}
	if !strings.Contains(err.Error(), "core.object_stor") {
		t.Errorf("error does not name the offending key: %v", err)
	}
	// The message has to be actionable on its own — an operator reading a boot
	// failure should not have to go find the struct to learn what was valid.
	if !strings.Contains(err.Error(), "object_store") {
		t.Errorf("error does not list the valid keys, so the typo is not obvious: %v", err)
	}
}

func TestLoadConfig_UnknownKeysAreRejectedAtEveryDepth(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "top level",
			yaml: "cor:\n  log_level: debug\n",
			want: `unknown key "cor"`,
		},
		{
			name: "under core",
			yaml: "core:\n  log_levl: debug\n",
			want: `unknown key "core.log_levl"`,
		},
		{
			name: "two deep",
			yaml: "core:\n  sessions:\n    roott: /tmp/x\n",
			want: `unknown key "core.sessions.roott"`,
		},
		{
			name: "three deep",
			yaml: "core:\n  object_store:\n    backend: s3\n    bucket: b\n    failure_polcy: strict\n",
			want: `unknown key "core.object_store.failure_polcy"`,
		},
		{
			name: "nested under engine",
			yaml: "engine:\n  shutdown:\n    drain_timeoutt: 5s\n",
			want: `unknown key "engine.shutdown.drain_timeoutt"`,
		},
		{
			// The one that was already in the tree: journal is top-level, and
			// configs/demo-rewind.yaml carried it under core with the wrong key
			// name on top of that. Both halves were dropped in silence.
			name: "right key, wrong parent",
			yaml: "core:\n  journal:\n    fsync: none\n",
			want: `unknown key "core.journal"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfigFromBytes([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("loaded without error; want %s", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %s", err, tc.want)
			}
		})
	}
}

// The check must not fire on the blocks whose keys are data rather than field
// names. Getting this wrong fails closed on a valid config, which is a worse
// outcome than the hole being closed — a repo full of working configs would
// stop booting.
func TestLoadConfig_ArbitraryKeyBlocksAreNotWalked(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			// Plugin IDs are not struct fields. These blocks fail closed via
			// their own JSON Schemas, which run later and know the plugin.
			name: "plugin ids and their config",
			yaml: `
plugins:
  active:
    - nexus.io.tui
  nexus.io.tui:
    anything_at_all: true
    nested:
      deeply: 1
`,
		},
		{
			// core.models is yaml:"-" on the struct and parsed out of the raw
			// map by hand, so reflection cannot discover it and its contents
			// are role names chosen by the user.
			name: "core.models roles",
			yaml: `
core:
  models:
    default:
      provider: nexus.llm.anthropic
      model: claude-opus-4-20250514
    whatever_role_name:
      provider: nexus.llm.openai
      model: gpt-4o
`,
		},
		{
			// Capability names are data too.
			name: "capabilities",
			yaml: `
capabilities:
  search.provider: nexus.search.brave
  vector.store: nexus.vectorstore.chromem
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadConfigFromBytes([]byte(tc.yaml)); err != nil {
				t.Fatalf("valid config rejected: %v", err)
			}
		})
	}
}

// core.models must survive the check with its contents intact — allowing a key
// by name is not the same as parsing it, and this is the field most likely to
// be broken by a careless change to the walk.
func TestLoadConfig_ModelsStillParseAfterTheCheck(t *testing.T) {
	cfg, err := LoadConfigFromBytes([]byte(`
core:
  models:
    default:
      provider: nexus.llm.anthropic
      model: claude-opus-4-20250514
`))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if len(cfg.Core.ModelsRaw) != 1 {
		t.Fatalf("ModelsRaw = %+v, want one role", cfg.Core.ModelsRaw)
	}
	if _, ok := cfg.Core.ModelsRaw["default"]; !ok {
		t.Errorf("ModelsRaw lost the 'default' role: %+v", cfg.Core.ModelsRaw)
	}
}

// Every config this repository ships has to keep loading. This is the
// regression guard for the check failing closed: it is a live inventory rather
// than a fixture, so a config added later is covered without anyone
// remembering to add it here.
//
// It earned its place immediately — the first run failed on
// configs/demo-rewind.yaml, which had carried a silently-ignored
// core.journal.fsync_mode block since it was written.
func TestEveryShippedConfigStillLoads(t *testing.T) {
	// Engine configs are not only under configs/ -- the demo recipes and the
	// desktop app's embedded configs are the same file format and break the
	// same way.
	globs := []string{
		"../../configs/*.yaml",
		"../../cmd/demo/config-*.yaml",
		"../../cmd/demo/recipe-*.yaml",
		"../../cmd/desktop/config-*.yaml",
	}

	// Not an engine config: it sits in cmd/demo because it accompanies the otel
	// recipe, but it is a docker-compose file and its top-level keys are
	// compose's.
	notAnEngineConfig := map[string]bool{
		"recipe-otel-trace-docker-compose.yaml": true,
	}

	var paths []string
	for _, glob := range globs {
		matched, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range matched {
			if notAnEngineConfig[filepath.Base(path)] {
				continue
			}
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		t.Fatal("no configs found; this test is not checking anything")
	}

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfigFromBytes(data); err != nil {
				t.Errorf("shipped config no longer loads: %v", err)
			}
		})
	}
}

// A struct field added to the config must be accepted without anyone editing
// the checker. This is what buys the reflection walk over a literal allowlist:
// a hand-kept list drifts, and it drifts in the direction of rejecting valid
// configs.
func TestUnknownKeyCheck_TracksTheStructNotAList(t *testing.T) {
	// Exercised through the real loader rather than by poking the walk, so a
	// change to how the two connect is caught too.
	for _, key := range []string{"log_level", "agent_id", "tick_interval", "max_concurrent_events"} {
		t.Run(key, func(t *testing.T) {
			var value string
			switch key {
			case "tick_interval":
				value = "5s"
			case "max_concurrent_events":
				value = "10"
			default:
				value = "x"
			}
			if _, err := LoadConfigFromBytes([]byte("core:\n  " + key + ": " + value + "\n")); err != nil {
				t.Errorf("valid key %q rejected: %v", key, err)
			}
		})
	}
}

// Several typos in one file are reported together. An operator fixing a config
// by trial and error, one boot per typo, is a bad afternoon.
func TestLoadConfig_ReportsEveryUnknownKeyAtOnce(t *testing.T) {
	_, err := LoadConfigFromBytes([]byte(`
core:
  log_levl: debug
  sessions:
    roott: /tmp/x
`))
	if err == nil {
		t.Fatal("loaded without error")
	}
	for _, want := range []string{"core.log_levl", "core.sessions.roott"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %s", err, want)
		}
	}
}
