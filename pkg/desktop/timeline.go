package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/journal"
)

// TimelineEvent is a lightweight projection of a journal envelope used
// by the desktop timeline panel. Payloads are intentionally omitted from
// the listing call so the frontend can stream tens of thousands of
// events without paying the marshal cost; full payloads are fetched
// on-demand via GetSessionEvent.
type TimelineEvent struct {
	Seq        uint64 `json:"seq"`
	Ts         string `json:"ts"`
	Type       string `json:"type"`
	Source     string `json:"source,omitempty"`
	EventID    string `json:"event_id,omitempty"`
	ParentSeq  uint64 `json:"parent_seq,omitempty"`
	SideEffect bool   `json:"side_effect,omitempty"`
	Vetoed     bool   `json:"vetoed,omitempty"`
}

// TimelineEventDetail extends TimelineEvent with the full payload, used
// when the operator clicks an event row to inspect its contents.
type TimelineEventDetail struct {
	TimelineEvent
	Payload any `json:"payload,omitempty"`
}

// RewindResultInfo is the JSON-friendly result returned to the frontend
// after a successful rewind.
type RewindResultInfo struct {
	ArchiveName    string `json:"archive_name"`
	TruncatedSeq   uint64 `json:"truncated_seq"`
	EventsKept     int    `json:"events_kept"`
	EventsArchived int    `json:"events_archived"`
}

// InspectSession returns the journal as a slice of lightweight
// timeline events, oldest-first. Payloads are omitted; use
// GetSessionEvent to fetch one. Returns an empty slice (not an error)
// when the session has no journal directory yet.
func (s *Shell) InspectSession(agentID, sessionID string) ([]TimelineEvent, error) {
	dir, err := s.sessionJournalDir(agentID, sessionID)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return []TimelineEvent{}, nil
	} else if err != nil {
		return nil, fmt.Errorf("stat journal dir: %w", err)
	}

	r, err := journal.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}

	events := make([]TimelineEvent, 0, 64)
	iterErr := r.Iter(func(env journal.Envelope) bool {
		events = append(events, TimelineEvent{
			Seq:        env.Seq,
			Ts:         env.Ts.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
			Type:       env.Type,
			Source:     env.Source,
			EventID:    env.EventID,
			ParentSeq:  env.ParentSeq,
			SideEffect: env.SideEffect,
			Vetoed:     env.Vetoed,
		})
		return true
	})
	if iterErr != nil {
		return nil, fmt.Errorf("iter journal: %w", iterErr)
	}
	return events, nil
}

// GetSessionEvent returns the full envelope (including payload) for the
// requested seq. Used by the timeline panel's payload preview.
func (s *Shell) GetSessionEvent(agentID, sessionID string, seq uint64) (TimelineEventDetail, error) {
	if seq == 0 {
		return TimelineEventDetail{}, fmt.Errorf("seq must be >= 1")
	}
	dir, err := s.sessionJournalDir(agentID, sessionID)
	if err != nil {
		return TimelineEventDetail{}, err
	}
	r, err := journal.Open(dir)
	if err != nil {
		return TimelineEventDetail{}, fmt.Errorf("open journal: %w", err)
	}

	var found *journal.Envelope
	iterErr := r.Iter(func(env journal.Envelope) bool {
		if env.Seq == seq {
			cp := env
			found = &cp
			return false
		}
		return true
	})
	if iterErr != nil {
		return TimelineEventDetail{}, fmt.Errorf("iter journal: %w", iterErr)
	}
	if found == nil {
		return TimelineEventDetail{}, fmt.Errorf("seq %d not found in journal", seq)
	}
	return TimelineEventDetail{
		TimelineEvent: TimelineEvent{
			Seq:        found.Seq,
			Ts:         found.Ts.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
			Type:       found.Type,
			Source:     found.Source,
			EventID:    found.EventID,
			ParentSeq:  found.ParentSeq,
			SideEffect: found.SideEffect,
			Vetoed:     found.Vetoed,
		},
		Payload: found.Payload,
	}, nil
}

// RewindSession archives the named session's journal and truncates it
// to toSeq inclusive. The session must not be the agent's currently
// running session — concurrent writes against a journal being rewound
// produce undefined state. Returns the archive name plus rewind stats.
func (s *Shell) RewindSession(agentID, sessionID string, toSeq uint64) (RewindResultInfo, error) {
	if err := s.ensureSessionNotLive(agentID, sessionID); err != nil {
		return RewindResultInfo{}, err
	}
	dir, err := s.sessionJournalDir(agentID, sessionID)
	if err != nil {
		return RewindResultInfo{}, err
	}
	res, err := journal.Rewind(dir, toSeq)
	if err != nil {
		return RewindResultInfo{}, err
	}
	return RewindResultInfo{
		ArchiveName:    res.ArchiveName,
		TruncatedSeq:   res.TruncatedSeq,
		EventsKept:     res.EventsKept,
		EventsArchived: res.EventsArchived,
	}, nil
}

// RestoreSession swaps the live journal for a previously archived
// snapshot. The current live journal is itself archived first. Same
// liveness rule as RewindSession: refuses to operate on the active
// session.
func (s *Shell) RestoreSession(agentID, sessionID, archiveName string) error {
	if err := s.ensureSessionNotLive(agentID, sessionID); err != nil {
		return err
	}
	dir, err := s.sessionJournalDir(agentID, sessionID)
	if err != nil {
		return err
	}
	return journal.Restore(dir, archiveName)
}

// ListSessionArchives returns the names of every rewind archive for the
// named session, oldest-first.
func (s *Shell) ListSessionArchives(agentID, sessionID string) ([]string, error) {
	dir, err := s.sessionJournalDir(agentID, sessionID)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return []string{}, nil
	}
	names, err := journal.ListArchives(dir)
	if err != nil {
		return nil, err
	}
	if names == nil {
		names = []string{}
	}
	return names, nil
}

// sessionJournalDir builds the absolute path to a session's journal
// directory. Validates the session id contains no path separators.
func (s *Shell) sessionJournalDir(agentID, sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("empty session id")
	}
	if strings.ContainsAny(sessionID, "/\\") {
		return "", fmt.Errorf("invalid session id %q", sessionID)
	}
	root, err := s.sessionsRootForAgent(agentID)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, sessionID, "journal"), nil
}

// sessionsRootForAgent resolves the on-disk sessions root the engine
// uses for the given agent. Honors the agent's own ConfigYAML when it
// sets core.sessions.root, falling back to the shell's resolved
// sessions root.
//
// This duplicates a minimal slice of engine config parsing rather than
// re-running a full LoadConfigFromBytes — we only need one field, and
// the YAML may contain unresolved ${...} placeholders for fields that
// don't affect the path.
func (s *Shell) sessionsRootForAgent(agentID string) (string, error) {
	agent := s.findAgent(agentID)
	if agent == nil {
		return "", fmt.Errorf("agent %q not registered", agentID)
	}
	// Try parsing the agent's config to read core.sessions.root.
	// resolveConfig may have ${var}s the user has not set, but
	// core.sessions.root is normally a literal path so we attempt a
	// permissive parse first.
	if cfg, err := engine.LoadConfigFromBytes(agent.ConfigYAML); err == nil {
		if root := strings.TrimSpace(cfg.Core.Sessions.Root); root != "" {
			return engine.ExpandPath(root), nil
		}
	}
	// Fall back to the shell-resolved sessions root.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home dir: %w", err)
	}
	return s.resolvedSessionsRoot(home), nil
}

// ensureSessionNotLive refuses to mutate the journal for the agent's
// currently running session. Rewind/restore against an active writer
// produces undefined state.
func (s *Shell) ensureSessionNotLive(agentID, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.agents[agentID]
	if !ok {
		return nil
	}
	if state.eng != nil && state.sessionID == sessionID {
		return fmt.Errorf("session %q is currently running; stop the agent or recall a different session before rewinding", sessionID)
	}
	return nil
}
