package otel

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const pluginID = "nexus.observe.otel"

// Plugin exports all bus events as OpenTelemetry spans via OTLP.
// One trace per session, one span per event. Rich attributes for
// LLM, tool, and agent event types.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace

	cfg    config
	unsub  func()
	tracer trace.Tracer
	tp     *shutdownableProvider

	mu       sync.Mutex
	rootSpan trace.Span
	rootCtx  context.Context
}

// New creates a new observe/otel plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return "OpenTelemetry Observer" }
func (p *Plugin) Version() string        { return "0.1.0" }
func (p *Plugin) Dependencies() []string { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return nil // uses SubscribeAll
}

func (p *Plugin) Emissions() []string {
	return nil
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session
	p.cfg = parseConfig(ctx.Config)

	tp, err := newTracerProvider(context.Background(), p.cfg)
	if err != nil {
		return err
	}
	p.tp = tp
	p.tracer = tp.provider.Tracer(pluginID)

	// Start root span for the session.
	sessionID := ""
	if ctx.Session != nil {
		sessionID = ctx.Session.ID
	}
	rootCtx, rootSpan := p.tracer.Start(context.Background(), "nexus.session",
		trace.WithAttributes(
			attribute.String("nexus.session.id", sessionID),
			attribute.String("nexus.service", p.cfg.serviceName),
		),
	)
	p.rootCtx = rootCtx
	p.rootSpan = rootSpan

	p.unsub = p.bus.SubscribeAll(p.handleEvent)

	p.logger.Info("otel observer initialized",
		"endpoint", p.cfg.endpoint,
		"protocol", p.cfg.protocol,
		"service_name", p.cfg.serviceName,
		"exclude_events", p.cfg.excludeEvents,
	)
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(ctx context.Context) error {
	if p.unsub != nil {
		p.unsub()
	}
	if p.rootSpan != nil {
		p.rootSpan.End()
	}
	if p.tp != nil {
		return p.tp.shutdown(ctx)
	}
	return nil
}

func (p *Plugin) handleEvent(e engine.Event[any]) {
	if p.isExcluded(e.Type) {
		return
	}

	p.mu.Lock()
	ctx := p.rootCtx
	p.mu.Unlock()

	_, span := p.tracer.Start(ctx, e.Type,
		trace.WithAttributes(
			attribute.String("nexus.event.id", e.ID),
			attribute.String("nexus.event.type", e.Type),
			attribute.String("nexus.event.source", e.Source),
			attribute.Int64("nexus.event.timestamp_unix_ms", e.Timestamp.UnixMilli()),
		),
	)
	defer span.End()

	// Add rich attributes based on payload type.
	attrs := extractAttributes(e.Payload)
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
}

func (p *Plugin) isExcluded(eventType string) bool {
	for _, excluded := range p.cfg.excludeEvents {
		if excluded == eventType {
			return true
		}
		// Support prefix wildcards like "llm.stream.*".
		if strings.HasSuffix(excluded, ".*") {
			prefix := strings.TrimSuffix(excluded, "*")
			if strings.HasPrefix(eventType, prefix) {
				return true
			}
		}
	}
	return false
}
