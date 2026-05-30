package icm

import (
	"os"
	"path/filepath"
)

// resolveWorkspacePath joins a workspace-relative path against the
// workspace root. Absolute paths pass through unchanged.
func resolveWorkspacePath(root, rel string) string {
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(root, rel)
}

// readFile is a thin wrapper around os.ReadFile so the predicate
// dispatchers can be unit-tested with a swap-in path.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
