package codeexec

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// skillHelpers holds the parsed helper source files for one active skill.
// Stored in plugin state between skill.loaded / skill.deactivate events.
type skillHelpers struct {
	Name    string
	BaseDir string
	Sources map[string]string // filename → source (package already normalized)
}

// loadSkillHelpers scans a skill's BaseDir for .go files (non-recursive)
// and returns their contents keyed by filename. Ignores test files.
//
// Each source file is rewritten so its package declaration becomes the
// virtual skill package — this lets skill authors name the package whatever
// is convenient while we present it to scripts as "skills/<skill_name>".
func loadSkillHelpers(name, baseDir string) (*skillHelpers, error) {
	if baseDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil, fmt.Errorf("skill %s: read %s: %w", name, baseDir, err)
	}

	sources := map[string]string{}
	pkgName := sanitizeSkillPackageName(name)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fn := e.Name()
		if !strings.HasSuffix(fn, ".go") || strings.HasSuffix(fn, "_test.go") {
			continue
		}
		full := filepath.Join(baseDir, fn)
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, fmt.Errorf("skill %s: read %s: %w", name, fn, err)
		}
		rewritten, err := rewriteSkillPackage(string(data), pkgName)
		if err != nil {
			return nil, fmt.Errorf("skill %s: rewrite %s: %w", name, fn, err)
		}
		sources[fn] = rewritten
	}
	if len(sources) == 0 {
		return nil, nil
	}
	return &skillHelpers{Name: name, BaseDir: baseDir, Sources: sources}, nil
}

// sanitizeSkillPackageName converts a skill name into a valid Go package
// identifier. "my-skill" → "my_skill", "skill.v2" → "skill_v2".
func sanitizeSkillPackageName(name string) string {
	if name == "" {
		return "skill"
	}
	var b strings.Builder
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			if i == 0 {
				b.WriteRune('s')
			}
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		return "skill"
	}
	return out
}

// rewriteSkillPackage replaces whatever package declaration a helper file
// opens with the canonical skill package name, so Yaegi resolves the whole
// set under one import. Comment lines preceding `package ...` are preserved.
func rewriteSkillPackage(src, pkgName string) (string, error) {
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "package ") {
			lines[i] = "package " + pkgName
			return strings.Join(lines, "\n"), nil
		}
	}
	return "", fmt.Errorf("no package declaration found")
}
