package engine

import (
	"fmt"
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
// The caller must guarantee the target session is not running — there
// is no in-process coordination; concurrent writes against a journal
// being rewound produce undefined state. The CLI driver enforces this
// by refusing to rewind a session that has an active lock file (TODO
// once we add session locks; for now, refusing to rewind the live
// session ID is the operator's responsibility).
//
// Returns the archive directory name for use with RestoreSession.
func RewindSession(cfg Config, sessionID string, toSeq uint64) (journal.RewindResult, error) {
	dir, err := sessionJournalDir(cfg, sessionID)
	if err != nil {
		return journal.RewindResult{}, err
	}
	return journal.Rewind(dir, toSeq)
}

// RestoreSession swaps the live journal for a previously archived
// snapshot. The current live journal is itself archived first so the
// flip is reversible.
func RestoreSession(cfg Config, sessionID, archiveName string) error {
	dir, err := sessionJournalDir(cfg, sessionID)
	if err != nil {
		return err
	}
	return journal.Restore(dir, archiveName)
}

// ListSessionArchives returns the names of every rewind archive for the
// named session, sorted ascending by timestamp.
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
