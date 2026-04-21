package codeexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSkillHelpers_ReadsGoFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(fn, body string) {
		if err := os.WriteFile(filepath.Join(dir, fn), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("util.go", "package helpers\n\nfunc Double(x int) int { return x * 2 }\n")
	write("more.go", "// top comment\npackage whatever\nfunc Triple(x int) int { return x * 3 }\n")
	write("util_test.go", "package helpers\nfunc TestOmit() {}\n")
	write("SKILL.md", "# skill doc")
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}

	h, err := loadSkillHelpers("my-skill", dir)
	if err != nil {
		t.Fatal(err)
	}
	if h == nil {
		t.Fatal("want helpers, got nil")
	}
	if len(h.Sources) != 2 {
		t.Fatalf("want 2 sources, got %d: %v", len(h.Sources), keys(h.Sources))
	}
	for fn, src := range h.Sources {
		if !strings.Contains(src, "package my_skill") {
			t.Errorf("%s: package not rewritten: %q", fn, firstLines(src, 3))
		}
	}
}

func TestLoadSkillHelpers_NoGoFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	h, err := loadSkillHelpers("s", dir)
	if err != nil {
		t.Fatal(err)
	}
	if h != nil {
		t.Fatalf("want nil, got %+v", h)
	}
}

func TestLoadSkillHelpers_MissingPackage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), []byte("// no package decl\nfunc X() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSkillHelpers("x", dir); err == nil {
		t.Fatal("expected error for missing package decl")
	}
}

func TestSanitizeSkillPackageName(t *testing.T) {
	cases := map[string]string{
		"my-skill": "my_skill",
		"skill.v2": "skill_v2",
		"":         "skill",
		"2fast":    "s2fast",
		"Hello":    "Hello",
	}
	for in, want := range cases {
		if got := sanitizeSkillPackageName(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
