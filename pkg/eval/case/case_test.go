package evalcase

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_Valid(t *testing.T) {
	dir := t.TempDir()
	writeBundle(t, dir, validBundle())

	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ID != filepath.Base(dir) {
		t.Errorf("ID=%q want %q", c.ID, filepath.Base(dir))
	}
	if c.Meta.Name != "build-error-fix" {
		t.Errorf("Meta.Name=%q", c.Meta.Name)
	}
	if len(c.Inputs) != 2 {
		t.Errorf("Inputs len=%d want 2", len(c.Inputs))
	}
	if len(c.Assertions.Deterministic) != 1 {
		t.Errorf("Deterministic len=%d want 1", len(c.Assertions.Deterministic))
	}
	if c.Assertions.Deterministic[0].Kind != "event_emitted" {
		t.Errorf("Kind=%q", c.Assertions.Deterministic[0].Kind)
	}
}

func TestLoad_MissingCaseYAML(t *testing.T) {
	dir := t.TempDir()
	bundle := validBundle()
	delete(bundle, "case.yaml")
	writeBundle(t, dir, bundle)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "case.yaml") {
		t.Errorf("err=%q does not mention case.yaml", err)
	}
}

func TestLoad_MissingName(t *testing.T) {
	dir := t.TempDir()
	bundle := validBundle()
	bundle["case.yaml"] = "description: x\n"
	writeBundle(t, dir, bundle)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("err=%q does not mention 'name'", err)
	}
}

func TestLoad_MalformedAssertions(t *testing.T) {
	dir := t.TempDir()
	bundle := validBundle()
	bundle["assertions.yaml"] = `deterministic:
  - kind: event_emitted
    # missing 'type'
`
	writeBundle(t, dir, bundle)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestLoad_UnknownKind(t *testing.T) {
	dir := t.TempDir()
	bundle := validBundle()
	bundle["assertions.yaml"] = `deterministic:
  - kind: nonsense
`
	writeBundle(t, dir, bundle)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "nonsense") {
		t.Errorf("err=%q", err)
	}
}

func TestLoad_NotADirectory(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(f)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// validBundle returns a minimal valid bundle. The journal/header.json is the
// minimum the journal package's Open accepts.
func validBundle() map[string]string {
	return map[string]string{
		"case.yaml": `name: build-error-fix
description: example
tags: [react, tools]
owner: test@example.com
freshness_days: 90
model_baseline: claude-haiku-4-5-20251001
`,
		"input/config.yaml": `core:
  log_level: warn
plugins:
  active: []
`,
		"input/inputs.yaml": `inputs:
  - first
  - second
`,
		"assertions.yaml": `deterministic:
  - kind: event_emitted
    type: io.input
    count:
      min: 1
`,
		"journal/header.json": `{"schema_version":"1","created_at":"2026-05-01T00:00:00Z","fsync_mode":"none","session_id":"test"}`,
	}
}

func writeBundle(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
