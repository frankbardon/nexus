// Package desktop provides the reusable shell framework for Nexus
// desktop applications. It manages Wails lifecycle, per-agent engine
// creation, scoped event bridging, and shell services (file dialogs,
// notifications, OS integration).
//
// Usage from a cmd/<app>/main.go:
//
//	desktop.Run(&desktop.Shell{
//	    Title:  "My App",
//	    Width:  900,
//	    Height: 720,
//	    Assets: assets,
//	    Agents: []desktop.Agent{{
//	        ID:         "my-agent",
//	        Name:       "My Agent",
//	        ConfigYAML: configYAML,
//	        Factories:  map[string]func() engine.Plugin{ ... },
//	    }},
//	})
package desktop

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	wailsio "github.com/frankbardon/nexus/plugins/io/wails"

	wailsapp "github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed frontend/dist
var defaultAssets embed.FS

// Agent represents a single Nexus configuration hosted by the shell.
// Domain logic flows through the bus via the agent's plugins — the
// Agent struct is pure registration data.
type Agent struct {
	ID          string                            // unique key: "staffing-match", "hello-world"
	Name        string                            // display: "Staffing Match"
	Description string                            // short blurb for the agent selector
	Icon        string                            // Font Awesome class (e.g. "fa-solid fa-handshake")
	ConfigYAML  []byte                            // embedded Nexus config for this agent's engine
	Factories   map[string]func() engine.Plugin   // custom plugin factories
	Settings    []SettingsField                   // user-configurable fields rendered in settings UI
}

// AgentStatus represents the lifecycle state of an agent's engine.
type AgentStatus string

const (
	AgentStatusIdle    AgentStatus = "idle"
	AgentStatusBooting AgentStatus = "booting"
	AgentStatusRunning AgentStatus = "running"
	AgentStatusError   AgentStatus = "error"

	longtermPluginID = "nexus.memory.longterm"
)

// AgentInfo is the serializable projection of Agent for the frontend.
type AgentInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	Status      string `json:"status"`
}

// Shell is the top-level desktop shell orchestrator. It manages
// per-agent engine lifecycles and provides Wails-bound shell services.
type Shell struct {
	Title   string
	Width   int
	Height  int
	Agents  []Agent
	Assets  embed.FS // custom frontend assets; zero value uses the default base template

	// Internal state set during OnStartup.
	ctx        context.Context
	mu         sync.Mutex
	agents     map[string]*agentState
	store      *fileStore
	sessionIdx *sessionIndex
	watcher    *fileWatcher
}

// FileInfo describes a single file entry returned by ListFiles.
type FileInfo struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	IsDir    bool      `json:"is_dir"`
}

// agentState tracks a running agent's engine and wails plugin reference.
type agentState struct {
	eng       *engine.Engine
	wailsP    *wailsio.Plugin
	status    AgentStatus
	sessionID string   // current engine session ID
	busUnsubs []func() // shell-installed bus subscriptions torn down on stop
}

// Run is the entry point for a desktop shell application. It
// configures and starts the Wails app with the given shell. This
// function blocks until the app exits.
func Run(shell *Shell) error {
	shell.agents = make(map[string]*agentState)

	assets := shell.Assets
	if !isValidFS(assets) {
		assets = defaultAssets
	}

	width := shell.Width
	if width == 0 {
		width = 900
	}
	height := shell.Height
	if height == 0 {
		height = 720
	}

	return wailsapp.Run(&options.App{
		Title:  shell.Title,
		Width:  width,
		Height: height,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		OnStartup:  shell.onStartup,
		OnShutdown: shell.onShutdown,
		Bind: []interface{}{
			shell,
		},
	})
}

func (s *Shell) onStartup(ctx context.Context) {
	s.ctx = ctx

	home, err := os.UserHomeDir()
	if err != nil {
		log.Printf("warning: cannot determine home dir, settings/sessions disabled: %v", err)
	} else {
		desktopDir := filepath.Join(home, ".nexus", "desktop")

		// Initialize settings store.
		store, err := newFileStore(desktopDir)
		if err != nil {
			log.Printf("warning: cannot load settings: %v", err)
		} else {
			s.store = store
		}

		// Initialize session index.
		idx, err := newSessionIndex(desktopDir)
		if err != nil {
			log.Printf("warning: cannot load session index: %v", err)
		} else {
			s.sessionIdx = idx
			sessionsRoot := filepath.Join(home, ".nexus", "sessions")
			s.runSessionMaintenance(sessionsRoot)
		}
	}

	// Initialize filesystem watcher for the file browser panel.
	fw, err := newFileWatcher(func(dir string) {
		// Notify the frontend that files changed in the watched directory.
		// Find which agent owns this directory so we can scope the event.
		s.mu.Lock()
		var ownerID string
		for id := range s.agents {
			if s.resolveInputDir(id) == dir {
				ownerID = id
				break
			}
		}
		s.mu.Unlock()
		if ownerID != "" && s.ctx != nil {
			wailsruntime.EventsEmit(s.ctx, ownerID+":files.changed")
		}
	})
	if err != nil {
		log.Printf("warning: cannot create file watcher: %v", err)
	} else {
		s.watcher = fw
	}

	// Initialize agent state entries so ListAgents can report status
	// before any agent is started. Engine boot is on-demand via
	// EnsureAgentRunning, triggered by the frontend nav.
	for _, a := range s.Agents {
		s.agents[a.ID] = &agentState{status: AgentStatusIdle}
	}
}

func (s *Shell) onShutdown(_ context.Context) {
	if s.watcher != nil {
		s.watcher.Close()
	}

	s.mu.Lock()
	// Snapshot running engines for shutdown outside the lock.
	type entry struct {
		id  string
		eng *engine.Engine
	}
	var running []entry
	for id, state := range s.agents {
		if state.eng != nil {
			running = append(running, entry{id, state.eng})
			state.eng = nil
			state.status = AgentStatusIdle
		}
	}
	s.mu.Unlock()

	for _, e := range running {
		if err := e.eng.Stop(context.Background()); err != nil {
			log.Printf("failed to stop agent %q: %v", e.id, err)
		}
	}
}

// ListAgents returns info about all registered agents, including
// their current lifecycle status.
func (s *Shell) ListAgents() []AgentInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	infos := make([]AgentInfo, len(s.Agents))
	for i, a := range s.Agents {
		status := AgentStatusIdle
		if state, ok := s.agents[a.ID]; ok {
			status = state.status
		}
		infos[i] = AgentInfo{
			ID:          a.ID,
			Name:        a.Name,
			Description: a.Description,
			Icon:        a.Icon,
			Status:      string(status),
		}
	}
	return infos
}

// EnsureAgentRunning creates and boots the engine for the given agent
// if it is not already running.
func (s *Shell) EnsureAgentRunning(agentID string) error {
	s.mu.Lock()
	state, exists := s.agents[agentID]
	if exists && state.status == AgentStatusRunning {
		s.mu.Unlock()
		return nil
	}
	if exists && state.status == AgentStatusBooting {
		s.mu.Unlock()
		return nil
	}
	// Mark as booting so the frontend can show a spinner.
	if exists {
		state.status = AgentStatusBooting
	}
	s.mu.Unlock()

	return s.bootAgent(agentID, "")
}

// NewSession stops the current engine for the agent (if running) and
// boots a fresh one with a new session.
func (s *Shell) NewSession(agentID string) error {
	_ = s.StopAgent(agentID)

	s.mu.Lock()
	if st, ok := s.agents[agentID]; ok {
		st.status = AgentStatusBooting
	}
	s.mu.Unlock()

	return s.bootAgent(agentID, "")
}

// RecallSession stops the current engine and boots a new one that
// resumes the given session ID. The engine replays conversation
// history via io.history.replay.
func (s *Shell) RecallSession(agentID, sessionID string) error {
	_ = s.StopAgent(agentID)

	s.mu.Lock()
	if st, ok := s.agents[agentID]; ok {
		st.status = AgentStatusBooting
	}
	s.mu.Unlock()

	return s.bootAgent(agentID, sessionID)
}

// ListSessions returns session metadata for the given agent, sorted
// most-recent first.
func (s *Shell) ListSessions(agentID string) []SessionMeta {
	if s.sessionIdx == nil {
		return nil
	}
	return s.sessionIdx.List(agentID)
}

// DeleteSession removes a session from the index and deletes its
// engine session directory from disk.
func (s *Shell) DeleteSession(agentID, sessionID string) error {
	if s.sessionIdx == nil {
		return fmt.Errorf("session index not initialized")
	}

	// Don't delete the currently running session.
	s.mu.Lock()
	if state, ok := s.agents[agentID]; ok && state.sessionID == sessionID {
		s.mu.Unlock()
		return fmt.Errorf("cannot delete the active session")
	}
	s.mu.Unlock()

	if err := s.sessionIdx.Delete(sessionID); err != nil {
		return err
	}

	// Aggressively remove the engine session directory.
	home, err := os.UserHomeDir()
	if err == nil {
		dir := filepath.Join(home, ".nexus", "sessions", sessionID)
		_ = os.RemoveAll(dir)
	}

	s.emitSessionsUpdated(agentID)
	return nil
}

// bootAgent is the shared engine creation and boot sequence used by
// both EnsureAgentRunning (fresh session) and RecallSession.
func (s *Shell) bootAgent(agentID, recallSessionID string) error {
	agent := s.findAgent(agentID)
	if agent == nil {
		return fmt.Errorf("agent %q not registered", agentID)
	}

	// Resolve ${var} placeholders in config YAML from settings store.
	configBytes := agent.ConfigYAML
	if s.store != nil && len(agent.Settings) > 0 {
		resolved, missing, err := resolveConfig(configBytes, agentID, agent.Settings, s.store)
		if err != nil {
			s.mu.Lock()
			if st, ok := s.agents[agentID]; ok {
				st.status = AgentStatusError
			}
			s.mu.Unlock()
			return fmt.Errorf("agent %q missing required settings: %v (open Settings to configure)", missing, err)
		}
		configBytes = resolved
	}

	eng, err := engine.NewFromBytes(configBytes)
	if err != nil {
		s.mu.Lock()
		if st, ok := s.agents[agentID]; ok {
			st.status = AgentStatusError
		}
		s.mu.Unlock()
		return fmt.Errorf("creating engine for agent %q: %w", agentID, err)
	}

	if recallSessionID != "" {
		eng.RecallSessionID = recallSessionID
	}

	// Inject agent_id into longterm memory plugin config so it can
	// resolve agent-scoped storage paths.
	if cfg := eng.Config.Plugins.Configs[longtermPluginID]; cfg != nil {
		cfg["agent_id"] = agentID
	}

	// Create the nexus.io.wails plugin instance so we can inject the
	// scoped runtime before boot.
	var wailsP *wailsio.Plugin
	for pluginID, factory := range agent.Factories {
		if pluginID == "nexus.io.wails" {
			p := factory()
			wailsP = p.(*wailsio.Plugin)
			eng.Registry.Register(pluginID, func() engine.Plugin { return wailsP })
		} else {
			f := factory // capture
			eng.Registry.Register(pluginID, f)
		}
	}

	if wailsP != nil {
		rt := newScopedRuntime(s.ctx, agentID, s.store)
		wailsP.Hub().SetRuntime(rt)
	}

	if err := eng.Boot(s.ctx); err != nil {
		s.mu.Lock()
		if st, ok := s.agents[agentID]; ok {
			st.status = AgentStatusError
		}
		s.mu.Unlock()
		return fmt.Errorf("booting agent %q: %w", agentID, err)
	}

	// Capture session ID from the engine.
	sessionID := ""
	if eng.Session != nil {
		sessionID = eng.Session.ID
	}

	// Install bus subscriptions for session metadata and UI state events.
	var busUnsubs []func()
	if sessionID != "" {
		busUnsubs = s.installSessionSubscriptions(eng, agentID, sessionID)
	}

	s.mu.Lock()
	s.agents[agentID] = &agentState{
		eng:       eng,
		wailsP:    wailsP,
		status:    AgentStatusRunning,
		sessionID: sessionID,
		busUnsubs: busUnsubs,
	}
	s.mu.Unlock()

	// Create the session index entry.
	if s.sessionIdx != nil && sessionID != "" {
		now := time.Now()
		title := "Untitled"
		status := "running"

		if recallSessionID != "" {
			// Recalling — mark as running again.
			_ = s.sessionIdx.Update(sessionID, func(m *SessionMeta) {
				m.Status = "running"
				m.UpdatedAt = now
			})
		} else {
			_ = s.sessionIdx.Add(SessionMeta{
				ID:        sessionID,
				AgentID:   agentID,
				Title:     title,
				Status:    status,
				CreatedAt: now,
				UpdatedAt: now,
			})
		}
		s.emitSessionsUpdated(agentID)
	}

	// On recall, emit saved UI state so the frontend can rehydrate.
	if recallSessionID != "" && eng.Session != nil {
		if data, err := eng.Session.ReadFile("ui-state.json"); err == nil {
			var state any
			if json.Unmarshal(data, &state) == nil {
				_ = eng.Bus.Emit("ui.state.restore", state)
			}
		}
	}

	return nil
}

// StopAgent stops the engine for the given agent and resets its
// status to idle so it can be restarted.
func (s *Shell) StopAgent(agentID string) error {
	s.mu.Lock()
	state, ok := s.agents[agentID]
	if !ok || state.eng == nil {
		s.mu.Unlock()
		return nil
	}
	eng := state.eng
	sessionID := state.sessionID

	// Tear down shell-installed bus subscriptions before engine stop.
	for _, unsub := range state.busUnsubs {
		unsub()
	}
	state.busUnsubs = nil
	state.eng = nil
	state.wailsP = nil
	state.sessionID = ""
	state.status = AgentStatusIdle
	s.mu.Unlock()

	err := eng.Stop(context.Background())

	// Mark the session as completed in the index.
	if s.sessionIdx != nil && sessionID != "" {
		_ = s.sessionIdx.Update(sessionID, func(m *SessionMeta) {
			if m.Status == "running" {
				m.Status = "completed"
				m.UpdatedAt = time.Now()
			}
		})
		s.emitSessionsUpdated(agentID)
	}

	return err
}

// PickFile presents a native file open dialog rooted in the agent's
// configured input_dir (or shared_data_dir as fallback).
func (s *Shell) PickFile(agentID, title, filter string) (string, error) {
	var filters []wailsruntime.FileFilter
	if filter != "" {
		filters = []wailsruntime.FileFilter{
			{DisplayName: "Files", Pattern: filter},
		}
	}
	defaultDir := s.resolveInputDir(agentID)
	return wailsruntime.OpenFileDialog(s.ctx, wailsruntime.OpenDialogOptions{
		Title:            title,
		DefaultDirectory: defaultDir,
		Filters:          filters,
	})
}

// PickFolder presents a native folder selection dialog.
func (s *Shell) PickFolder(agentID, title string) (string, error) {
	return wailsruntime.OpenDirectoryDialog(s.ctx, wailsruntime.OpenDialogOptions{
		Title: title,
	})
}

// OpenExternal opens a file or URL using the system default handler.
// For http/https URLs it uses the Wails runtime; for file paths it
// shells out to the OS open command.
func (s *Shell) OpenExternal(target string) error {
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		wailsruntime.BrowserOpenURL(s.ctx, target)
		return nil
	}
	// Strip file:// prefix if present.
	path := strings.TrimPrefix(target, "file://")
	return openWithOS(path)
}

// RevealInFinder opens the system file manager with the given path
// selected (macOS) or its parent directory open (Linux/Windows).
func (s *Shell) RevealInFinder(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", "-R", path).Start()
	case "windows":
		return exec.Command("explorer", "/select,", path).Start()
	default:
		// Linux: open the parent directory.
		return exec.Command("xdg-open", filepath.Dir(path)).Start()
	}
}

// openWithOS opens a file path using the OS default handler.
func openWithOS(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", path).Start()
	case "windows":
		return exec.Command("cmd", "/c", "start", "", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

// Notify sends an OS notification.
func (s *Shell) Notify(title, body string) error {
	// Wails v2 doesn't have a built-in notification API. This is a
	// placeholder for phase 2 when we add OS notification support.
	log.Printf("notification: %s — %s", title, body)
	return nil
}

// ── File Portal services ────────────────────────────────────────

// ListFiles returns the contents of the agent's configured input_dir.
// filter is a glob pattern (e.g. "*.pdf"); empty string returns all.
func (s *Shell) ListFiles(agentID, filter string) ([]FileInfo, error) {
	dir := s.resolveInputDir(agentID)
	if dir == "" {
		return nil, fmt.Errorf("no input directory configured for agent %q", agentID)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading directory: %w", err)
	}

	var files []FileInfo
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue // skip hidden files
		}
		if filter != "" {
			matched, _ := filepath.Match(filter, name)
			if !matched {
				continue
			}
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, FileInfo{
			Name:     name,
			Path:     filepath.Join(dir, name),
			Size:     info.Size(),
			Modified: info.ModTime(),
			IsDir:    e.IsDir(),
		})
	}
	return files, nil
}

// OutputDir returns the agent's configured output_dir, creating it
// if it does not exist.
func (s *Shell) OutputDir(agentID string) (string, error) {
	dir := s.resolveOutputDir(agentID)
	if dir == "" {
		return "", fmt.Errorf("no output directory configured for agent %q", agentID)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating output directory: %w", err)
	}
	return dir, nil
}

// CopyFileToInputDir copies a file from sourcePath into the agent's
// input_dir. Used by the frontend for drag-and-drop file imports.
// Returns the destination path.
func (s *Shell) CopyFileToInputDir(agentID, sourcePath string) (string, error) {
	dir := s.resolveInputDir(agentID)
	if dir == "" {
		return "", fmt.Errorf("no input directory configured for agent %q", agentID)
	}

	name := filepath.Base(sourcePath)
	destPath := filepath.Join(dir, name)

	// Don't copy over itself.
	if sourcePath == destPath {
		return destPath, nil
	}

	src, err := os.Open(sourcePath)
	if err != nil {
		return "", fmt.Errorf("opening source file: %w", err)
	}
	defer src.Close()

	dst, err := os.Create(destPath)
	if err != nil {
		return "", fmt.Errorf("creating destination file: %w", err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return "", fmt.Errorf("copying file: %w", err)
	}
	return destPath, nil
}

// WriteFileToInputDir writes base64-encoded file data into the agent's
// input_dir. Used as a fallback for drag-and-drop when the webview
// does not expose the native file path.
func (s *Shell) WriteFileToInputDir(agentID, name, base64Data string) (string, error) {
	dir := s.resolveInputDir(agentID)
	if dir == "" {
		return "", fmt.Errorf("no input directory configured for agent %q", agentID)
	}

	data, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return "", fmt.Errorf("decoding file data: %w", err)
	}

	destPath := filepath.Join(dir, name)
	if err := os.WriteFile(destPath, data, 0o644); err != nil {
		return "", fmt.Errorf("writing file: %w", err)
	}
	return destPath, nil
}

// WatchInputDir starts the file watcher on the given agent's input
// directory. Called by the frontend when the file browser panel opens
// or when the active agent changes.
func (s *Shell) WatchInputDir(agentID string) {
	if s.watcher == nil {
		return
	}
	dir := s.resolveInputDir(agentID)
	s.watcher.Watch(dir)
}

// resolveInputDir returns the agent's input_dir setting, falling back
// to shell.shared_data_dir, then to the user's Documents folder.
func (s *Shell) resolveInputDir(agentID string) string {
	if s.store != nil {
		// Agent-scoped input_dir.
		if val, ok := s.store.Resolve(agentID, "input_dir", false); ok && val != "" {
			return val
		}
		// Shell-scoped shared_data_dir.
		if val, ok := s.store.Resolve("shell", "shared_data_dir", false); ok && val != "" {
			return val
		}
	}
	// Fallback to user's home/Documents.
	if home, err := os.UserHomeDir(); err == nil {
		docs := filepath.Join(home, "Documents")
		if info, err := os.Stat(docs); err == nil && info.IsDir() {
			return docs
		}
	}
	return ""
}

// resolveOutputDir returns the agent's output_dir setting.
func (s *Shell) resolveOutputDir(agentID string) string {
	if s.store == nil {
		return ""
	}
	val, ok := s.store.Resolve(agentID, "output_dir", false)
	if ok && val != "" {
		return val
	}
	return ""
}

// GetSettingsSchema returns the full settings schema for the frontend
// to render dynamically. Includes shell-level and per-agent fields.
func (s *Shell) GetSettingsSchema() SettingsSchema {
	schema := SettingsSchema{
		Shell:  make([]SettingsFieldInfo, len(shellSettings)),
		Agents: make(map[string][]SettingsFieldInfo),
	}
	for i, f := range shellSettings {
		schema.Shell[i] = f.toInfo()
	}
	for _, a := range s.Agents {
		infos := make([]SettingsFieldInfo, len(a.Settings))
		for i, f := range a.Settings {
			infos[i] = f.toInfo()
		}
		schema.Agents[a.ID] = infos
	}
	return schema
}

// GetSettings returns all current setting values. Secret fields show
// "__keychain__" instead of the actual value.
func (s *Shell) GetSettings() map[string]map[string]any {
	if s.store == nil {
		return map[string]map[string]any{"shell": {}}
	}
	return s.store.AllValues()
}

// UpdateSetting writes a plaintext setting value. Scope is an agent
// ID or "shell".
func (s *Shell) UpdateSetting(scope, key string, value any) error {
	if s.store == nil {
		return fmt.Errorf("settings store not initialized")
	}
	return s.store.Set(scope, key, value)
}

// UpdateSecret writes a secret value to the OS keychain.
func (s *Shell) UpdateSecret(scope, key, value string) error {
	if s.store == nil {
		return fmt.Errorf("settings store not initialized")
	}
	return s.store.SetSecret(scope, key, value)
}

// DeleteSetting removes a plaintext setting or secret.
func (s *Shell) DeleteSetting(scope, key string, secret bool) error {
	if s.store == nil {
		return fmt.Errorf("settings store not initialized")
	}
	if secret {
		return s.store.DeleteSecret(scope, key)
	}
	return s.store.Delete(scope, key)
}

// HasMissingRequired checks whether any agent has required settings
// that are not yet configured. Returns a map of agentID → missing keys.
func (s *Shell) HasMissingRequired() map[string][]string {
	if s.store == nil {
		return nil
	}
	result := make(map[string][]string)
	for _, a := range s.Agents {
		for _, f := range a.Settings {
			if !f.Required {
				continue
			}
			scope := a.ID
			key := f.Key
			if len(key) > 6 && key[:6] == "shell." {
				scope = "shell"
				key = key[6:]
			}
			val, found := s.store.Resolve(scope, key, f.Secret)
			if !found || val == "" {
				// Check default.
				if f.Default == nil || fmt.Sprintf("%v", f.Default) == "" {
					result[a.ID] = append(result[a.ID], f.Key)
				}
			}
		}
	}
	return result
}

// installSessionSubscriptions subscribes to session metadata events on
// the engine's bus. Returns unsub functions for cleanup on stop.
func (s *Shell) installSessionSubscriptions(eng *engine.Engine, agentID, sessionID string) []func() {
	var unsubs []func()

	if s.sessionIdx != nil {
		// session.meta.title — agent provides a human-readable title.
		unsubs = append(unsubs, eng.Bus.Subscribe("session.meta.title", func(event engine.Event[any]) {
			payload, ok := event.Payload.(map[string]any)
			if !ok {
				return
			}
			title, _ := payload["title"].(string)
			if title == "" {
				return
			}
			_ = s.sessionIdx.Update(sessionID, func(m *SessionMeta) {
				m.Title = title
				m.UpdatedAt = time.Now()
			})
			s.emitSessionsUpdated(agentID)
		}))

		// session.meta.preview — agent provides summary data for the list.
		unsubs = append(unsubs, eng.Bus.Subscribe("session.meta.preview", func(event engine.Event[any]) {
			_ = s.sessionIdx.Update(sessionID, func(m *SessionMeta) {
				m.Preview = event.Payload
				m.UpdatedAt = time.Now()
			})
			s.emitSessionsUpdated(agentID)
		}))

		// session.meta.status — agent explicitly sets status.
		unsubs = append(unsubs, eng.Bus.Subscribe("session.meta.status", func(event engine.Event[any]) {
			payload, ok := event.Payload.(map[string]any)
			if !ok {
				return
			}
			status, _ := payload["status"].(string)
			if status == "" {
				return
			}
			_ = s.sessionIdx.Update(sessionID, func(m *SessionMeta) {
				m.Status = status
				m.UpdatedAt = time.Now()
			})
			s.emitSessionsUpdated(agentID)
		}))

		// io.session.end — engine or plugin signals session end.
		unsubs = append(unsubs, eng.Bus.Subscribe("io.session.end", func(event engine.Event[any]) {
			_ = s.sessionIdx.Update(sessionID, func(m *SessionMeta) {
				m.Status = "completed"
				m.UpdatedAt = time.Now()
			})
			s.emitSessionsUpdated(agentID)
		}))
	}

	// ui.state.save — frontend persists its UI state as an opaque JSON blob.
	unsubs = append(unsubs, eng.Bus.Subscribe("ui.state.save", func(event engine.Event[any]) {
		if eng.Session == nil {
			return
		}
		data, err := json.Marshal(event.Payload)
		if err != nil {
			return
		}
		_ = eng.Session.WriteFile("ui-state.json", data)
	}))

	// io.file.output_dir.request — plugin asks where to write outputs.
	unsubs = append(unsubs, eng.Bus.Subscribe("io.file.output_dir.request", func(event engine.Event[any]) {
		var req events.FileOutputDirRequest
		switch p := event.Payload.(type) {
		case events.FileOutputDirRequest:
			req = p
		case map[string]any:
			req.RequestID, _ = p["requestID"].(string)
			if req.RequestID == "" {
				req.RequestID, _ = p["RequestID"].(string)
			}
		default:
			return
		}

		resp := events.FileOutputDirResponse{RequestID: req.RequestID}
		dir, err := s.OutputDir(agentID)
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.Path = dir
		}
		_ = eng.Bus.Emit("io.file.output_dir.response", resp)
	}))

	return unsubs
}

// emitSessionsUpdated notifies the frontend that the session list for
// an agent has changed. The frontend listens on "{agentID}:sessions.updated".
func (s *Shell) emitSessionsUpdated(agentID string) {
	if s.ctx == nil {
		return
	}
	sessions := s.sessionIdx.List(agentID)
	data, err := json.Marshal(sessions)
	if err != nil {
		return
	}
	wailsruntime.EventsEmit(s.ctx, agentID+":sessions.updated", string(data))
}

// runSessionMaintenance performs cleanup and reconciliation of the
// session index on startup.
func (s *Shell) runSessionMaintenance(sessionsRoot string) {
	if s.sessionIdx == nil {
		return
	}

	// Determine retention days from shell settings.
	retentionDays := 30 // default
	if s.store != nil {
		if val, ok := s.store.Resolve("shell", "session_retention_days", false); ok && val != "" {
			if n, err := strconv.Atoi(val); err == nil {
				retentionDays = n
			}
		}
	}

	// Cleanup expired sessions.
	if removed, err := s.sessionIdx.Cleanup(retentionDays, sessionsRoot); err != nil {
		log.Printf("warning: session cleanup error: %v", err)
	} else if removed > 0 {
		log.Printf("session cleanup: removed %d expired sessions", removed)
	}

	// Reconcile orphaned/stale entries.
	if err := s.sessionIdx.Reconcile(sessionsRoot, s.Agents, retentionDays); err != nil {
		log.Printf("warning: session reconciliation error: %v", err)
	}
}

func (s *Shell) findAgent(id string) *Agent {
	for i := range s.Agents {
		if s.Agents[i].ID == id {
			return &s.Agents[i]
		}
	}
	return nil
}

// isValidFS checks whether an embed.FS has any content. A zero-value
// embed.FS will fail ReadDir at the root.
func isValidFS(fs embed.FS) bool {
	_, err := fs.ReadDir(".")
	return err == nil
}
