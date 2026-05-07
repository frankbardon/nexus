package engine_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/allplugins"
)

// TestConfigsSmokeBoot loads every YAML under configs/ and runs the schema
// validator over the resulting Config + active plugin set. This is the canary
// that catches future YAML/schema drift: any YAML file in the repo must
// continue to satisfy every plugin schema. It does not actually Boot the
// engine (live external dependencies, API keys, etc) — schema validation
// alone is the contract the test enforces.
func TestConfigsSmokeBoot(t *testing.T) {
	repoRoot := findRepoRoot(t)
	configDir := filepath.Join(repoRoot, "configs")
	entries, err := os.ReadDir(configDir)
	if err != nil {
		t.Fatalf("read configs dir: %v", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		name := entry.Name()
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(configDir, name)
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
