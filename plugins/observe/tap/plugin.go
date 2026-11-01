// Package tap is the nexus.observe.tap plugin: a read-only, off-process view
// of the event bus. It wildcard-subscribes, renders every event as the same
// journal.Envelope the session journal stores, and streams it over SSE to
// external consumers that cannot (or should not) be compiled in as plugins.
//
// It is strictly an observer. There is no inbound path, it emits nothing on
// the bus, and it cannot veto: a wildcard handler runs after every typed
// handler, so vetoable events arrive with their outcome already decided and
// are reported, not influenced.
//
// Consumers choose their own starting point per connection — live only, the
// in-memory ring, or the full session journal merged with the ring — and
// resume exactly with Last-Event-ID. See docs/src/plugins/observers/tap.md.
package tap

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/journal"
)

const (
	pluginID   = "nexus.observe.tap"
	pluginName = "Event Bus Tap"
	version    = "0.1.0"
)

// Plugin is the engine.Plugin implementation.
type Plugin struct {
	bus    engine.EventBus
	seqs   journal.SeqSource // nil when the bus does not expose seqs
	logger *slog.Logger

	cfg    config
	hub    *hub
	server *server
	unsub  func()
}

// New creates a new tap plugin.
func New() engine.Plugin { return &Plugin{} }

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return pluginName }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

// Subscriptions is empty: the tap uses SubscribeAllReplay, which the
// declared-subscription table does not model (same as nexus.observe.otel).
func (p *Plugin) Subscriptions() []engine.EventSubscription { return nil }

// Emissions is empty by construction. A tap that emitted would observe its
// own emissions and feed them to every consumer.
func (p *Plugin) Emissions() []string { return nil }

// Init parses config and subscribes. Subscribing here rather than in Ready
// keeps the boot-time events: SubscribeAllReplay hands over the bus ring.
func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	if s, ok := ctx.Bus.(journal.SeqSource); ok {
		p.seqs = s
	}

	cfg, err := parseConfig(ctx.Config)
	if err != nil {
		return fmt.Errorf("%s: %w", pluginID, err)
	}
	p.cfg = cfg

	p.hub = newHub(engine.GenerateID(), cfg.ringSize)

	var journalDir string
	if ctx.Session != nil && ctx.Session.RootDir != "" {
		journalDir = filepath.Join(ctx.Session.RootDir, "journal")
	}
	p.server = &server{cfg: cfg, hub: p.hub, journalDir: journalDir, logger: p.logger}

	p.unsub = p.bus.SubscribeAllReplay(p.handleEvent)

	p.logger.Info("tap observer initialized",
		"bind", cfg.bind,
		"auth", cfg.chain.Enabled(),
		"auth_validators", cfg.chain.Names(),
		"include_events", cfg.include,
		"exclude_events", cfg.exclude,
		"ring_size", cfg.ringSize,
	)
	return nil
}

// Ready binds the listener once every plugin has initialized.
func (p *Plugin) Ready() error {
	if err := p.server.start(); err != nil {
		return fmt.Errorf("%s: %w", pluginID, err)
	}
	return nil
}

// Shutdown ends every stream, then stops the listener. The hub closes first
// so open SSE handlers return and http.Server.Shutdown is not left waiting
// on them.
func (p *Plugin) Shutdown(_ context.Context) error {
	if p.unsub != nil {
		p.unsub()
		p.unsub = nil
	}
	if p.hub != nil {
		p.hub.close()
	}
	if p.server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.server.shutdown(ctx); err != nil {
		return fmt.Errorf("%s: shutting down: %w", pluginID, err)
	}
	return nil
}

// Addr is the bound listener address, empty before Ready. For tests and
// embedders that bind port 0.
func (p *Plugin) Addr() string {
	if p.server == nil {
		return ""
	}
	return p.server.addr
}

// handleEvent runs on the dispatching goroutine. It marshals here, not later:
// payloads are live pointers and handlers mutate some in place, so a deferred
// marshal could ship a payload other than the one dispatched.
func (p *Plugin) handleEvent(ev engine.Event[any]) {
	if !p.server.ceiling().match(ev.Type) {
		return
	}
	var seq, parent uint64
	if p.seqs != nil {
		seq, parent = p.seqs.CurrentSeq(), p.seqs.ParentSeq()
	}
	env := engine.BuildEnvelope(ev, seq, parent)
	data, err := json.Marshal(env)
	if err != nil {
		// Same fallback as the journal writer: keep the slot, mark the
		// payload, so the consumer sees the event happened.
		fallback := *env
		fallback.Payload = map[string]string{"__tap_error": err.Error()}
		if data, err = json.Marshal(fallback); err != nil {
			return
		}
	}
	p.hub.publish(&frame{seq: env.Seq, typ: env.Type, data: data})
}
