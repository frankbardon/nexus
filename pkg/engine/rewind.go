package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/frankbardon/nexus/pkg/engine/journal"
)

// RewindSession is the offline equivalent of engine.ReplaySession.
//
// It archives the named session's journal and writes a truncated copy
// that ends at toSeq inclusive. The original journal is preserved at
// <sessions.root>/<sessionID>/journal/archive/<timestamp>/ so the
// operation is reversible via RestoreSession.
//
// The caller must guarantee the target session is not running. The
// engine writes a session.lock file under <sessions.root>/<sessionID>/
// while a session is active; RewindSession refuses to operate when a
// non-stale lock is present. Pass force=true to bypass the check (the
// CLI surfaces this as --force) — concurrent writes against a journal
// being rewound produce undefined state, so this should only be used
// when the operator is certain the lock is from a wedged process that
// cannot be killed cleanly.
//
// Returns the archive directory name for use with RestoreSession.
func RewindSession(cfg Config, sessionID string, toSeq uint64, force bool) (journal.RewindResult, error) {
	if err := checkSessionLock(cfg, sessionID, force); err != nil {
		return journal.RewindResult{}, err
	}
	dir, err := sessionJournalDir(cfg, sessionID)
	if err != nil {
		return journal.RewindResult{}, err
	}
	return journal.Rewind(dir, toSeq)
}

// RestoreSession swaps the live journal for a previously archived
// snapshot. The current live journal is itself archived first so the
// flip is reversible. Like RewindSession, refuses to operate when a
// non-stale session lock is present unless force is set.
func RestoreSession(cfg Config, sessionID, archiveName string, force bool) error {
	if err := checkSessionLock(cfg, sessionID, force); err != nil {
		return err
	}
	dir, err := sessionJournalDir(cfg, sessionID)
	if err != nil {
		return err
	}
	return journal.Restore(dir, archiveName)
}

// ListSessionArchives returns the names of every rewind archive for the
// named session, sorted ascending by timestamp. Read-only — does not
// consult the session lock.
func ListSessionArchives(cfg Config, sessionID string) ([]string, error) {
	dir, err := sessionJournalDir(cfg, sessionID)
	if err != nil {
		return nil, err
	}
	return journal.ListArchives(dir)
}

func sessionJournalDir(cfg Config, sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("rewind: empty session id")
	}
	root := ExpandPath(cfg.Core.Sessions.Root)
	if root == "" {
		return "", fmt.Errorf("rewind: sessions.root not configured")
	}
	return filepath.Join(root, sessionID, "journal"), nil
}

// sessionRootDir returns <sessions.root>/<sessionID>, the directory
// that owns the session.lock file. Distinct from sessionJournalDir
// because the journal lives one level deeper.
func sessionRootDir(cfg Config, sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("rewind: empty session id")
	}
	root := ExpandPath(cfg.Core.Sessions.Root)
	if root == "" {
		return "", fmt.Errorf("rewind: sessions.root not configured")
	}
	return filepath.Join(root, sessionID), nil
}

// checkSessionLock returns a SessionLockedError when the session has a
// non-stale lock and force is false. Stale locks pass silently — they
// are removed by the next Boot. Missing locks pass.
func checkSessionLock(cfg Config, sessionID string, force bool) error {
	if force {
		return nil
	}
	dir, err := sessionRootDir(cfg, sessionID)
	if err != nil {
		return err
	}
	lock, err := ReadSessionLock(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		// A malformed lock is not a green light — surface it so the
		// operator can investigate. --force will still bypass.
		return fmt.Errorf("session lock: %w", err)
	}
	if IsLockStale(lock) {
		return nil
	}
	return &SessionLockedError{Dir: dir, Lock: lock}
}
