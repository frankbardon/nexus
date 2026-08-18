// Package a2a implements the nexus.io.a2a serve plugin: an HTTP listener that
// exposes this Nexus instance as an Agent2Agent (A2A) agent.
//
// It mounts three surfaces on one listener:
//
//   - GET /.well-known/agent-card.json — the discovery document (specification
//     section 8.2), served from hand-authored config with its interfaces,
//     capabilities and security requirements derived from the live listener.
//   - POST <jsonrpc_path> — the JSON-RPC 2.0 binding (section 9).
//   - <rest_prefix>/… — the HTTP+JSON/REST binding (section 11).
//
// The wire format is entirely pkg/a2a's; this package contributes the listener,
// the credential guard, the card assembly and the routing. It is the A2A
// sibling of nexus.io.agui and inherits that plugin's exemption from the
// browser/wails transport parity rule (see .claude/docs/io-transport.md): an
// external interop transport is not a Nexus UI.
//
// # What this plugin does NOT do yet
//
// Nothing here touches the event bus. Subscriptions and Emissions are
// deliberately empty and the contract harness asserts that they stay honest.
// Every A2A operation is decoded, version-checked and authenticated, and then
// answered with an UnsupportedOperationError naming the operation — see
// implementedOperations, which is the single switch that both gates dispatch and
// determines what the Agent Card claims. Driving an actual agent turn lands in a
// later story; when it does, adding an entry to that map flips both the
// behaviour and the advertised capability together, so the two cannot disagree.
//
// # Concurrency
//
// The plugin owns one *http.Server. Handlers run on net/http goroutines and read
// only immutable state built during Init (the resolved config, the rendered
// card, the validator chain), so there is no shared mutable state to guard. When
// bus mapping arrives it must follow nexus.io.agui's model: bus handlers never
// touch an SSE writer, and a mutex guards the active-run pointer.
package a2a

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
)

const pluginID = "nexus.io.a2a"

// legacyBearerPrincipal is the identity a request authenticated by the
// bearer_token / bearer_token_env config acts as.
//
// The keys carry no notion of who the holder is — they are one shared secret —
// but nexusauth requires a non-empty Principal.ID for a validation to count as a
// success. Naming the credential source rather than inventing an identity keeps
// the audit record honest: "this caller presented the configured shared token".
const legacyBearerPrincipal = "bearer_token"

// implementedOperations is the set of A2A operations this plugin actually
// executes. It is the ONE place the plugin's maturity is recorded.
//
// It gates two things that must never disagree: which operations dispatch
// instead of returning UnsupportedOperationError, and which capability booleans
// the Agent Card advertises. Wiring an operation is therefore a single edit
// here plus the handler, and it is not possible to ship a card claiming
// streaming while SendStreamingMessage refuses every caller.
//
// Empty in this story by design: the listener is discoverable and authenticated,
// and no operation drives a turn yet.
var implementedOperations = map[string]bool{}

// operationImplemented reports whether an A2A operation is wired to real
// behaviour.
func operationImplemented(operation string) bool { return implementedOperations[operation] }

// Plugin is the A2A serve plugin.
type Plugin struct {
	logger *slog.Logger
	cfg    *config
	card   *servedCard
	server *Server
}

// New creates a new A2A serve plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "A2A IO" }
func (p *Plugin) Version() string                   { return "0.1.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

// Subscriptions declares the bus events this plugin consumes.
//
// It is empty, and honestly so: this story stands up the transport, not the
// bus mapping. Declaring the events a future story will need would make the
// contract harness pass against an intention rather than against behaviour,
// which is the one thing the harness exists to prevent.
func (p *Plugin) Subscriptions() []engine.EventSubscription { return nil }

// Emissions declares the bus events this plugin publishes. Empty for the same
// reason as Subscriptions: no inbound request reaches the bus yet.
func (p *Plugin) Emissions() []string { return nil }

// Init resolves configuration, renders the Agent Card and constructs the
// server. Nothing binds a socket here; the listener starts in Ready so every
// plugin has finished Init first.
func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.logger = ctx.Logger

	cfg, err := parseConfig(ctx.Config)
	if err != nil {
		return err
	}
	p.cfg = cfg

	// The card is rendered and validated at boot. A card that cannot be served
	// is a configuration error, and an operator should learn about it from a
	// failed start rather than from a partner's client.
	card, err := buildCard(cfg)
	if err != nil {
		return err
	}
	p.card = card

	p.server = NewServer(serverConfig{
		cfg:    cfg,
		card:   card,
		logger: p.logger,
	})

	// One line an operator can read the whole posture off: where it binds, what
	// it will accept, which validators are live and in what order, and both
	// explicit policy decisions.
	p.logger.Info("a2a serve plugin initialized",
		"bind", cfg.bindAddr,
		"public_url", cfg.publicURL,
		"jsonrpc_path", cfg.jsonrpcPath,
		"rest_prefix", cfg.restPrefix,
		"auth", cfg.chain.Enabled(),
		"auth_validators", cfg.chain.Names(),
		"card_requires_auth", cfg.cardRequiresAuth,
		"assumed_version", cfg.assumedVersion(),
		"cors_origins", len(cfg.corsOrigins),
		"agent_card", card.card.Name,
		"skills", len(card.card.Skills),
		"implemented_operations", len(implementedOperations),
	)
	return nil
}

// Ready starts the HTTP listener.
func (p *Plugin) Ready() error {
	if err := p.server.Start(); err != nil {
		return fmt.Errorf("starting a2a server: %w", err)
	}
	return nil
}

// Shutdown gracefully stops the HTTP server.
func (p *Plugin) Shutdown(_ context.Context) error {
	if p.server == nil {
		return nil
	}
	// A fresh context with a deadline: the incoming one may already be
	// cancelled during engine teardown.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down a2a server: %w", err)
	}
	return nil
}

// Card returns the rendered Agent Card. It exists for tests and for embedders
// that need to publish the same document out-of-band (specification section
// 8.2 lists direct configuration as a discovery mechanism).
func (p *Plugin) Card() a2a.AgentCard {
	if p.card == nil {
		return a2a.AgentCard{}
	}
	return p.card.card
}
