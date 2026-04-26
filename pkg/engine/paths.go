package engine

import (
	"os"
	"path/filepath"
	"strings"
)

// ExpandPath expands a leading "~" or "~/..." in a filesystem path to the
// user's home directory. Bare "~" maps to the home dir; "~/foo" maps to
// "<home>/foo". Other input is returned unchanged. If the home directory
// cannot be determined, the input is returned as-is.
//
// This is the canonical helper for tilde expansion in Nexus. Plugins and
// the desktop shell should funnel every config-supplied path through this
// function so users can write "~/.nexus/foo" anywhere a path is accepted.
func ExpandPath(path string) string {
	if path == "" {
		return path
	}
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}
