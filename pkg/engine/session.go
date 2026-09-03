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

	root = ExpandPath(root)
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
	ID                   string            `json:"id"`
	StartedAt            time.Time         `json:"started_at"`
	EndedAt              *time.Time        `json:"ended_at,omitempty"`
	Profile              string            `json:"profile"`
	Plugins              []string          `json:"plugins"`
	Labels               map[string]string `json:"labels"`
	TurnCount            int               `json:"turn_count"`
	TokensUsed           int               `json:"tokens_used"`
	PromptTokensUsed     int               `json:"prompt_tokens_used"`
	CompletionTokensUsed int               `json:"completion_tokens_used"`
	CostUSD              float64           `json:"cost_usd"`
	Status               string            `json:"status"`
}

// NewSessionWorkspace creates a new session workspace with the standard directory structure.
func NewSessionWorkspace(rootDir string, bus EventBus) (*SessionWorkspace, error) {
	return newSessionWorkspaceAt(rootDir, GenerateID(), bus)
}

// newSessionWorkspaceAt creates a session workspace under a caller-supplied ID.
//
// Split out of NewSessionWorkspace for the object-store hydrate path: resuming
// a session ID the store has never seen must yield a tree indistinguishable
// from a brand-new local session, and the only way to guarantee
// "indistinguishable" is to run the same code rather than a second
// almost-identical directory-creation routine that will drift.
//
// Unexported because minting a session under a caller-chosen ID is an
// engine-internal concern; every external caller wants a fresh ID.
func newSessionWorkspaceAt(rootDir string, id string, bus EventBus) (*SessionWorkspace, error) {
	now := time.Now()
	sessionDir := filepath.Join(rootDir, id)

	// The bus is attached only after the bootstrap metadata write below, so
	// that write stays silent. Engine.prepareSession documents that creating
	// the workspace emits no bus events, and that is a hard invariant rather
	// than a tidiness preference: it runs before startJournal, and the bus
	// assigns a dispatch seq to every event whether or not the journal's
	// wildcard is subscribed yet. An event here would consume seq 1 and never
	// reach the writer, whose drain writes only in contiguous seq order — so
	// the missing seq would stall every later envelope forever and the journal
	// would be empty. StartSession re-saves the metadata once the journal is
	// running, so the file is still announced, just not from in here.
	s := &SessionWorkspace{
		ID:        id,
		RootDir:   sessionDir,
		StartedAt: now,
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

	s.bus = bus
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

	// Bus attached after the reactivation write below, for the same reason as
	// newSessionWorkspaceAt: this runs before startJournal.
	s := &SessionWorkspace{
		ID:      sessionID,
		RootDir: sessionDir,
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

	s.bus = bus
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

// BlobsDir returns the path to the per-session blobs subdirectory used by
// pkg/engine/blobs. Lazily created by the blob store on first Put — no
// directory is forced at session boot.
func (s *SessionWorkspace) BlobsDir() string {
	return filepath.Join(s.RootDir, "blobs")
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
// It emits session.file.created or session.file.updated events.
//
// It reuses WriteFile's two event types rather than introducing a third
// "appended" type. Every existing subscriber — nexus.io.tui, nexus.io.browser,
// nexus.io.wails, nexus.tool.fileio — treats these as "this path changed,
// re-read it", which is exactly true of an append; a new type would have left
// all of them silently blind to appends until each was updated, which is the
// same invisibility this emission exists to remove. An append-aware delta
// (how many bytes landed, and at what offset) is additional information about
// the same change and belongs on the payload, not in a separate event type.
//
// Until that emission existed the highest-churn writers in a session were
// invisible to the bus: conversation history (context/conversation.jsonl),
// turn timing, compaction output, shell history and the HITL cache all append
// here. conversation.jsonl was the sharpest case — WriteFile covered its
// rewrites and nothing covered its appends, so it looked correct in a smoke
// test and dropped data in a real run.
func (s *SessionWorkspace) AppendFile(subpath string, data []byte) error {
	fullPath := filepath.Join(s.RootDir, subpath)

	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating directory for %s: %w", subpath, err)
	}

	// Sampled before the open: O_CREATE makes the file exist as a side effect,
	// so checking afterwards would report every first append as an update and
	// no subscriber would ever see the file appear.
	existed := s.FileExists(subpath)

	f, err := os.OpenFile(fullPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening file %s for append: %w", subpath, err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("appending to file %s: %w", subpath, err)
	}

	if s.bus != nil {
		// size is the size of the file after the write, not the length of the
		// appended chunk, so the field means the same thing here as it does in
		// WriteFile and a subscriber never has to know which helper produced
		// the event. Stat goes through the open descriptor rather than the
		// path so a concurrent append cannot make the two disagree. A failed
		// Stat reports 0 rather than dropping the event: a subscriber that
		// re-reads the path is still correct with a wrong size, and is
		// unrecoverably wrong with no event at all.
		var size int
		if fi, statErr := f.Stat(); statErr == nil {
			size = int(fi.Size())
		}
		eventType := "session.file.created"
		if existed {
			eventType = "session.file.updated"
		}
		_ = s.bus.Emit(eventType, map[string]any{
			"session_id": s.ID,
			"path":       subpath,
			"size":       size,
		})
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
// It emits session.file.created or session.file.updated for
// metadata/session.json.
//
// The write is routed through WriteFile rather than calling os.WriteFile and
// emitting alongside it. metadata/session.json is rewritten on every
// llm.response (token and cost totals) and every agent.turn.end (turn count),
// so it is one of the most frequently changed files in a session and was
// previously changing without a single event. Delegating means there is one
// implementation of "write a session file and announce it" — a second copy
// here would be free to drift in event type, payload shape or permissions,
// and the two would then disagree about the most-written file in the tree.
func (s *SessionWorkspace) SaveMeta(meta *SessionMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling session metadata: %w", err)
	}

	// Slash-separated literal, not filepath.Join: subpath is both a path
	// fragment and the session-relative "path" carried on the emitted event,
	// and every other caller of WriteFile spells it with forward slashes.
	// filepath.Join under the session root handles the separator on the way to
	// disk; joining here would put a backslash on the wire on Windows.
	if err := s.WriteFile("metadata/session.json", data); err != nil {
		return fmt.Errorf("writing session metadata: %w", err)
	}
	return nil
}
