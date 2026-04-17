//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// copyConfig loads a base config, overrides specific plugin configs, writes
// to a temp file, and returns the path. Used when tests need different inputs
// or settings than the base config provides.
func copyConfig(t *testing.T, baseConfigPath string, overrides map[string]any) string {
	t.Helper()

	root := findRoot(t)
	absPath := filepath.Join(root, baseConfigPath)

	data, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatalf("copyConfig: read %s: %v", absPath, err)
	}

	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("copyConfig: parse %s: %v", absPath, err)
	}

	// Merge overrides into plugins section.
	plugins, ok := cfg["plugins"].(map[string]any)
	if !ok {
		t.Fatal("copyConfig: no plugins section in config")
	}
	for k, v := range overrides {
		plugins[k] = v
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("copyConfig: marshal: %v", err)
	}

	tmpDir := t.TempDir()
	tmpPath := filepath.Join(tmpDir, "test-config.yaml")
	if err := os.WriteFile(tmpPath, out, 0o644); err != nil {
		t.Fatalf("copyConfig: write: %v", err)
	}

	return tmpPath
}

func findRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("cannot get working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot find project root (no go.mod)")
		}
		dir = parent
	}
}
