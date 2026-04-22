package logger

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
)

const pluginID = "nexus.observe.logger"

// Plugin observes all events and logs them in structured JSON format.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace

	output   string // "stdout" or "file"
	filePath string
	level    slog.Level
	unsub    func()

	eventLogger *slog.Logger
}

// New creates a new observer/logger plugin.
func New() engine.Plugin {
	return &Plugin{
		output: "stdout",
		level:  slog.LevelInfo,
	}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return "Event Logger" }
func (p *Plugin) Version() string        { return "0.1.0" }
func (p *Plugin) Dependencies() []string { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	// No specific subscriptions; uses SubscribeAll.
	return nil
}

func (p *Plugin) Emissions() []string {
	return nil
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session

	// Read config.
	if v, ok := ctx.Config["output"]; ok {
		if s, ok := v.(string); ok {
			p.output = s
		}
	}
	if v, ok := ctx.Config["file_path"]; ok {
		if s, ok := v.(string); ok {
			p.filePath = s
		}
	}
	if v, ok := ctx.Config["level"]; ok {
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

	// Create the event logger based on output config.
	p.eventLogger = p.createEventLogger()

	// Subscribe to all events.
	p.unsub = p.bus.SubscribeAll(p.handleEvent)

	p.logger.Info("event logger plugin initialized", "output", p.output, "level", p.level.String())
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	if p.unsub != nil {
		p.unsub()
	}
	return nil
}

func (p *Plugin) handleEvent(e engine.Event[any]) {
	p.eventLogger.LogAttrs(context.Background(), slog.LevelDebug, "event",
		slog.String("event_type", e.Type),
		slog.String("event_id", e.ID),
		slog.String("source", e.Source),
		slog.Time("timestamp", e.Timestamp),
	)

	// If session workspace is available, also append to the plugin's event log.
	if p.session != nil {
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
		subpath := "plugins/" + pluginID + "/events.jsonl"
		_ = p.session.AppendFile(subpath, data)
	}
}

func (p *Plugin) createEventLogger() *slog.Logger {
	opts := &slog.HandlerOptions{Level: p.level}

	switch p.output {
	case "file":
		if p.filePath != "" {
			f, err := os.OpenFile(p.filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				p.logger.Error("failed to open log file, falling back to stderr", "path", p.filePath, "error", err)
				return slog.New(slog.NewJSONHandler(os.Stderr, opts))
			}
			return slog.New(slog.NewJSONHandler(f, opts))
		}
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	default:
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
}

// eventLogEntry is the JSON structure written to the session event log.
type eventLogEntry struct {
	Type      string    `json:"type"`
	ID        string    `json:"id"`
	Source    string    `json:"source"`
	Timestamp time.Time `json:"timestamp"`
}
