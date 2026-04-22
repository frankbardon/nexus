package desktop

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// SessionMeta is the shell-level metadata for a single engine session.
// The engine's own SessionWorkspace (under ~/.nexus/sessions/<id>/) is
// the source of truth for session content; this struct tracks the
// shell-layer projection: which agent owns it, the user-facing title,
// status, and optional preview data contributed by the agent plugin.
type SessionMeta struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"` // "running", "completed", "failed"
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Preview   any       `json:"preview,omitempty"`
}

// sessionsData is the on-disk JSON structure for the session index.
type sessionsData struct {
	Version  int           `json:"version"`
	Sessions []SessionMeta `json:"sessions"`
}

// sessionIndex is the shell-level session metadata store, backed by a
// JSON file at ~/.nexus/desktop/sessions.json. It does not own the
// engine session directories — only the metadata projection.
type sessionIndex struct {
	mu       sync.Mutex
	path     string
	sessions []SessionMeta
}

func newSessionIndex(dir string) (*sessionIndex, error) {
	path := filepath.Join(dir, "sessions.json")

	idx := &sessionIndex{path: path}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			idx.sessions = []SessionMeta{}
			return idx, nil
		}
		return nil, fmt.Errorf("reading session index: %w", err)
	}

	var stored sessionsData
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("parsing session index: %w", err)
	}
	idx.sessions = stored.Sessions
	if idx.sessions == nil {
		idx.sessions = []SessionMeta{}
	}
	return idx, nil
}

// Add inserts a new session entry and persists the index.
func (idx *sessionIndex) Add(meta SessionMeta) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	idx.sessions = append(idx.sessions, meta)
	return idx.save()
}

// Update applies fn to the session with the given ID and persists.
func (idx *sessionIndex) Update(sessionID string, fn func(*SessionMeta)) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	for i := range idx.sessions {
		if idx.sessions[i].ID == sessionID {
			fn(&idx.sessions[i])
			return idx.save()
		}
	}
	return fmt.Errorf("session %q not found in index", sessionID)
}

// Get returns the session with the given ID, if it exists.
func (idx *sessionIndex) Get(sessionID string) (SessionMeta, bool) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	for _, s := range idx.sessions {
		if s.ID == sessionID {
			return s, true
		}
	}
	return SessionMeta{}, false
}

// List returns sessions for the given agent, sorted most-recent first.
func (idx *sessionIndex) List(agentID string) []SessionMeta {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var result []SessionMeta
	for _, s := range idx.sessions {
		if s.AgentID == agentID {
			result = append(result, s)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

// Delete removes a session from the index and persists.
func (idx *sessionIndex) Delete(sessionID string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	for i, s := range idx.sessions {
		if s.ID == sessionID {
			idx.sessions = append(idx.sessions[:i], idx.sessions[i+1:]...)
			return idx.save()
		}
	}
	return nil // not found is not an error
}

// Cleanup removes sessions older than retentionDays from both the index
// and the engine's session directories on disk. Returns the number of
// sessions removed.
func (idx *sessionIndex) Cleanup(retentionDays int, sessionsRoot string) (int, error) {
	if retentionDays <= 0 {
		return 0, nil
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	var kept []SessionMeta
	removed := 0

	for _, s := range idx.sessions {
		if s.UpdatedAt.Before(cutoff) {
			// Delete the engine session directory.
			dir := filepath.Join(sessionsRoot, s.ID)
			_ = os.RemoveAll(dir)
			removed++
		} else {
			kept = append(kept, s)
		}
	}

	if removed > 0 {
		idx.sessions = kept
		if idx.sessions == nil {
			idx.sessions = []SessionMeta{}
		}
		if err := idx.save(); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// Reconcile synchronizes the index with what's actually on disk.
//   - Orphaned engine directories (exist on disk, not in index) are
//     adopted with a generic title if newer than cutoff, or deleted.
//   - Stale index entries (in index, missing from disk) are removed.
func (idx *sessionIndex) Reconcile(sessionsRoot string, agents []Agent, retentionDays int) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	cutoff := time.Now().AddDate(0, 0, -retentionDays)

	// Build set of known session IDs.
	known := make(map[string]bool, len(idx.sessions))
	for _, s := range idx.sessions {
		known[s.ID] = true
	}

	// Scan the sessions root for directories.
	entries, err := os.ReadDir(sessionsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading sessions root: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sid := entry.Name()
		if known[sid] {
			continue
		}
		// Orphaned session directory — check age.
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(sessionsRoot, sid))
			continue
		}
		// Adopt: try to read engine metadata to extract creation time.
		meta := SessionMeta{
			ID:        sid,
			Title:     "Untitled",
			Status:    "completed",
			CreatedAt: info.ModTime(),
			UpdatedAt: info.ModTime(),
		}
		// Try reading the engine's session.json for better timestamps.
		engineMetaPath := filepath.Join(sessionsRoot, sid, "metadata", "session.json")
		if data, err := os.ReadFile(engineMetaPath); err == nil {
			var eMeta struct {
				StartedAt time.Time `json:"started_at"`
				Status    string    `json:"status"`
			}
			if json.Unmarshal(data, &eMeta) == nil {
				if !eMeta.StartedAt.IsZero() {
					meta.CreatedAt = eMeta.StartedAt
					meta.UpdatedAt = eMeta.StartedAt
				}
				if eMeta.Status != "" {
					meta.Status = eMeta.Status
				}
			}
		}
		// Try to match to an agent by checking the config snapshot.
		configPath := filepath.Join(sessionsRoot, sid, "metadata", "config-snapshot.yaml")
		if _, err := os.Stat(configPath); err == nil {
			// We can't easily determine agent ID from the config snapshot
			// without parsing all agent configs. Leave AgentID empty —
			// these orphans won't appear in any agent's session list.
		}
		idx.sessions = append(idx.sessions, meta)
	}

	// Remove stale entries whose directories no longer exist.
	var kept []SessionMeta
	for _, s := range idx.sessions {
		dir := filepath.Join(sessionsRoot, s.ID)
		if _, err := os.Stat(dir); err == nil {
			kept = append(kept, s)
		}
	}
	idx.sessions = kept
	if idx.sessions == nil {
		idx.sessions = []SessionMeta{}
	}

	return idx.save()
}

func (idx *sessionIndex) save() error {
	data, err := json.MarshalIndent(sessionsData{
		Version:  1,
		Sessions: idx.sessions,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling session index: %w", err)
	}
	return os.WriteFile(idx.path, data, 0o644)
}
