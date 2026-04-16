package skills

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxScanDepth = 4
	maxScanDirs  = 2000
	skillFile    = "SKILL.md"
)

// skipDirs contains directory names that should be skipped during scanning.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"__pycache__":  true,
}

// ScanForSkills scans the given paths for directories containing SKILL.md files.
func ScanForSkills(paths []string, logger *slog.Logger) []SkillRecord {
	seen := make(map[string]*SkillRecord) // name -> first-found record
	var result []SkillRecord
	dirsScanned := 0

	for _, basePath := range paths {
		scope := inferScope(basePath)
		scanDir(basePath, scope, 0, &dirsScanned, seen, &result, logger)
	}

	return result
}

// DefaultScanPaths returns the standard skill scan locations.
func DefaultScanPaths(cwd string) []string {
	home, _ := os.UserHomeDir()

	paths := []string{
		filepath.Join(cwd, ".nexus", "skills"),
		filepath.Join(cwd, ".agents", "skills"),
	}
	if home != "" {
		paths = append(paths,
			filepath.Join(home, ".nexus", "skills"),
			filepath.Join(home, ".agents", "skills"),
		)
	}
	return paths
}

func scanDir(dir string, scope string, depth int, dirsScanned *int, seen map[string]*SkillRecord, result *[]SkillRecord, logger *slog.Logger) {
	if depth > maxScanDepth || *dirsScanned > maxScanDirs {
		return
	}

	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return
	}
	*dirsScanned++

	// Check if this directory contains a SKILL.md.
	skillPath := filepath.Join(dir, skillFile)
	if _, err := os.Stat(skillPath); err == nil {
		record, err := ParseSkillFile(skillPath)
		if err != nil {
			logger.Warn("failed to parse skill file", "path", skillPath, "error", err)
		} else {
			record.BaseDir = dir
			record.Scope = scope

			if existing, ok := seen[record.Name]; ok {
				// Project scope wins over user scope.
				if scopePriority(record.Scope) < scopePriority(existing.Scope) {
					logger.Warn("skill name collision, overriding",
						"name", record.Name,
						"new_scope", record.Scope,
						"existing_scope", existing.Scope,
					)
					*existing = *record
				} else {
					logger.Warn("skill name collision, keeping existing",
						"name", record.Name,
						"new_scope", record.Scope,
						"existing_scope", existing.Scope,
					)
				}
			} else {
				seen[record.Name] = record
				*result = append(*result, *record)
			}
		}
		return // Don't scan subdirectories of skill directories.
	}

	// Recurse into subdirectories.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if skipDirs[name] || strings.HasPrefix(name, ".") {
			continue
		}
		scanDir(filepath.Join(dir, name), scope, depth+1, dirsScanned, seen, result, logger)
	}
}

func inferScope(path string) string {
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(path, home) {
		// Paths under ~/.nexus or ~/.agents are user scope.
		rel, _ := filepath.Rel(home, path)
		if strings.HasPrefix(rel, ".nexus") || strings.HasPrefix(rel, ".agents") {
			return "user"
		}
	}
	return "project"
}

func scopePriority(scope string) int {
	switch scope {
	case "project":
		return 0
	case "config":
		return 1
	case "user":
		return 2
	case "builtin":
		return 3
	default:
		return 99
	}
}
