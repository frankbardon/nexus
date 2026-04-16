package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SessionConfigSnapshotPath returns the path to a session's config snapshot.
// It uses the sessions root from the provided config file (or the default) to locate
// the session directory. The configPath is needed to determine the sessions root dir.
func SessionConfigSnapshotPath(configPath string, sessionID string) (string, error) {
	var root string
	if configPath == "" {
		root = DefaultConfig().Core.Sessions.Root
	} else {
		cfg, err := LoadConfig(configPath)
		if err != nil {
			// Fall back to default root if config can't be loaded.
			root = DefaultConfig().Core.Sessions.Root
		} else {
			root = cfg.Core.Sessions.Root
		}
	}

	root = expandHome(root)
	snapshotPath := filepath.Join(root, sessionID, "metadata", "config-snapshot.yaml")

	if _, err := os.Stat(snapshotPath); err != nil {
		return "", fmt.Errorf("config snapshot not found for session %q: %w", sessionID, err)
	}

	return snapshotPath, nil
}

// SessionWorkspace manages a session's file-based workspace.
type SessionWorkspace struct {
	ID        string
	RootDir   string
	StartedAt time.Time
	bus       EventBus
}

// SessionMeta holds metadata about a session.
type SessionMeta struct {
	ID         string            `json:"id"`
	StartedAt  time.Time         `json:"started_at"`
	EndedAt    *time.Time        `json:"ended_at,omitempty"`
	Profile    string            `json:"profile"`
	Plugins    []string          `json:"plugins"`
	Labels     map[string]string `json:"labels"`
	TurnCount  int               `json:"turn_count"`
	TokensUsed int               `json:"tokens_used"`
	Status     string            `json:"status"`
}

// NewSessionWorkspace creates a new session workspace with the standard directory structure.
func NewSessionWorkspace(rootDir string, bus EventBus) (*SessionWorkspace, error) {
	id := generateID()
	now := time.Now()
	sessionDir := filepath.Join(rootDir, id)

	s := &SessionWorkspace{
		ID:        id,
		RootDir:   sessionDir,
		StartedAt: now,
		bus:       bus,
	}

	// Create directory structure.
	dirs := []string{
		s.ContextDir(),
		s.FilesDir(),
		filepath.Join(sessionDir, "plugins"),
		s.MetadataDir(),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating session directory %s: %w", dir, err)
		}
	}

	// Write initial metadata.
	meta := &SessionMeta{
		ID:        id,
		StartedAt: now,
		Labels:    make(map[string]string),
		Status:    "active",
	}
	if err := s.SaveMeta(meta); err != nil {
		return nil, fmt.Errorf("saving initial metadata: %w", err)
	}

	return s, nil
}

// LoadSessionWorkspace opens an existing session workspace by ID.
// It reads the session metadata and returns a workspace pointing at the existing directory.
func LoadSessionWorkspace(rootDir string, sessionID string, bus EventBus) (*SessionWorkspace, error) {
	sessionDir := filepath.Join(rootDir, sessionID)

	info, err := os.Stat(sessionDir)
	if err != nil {
		return nil, fmt.Errorf("session %q not found: %w", sessionID, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("session path %q is not a directory", sessionDir)
	}

	s := &SessionWorkspace{
		ID:      sessionID,
		RootDir: sessionDir,
		bus:     bus,
	}

	// Read existing metadata to restore StartedAt.
	meta, err := s.SessionMetadata()
	if err != nil {
		return nil, fmt.Errorf("reading session metadata: %w", err)
	}
	s.StartedAt = meta.StartedAt

	// Mark session as active again.
	meta.EndedAt = nil
	meta.Status = "active"
	if err := s.SaveMeta(meta); err != nil {
		return nil, fmt.Errorf("updating session metadata: %w", err)
	}

	return s, nil
}

// ContextDir returns the path to the context subdirectory.
func (s *SessionWorkspace) ContextDir() string {
	return filepath.Join(s.RootDir, "context")
}

// FilesDir returns the path to the files subdirectory.
func (s *SessionWorkspace) FilesDir() string {
	return filepath.Join(s.RootDir, "files")
}

// MetadataDir returns the path to the metadata subdirectory.
func (s *SessionWorkspace) MetadataDir() string {
	return filepath.Join(s.RootDir, "metadata")
}

// PluginDir returns the path to a plugin-specific directory, creating it lazily.
func (s *SessionWorkspace) PluginDir(pluginID string) string {
	dir := filepath.Join(s.RootDir, "plugins", pluginID)
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// WriteFile writes data to a file within the session workspace.
// It emits session.file.created or session.file.updated events.
func (s *SessionWorkspace) WriteFile(subpath string, data []byte) error {
	fullPath := filepath.Join(s.RootDir, subpath)

	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating directory for %s: %w", subpath, err)
	}

	existed := s.FileExists(subpath)

	if err := os.WriteFile(fullPath, data, 0o644); err != nil {
		return fmt.Errorf("writing file %s: %w", subpath, err)
	}

	if s.bus != nil {
		eventType := "session.file.created"
		if existed {
			eventType = "session.file.updated"
		}
		_ = s.bus.Emit(eventType, map[string]any{
			"session_id": s.ID,
			"path":       subpath,
			"size":       len(data),
		})
	}

	return nil
}

// ReadFile reads a file from the session workspace.
func (s *SessionWorkspace) ReadFile(subpath string) ([]byte, error) {
	fullPath := filepath.Join(s.RootDir, subpath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("reading file %s: %w", subpath, err)
	}
	return data, nil
}

// AppendFile appends data to a file in the session workspace.
func (s *SessionWorkspace) AppendFile(subpath string, data []byte) error {
	fullPath := filepath.Join(s.RootDir, subpath)

	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating directory for %s: %w", subpath, err)
	}

	f, err := os.OpenFile(fullPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening file %s for append: %w", subpath, err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("appending to file %s: %w", subpath, err)
	}

	return nil
}

// ListFiles lists files under a subdirectory in the session workspace.
func (s *SessionWorkspace) ListFiles(subpath string) ([]string, error) {
	fullPath := filepath.Join(s.RootDir, subpath)

	entries, err := os.ReadDir(fullPath)
	if err != nil {
		return nil, fmt.Errorf("listing files in %s: %w", subpath, err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// FileExists returns true if a file exists in the session workspace.
func (s *SessionWorkspace) FileExists(subpath string) bool {
	fullPath := filepath.Join(s.RootDir, subpath)
	_, err := os.Stat(fullPath)
	return err == nil
}

// SessionMeta reads and returns the session metadata.
func (s *SessionWorkspace) SessionMetadata() (*SessionMeta, error) {
	data, err := os.ReadFile(filepath.Join(s.MetadataDir(), "session.json"))
	if err != nil {
		return nil, fmt.Errorf("reading session metadata: %w", err)
	}

	var meta SessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parsing session metadata: %w", err)
	}
	return &meta, nil
}

// SaveMeta writes session metadata to disk.
func (s *SessionWorkspace) SaveMeta(meta *SessionMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling session metadata: %w", err)
	}

	metaPath := filepath.Join(s.MetadataDir(), "session.json")
	if err := os.WriteFile(metaPath, data, 0o644); err != nil {
		return fmt.Errorf("writing session metadata: %w", err)
	}
	return nil
}
