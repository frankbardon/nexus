package logger

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
)

const pluginID = "nexus.observe.logger"

// Plugin is the unified observer for the engine: it registers as a sink on
// the engine's LoggingHost so every slog record — including those emitted
// before this plugin booted — is captured, and it subscribes to the bus with
// replay so every event since construction is captured too. Both streams
// write JSONL into the session workspace.
//
// Messages and events go to separate files so they can be tailed and filtered
// independently. Records from the main Go logger land in messages.jsonl;
// every bus event lands in events.jsonl. Paths may be overridden to absolute
// locations outside the session dir — useful for dev or centralized log
// collection — but by default everything lives under
// <session>/plugins/nexus.observe.logger/.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace
	logging engine.LoggingHost

	logMessages bool
	logEvents   bool
	level       slog.Level

	messagesPath string // absolute override; empty = session default
	eventsPath   string // absolute override; empty = session default

	// Resolved at Init; held for Shutdown.
	messagesFile   *os.File
	messagesMu     sync.Mutex // guards messagesFile writes; file handles are not safe for concurrent use
	removeLogSink  func()
	unsubEvents    func()
	eventsFile     *os.File
	eventsMu       sync.Mutex
}

// New creates a new observer/logger plugin with defaults.
func New() engine.Plugin {
	return &Plugin{
		logMessages: true,
		logEvents:   true,
		level:       slog.LevelInfo,
	}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "Event Logger" }
func (p *Plugin) Version() string                   { return "0.2.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	// Uses SubscribeAllReplay via PluginContext.Bus.
	return nil
}

func (p *Plugin) Emissions() []string {
	return nil
}

// LateShutdown returns true so this plugin is torn down after every non-late
// plugin. Peer Shutdown methods that emit slog records during teardown still
// reach the messages sink this plugin owns.
func (p *Plugin) LateShutdown() bool { return true }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session
	p.logging = ctx.Logging

	p.applyConfig(ctx.Config)

	if p.logMessages {
		if err := p.startMessagesSink(); err != nil {
			return fmt.Errorf("starting messages sink: %w", err)
		}
	}
	if p.logEvents {
		if err := p.startEventsSink(); err != nil {
			return fmt.Errorf("starting events sink: %w", err)
		}
	}

	p.logger.Info("event logger plugin initialized",
		"log_messages", p.logMessages,
		"log_events", p.logEvents,
		"level", p.level.String())
	return nil
}

func (p *Plugin) Ready() error { return nil }

// Shutdown deregisters the log sink, unsubscribes from the bus, and closes
// the output files. Runs in the late phase so teardown records still land.
func (p *Plugin) Shutdown(_ context.Context) error {
	if p.removeLogSink != nil {
		p.removeLogSink()
		p.removeLogSink = nil
	}
	if p.unsubEvents != nil {
		p.unsubEvents()
		p.unsubEvents = nil
	}

	p.messagesMu.Lock()
	if p.messagesFile != nil {
		_ = p.messagesFile.Close()
		p.messagesFile = nil
	}
	p.messagesMu.Unlock()

	p.eventsMu.Lock()
	if p.eventsFile != nil {
		_ = p.eventsFile.Close()
		p.eventsFile = nil
	}
	p.eventsMu.Unlock()

	return nil
}

func (p *Plugin) applyConfig(cfg map[string]any) {
	if v, ok := cfg["log_messages"]; ok {
		if b, ok := v.(bool); ok {
			p.logMessages = b
		}
	}
	if v, ok := cfg["log_events"]; ok {
		if b, ok := v.(bool); ok {
			p.logEvents = b
		}
	}
	if v, ok := cfg["level"]; ok {
		if s, ok := v.(string); ok {
			switch s {
			case "debug":
				p.level = slog.LevelDebug
			case "info":
				p.level = slog.LevelInfo
			case "warn":
				p.level = slog.LevelWarn
			case "error":
				p.level = slog.LevelError
			}
		}
	}
	if v, ok := cfg["messages_file_path"]; ok {
		if s, ok := v.(string); ok {
			p.messagesPath = s
		}
	}
	if v, ok := cfg["events_file_path"]; ok {
		if s, ok := v.(string); ok {
			p.eventsPath = s
		}
	}
}

func (p *Plugin) startMessagesSink() error {
	if p.logging == nil {
		// No host means the engine was constructed without a LoggingHost
		// wired. That shouldn't happen for plugin-managed boots, but fall
		// back gracefully so embedders who skip the field still boot.
		p.logger.Warn("no LoggingHost on PluginContext; messages sink disabled")
		return nil
	}

	path := p.messagesPath
	if path == "" {
		if p.session == nil {
			p.logger.Warn("no session workspace; messages sink disabled (set messages_file_path to override)")
			return nil
		}
		path = filepath.Join(p.session.PluginDir(pluginID), "messages.jsonl")
	}

	f, err := openAppend(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	p.messagesFile = f

	// Wrap the file in a mutex-serialized writer so concurrent Handle calls
	// from the fanout do not interleave partial JSON lines. slog's JSON
	// handler issues a single Write per record, but we still serialize to
	// guard against torn writes when multiple sinks share a process.
	sink := slog.NewJSONHandler(&lockedWriter{mu: &p.messagesMu, w: f}, &slog.HandlerOptions{
		Level: p.level,
	})
	p.removeLogSink = p.logging.AddLogSink(sink)
	return nil
}

func (p *Plugin) startEventsSink() error {
	if p.bus == nil {
		return nil
	}

	path := p.eventsPath
	if path == "" {
		if p.session == nil {
			p.logger.Warn("no session workspace; events sink disabled (set events_file_path to override)")
			return nil
		}
		path = filepath.Join(p.session.PluginDir(pluginID), "events.jsonl")
	}

	f, err := openAppend(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	p.eventsFile = f

	p.unsubEvents = p.bus.SubscribeAllReplay(p.handleEvent)
	return nil
}

func (p *Plugin) handleEvent(e engine.Event[any]) {
	entry := eventLogEntry{
		Type:      e.Type,
		ID:        e.ID,
		Source:    e.Source,
		Timestamp: e.Timestamp,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	data = append(data, '\n')

	p.eventsMu.Lock()
	defer p.eventsMu.Unlock()
	if p.eventsFile == nil {
		return
	}
	_, _ = p.eventsFile.Write(data)
}

// eventLogEntry is the JSON shape written to events.jsonl. Keep this struct
// stable — external tools tail this file for observability.
type eventLogEntry struct {
	Type      string    `json:"type"`
	ID        string    `json:"id"`
	Source    string    `json:"source"`
	Timestamp time.Time `json:"timestamp"`
}

// lockedWriter serializes Write calls to an underlying writer. The fanout
// handler can dispatch records from multiple goroutines; slog.JSONHandler
// itself is safe, but the io.Writer contract does not guarantee atomicity
// across concurrent Writes, so we mediate.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w == nil {
		return 0, nil
	}
	return l.w.Write(p)
}

func openAppend(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}
