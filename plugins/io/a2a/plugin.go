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
// # Round-trip
//
// Inbound: a SendMessage or SendStreamingMessage becomes a Nexus io.input. The
// message's text parts are the turn's prompt and its contextId selects the
// session (see bindContextLocked). Each call creates exactly one Task, which is
// one Nexus agent turn: created SUBMITTED, moved to WORKING when the agent turn
// starts, and ended COMPLETED at agent.turn.end — or FAILED when the turn dies
// with an error nobody will retry.
//
// Outbound: the plugin subscribes to the bus events that describe a turn and
// translates them into A2A stream frames. SendStreamingMessage writes them as
// text/event-stream records; the blocking SendMessage folds the same frames into
// the Task it returns, so both bindings answer from one translation rather than
// two that can drift. The turn's final assistant text rides out as an Artifact
// carrying a text Part.
//
// # Durability
//
// Tasks are not a transient side effect of the request that created them. Each
// one is written to a session-scoped SQLite store (taskstore.go) BEFORE its turn
// is allowed to start, and every status transition and artifact is written
// through as the frame reporting it is queued for the wire. The record is filed
// under the authenticated Principal, and the store's only read surface is scoped
// to one principal, so a task can never be reached by a caller that did not
// create it. Retention (tasks.ttl, tasks.max_per_context) bounds the store.
//
// # Reading tasks back
//
// GetTask, ListTasks and SubscribeToTask answer from that store (tasks.go).
// Every one of them goes through the store's principal-scoped view, so a task
// belonging to another caller is reported exactly as an unknown task id is —
// the same TaskNotFoundError, from the same single lookup, with no second query
// whose presence or absence could leak the difference. SubscribeToTask attaches
// to the live run when there is one and replays the current state first, so a
// client that joins mid-turn is never told about an update to a task it has not
// been shown.
//
// # What this plugin does NOT do yet
//
// CancelTask is decoded, version-checked and authenticated, and then answered
// with an UnsupportedOperationError naming the operation — see
// implementedOperations, which is the single switch that both gates dispatch and
// determines what the Agent Card claims. Adding an entry there flips the
// behaviour and the advertised capability together, so the two cannot disagree.
//
// # Concurrency
//
// The model is nexus.io.agui's, extended in one direction: from one observer per
// run to many. Bus handlers run on arbitrary engine goroutines and NEVER touch a
// response writer. Each handler translates its payload and hands the frames to
// the active run, which — under one lock — folds them into the task snapshot and
// copies them into every attached stream's buffered channel. Each of those
// channels is drained by exactly ONE HTTP handler goroutine, which is the sole
// writer of that goroutine's response. A stream too far behind to be given a
// coherent sequence is dropped rather than allowed to stall the agent loop.
//
// A mutex guards the active-run pointer and the bound context. Everything else
// the handlers read (the resolved config, the rendered card, the validator
// chain) is immutable after Init.
package a2a

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/storage"
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
// SendMessage and SendStreamingMessage drive a real Nexus turn; GetTask,
// ListTasks and SubscribeToTask read and follow the tasks those turns create.
// CancelTask is the one operation still missing, so it stays out of the map and
// the card keeps reporting what is true.
var implementedOperations = map[string]bool{
	a2a.MethodSendMessage:          true,
	a2a.MethodSendStreamingMessage: true,
	a2a.MethodGetTask:              true,
	a2a.MethodListTasks:            true,
	a2a.MethodSubscribeToTask:      true,
}

// operationImplemented reports whether an A2A operation is wired to real
// behaviour.
func operationImplemented(operation string) bool { return implementedOperations[operation] }

// Plugin is the A2A serve plugin.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger
	cfg    *config
	card   *servedCard
	server *Server

	// tasks is the durable task store. Every task this listener creates is
	// recorded there before its turn is allowed to start, and every transition
	// is written through as the frame that reports it is queued. Reads are only
	// reachable through a principal-scoped view — see taskstore.go.
	tasks *taskStore

	// sessionID is the Nexus session this listener serves. One process owns one
	// session, which is what makes the contextId binding below single-valued.
	sessionID string

	// mu guards active and contextID: both are read by net/http goroutines and
	// written by them, while bus handlers read active from arbitrary engine
	// goroutines.
	mu sync.Mutex
	// active is the single in-flight task. At most one runs at a time: the
	// listener fronts one agent loop, and two turns would interleave on the bus.
	active *run
	// contextID is the A2A context this process is bound to, claimed by the
	// first turn. See bindContextLocked.
	contextID string

	unsubs []func()
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

// Subscriptions declares every bus event the outbound translator consumes. It
// must stay in lockstep with the Subscribe calls in Init; the contract harness
// enforces that, and each entry below earns its place by driving a frame:
//
//   - agent.turn.start  -> the WORKING status update
//   - llm.response      -> the terminal assistant text (tool-call-free responses)
//   - io.output         -> the same text after the output gates had their say
//   - agent.turn.end    -> the artifact plus the COMPLETED status
//   - core.error        -> a FAILED status when nothing will retry
func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "agent.turn.start", Priority: 50},
		{EventType: "agent.turn.end", Priority: 50},
		{EventType: "llm.response", Priority: 50},
		{EventType: "io.output", Priority: 50},
		{EventType: "core.error", Priority: 50},
	}
}

// Emissions declares the bus events the inbound mapping publishes: one turn's
// worth of user input, gated first so the same guardrails that see a TUI keypress
// see an A2A message.
func (p *Plugin) Emissions() []string {
	return []string{
		"before:io.input",
		"io.input",
	}
}

// Init resolves configuration, renders the Agent Card and constructs the
// server. Nothing binds a socket here; the listener starts in Ready so every
// plugin has finished Init first.
func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	if ctx.Session != nil {
		p.sessionID = ctx.Session.ID
	}

	cfg, err := parseConfig(ctx.Config)
	if err != nil {
		return err
	}
	p.cfg = cfg

	// The task store is a hard requirement, not a best-effort extra. Without it
	// a task would exist only for the lifetime of the request that created it,
	// which is precisely the lie the store exists to prevent — so a listener
	// that cannot open one does not start at all.
	if ctx.Storage == nil {
		return fmt.Errorf("%s: no per-plugin storage was provided; A2A tasks must be durable", pluginID)
	}
	st, err := ctx.Storage(storage.ScopeSession)
	if err != nil {
		return fmt.Errorf("%s: opening session-scoped storage for the task store: %w", pluginID, err)
	}
	store, err := openTaskStore(st, cfg.retention, ctx.Logger)
	if err != nil {
		return err
	}
	p.tasks = store

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
		bridge: p,
	})

	// Outbound translation (engine -> A2A frames). Handlers translate and
	// enqueue onto the active run; they never write a response.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("agent.turn.start", p.handleTurnStart, engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.turn.end", p.handleTurnEnd, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.response", p.handleLLMResponse, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.output", p.handleOutput, engine.WithSource(pluginID)),
		p.bus.Subscribe("core.error", p.handleError, engine.WithSource(pluginID)),
	)

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
		"task_ttl", cfg.retention.ttl,
		"tasks_per_context", cfg.retention.maxPerContext,
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

// Shutdown unsubscribes, fails any in-flight task so its HTTP handler returns
// promptly, and gracefully stops the HTTP server.
func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	p.unsubs = nil

	if r := p.currentRun(); r != nil {
		r.fail("the agent is shutting down")
	}

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
