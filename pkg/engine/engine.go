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

	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/engine/storage"
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
	// Journal is the durable per-session event log. Always non-nil after
	// Boot (or ResumeSession) succeeds; closed by Stop. The bus's wildcard
	// dispatch feeds it via a writer goroutine, so handler latency does not
	// block I/O.
	Journal *journal.Writer
	// Replay is the engine-wide replay coordination point. Always non-nil
	// after construction; idle until a replay coordinator activates it.
	// Plugins inspect Replay.Active() to short-circuit side effects.
	Replay *ReplayState
	// Storage opens scoped per-plugin SQLite-backed storage. Always
	// non-nil after construction; closed by Stop. Plugins access it via
	// PluginContext.Storage rather than reaching here directly.
	Storage *storage.Manager

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

	// The bus ring is sized for the boot-time pre-subscription gap only;
	// LoggingConfig.BufferSize sizes the slog fanout above and is not
	// shared with the bus ring anymore — durable event history lives in
	// the journal.
	bus := NewEventBus()
	if setter, ok := bus.(interface{ SetLogger(*slog.Logger) }); ok {
		setter.SetLogger(logger.With("subsystem", "bus"))
	}
	registry := NewPluginRegistry()
	models := NewModelRegistry(cfg.Core.ModelsRaw)
	prompts := NewPromptRegistry()
	schemas := NewSchemaRegistry(logger)
	system := DetectSystem()
	lifecycle := NewLifecycleManager(registry, bus, cfg, logger, models, prompts, schemas, system)
	lifecycle.logging = fanout
	ctxMgr := NewContextManager(bus, logger)
	replay := NewReplayState()
	lifecycle.replay = replay

	storageRoot := storageRoot(cfg)
	storageMgr := storage.NewManager(
		storageRoot,
		cfg.Core.AgentID,
		nil, // session resolver wired post-construction; see attachStorageSession
		storageOptions(cfg.Core.Storage),
	)
	lifecycle.storage = storageMgr

	eng := &Engine{
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
		Replay:    replay,
		Storage:   storageMgr,
	}
	// Resolve session-scoped storage paths against whatever Session is set
	// at the time of Open. Captured here rather than at construction so a
	// session created later in Boot is visible to plugins that call
	// Storage(ScopeSession) during Init.
	storageMgr.AttachSessionResolver(func() string {
		if eng.Session == nil {
			return ""
		}
		return eng.Session.RootDir
	})
	return eng
}

// storageRoot resolves the data root for App and Agent scope storage.
// Defaults to ~/.nexus when storage.root is unset; the same root used for
// session workspaces.
func storageRoot(cfg *Config) string {
	if cfg.Core.Storage.Root != "" {
		return ExpandPath(cfg.Core.Storage.Root)
	}
	return ExpandPath("~/.nexus")
}

// storageOptions converts the YAML config block into the storage package's
// per-handle SQLite options. Zero fields fall back to library defaults.
func storageOptions(c StorageConfig) *storage.SQLiteOptions {
	return &storage.SQLiteOptions{
		BusyTimeoutMs: c.BusyTimeoutMs,
		CacheSizeKB:   c.CacheSizeKB,
		PoolMaxIdle:   c.PoolMaxIdle,
		PoolMaxOpen:   c.PoolMaxOpen,
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
	// Two-phase session startup: create the workspace directory first so
	// the journal writer has a place to land its files, subscribe the
	// writer, then publish the session-start metadata + event so they are
	// the first entries the journal records (seq=1+).
	if e.RecallSessionID != "" {
		if err := e.prepareResumeSession(e.RecallSessionID); err != nil {
			return fmt.Errorf("session recall failed: %w", err)
		}
	} else {
		if err := e.prepareSession(); err != nil {
			return fmt.Errorf("session start failed: %w", err)
		}
	}

	// Acquire the session lock immediately after the workspace exists
	// and before any plugin Init runs. Refuses to boot when an existing
	// non-stale lock is found; overwrites a stale one with a warning.
	if err := e.acquireSessionLock(); err != nil {
		return err
	}
	// Release the lock if Boot fails after acquisition — the caller
	// will not call Stop on a failed boot. Cleared at the end of a
	// successful Boot so Stop owns the teardown.
	bootSucceeded := false
	defer func() {
		if !bootSucceeded && e.Session != nil {
			_ = RemoveSessionLock(e.Session.RootDir)
		}
	}()

	if err := e.startJournal(); err != nil {
		return fmt.Errorf("starting journal: %w", err)
	}

	if e.RecallSessionID != "" {
		if err := e.announceResume(); err != nil {
			return fmt.Errorf("session recall failed: %w", err)
		}
	} else {
		if err := e.StartSession(); err != nil {
			return fmt.Errorf("session start failed: %w", err)
		}
	}

	// Sweep aged journals from prior sessions in the background — a slow
	// filesystem must not delay the active session's boot.
	go func() {
		root := ExpandPath(e.Config.Core.Sessions.Root)
		_ = journal.Sweep(root, e.Config.Journal.RetainDays)
	}()

	if err := e.Lifecycle.Boot(ctx); err != nil {
		return fmt.Errorf("boot failed: %w", err)
	}

	// Replay conversation history to the UI after boot when recalling a session.
	if e.RecallSessionID != "" {
		if err := e.replayHistory(); err != nil {
			e.Logger.Warn("failed to replay conversation history", "error", err)
		}
		// Crash recovery: if the recalled session's journal ends mid-turn,
		// re-fire the io.input that started the unfinished turn so the
		// agent restarts and completes it. The agent's memory has already
		// been restored from conversation.jsonl; re-emitting the input is
		// what kicks the live ReAct loop back into motion.
		if err := e.crashResumeIfPartial(); err != nil {
			e.Logger.Warn("crash resume failed", "error", err)
		}
	}

	// Install schema registry bus subscriptions.
	e.runUnsubs = append(e.runUnsubs, e.Schemas.Install(e.Bus)...)

	// Seed baseline cost-attribution tags onto every llm.request before any
	// router/gate runs. Tags must be attached at request creation per the
	// DigitalApplied 2026 production guide; the engine owns the session
	// dimensions (tenant/project/user/session_id), plugins layer their own
	// (source_plugin, task_kind, parent_call_id) downstream.
	e.runUnsubs = append(e.runUnsubs, e.Bus.Subscribe("before:llm.request", func(event Event[any]) {
		vp, ok := event.Payload.(*VetoablePayload)
		if !ok || e.Session == nil {
			return
		}
		req, ok := vp.Original.(*events.LLMRequest)
		if !ok {
			return
		}
		meta, err := e.Session.SessionMetadata()
		if err != nil {
			return
		}
		if req.Tags == nil {
			req.Tags = make(map[string]string)
		}
		setTagIfAbsent(req.Tags, "session_id", e.Session.ID)
		for _, k := range []string{"tenant", "project", "user"} {
			if v, ok := meta.Labels[k]; ok && v != "" {
				setTagIfAbsent(req.Tags, k, v)
			}
		}
	}, WithPriority(100), WithSource("nexus.engine.tag_seeder")))

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

	bootSucceeded = true
	return nil
}

// acquireSessionLock writes session.lock under the active session
// directory. If a lock is already present and its PID is alive, the
// boot is refused with a SessionLockedError. A stale lock (PID gone)
// is overwritten with a warning so a crashed prior run does not block
// recovery.
func (e *Engine) acquireSessionLock() error {
	if e.Session == nil {
		return fmt.Errorf("acquire lock: no session workspace")
	}
	dir := e.Session.RootDir
	if existing, err := ReadSessionLock(dir); err == nil {
		if !IsLockStale(existing) {
			return &SessionLockedError{Dir: dir, Lock: existing}
		}
		e.Logger.Warn("overwriting stale session lock",
			"session_id", e.Session.ID,
			"stale_pid", existing.PID,
			"started_at", existing.StartedAt,
		)
	}
	return WriteSessionLock(dir, os.Getpid())
}

// crashResumeIfPartial inspects the current session's journal for an
// unfinished turn and, if present, re-emits the io.input that started it
// so the live agent restarts the turn from scratch. Idempotent — a
// well-formed journal (no partial turn) is a no-op.
//
// Phase 3 minimum: restart the partial turn rather than mid-step resume.
// The agent's memory has already been restored via replayHistory; the
// only missing piece is the in-flight bus state, which is reconstructed
// by re-firing the input. Mid-step resume (re-emit the in-flight
// tool.invoke after replay-stash short-circuits the completed prefix) is
// a future PR.
//
// Caveat: re-firing the input mints fresh seqs that append to the same
// journal alongside the orphaned partial-turn events. A subsequent
// --replay against this session will see both the orphaned io.input and
// the re-fired one. Document but do not fix in this pass.
func (e *Engine) crashResumeIfPartial() error {
	if e.Session == nil {
		return nil
	}
	journalDir := filepath.Join(e.Session.RootDir, "journal")
	coord, err := journal.NewCoordinator(journalDir, e.Bus, e.Replay, journal.CoordinatorOptions{
		Logger:           e.Logger.With("subsystem", "crash-resume"),
		PayloadConverter: replayPayloadConverter,
	})
	if err != nil {
		// Missing journal is fine — older sessions predate Phase 1.
		return nil
	}
	if !coord.IsPartialTurn() {
		return nil
	}
	partial, ok := coord.PartialInput()
	if !ok {
		e.Logger.Info("crash resume: partial turn detected but no io.input above the boundary",
			"last_turn_end_seq", func() uint64 {
				s, _ := coord.LastTurnBoundary()
				return s
			}())
		return nil
	}
	payload, _ := replayPayloadConverter("io.input", partial.Payload)
	e.Logger.Info("crash resume: re-emitting partial io.input",
		"original_seq", partial.Seq,
		"last_turn_end_seq", func() uint64 {
			s, _ := coord.LastTurnBoundary()
			return s
		}())
	return e.Bus.Emit("io.input", payload)
}

// replayPayloadConverter rehydrates a journal-deserialized payload (a
// map[string]any after JSON round-trip) into the typed struct that live
// subscribers expect. Centralized here because the journal package cannot
// import the events package, and per-call type switches in the coordinator
// would scatter event-type knowledge across both packages.
func replayPayloadConverter(eventType string, payload any) (any, error) {
	switch eventType {
	case "io.input":
		return journal.PayloadAs[events.UserInput](payload)
	case "llm.response":
		return journal.PayloadAs[events.LLMResponse](payload)
	case "tool.result":
		return journal.PayloadAs[events.ToolResult](payload)
	case "hitl.responded":
		return journal.PayloadAs[events.HITLResponse](payload)
	default:
		return payload, nil
	}
}

// ReplaySession runs deterministic replay against a previously-journaled
// session. The engine must already be booted; the caller is responsible
// for Stop afterward.
//
// Replay opens the source session's journal at <sessions.root>/<id>/journal,
// seeds the engine's ReplayState with journaled responses, and re-emits
// io.input events in seq order. Side-effecting plugins (LLM providers,
// tools) detect engine.Replay.Active() and pop stashed responses instead
// of calling out. The current session (the one this engine booted) writes
// its own fresh journal — the source is read-only.
//
// For Phase 2, replay produces functional equivalence (same final
// assistant outputs) rather than byte-identical event re-emission. Phase 3
// will extend this to crash-resume, where partial-turn detection picks up
// the live mode after replay completes.
func (e *Engine) ReplaySession(ctx context.Context, sourceSessionID string) error {
	if e.Session == nil {
		return fmt.Errorf("engine not booted")
	}
	if e.Replay == nil {
		return fmt.Errorf("replay state missing")
	}

	root := ExpandPath(e.Config.Core.Sessions.Root)
	sourceJournal := filepath.Join(root, sourceSessionID, "journal")

	coord, err := journal.NewCoordinator(sourceJournal, e.Bus, e.Replay, journal.CoordinatorOptions{
		TurnTimeout:      30 * time.Second,
		Logger:           e.Logger.With("subsystem", "replay"),
		PayloadConverter: replayPayloadConverter,
	})
	if err != nil {
		return fmt.Errorf("coordinator: %w", err)
	}

	// Wire turn-end synchronization so the coordinator waits for the live
	// agent to finish each turn before re-emitting the next io.input.
	coord.AttachTurnSync(func(handler func(seq uint64)) func() {
		return e.Bus.Subscribe("agent.turn.end", func(_ Event[any]) {
			handler(0)
		})
	})

	if err := coord.Run(ctx); err != nil {
		return fmt.Errorf("replay run: %w", err)
	}
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

	// Close the journal last so any teardown events (plugin Shutdown,
	// session.end finalization) reach disk. Use a short-deadline context
	// so a stuck drain cannot block engine shutdown indefinitely.
	if e.Journal != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = e.Journal.Close(closeCtx)
		cancel()
		e.Journal = nil
	}

	if e.Storage != nil {
		if err := e.Storage.Close(); err != nil {
			e.Logger.Warn("storage close error", "error", err)
		}
		e.Storage = nil
	}

	// Release the session lock last so a crash anywhere above leaves a
	// stale lock that the next Boot can detect and overwrite, rather
	// than no lock at all.
	if e.Session != nil {
		if err := RemoveSessionLock(e.Session.RootDir); err != nil {
			e.Logger.Warn("removing session lock failed", "error", err)
		}
	}

	return nil
}

// startJournal constructs the per-session journal writer and installs the
// bus wildcard handler that feeds it. Called from Boot after the session
// workspace exists and before any plugin Init runs.
func (e *Engine) startJournal() error {
	if e.Session == nil {
		return fmt.Errorf("no session workspace")
	}
	bus, ok := e.Bus.(journal.SeqSource)
	if !ok {
		return fmt.Errorf("bus does not implement journal.SeqSource")
	}

	journalDir := filepath.Join(e.Session.RootDir, "journal")
	rotateBytes := int64(e.Config.Journal.RotateSizeMB) << 20
	if rotateBytes <= 0 {
		rotateBytes = 4 << 20
	}

	// On session recall the journal dir already exists with prior seqs.
	// Prime both the bus counter and the writer's drain so freshly-
	// dispatched events continue monotonically and the writer's reorder
	// buffer does not stall waiting for a seq the new run will not produce.
	var initialSeq uint64 = 1
	if existing, err := journal.Open(journalDir); err == nil {
		if last, lerr := existing.LastSeq(); lerr == nil && last > 0 {
			if ctrl, ok := e.Bus.(SeqController); ok {
				ctrl.SetSeqFloor(last)
			}
			initialSeq = last + 1
		}
	}

	w, err := journal.NewWriter(journalDir, journal.WriterOptions{
		FsyncMode:     journal.ParseFsyncMode(e.Config.Journal.Fsync),
		RotateBytes:   rotateBytes,
		BufferSize:    1024,
		SchemaVersion: journal.SchemaVersion,
		SessionID:     e.Session.ID,
		InitialSeq:    initialSeq,
	})
	if err != nil {
		return err
	}
	e.Journal = w
	e.Lifecycle.journal = w

	// Tool result cache: args-keyed disk cache rooted at journal/cache/.
	// Bus subscriptions auto-populate it on every tool.invoke / tool.result
	// pair, so live tools require no per-plugin wiring. Replay short-
	// circuits look up here before the FIFO stash.
	cacheDir := filepath.Join(journalDir, "cache")
	toolCache := NewToolCache(cacheDir, e.Logger.With("subsystem", "toolcache"))
	e.Replay.SetToolCache(toolCache)
	e.runUnsubs = append(e.runUnsubs, toolCache.Install(e.Bus)...)

	// Wildcard handler builds an envelope for every dispatched event. Seq
	// + ParentSeq are pulled from the bus's per-goroutine dispatch stack
	// (assigned at EmitEvent / EmitVetoable entry, before typed handlers).
	unsub := e.Bus.SubscribeAll(func(ev Event[any]) {
		env := buildEnvelope(ev, bus.CurrentSeq(), bus.ParentSeq())
		w.Append(env)
	})
	e.runUnsubs = append(e.runUnsubs, unsub)
	return nil
}

// buildEnvelope materializes the on-disk record from an in-flight event.
// Detects vetoable events via the *VetoablePayload wrapper so the journal
// records the veto outcome without a separate envelope.
func buildEnvelope(ev Event[any], seq, parentSeq uint64) *journal.Envelope {
	env := &journal.Envelope{
		Seq:        seq,
		ParentSeq:  parentSeq,
		Ts:         ev.Timestamp,
		Type:       ev.Type,
		EventID:    ev.ID,
		Source:     ev.Source,
		SideEffect: journal.IsSideEffect(ev.Type),
		Payload:    ev.Payload,
	}
	if vp, ok := ev.Payload.(*VetoablePayload); ok {
		env.Vetoed = vp.Veto.Vetoed
		env.VetoReason = vp.Veto.Reason
		env.Payload = vp.Original
	}
	return env
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
		// Cancel the active agent turn so in-flight LLM requests abort immediately.
		_ = e.Bus.Emit("cancel.request", events.CancelRequest{Source: "signal:" + sig.String()})
	case <-e.SessionEnded():
		e.Logger.Info("session ended")
	case <-ctx.Done():
		e.Logger.Info("context cancelled")
		_ = e.Bus.Emit("cancel.request", events.CancelRequest{Source: "context"})
	}

	// Use a fresh background context for shutdown so plugin Shutdown calls
	// are not handed an already-cancelled context.
	return e.Stop(context.Background())
}

// ResumeSession loads an existing session workspace for recall and emits
// the session-start event. Like StartSession, it is split into a workspace-
// load phase (no events) and an announce phase (the io.session.start emit)
// so the journal writer can subscribe between them.
func (e *Engine) ResumeSession(sessionID string) error {
	if e.Session == nil {
		if err := e.prepareResumeSession(sessionID); err != nil {
			return err
		}
	}
	return e.announceResume()
}

// prepareResumeSession loads the workspace without emitting any bus events.
func (e *Engine) prepareResumeSession(sessionID string) error {
	root := ExpandPath(e.Config.Core.Sessions.Root)

	session, err := LoadSessionWorkspace(root, sessionID, e.Bus)
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}
	e.Session = session
	e.Lifecycle.session = session
	return nil
}

// announceResume emits the io.session.start event for a resumed session.
func (e *Engine) announceResume() error {
	if e.Session == nil {
		return fmt.Errorf("no session to announce")
	}
	e.Logger.Info("session recalled", "session_id", e.Session.ID, "root", e.Session.RootDir)
	return e.Bus.Emit("io.session.start", map[string]any{
		"session_id": e.Session.ID,
		"root_dir":   e.Session.RootDir,
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

// StartSession creates a new session workspace, writes its initial metadata
// artifacts, and emits the session start event. The artifact writes go via
// SessionWorkspace.WriteFile, which dispatches session.file.created on the
// bus — so the journal must be running before this is called for those
// events to be captured. Boot enforces that ordering.
func (e *Engine) StartSession() error {
	if e.Session == nil {
		if err := e.prepareSession(); err != nil {
			return err
		}
	}

	session := e.Session

	// Write config snapshot to metadata. Prefer the original raw YAML bytes
	// over yaml.Marshal(e.Config): the typed Config drops core.models and
	// per-plugin configs (yaml:"-"), so re-marshaling would produce a snapshot
	// that fails on recall.
	if len(e.Config.Raw) > 0 {
		_ = session.WriteFile("metadata/config-snapshot.yaml", e.Config.Raw)
	} else if cfgData, err := yaml.Marshal(e.Config); err == nil {
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

// prepareSession creates the session workspace directory without emitting
// any bus events. Boot calls this before startJournal so the workspace
// exists for the writer to land its files in, while io.session.start and
// the metadata-artifact writes are deferred until after the writer is
// subscribed.
func (e *Engine) prepareSession() error {
	root := ExpandPath(e.Config.Core.Sessions.Root)

	session, err := NewSessionWorkspace(root, e.Bus)
	if err != nil {
		return fmt.Errorf("creating session workspace: %w", err)
	}
	e.Session = session
	e.Lifecycle.session = session
	return nil
}

// parseLogLevel converts a string log level to slog.Level.
// setTagIfAbsent assigns key=value only when the caller hasn't already set
// the key. Lets per-plugin tags (set by emitting plugin) override
// session-level seeds — important for `tenant` overrides in batch jobs and
// for plugins that explicitly want a different attribution.
func setTagIfAbsent(tags map[string]string, key, value string) {
	if _, exists := tags[key]; !exists {
		tags[key] = value
	}
}

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
