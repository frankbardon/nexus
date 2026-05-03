package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSkill(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseRuntimeDefault(t *testing.T) {
	path := writeSkill(t, t.TempDir(), `---
name: alpha
description: a skill
---
body`)
	rec, err := ParseSkillFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Runtime != "" {
		t.Fatalf("expected empty Runtime by default, got %q", rec.Runtime)
	}
}

func TestParseRuntimeYaegi(t *testing.T) {
	path := writeSkill(t, t.TempDir(), `---
name: alpha
description: a skill
runtime: yaegi
---
body`)
	rec, err := ParseSkillFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Runtime != "yaegi" {
		t.Fatalf("got %q, want yaegi", rec.Runtime)
	}
}

func TestParseRuntimeWasm(t *testing.T) {
	path := writeSkill(t, t.TempDir(), `---
name: alpha
description: a skill
runtime: wasm
---
body`)
	rec, err := ParseSkillFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Runtime != "wasm" {
		t.Fatalf("got %q, want wasm", rec.Runtime)
	}
}

func TestParseRuntimeInvalid(t *testing.T) {
	path := writeSkill(t, t.TempDir(), `---
name: alpha
description: a skill
runtime: bogus
---
body`)
	_, err := ParseSkillFile(path)
	if err == nil {
		t.Fatal("expected error for invalid runtime")
	}
}
