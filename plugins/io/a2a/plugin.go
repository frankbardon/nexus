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
// session (see resolveContextLocked). Each call creates exactly one Task, which
// is one Nexus agent turn: created SUBMITTED, moved to WORKING when the agent
// turn starts, and ended COMPLETED at agent.turn.end — or FAILED when the turn
// dies with an error nobody will retry.
//
// Outbound: the plugin subscribes to the bus events that describe a turn and
// translates them into A2A stream frames. SendStreamingMessage writes them as
// text/event-stream records; the blocking SendMessage folds the same frames into
// the Task it returns, so both bindings answer from one translation rather than
// two that can drift.
//
// # What a turn publishes
//
// Artifacts carry OUTPUT (specification section 3.7):
//
//   - The final assistant text, plus an application/json Part when that text is
//     a JSON document, so structured output is a document rather than a string.
//   - Every tool result, UNCONDITIONALLY. It is not behind a flag: an interop
//     transport whose observability depends on the operator having enabled it is
//     one a partner cannot rely on.
//   - Every file the turn wrote, with contents inline as a base64 raw Part.
//
// The Nexus extension carries what A2A has no canonical field for — thinking
// steps, tool calls, subagent progress and token usage — as
// TaskStatusUpdateEvent.metadata keyed by a2a.NexusExtensionURI. It is declared
// in the Agent Card and delivered ONLY to clients that opted in with the
// A2A-Extensions service parameter, so a client that did not ask gets an
// unpolluted canonical stream.
//
// Volume is bounded, and the bound is load-bearing rather than tuning:
// unconditional tool-result artifacts, times inline base64 file parts, times a
// disk-persisted store is an unbounded product. artifacts.max_file_bytes and
// artifacts.max_tool_output_bytes bound one artifact, artifacts.max_task_bytes
// bounds one task, and tasks.max_per_context bounds the store. Every cap
// DEGRADES rather than drops: an over-cap file becomes a metadata note naming
// it, and a task that spends its budget publishes a notice saying how much it
// withheld. See artifacts.go.
//
// File detection is tool.result-based and therefore incomplete BY DESIGN: a file
// is published only when a tool reports having written it (ToolResult.OutputFile,
// or a structured key named by artifacts.file_sources). Snapshot-diffing the
// workspace is out of scope, so an uninstrumented write is missed.
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
// # Interruption and cancellation
//
// A task leaves the ordinary WORKING path two ways, and both are bus-driven.
//
// A hitl.requested — a Nexus agent asking a human something — parks the task at
// TASK_STATE_INPUT_REQUIRED with the question on the status message. The task
// stays LIVE: attached streams stay open (specification section 11.7's close
// rule keys off terminal states, which this is not), the transition is written
// through to the store, and a client that reconnects meanwhile reads the
// question from GetTask or from SubscribeToTask's opening snapshot. The client
// answers with a new message naming the same taskId and contextId, which is
// A2A's own resume mechanism (section 3.4): the answer is routed to
// hitl.responded and the task returns to WORKING inside the SAME turn — no
// io.input, no second task. Because a human takes human time, the wait has a
// deadline (tasks.input_timeout); on expiry the task is failed and hitl.cancel
// retracts the question so the agent loop stops waiting.
//
// A CancelTask settles the task at TASK_STATE_CANCELED, then tells the bus:
// hitl.cancel if it was parked, then cancel.request, which is the
// control.cancel capability's entry point — the same event the TUI emits. An
// already-terminal task is refused with TaskNotCancelableError and nothing is
// written.
//
// # Task lifetime is the task's, not the request's
//
// A run is registered as this listener's single active task and released when
// the TASK reaches a terminal state, not when the HTTP request that started it
// returns. So a client may disconnect mid-turn and reattach with
// SubscribeToTask, a question may be parked for as long as answering it takes,
// and configuration.returnImmediately is answerable rather than refused. The
// cost is the reason CancelTask landed alongside: a turn nobody is watching
// would otherwise hold the process's one agent loop with nothing able to
// interrupt it.
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
// ListTasks and SubscribeToTask read and follow the tasks those turns create;
// CancelTask settles one. Every A2A operation this codebase decodes outside the
// push-notification family is therefore wired, and what remains out of the map —
// the four *TaskPushNotificationConfig operations and GetExtendedAgentCard — is
// deliberately unimplemented rather than pending.
var implementedOperations = map[string]bool{
	a2a.MethodSendMessage:          true,
	a2a.MethodSendStreamingMessage: true,
	a2a.MethodGetTask:              true,
	a2a.MethodListTasks:            true,
	a2a.MethodSubscribeToTask:      true,
	a2a.MethodCancelTask:           true,
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
	// contextID is the A2A context this process is bound to. The first turn to
	// be ACCEPTED claims it, in the same lock hold that takes active — a
	// refused request never binds. See resolveContextLocked and startTurn.
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
//   - llm.request       -> the output schema the turn was constrained to
//   - llm.response      -> the terminal assistant text (tool-call-free responses),
//     and per-call token usage as extension telemetry
//   - io.output         -> the same text after the output gates had their say
//   - agent.turn.end    -> the artifacts plus the COMPLETED status
//   - core.error        -> a FAILED status when nothing will retry
//   - hitl.requested    -> the INPUT_REQUIRED status carrying the question
//   - hitl.responded    -> back to WORKING once the question is answered
//   - tool.invoke       -> a tool call, as extension telemetry
//   - tool.result       -> an ARTIFACT per result, plus an artifact per file the
//     result reports having written, plus the same as telemetry
//   - thinking.step     -> a reasoning step, as extension telemetry
//   - subagent.started / .iteration / .complete -> delegated-work progress, as
//     extension telemetry
//
// The two hitl.* entries are how a Nexus agent asking a human becomes A2A's own
// interruption mechanism. They are SUBSCRIPTIONS, not calls: nexus.control.hitl
// owns ask_user and the pending-request registry, and this plugin renders what
// it sees on the bus rather than reaching into it. The same is true of every
// entry below it: this transport watches the bus and renders what it sees.
func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "agent.turn.start", Priority: 50},
		{EventType: "agent.turn.end", Priority: 50},
		{EventType: "llm.request", Priority: 50},
		{EventType: "llm.response", Priority: 50},
		{EventType: "io.output", Priority: 50},
		{EventType: "core.error", Priority: 50},
		{EventType: "hitl.requested", Priority: 50},
		{EventType: "hitl.responded", Priority: 50},
		{EventType: "tool.invoke", Priority: 50},
		{EventType: "tool.result", Priority: 50},
		{EventType: "thinking.step", Priority: 50},
		{EventType: "subagent.started", Priority: 50},
		{EventType: "subagent.iteration", Priority: 50},
		{EventType: "subagent.complete", Priority: 50},
	}
}

// Emissions declares the bus events the inbound mapping publishes:
//
//   - before:io.input / io.input — one turn's worth of user input, gated first
//     so the same guardrails that see a TUI keypress see an A2A message.
//   - hitl.responded — a message continuing an interrupted task, routed to the
//     question that task is parked on. It is NOT an io.input: a resumed task
//     continues the turn that asked, and emitting input would start a second.
//   - hitl.cancel — the parked question is retracted when the task is canceled
//     or its input deadline elapses, so the agent loop blocked in ask_user
//     unblocks instead of waiting for an answer that is never coming.
//   - cancel.request — CancelTask's route to the control.cancel capability. The
//     same event the TUI and the browser transport emit; cancellation is that
//     plugin's job, not this one's.
func (p *Plugin) Emissions() []string {
	return []string{
		"before:io.input",
		"io.input",
		"hitl.responded",
		"hitl.cancel",
		"cancel.request",
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

	// The directory reported file paths are resolved against and confined to.
	// It defaults to the session's own files workspace because that is where
	// nexus.tool.fileio writes unless an operator moved it, and because
	// confining reads to a directory the SESSION owns is the difference between
	// publishing what the turn produced and publishing whatever path a tool
	// happened to name. With no session and no explicit setting there is no safe
	// base, so file artifacts stay off rather than falling back to the process's
	// working directory.
	if cfg.artifacts.fileBaseDir == "" && ctx.Session != nil {
		cfg.artifacts.fileBaseDir = ctx.Session.FilesDir()
	}

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
		p.bus.Subscribe("hitl.requested", p.handleHITLRequested, engine.WithSource(pluginID)),
		p.bus.Subscribe("hitl.responded", p.handleHITLResponded, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.request", p.handleLLMRequest, engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.invoke", p.handleToolInvoke, engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.result", p.handleToolResult, engine.WithSource(pluginID)),
		p.bus.Subscribe("thinking.step", p.handleThinking, engine.WithSource(pluginID)),
		p.bus.Subscribe("subagent.started", p.handleSubagentStarted, engine.WithSource(pluginID)),
		p.bus.Subscribe("subagent.iteration", p.handleSubagentIteration, engine.WithSource(pluginID)),
		p.bus.Subscribe("subagent.complete", p.handleSubagentComplete, engine.WithSource(pluginID)),
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
		"input_timeout", cfg.inputTimeout,
		"artifact_max_file_bytes", cfg.artifacts.maxFileBytes,
		"artifact_max_tool_output_bytes", cfg.artifacts.maxToolOutputBytes,
		"artifact_max_task_bytes", cfg.artifacts.maxTaskBytes,
		"artifact_file_base_dir", cfg.artifacts.fileBaseDir,
		"nexus_extension", a2a.NexusExtensionURI,
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
