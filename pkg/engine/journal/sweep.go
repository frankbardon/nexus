package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Sweep removes session journal directories whose enclosing session was last
// modified more than retainDays ago. Called from engine boot in a goroutine
// so a slow filesystem does not delay startup.
//
// root is the sessions root (e.g. ~/.nexus/sessions). retainDays <= 0 is a
// no-op so accidental misconfiguration cannot purge a freshly-cleaned home.
func Sweep(root string, retainDays int) error {
	if retainDays <= 0 {
		return nil
	}
	cutoff := time.Now().Add(-time.Duration(retainDays) * 24 * time.Hour)

	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("listing sessions root: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		journalDir := filepath.Join(root, entry.Name(), "journal")
		info, err := os.Stat(journalDir)
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		// Use ModTime of the journal dir (not session dir) so an active
		// session's metadata writes do not protect a stale journal. The
		// directory's mtime is bumped whenever a child file is written.
		_ = os.RemoveAll(journalDir)
	}
	return nil
}
