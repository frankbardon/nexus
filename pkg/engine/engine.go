package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"gopkg.in/yaml.v3"
)

// Engine is the top-level orchestrator that ties all engine components together.
type Engine struct {
	Config    *Config
	Bus       EventBus
	Registry  *PluginRegistry
	Lifecycle *LifecycleManager
	Context   *ContextManager
	Session   *SessionWorkspace
	Models    *ModelRegistry
	Prompts   *PromptRegistry
	Schemas   *SchemaRegistry
	System    *SystemInfo
	Logger    *slog.Logger
	// Logging is the LoggingHost passed to plugins via PluginContext. It is
	// the same FanoutHandler that backs Logger — exposed here so embedders
	// can register their own sinks without waiting for a plugin to do it.
	Logging LoggingHost

	// RecallSessionID, when set, causes Boot to resume an existing session
	// instead of creating a new one.
	RecallSessionID string

	// Run-scoped state installed by Boot and torn down by Stop. Embedders
	// should not touch these fields directly.
	runUnsubs  []func()
	runCancel  context.CancelFunc
	sessionEnd chan struct{}
}

// New creates a fully wired Engine from a config file path.
// If configPath is empty, default configuration is used.
func New(configPath string) (*Engine, error) {
	var cfg *Config
	var err error

	if configPath == "" {
		cfg = DefaultConfig()
	} else {
		cfg, err = LoadConfig(configPath)
		if err != nil {
			return nil, fmt.Errorf("loading config: %w", err)
		}
	}

	return newFromConfig(cfg), nil
}

// NewFromBytes creates a fully wired Engine from an in-memory YAML
// config. This is the embedder-facing constructor for binaries that
// //go:embed their config.yaml alongside the main package and do
// not want a filesystem dependency at boot.
//
// Prefer this over New("") + manual eng.Config mutation: the
// manual path cannot populate core.models (which is loaded via the
// YAML second-pass in LoadConfigFromBytes) and requires callers to
// hand-roll per-plugin config installation.
func NewFromBytes(configBytes []byte) (*Engine, error) {
	cfg, err := LoadConfigFromBytes(configBytes)
	if err != nil {
		return nil, fmt.Errorf("loading embedded config: %w", err)
	}
	return newFromConfig(cfg), nil
}

// newFromConfig is the shared Engine constructor used by both New
// and NewFromBytes. Factored out so the two entry points cannot
// drift on logger setup, bus creation, or lifecycle wiring.
func newFromConfig(cfg *Config) *Engine {
	level := parseLogLevel(cfg.Core.LogLevel)

	// Build the fanout first so every later component (lifecycle, schema
	// registry, context manager) writes through it from construction time.
	// Pre-boot records land in the ring until a sink registers.
	fanout := NewFanoutHandler(cfg.Core.Logging.BufferSize, level)
	if cfg.Core.Logging.BootstrapStderr {
		// Config.validate has already rejected the stderr+visual combo, so
		// it is safe to add the sink here. Callers who want stderr output
		// during bootstrap (CLI, headless dev) opt in via this flag.
		fanout.AddLogSink(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	}
	logger := slog.New(fanout)

	bus := NewEventBusWithRingSize(cfg.Core.Logging.BufferSize)
	registry := NewPluginRegistry()
	models := NewModelRegistry(cfg.Core.ModelsRaw)
	prompts := NewPromptRegistry()
	schemas := NewSchemaRegistry(logger)
	system := DetectSystem()
	lifecycle := NewLifecycleManager(registry, bus, cfg, logger, models, prompts, schemas, system)
	lifecycle.logging = fanout
	ctxMgr := NewContextManager(bus, logger)

	return &Engine{
		Config:    cfg,
		Bus:       bus,
		Registry:  registry,
		Lifecycle: lifecycle,
		Context:   ctxMgr,
		Models:    models,
		Prompts:   prompts,
		Schemas:   schemas,
		System:    system,
		Logger:    logger,
		Logging:   fanout,
	}
}

// Boot starts a session, initializes all active plugins, installs run-scoped
// bus subscriptions, and starts the tick heartbeat. It returns as soon as the
// engine is ready — it does not block.
//
// Boot is the embedder-facing entry point. Host processes (Wails, tests, other
// Go binaries) call Boot, then select on SessionEnded() alongside their own
// lifecycle signals, and finally call Stop. Boot never installs OS signal
// handlers; that is the host's job.
//
// The CLI convenience wrapper Run calls Boot + wait-for-signal-or-session-end
// + Stop in a single blocking call.
func (e *Engine) Boot(ctx context.Context) error {
	if e.RecallSessionID != "" {
		if err := e.ResumeSession(e.RecallSessionID); err != nil {
			return fmt.Errorf("session recall failed: %w", err)
		}
	} else {
		if err := e.StartSession(); err != nil {
			return fmt.Errorf("session start failed: %w", err)
		}
	}

	if err := e.Lifecycle.Boot(ctx); err != nil {
		return fmt.Errorf("boot failed: %w", err)
	}

	// Replay conversation history to the UI after boot when recalling a session.
	if e.RecallSessionID != "" {
		if err := e.replayHistory(); err != nil {
			e.Logger.Warn("failed to replay conversation history", "error", err)
		}
	}

	// Install schema registry bus subscriptions.
	e.runUnsubs = append(e.runUnsubs, e.Schemas.Install(e.Bus)...)

	// Track token usage and cost from LLM responses.
	e.runUnsubs = append(e.runUnsubs, e.Bus.Subscribe("llm.response", func(event Event[any]) {
		resp, ok := event.Payload.(events.LLMResponse)
		if !ok || e.Session == nil {
			return
		}
		meta, err := e.Session.SessionMetadata()
		if err != nil {
			return
		}
		meta.TokensUsed += resp.Usage.TotalTokens
		meta.PromptTokensUsed += resp.Usage.PromptTokens
		meta.CompletionTokensUsed += resp.Usage.CompletionTokens
		meta.CostUSD += resp.CostUSD
		_ = e.Session.SaveMeta(meta)
	}))

	// Track turn timing and count.
	turnStarts := make(map[string]time.Time)
	var timingMu sync.Mutex
	type turnTiming struct {
		TurnID    string  `json:"turn_id"`
		StartedAt string  `json:"started_at"`
		Duration  float64 `json:"duration_ms"`
	}

	e.runUnsubs = append(e.runUnsubs, e.Bus.Subscribe("agent.turn.start", func(event Event[any]) {
		info, ok := event.Payload.(events.TurnInfo)
		if !ok {
			return
		}
		timingMu.Lock()
		turnStarts[info.TurnID] = time.Now()
		timingMu.Unlock()
	}))

	e.runUnsubs = append(e.runUnsubs, e.Bus.Subscribe("agent.turn.end", func(event Event[any]) {
		if e.Session == nil {
			return
		}
		info, _ := event.Payload.(events.TurnInfo)

		meta, err := e.Session.SessionMetadata()
		if err != nil {
			return
		}
		meta.TurnCount++
		_ = e.Session.SaveMeta(meta)

		// Append turn timing.
		timingMu.Lock()
		start, ok := turnStarts[info.TurnID]
		if ok {
			delete(turnStarts, info.TurnID)
		}
		timingMu.Unlock()

		if ok {
			entry := turnTiming{
				TurnID:    info.TurnID,
				StartedAt: start.Format(time.RFC3339Nano),
				Duration:  float64(time.Since(start).Milliseconds()),
			}
			if data, err := json.Marshal(entry); err == nil {
				data = append(data, '\n')
				_ = e.Session.AppendFile("metadata/timing.jsonl", data)
			}
		}
	}))

	// Surface errors to the UI.
	e.runUnsubs = append(e.runUnsubs, e.Bus.Subscribe("core.error", func(event Event[any]) {
		errInfo, ok := event.Payload.(events.ErrorInfo)
		if !ok {
			return
		}
		_ = e.Bus.Emit("io.output", events.AgentOutput{
			Content: fmt.Sprintf("[%s] %s", errInfo.Source, errInfo.Err.Error()),
			Role:    "error",
		})
	}))

	// Listen for session end events so Run (or an embedder) can react.
	e.sessionEnd = make(chan struct{}, 1)
	e.runUnsubs = append(e.runUnsubs, e.Bus.Subscribe("io.session.end", func(_ Event[any]) {
		select {
		case e.sessionEnd <- struct{}{}:
		default:
		}
	}))

	// Start core.tick heartbeat. The tick goroutine lives on its own
	// context so Stop can cancel it independently of the caller's context.
	tickCtx, tickCancel := context.WithCancel(context.Background())
	e.runCancel = tickCancel
	tickInterval := e.Config.Core.TickInterval
	if tickInterval <= 0 {
		tickInterval = 5 * time.Second
	}
	ticker := time.NewTicker(tickInterval)
	tickSeq := 0
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-tickCtx.Done():
				return
			case t := <-ticker.C:
				tickSeq++
				_ = e.Bus.Emit("core.tick", events.TickInfo{
					Sequence: tickSeq,
					Time:     t,
				})
			}
		}
	}()

	return nil
}

// Stop tears down the run-scoped state installed by Boot: the tick heartbeat,
// run-scoped bus subscriptions, session metadata finalization, and plugin
// shutdown in reverse dependency order. Stop is safe to call from a host's
// shutdown hook (e.g. Wails OnShutdown).
func (e *Engine) Stop(ctx context.Context) error {
	if e.runCancel != nil {
		e.runCancel()
		e.runCancel = nil
	}

	for _, unsub := range e.runUnsubs {
		unsub()
	}
	e.runUnsubs = nil

	e.EndSession()

	if err := e.Lifecycle.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown failed: %w", err)
	}

	return nil
}

// SessionEnded returns a channel that is signalled when a plugin emits
// io.session.end. The channel is created by Boot; calling SessionEnded
// before Boot returns nil.
func (e *Engine) SessionEnded() <-chan struct{} {
	return e.sessionEnd
}

// Capabilities returns a snapshot of the capability → provider-IDs map
// resolved at boot. Safe to call after Boot; before Boot it returns nil.
func (e *Engine) Capabilities() map[string][]string {
	return e.Lifecycle.Capabilities()
}

// Run is the CLI convenience wrapper: Boot + wait-for-signal-or-session-end
// + Stop. It installs SIGINT/SIGTERM handlers and blocks until one of them
// fires, a plugin emits io.session.end, or the passed context is cancelled.
//
// Run is intended for the stock cmd/nexus binary. Embedders (Wails, tests,
// other Go hosts) must call Boot and Stop directly and must not call Run,
// because Run owns signals and blocking behavior that conflict with a host
// process owning the lifecycle.
func (e *Engine) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := e.Boot(ctx); err != nil {
		return err
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case sig := <-sigCh:
		e.Logger.Info("received signal", "signal", sig)
	case <-e.SessionEnded():
		e.Logger.Info("session ended")
	case <-ctx.Done():
		e.Logger.Info("context cancelled")
	}

	// Use a fresh background context for shutdown so plugin Shutdown calls
	// are not handed an already-cancelled context.
	return e.Stop(context.Background())
}

// ResumeSession loads an existing session workspace for recall.
// It restores the session workspace and emits a session recall event.
func (e *Engine) ResumeSession(sessionID string) error {
	root := expandHome(e.Config.Core.Sessions.Root)

	session, err := LoadSessionWorkspace(root, sessionID, e.Bus)
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}
	e.Session = session
	e.Lifecycle.session = session

	e.Logger.Info("session recalled", "session_id", session.ID, "root", session.RootDir)

	return e.Bus.Emit("io.session.start", map[string]any{
		"session_id": session.ID,
		"root_dir":   session.RootDir,
		"recalled":   true,
	})
}

// replayHistory reads persisted conversation messages and emits them as
// an io.history.replay event so the UI can display prior conversation.
func (e *Engine) replayHistory() error {
	if e.Session == nil || !e.Session.FileExists("context/conversation.jsonl") {
		return nil
	}

	data, err := e.Session.ReadFile("context/conversation.jsonl")
	if err != nil {
		return fmt.Errorf("reading conversation history: %w", err)
	}

	var messages []events.Message
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg events.Message
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			e.Logger.Warn("skipping malformed history entry", "error", err)
			continue
		}
		messages = append(messages, msg)
	}

	if len(messages) == 0 {
		return nil
	}

	e.Logger.Info("replaying conversation history", "messages", len(messages))
	return e.Bus.Emit("io.history.replay", events.HistoryReplay{
		Messages: messages,
	})
}

// EndSession finalizes the session metadata with ended_at and status.
func (e *Engine) EndSession() {
	if e.Session == nil {
		return
	}
	meta, err := e.Session.SessionMetadata()
	if err != nil {
		return
	}
	now := time.Now()
	meta.EndedAt = &now
	meta.Status = "completed"
	_ = e.Session.SaveMeta(meta)
}

// StartSession creates a new session workspace and emits the session start event.
func (e *Engine) StartSession() error {
	root := expandHome(e.Config.Core.Sessions.Root)

	session, err := NewSessionWorkspace(root, e.Bus)
	if err != nil {
		return fmt.Errorf("creating session workspace: %w", err)
	}
	e.Session = session
	e.Lifecycle.session = session

	// Write config snapshot to metadata.
	if cfgData, err := yaml.Marshal(e.Config); err == nil {
		_ = session.WriteFile("metadata/config-snapshot.yaml", cfgData)
	}

	// Write active plugins manifest to metadata.
	pluginsManifest := map[string]any{
		"active": e.Config.Plugins.Active,
	}
	if pluginsData, err := json.MarshalIndent(pluginsManifest, "", "  "); err == nil {
		_ = session.WriteFile("metadata/plugins.json", pluginsData)
	}

	// Populate session metadata with profile and plugin info.
	meta, err := session.SessionMetadata()
	if err == nil {
		meta.Plugins = e.Config.Plugins.Active
		_ = session.SaveMeta(meta)
	}

	e.Logger.Info("session started", "session_id", session.ID, "root", session.RootDir)

	return e.Bus.Emit("io.session.start", map[string]any{
		"session_id": session.ID,
		"root_dir":   session.RootDir,
	})
}

// expandHome replaces a leading ~ with the user's home directory.
func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// parseLogLevel converts a string log level to slog.Level.
func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
