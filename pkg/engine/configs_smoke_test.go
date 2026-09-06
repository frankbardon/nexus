package engine_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/allplugins"
)

// TestConfigsSmokeBoot loads every engine YAML in the repository and runs the schema
// validator over the resulting Config + active plugin set. This is the canary
// that catches future YAML/schema drift: any YAML file in the repo must
// continue to satisfy every plugin schema. It does not actually Boot the
// engine (live external dependencies, API keys, etc) — schema validation
// alone is the contract the test enforces.
func TestConfigsSmokeBoot(t *testing.T) {
	repoRoot := findRepoRoot(t)

	// The demo recipes are engine configs in the same format and were covered by
	// nothing, so they are swept here too.
	//
	// cmd/demo/config-*.yaml and cmd/desktop/config-*.yaml are deliberately NOT
	// here. They name plugins that are not in the default registry -- nexus.io.wails
	// is behind a build tag, nexus.app.staffingmatch belongs to one app -- so
	// SmokeValidateConfig cannot resolve their active set and fails for a reason
	// that says nothing about the config. They are covered by
	// TestEveryShippedConfigStillLoads in config_strict_test.go, which checks the
	// half that does not need a plugin registry: that the YAML loads and names no
	// unknown key.
	globs := []string{
		filepath.Join(repoRoot, "configs", "*.yaml"),
		filepath.Join(repoRoot, "cmd", "demo", "recipe-*.yaml"),
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
			t.Fatalf("glob %s: %v", glob, err)
		}
		for _, path := range matched {
			if notAnEngineConfig[filepath.Base(path)] {
				continue
			}
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		t.Fatal("no configs matched; this test is not checking anything")
	}

	for _, path := range paths {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}

			eng, err := engine.NewFromBytes(data)
			if err != nil {
				t.Fatalf("NewFromBytes(%s): %v", name, err)
			}
			allplugins.RegisterAll(eng.Registry)

			if err := engine.SmokeValidateConfig(eng); err != nil {
				t.Fatalf("schema validation failed for %s:\n%v", name, err)
			}
		})
	}
}

// findRepoRoot walks up from the test's CWD looking for the configs/ dir.
// Avoids hardcoding paths so the test runs from any package depth.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "configs")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate repo root from %s", wd)
	return ""
}
