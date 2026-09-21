// Package agui implements the nexus.io.agui serve plugin: an HTTP listener that
// exposes the AG-UI ("Agent-User Interaction") endpoint. Clients POST a
// RunAgentInput and receive a text/event-stream SSE response, one stream per
// run. The wire format is defined by pkg/agui (not the pkg/ui Envelope used by
// the browser/wails transports).
//
// # Round-trip
//
// Inbound: a POST RunAgentInput maps its messages to a Nexus io.input on the
// bus (threadId selects/records the session, runId identifies the turn).
// Outbound: the plugin subscribes to the same bus events as the browser
// transport and translates them to canonical AG-UI SSE events for the single
// in-flight run, terminating the stream at RunFinished.
//
// Concurrency model: bus handlers run on arbitrary engine goroutines and never
// touch the SSE writer. Each handler translates its payload and pushes AG-UI
// events onto the active run's buffered channel; the HTTP handler goroutine is
// the sole reader of that channel and the sole writer to the SSE stream. A
// mutex guards the "active run" pointer. This keeps the SSEWriter race-free.
//
// Non-canonical Nexus bus events (workflow.progress, subagent.*,
// code.exec.stdout, ...) have no canonical AG-UI equivalent; they consistently
// ride the AG-UI Custom event (name = bus event type). See
// docs/src/plugins/io-agui.md.
package agui

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/nexusauth"
	"github.com/frankbardon/nexus/pkg/roundtrip"
)

const pluginID = "nexus.io.agui"

// cancelSource names this transport on the cancel.request emitted when a run's
// stream dies under a live turn. The field is a transport name ("tui",
// "browser", "broker", "a2a"), not a reason.
const cancelSource = "agui"

// Config keys this plugin reads for authentication. bearer_token /
// bearer_token_env are the original single-shared-secret form; auth is the
// full validator-chain block shared with cmd/nexus-broker.
const (
	cfgKeyBearerToken    = "bearer_token"
	cfgKeyBearerTokenEnv = "bearer_token_env"
	cfgKeyAuth           = "auth"
)

// defaultBindAddr binds loopback by default so the endpoint is not exposed on
// the network without explicit operator opt-in.
const defaultBindAddr = "127.0.0.1:8090"

// reservedPrincipalIDKey is the reserved-namespace session label startRun/
// resumeRun bind the request's resolved principal into, and handleTurnEnd
// clears once the turn it belongs to has ended. See bindSessionContext.
//
// Aliased from the engine rather than spelled again here: nexus.io.broker
// binds the same label from the principal the broker resolved, and two
// transports writing one label from two copies of its name is exactly the
// drift worth designing out.
const reservedPrincipalIDKey = engine.ReservedPrincipalIDKey

// customBridgedEvents are Nexus-specific bus events with no canonical AG-UI
// equivalent. They ride the AG-UI Custom event (name = bus event type) so a
// conformance client sees a documented superset rather than silently dropping
// them. This is the single, consistent approach chosen for non-canonical events.
var customBridgedEvents = []string{
	"workflow.progress",
	"subagent.started",
	"subagent.iteration",
	"subagent.complete",
	"code.exec.stdout",
}

// Plugin is the AG-UI serve plugin. It stands up an embedded *http.Server that
// exposes the AG-UI POST endpoint with safe-by-default exposure: loopback bind,
// optional bearer-token auth, and configurable CORS. It also owns the bus
// subscriptions that feed the outbound SSE translation.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	server *Server

	sessionID string
	// session is the SessionWorkspace startRun/resumeRun bind the per-run
	// identity and context tags on, and handleTurnEnd/Shutdown clear them from
	// (see bindSessionContext and clearSessionIdentity). Nil when the plugin is
	// constructed without a session (some unit tests); every tag write is a
	// no-op in that case.
	session *engine.SessionWorkspace

	bindAddr string

	// authChain is the resolved credential validator chain. It is never nil after
	// Init; a chain with no validators means auth is disabled.
	authChain *nexusauth.Chain

	corsOrigins []string

	// emitState gates the AG-UI shared-state feature (E3-S1). When true the
	// plugin mirrors the session's scene store as an AG-UI shared-state document,
	// emitting a StateSnapshot at run start and ordered StateDeltas as scenes
	// mutate. Off by default because it adds bus subscriptions and per-mutation
	// diffing overhead most transports do not need.
	emitState bool

	// stateMu guards sharedState. Scene bus events arrive on arbitrary engine
	// goroutines; serializing them here keeps the snapshot and the deltas
	// consistent and ordered.
	stateMu sync.Mutex
	// sharedState is the AG-UI shared-state document: scene_id -> the scene's
	// current content (JSON-encoded). It lives on the plugin (not the run)
	// because scenes are session-scoped and persist across runs on this listener.
	sharedState map[string]json.RawMessage

	// mu guards active, identityRun and identityTurn: at most one run is in
	// flight per listener for this scope (single engine/session per listener,
	// mirroring io/browser).
	mu     sync.Mutex
	active *run
	// identityRun is the run whose bind currently owns the session identity
	// (_principal_id + _header.*). It is NOT the active run: the identity
	// outlives the run that bound it (a HITL park, a client-tool suspend or a
	// disconnect releases the slot with the turn still in flight). Set by
	// bindSessionContext, cleared by the turn-end clear and by Shutdown.
	identityRun *run
	// identityTurn is the TurnID of the work that identity was bound for: the
	// first turn to start under identityRun, or the parked turn a continuation
	// run adopts on resume. It is what lets handleTurnEnd tell "the turn this
	// identity belongs to has ended" from "an older, abandoned turn ended
	// while a newer run already holds its own identity" — a run pointer alone
	// cannot, because on the happy path the turn ends while its OWN run is
	// still draining, and after a late-turn race the newer run is both active
	// AND the identity owner. Empty when no turn has been correlated yet.
	identityTurn string
	// retiredTurn is the TurnID the most recently released run was carrying,
	// recorded by endRun. Releasing the slot does not stop the turn — a
	// disconnect frees it and only then asks the agent to cancel, a HITL park
	// frees it with the agent still blocked — so that turn's trailing events
	// keep arriving after the NEXT run has taken the slot. This is what lets
	// the content handlers tell those apart from the synthetic sub-turn ids a
	// delegated remote legitimately publishes under. See runAcceptsTurnEvent.
	retiredTurn string

	// pendingMu guards pending. A virtual-run interrupt records the mapping
	// from the AG-UI interruptId to the underlying HITL request so the resume
	// side (E2-S2) can correlate a ResumeItem back to the still-blocked
	// in-process hitl and emit the matching hitl.responded. Populated on
	// hitl.requested; the resume handler (E2-S2) is the sole consumer/remover.
	pendingMu sync.Mutex
	pending   map[string]pendingInterrupt

	unsubs []func()
}

// pendingInterrupt records the correlation between an AG-UI interrupt (surfaced
// to the client via a RunFinished(interrupt) outcome) and the in-process HITL
// request it suspended. It is the seam E2-S2 uses on resume: given a
// ResumeItem.InterruptID it looks up the RequestID to emit hitl.responded, and
// the SessionID/TurnID to open the continuation run against the right thread.
type pendingInterrupt struct {
	// Kind distinguishes a HITL suspension (resolved via hitl.responded) from a
	// client-executed-tool suspension (resolved by feeding a tool.result back to
	// the parked agent). It selects which unblock event the resume path emits.
	Kind interruptKind
	// InterruptID is the AG-UI-facing id echoed by the client on resume.
	InterruptID string
	// RequestID is the underlying HITLRequest.ID to resolve via hitl.responded.
	// Empty for a client-tool interrupt.
	RequestID string
	// SessionID / TurnID / ThreadID / RunID scope the interrupt to the thread
	// and turn it suspended so the continuation run targets the same session.
	SessionID string
	TurnID    string
	ThreadID  string
	RunID     string
	// Mode is the HITL response shape, carried so the resume side can validate
	// a ResumeItem payload against what the request accepts. Unused for a
	// client-tool interrupt.
	Mode events.HITLMode
	// ToolCallID / ToolName identify the client-executed tool call this interrupt
	// suspended, so the resume path can synthesize the tool.result the agent
	// awaits. Empty for a HITL interrupt.
	ToolCallID string
	ToolName   string
}

// interruptKind selects the unblock mechanism a pending interrupt uses on
// resume: a HITL interrupt emits hitl.responded; a client-tool interrupt emits
// a synthetic tool.result.
type interruptKind int

const (
	interruptHITL interruptKind = iota
	interruptClientTool
)

// New creates a new AG-UI serve plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "AG-UI IO" }
func (p *Plugin) Version() string                   { return "0.1.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

// Subscriptions declares every bus event the outbound translator consumes. It
// must stay in lockstep with the Subscribe calls in Init (the contract harness
// enforces this).
func (p *Plugin) Subscriptions() []engine.EventSubscription {
	subs := []engine.EventSubscription{
		{EventType: "agent.turn.start", Priority: 50},
		{EventType: "agent.turn.end", Priority: 50},
		{EventType: "llm.stream.chunk", Priority: 50},
		{EventType: "llm.stream.hold", Priority: 50},
		{EventType: "llm.stream.retract", Priority: 50},
		{EventType: "llm.stream.end", Priority: 50},
		{EventType: "io.output", Priority: 50},
		// llm.response is consumed for its Metadata alone -- the provider
		// continuity tokens a later request has to echo back. It runs at 20
		// because nexus.agent.react dispatches tool.invoke from INSIDE its own
		// llm.response handler at 50: read after it and the stash would be empty
		// for exactly the calls it exists to annotate.
		{EventType: "llm.response", Priority: 20},
		// The agent emits tool.invoke (not tool.call) to run a tool; that is the
		// event that carries the resolved arguments and drives ToolCallStart/
		// Args/End. A client-executed tool (advertised via RunAgentInput.tools)
		// rides the same event and additionally suspends the run.
		{EventType: "tool.invoke", Priority: 50},
		{EventType: "tool.result", Priority: 50},
		{EventType: "thinking.step", Priority: 50},
		// HITL suspends the run at the transport boundary (virtual-run model):
		// hitl.requested ends the SSE with an interrupt outcome; hitl.cancel
		// ends it with a cancelled outcome. Neither unblocks the in-process
		// agent — that is the resume side's job (E2-S2).
		{EventType: "hitl.requested", Priority: 50},
		{EventType: "hitl.cancel", Priority: 50},
		// Client-executed frontend tools (E2-S3) are advertised per-run via
		// RunAgentInput.tools. The plugin appends them to the catalog snapshot the
		// agent assembles, so it subscribes to the catalog query at a priority
		// that runs after nexus.tool.catalog (priority 10) has filled the base
		// list.
		{EventType: "tool.catalog.query", Priority: 60},
	}
	for _, et := range customBridgedEvents {
		subs = append(subs, engine.EventSubscription{EventType: et, Priority: 50})
	}
	// Shared-state (E3-S1) is opt-in: only when emit_state is enabled does the
	// plugin subscribe to the scene store's mutation events to mirror them as
	// AG-UI StateSnapshot/StateDelta. Subscriptions() is read after Init, so this
	// reflects the resolved config and stays in lockstep with the Subscribe calls.
	if p.emitState {
		for _, et := range stateEventTypes {
			subs = append(subs, engine.EventSubscription{EventType: et, Priority: 50})
		}
	}
	return subs
}

// Emissions declares the event types the inbound handler emits onto the bus.
// hitl.responded is emitted by the resume path (E2-S2): a continuation
// RunAgentInput carrying resume[] resolves the pending interrupt(s) that ended a
// prior run, unblocking the still-parked in-process agent. Both the resolved and
// the cancelled resume statuses ride hitl.responded (Cancelled:true for the
// latter) — the shape the control/hitl waiter matches on.
func (p *Plugin) Emissions() []string {
	return []string{
		"before:io.input",
		"io.input",
		"hitl.responded",
		// A client-executed tool's result rides tool.result on resume (E2-S3):
		// the ToolCallResult carried in resume[] is fed back to the still-parked
		// agent as the tool.result it was waiting on.
		"tool.result",
		// Inbound shared state (E3-S2) seeds the scene store by emitting a
		// scene_create tool.invoke per client-authored scene, so the agent
		// observes the client's state via scene_get/scene_list.
		"tool.invoke",
		// A run whose SSE stream dies under a live turn asks the
		// control.cancel capability to stop that turn, so an agent does not
		// keep spending the session's budget for a client that is gone. See
		// streamDied.
		"cancel.request",
	}
}

// Init reads config, constructs the server, and wires the outbound bus
// subscriptions. Nothing binds a socket here; the listener starts in Ready so
// all plugins have finished Init first.
func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.pending = make(map[string]pendingInterrupt)

	if ctx.Session != nil {
		p.sessionID = ctx.Session.ID
		p.session = ctx.Session
	}

	p.bindAddr = defaultBindAddr
	if v, ok := ctx.Config["bind"].(string); ok && strings.TrimSpace(v) != "" {
		p.bindAddr = strings.TrimSpace(v)
	}

	chain, err := resolveAuthChain(ctx.Config)
	if err != nil {
		return err
	}
	p.authChain = chain

	p.corsOrigins = parseCORSOrigins(ctx.Config["cors_origins"])

	// Shared-state emission (E3-S1) is opt-in. When enabled the plugin mirrors
	// the scene store as an AG-UI shared-state document.
	if v, ok := ctx.Config["emit_state"].(bool); ok {
		p.emitState = v
	}
	p.sharedState = make(map[string]json.RawMessage)

	p.server = NewServer(serverConfig{
		addr:        p.bindAddr,
		chain:       p.authChain,
		corsOrigins: p.corsOrigins,
		logger:      p.logger,
		bridge:      p,
	})

	// Wire outbound bus subscriptions (engine -> AG-UI SSE). Handlers translate
	// and enqueue onto the active run; they never write the SSE directly.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("agent.turn.start", p.handleTurnStart, engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.turn.end", p.handleTurnEnd, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.stream.chunk", p.handleStreamChunk, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.stream.hold", p.handleStreamHold, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.stream.retract", p.handleStreamRetract, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.stream.end", p.handleStreamEnd, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.output", p.handleOutput, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.response", p.handleLLMResponse,
			engine.WithPriority(20), engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.invoke", p.handleToolInvoke, engine.WithSource(pluginID)),
		p.bus.Subscribe("tool.result", p.handleToolResult, engine.WithSource(pluginID)),
		p.bus.Subscribe("thinking.step", p.handleThinkingStep, engine.WithSource(pluginID)),
		p.bus.Subscribe("hitl.requested", p.handleHITLRequested, engine.WithSource(pluginID)),
		p.bus.Subscribe("hitl.cancel", p.handleHITLCancel, engine.WithSource(pluginID)),
		// Append per-run client tools to the catalog snapshot the agent builds.
		// Priority 60 runs after nexus.tool.catalog (10) fills the base list.
		p.bus.Subscribe("tool.catalog.query", p.handleCatalogQuery,
			engine.WithPriority(60), engine.WithSource(pluginID)),
	)
	for _, et := range customBridgedEvents {
		eventType := et
		p.unsubs = append(p.unsubs,
			p.bus.Subscribe(eventType, func(e engine.Event[any]) {
				if r := p.currentRun(); r != nil {
					r.onCustom(eventType, e.Payload)
				}
			}, engine.WithSource(pluginID)),
		)
	}

	// Shared-state mirror (E3-S1): only wired when opt-in. These translate scene
	// store mutations into AG-UI StateSnapshot/StateDelta on the active run.
	if p.emitState {
		p.unsubs = append(p.unsubs,
			p.bus.Subscribe(sceneCreatedType, p.handleSceneCreated, engine.WithSource(pluginID)),
			p.bus.Subscribe(scenePatchedType, p.handleScenePatched, engine.WithSource(pluginID)),
			p.bus.Subscribe(sceneDeletedType, p.handleSceneDeleted, engine.WithSource(pluginID)),
		)
	}

	// The validator names make the auth posture readable in one line: which
	// validators are live and in what order they are tried.
	p.logger.Info("agui serve plugin initialized",
		"bind", p.bindAddr,
		"auth", p.authChain.Enabled(),
		"auth_validators", p.authChain.Names(),
		"cors_origins", len(p.corsOrigins),
		"emit_state", p.emitState,
	)
	return nil
}

// resolveAuthChain turns the plugin's authentication config into a validator
// chain. A chain with no validators means auth is disabled, which is what a
// listener with neither a bearer token nor an `auth:` block has always been.
//
// Two spellings are accepted and they are MUTUALLY EXCLUSIVE:
//
//   - `bearer_token` / `bearer_token_env` — the original single-shared-secret
//     form, desugared into a one-entry `static` validator. Its behaviour and its
//     precedence (inline first, then the env var) are unchanged, and it is not
//     deprecated: it is the right amount of configuration for a loopback
//     listener fronting one developer's UI.
//   - `auth:` — the full validator-chain block, parsed by the same
//     nexusauth.ChainFromMap the session broker uses, which is what makes JWKS,
//     RFC 7662 introspection and proxy-header identity available here.
//
// Setting both is a boot error rather than a precedence rule, following the same
// reasoning nexusauth applies to `client_secret` / `client_secret_env`: two
// sources for one security decision means one of them is stale, and quietly
// preferring either is how an operator comes to believe a credential was
// tightened when it was not. The error names both keys so the fix is obvious.
func resolveAuthChain(cfg map[string]any) (*nexusauth.Chain, error) {
	var inline, envVar string
	if v, ok := cfg[cfgKeyBearerToken].(string); ok {
		inline = strings.TrimSpace(v)
	}
	if v, ok := cfg[cfgKeyBearerTokenEnv].(string); ok {
		envVar = strings.TrimSpace(v)
	}

	rawAuth, hasAuth := cfg[cfgKeyAuth]
	if rawAuth == nil {
		hasAuth = false
	}

	if hasAuth {
		// Presence of the legacy keys is what conflicts, not the value they
		// resolve to: `bearer_token_env` naming an unset variable is still an
		// operator saying "authenticate with a shared token", and must not slip
		// through the check just because the variable happened to be empty.
		if inline != "" || envVar != "" {
			return nil, fmt.Errorf("%s: %s and %s/%s are mutually exclusive: express the shared token as an %s validator with type: %s, or drop the %s block",
				pluginID, cfgKeyAuth, cfgKeyBearerToken, cfgKeyBearerTokenEnv,
				cfgKeyAuth, nexusauth.ValidatorTypeStatic, cfgKeyAuth)
		}
		m, ok := rawAuth.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: %s: want a mapping, got %T", pluginID, cfgKeyAuth, rawAuth)
		}
		chain, err := nexusauth.ChainFromMap(m)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", pluginID, cfgKeyAuth, err)
		}
		return chain, nil
	}

	// Legacy path. Precedence is preserved exactly: an inline token wins, and the
	// env var is consulted only when there is no inline token.
	token := inline
	if token == "" && envVar != "" {
		token = strings.TrimSpace(os.Getenv(envVar))
	}
	if token == "" {
		return nexusauth.NewChain(), nil
	}
	return staticChainFromToken(token)
}

// Ready starts the HTTP listener.
func (p *Plugin) Ready() error {
	if err := p.server.Start(); err != nil {
		return fmt.Errorf("starting agui server: %w", err)
	}
	return nil
}

// Shutdown gracefully stops the HTTP server and unsubscribes from the bus.
func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	p.unsubs = nil

	// Fail any in-flight run so its HTTP handler returns promptly. The return
	// is deliberately ignored: teardown is not stream death, and a
	// cancel.request emitted into a bus that is already stopping would reach
	// an agent that is going away regardless.
	if r := p.currentRun(); r != nil {
		r.fail("agui server shutting down")
	}

	// Backstop, and the only one: handleTurnEnd releases the bound identity
	// when the turn it belongs to ends, so a turn that never emits
	// agent.turn.end (wedged provider, killed sub-process, a run failed here)
	// would otherwise leave _principal_id and _header.* bound in the session
	// metadata for whoever reads it next. Unconditional — by the time
	// Shutdown runs, no turn can still legitimately own an identity. There is
	// deliberately no TTL, lease or timer anywhere on this path: a deadline
	// would re-create the original bug for any legitimately long turn.
	p.mu.Lock()
	p.identityRun = nil
	p.identityTurn = ""
	p.mu.Unlock()
	p.clearSessionIdentity()

	if p.server == nil {
		return nil
	}
	// Use a fresh context with a deadline: the incoming context may already be
	// cancelled during engine teardown.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down agui server: %w", err)
	}
	return nil
}

// --- bridge: inbound (server -> bus) and run lifecycle ---

// startRun registers a new run as the single active run and emits the inbound
// io.input on the bus. It returns the run and an ok flag; ok is false when
// another run is already in flight (one run per listener for this scope), in
// which case the caller must reject with a RunError.
func (p *Plugin) startRun(input runInput) (*run, bool) {
	r := newRun(input.threadID, input.runID, input.tools)

	// RunStarted is emitted eagerly so even a run with no agent produces a
	// well-formed lifecycle. The first agent.turn.start will not duplicate it.
	//
	// It is queued BEFORE the run is published as p.active, and that ordering
	// is load-bearing rather than tidy: every terminal verb (finish, fail,
	// interrupt, cancelTerminal) closes r.done, and queue DROPS silently once
	// done is closed. A run published first and queued second is reachable by
	// a bus handler in between — which is exactly the window a turn ending
	// under the PREVIOUS run lands in — so the client got HTTP 200 and a
	// stream whose RunStarted had been discarded. Nothing can reach a run that
	// has not been published, so queueing first closes that window
	// structurally. The turn-end scoping in handleTurnEnd is the other half;
	// neither substitutes for the other.
	r.markStarted()
	r.queue(newRunStarted(input.threadID, input.runID))

	p.mu.Lock()
	if p.active != nil {
		p.mu.Unlock()
		return nil, false
	}
	p.active = r
	p.mu.Unlock()

	// Bind the per-run identity and context tags BEFORE the run's io.input is
	// emitted below (ordering matters — see bindSessionContext), mirroring
	// this package's existing "register before unblocking" discipline.
	p.bindSessionContext(input, r)

	// Inbound shared state (E3-S2): a client-authored RunAgentInput.state is
	// reconciled into the scene store and the mirror BEFORE the initial snapshot
	// so the agent observes the client's view and the snapshot reflects it. No-op
	// when shared-state is disabled or no state is sent.
	p.applyInboundState(input.threadID, input.state)

	// When shared-state is enabled, a StateSnapshot immediately follows
	// RunStarted so the client can render the agent's current scene state before
	// any StateDelta. No-op when disabled.
	p.emitInitialSnapshot(r)

	// Map messages -> io.input and publish. The last user message drives the
	// turn; earlier messages are preloaded so resume/history stays intact.
	ui := p.buildUserInput(input)
	go func() {
		if veto, err := p.bus.EmitVetoable("before:io.input", &ui); err == nil && veto.Vetoed {
			r.fail("io.input vetoed")
			return
		}
		if err := p.bus.Emit("io.input", ui); err != nil {
			r.fail(fmt.Sprintf("emit io.input: %v", err))
		}
	}()
	return r, true
}

// endRun releases the active-run slot if it still points at r. That is its
// whole job: it does NOT touch the identity bound at startRun/resumeRun.
//
// The two used to be one function, and that was the bug. They have different
// lifetimes. The slot is transport-scoped — p.active == nil is what unblocks
// startRun for the NEXT run on this listener (see startRun's "another run
// already in flight" check), so it must be freed the moment this run's SSE
// stream is done. The identity (_principal_id plus the _header.* namespace)
// is turn-scoped: it belongs to the work, which runs detached on engine
// goroutines and routinely outlives the HTTP request that started it.
//
// A HITL park is the case that proves the two differ, and it needs no error
// path to reach: handleHITLRequested ends the stream and calls endRun while
// the agent is still blocked in-process on the pending question (see
// interrupt.go), as does a client-executed tool suspend (clienttools.go). A
// client disconnect mid-turn is the same shape. Clearing identity here would
// de-authenticate a turn that is still running, for as long as a human takes
// to answer.
//
// The identity clear lives in handleTurnEnd instead, on the turn's own
// agent.turn.end, with Shutdown as the only backstop for a turn that never
// ends. See clearSessionIdentity.
//
// It also records the turn the released run was carrying, as p.retiredTurn.
// That is the one fact a SUCCESSOR run needs and cannot otherwise know: the
// released run's turn may still be alive on an engine goroutine (a disconnect,
// a park), and the trailing events it emits carry a turn id the next run never
// saw start. See runAcceptsTurnEvent.
func (p *Plugin) endRun(r *run) {
	// Read the run's turn before taking p.mu: r.boundTurn takes r's own lock,
	// and this package never nests the two.
	var retired string
	if r != nil {
		retired = r.boundTurn()
	}

	p.mu.Lock()
	if p.active == r {
		p.active = nil
	}
	// Only a run that actually bound a turn overwrites the record. A run
	// released without one (an input vetoed before any agent ran, a refused
	// resume) has no trailing events to disown, and letting it clear the field
	// would forget a predecessor whose turn is still live.
	if retired != "" {
		p.retiredTurn = retired
	}
	p.mu.Unlock()
}

// runOwnsTurnEnd reports whether an agent.turn.end naming turnID is r's own
// turn ending, and therefore whether it may terminate r's stream.
//
// This is the same discriminator the identity clear in handleTurnEnd already
// uses, applied to the other half of that handler, and it is the rule
// nexus.io.a2a states for its own task lifetime (see that plugin's
// run.onTurnEnd). Both halves of it matter:
//
//   - A DIFFERENT id is somebody else's turn, and always was.
//   - An id when this run has bound NONE is also somebody else's: a run is
//     published before its io.input is emitted and a continuation adopts its
//     parked turn before it is published, so a run cannot miss its own turn
//     start. This case is the reported defect — a client disconnect releases
//     the slot and only then asks the agent to stop, so the cancelled turn's
//     agent.turn.end arrives after the next POST has already taken the slot,
//     and finished a run that had not started working.
//
// An EMPTY id on the event is not correlatable at all, and the run takes it:
// ignoring it would leave the client on a stream that never terminates, which
// is worse.
func (p *Plugin) runOwnsTurnEnd(r *run, turnID string) bool {
	if turnID == "" {
		return true
	}
	return r.boundTurn() == turnID
}

// runAcceptsTurnEvent reports whether a turn-scoped CONTENT event (io.output,
// tool.invoke, tool.result) naming turnID should be rendered onto r's stream.
//
// It is deliberately more permissive than runOwnsTurnEnd, because the two
// questions are different and so are the vocabularies they range over.
// Terminating a run is severe and only three plugins emit agent.turn.end, each
// carrying the top-level turn's own id, so there a mismatch is always foreign.
// io.output's emitters are an OPEN set, and two kinds of legitimate event
// would be deleted by a strict match:
//
//   - Most gates emit io.output carrying no TurnID at all (a budget warning, a
//     stop-word refusal, the engine's own error output). Uncorrelatable, and
//     they must still reach the client.
//   - nexus.agent.aguiremote and nexus.agent.a2aremote republish a delegated
//     remote's narration under a SYNTHETIC sub-turn id ("agui_remote_<spawn>",
//     "a2a_remote_<spawn>") precisely so a transport can group it apart from
//     the local turn that asked for it. No agent.turn.start ever announces
//     one, so it can never match, and dropping it would silently remove a
//     built feature.
//
// So it refuses exactly what it can prove is foreign:
//
//   - a named turn arriving at a run that has bound none (runOwnsTurnEnd's
//     second bullet, for the same reason), and
//   - a named turn belonging to the run this one replaced, which endRun
//     recorded. That is the cancelled turn whose trailing io.output was being
//     rendered as the answer to the next question.
//
// Its refusals are therefore a strict subset of runOwnsTurnEnd's, which is the
// right direction: mis-delivering content is bad, and terminating the wrong
// stream is worse.
func (p *Plugin) runAcceptsTurnEvent(r *run, turnID string) bool {
	if turnID == "" {
		return true
	}
	bound := r.boundTurn()
	if bound == turnID {
		// Its own turn always wins, including when that turn is also the
		// retired one — a continuation run adopts the parked turn it is
		// resuming, and those events are its own.
		return true
	}
	if bound == "" {
		return false
	}
	p.mu.Lock()
	retired := p.retiredTurn
	p.mu.Unlock()
	return turnID != retired
}

// streamDied reports that r's SSE stream died — the client went away, or a
// write to it failed — and asks the control.cancel capability to stop the turn
// that was running for it.
//
// Without this an orphaned turn runs to completion for nobody: the agent keeps
// iterating, calling tools and spending tokens against the session's budget
// with no reader on the other end. r.fail alone only closes the channel this
// side of the bus; it never reaches the agent.
//
// It is deliberately NOT a "the handler returned" hook. Returning is how a run
// ends normally, and it is also how a HITL park and a client-executed-tool
// suspend end — both of which leave the agent alive and parked ON PURPOSE.
// Wired without the two guards below, every parked turn would cancel itself:
//
//   - The caller only reaches here when its own r.fail performed the run's
//     one-shot close. A completed run (finish), a parked one (interrupt) or a
//     retracted question (cancelTerminal) closed the run first, so the fail
//     behind it returns false and never calls in.
//   - r.suspended is set before interrupt's close, so a disconnect that wins
//     the race against a park still reads the park and stays out of it.
//
// A run that never saw an agent.turn.start has no turn to name and nothing to
// cancel — an input vetoed before any agent ran, for instance — and emits
// nothing.
//
// This asks rather than reaching into the agent: nexus.control.cancel owns turn
// cancellation for every transport (the TUI, the browser and nexus.io.a2a all
// enter the same way) and answers with cancel.active, which the agent loop
// turns into a cancel.complete{Resumable: true} and a final agent.turn.end. The
// work is therefore suspended and resumable, not destroyed — and that
// agent.turn.end is also what releases the identity this run bound (see
// handleTurnEnd), so a cancelled turn closes its identity lifetime through the
// ordinary path with no second mechanism.
func (p *Plugin) streamDied(r *run) {
	if r == nil || p.bus == nil {
		return
	}
	if r.isSuspended() {
		return
	}
	turnID := r.boundTurn()
	if turnID == "" {
		return
	}

	p.logger.Info("agui stream died under a live turn; cancelling it",
		"thread_id", r.threadID,
		"run_id", r.runID,
		"turn_id", turnID,
	)
	if err := p.bus.Emit("cancel.request", events.CancelRequest{
		SchemaVersion: events.CancelRequestVersion,
		TurnID:        turnID,
		Source:        cancelSource,
	}); err != nil {
		p.logger.Warn("emitting cancel.request for a dead agui stream failed",
			"error", err, "turn_id", turnID)
	}
}

// clearSessionIdentity drops the reserved labels bindSessionContext wrote:
// _principal_id and the whole _header.* namespace, always together, because
// they describe one caller and a half-cleared identity is worse than either
// whole state. No-op when the plugin was constructed without a session.
func (p *Plugin) clearSessionIdentity() {
	if p.session == nil {
		return
	}
	if err := p.session.SetPrincipalID(""); err != nil {
		p.logger.Warn("clearing _principal_id session label failed", "error", err)
	}
	if err := p.session.SetRequestHeaders(nil); err != nil {
		p.logger.Warn("clearing X-Nexus-* request header labels failed", "error", err)
	}
}

// bindSessionContext writes the per-run identity and business-context
// signals into the session's tag store. It is called by both startRun and
// resumeRun BEFORE the run's io.input (startRun) or hitl.responded/
// tool.result (resumeRun) is emitted — ordering matters, mirroring this
// package's existing "register before unblocking" discipline (see resume.go's
// run-registration comment) so a consumer of session.tag.set/deleted
// (e.g. an external identity registry) never observes the turn's first
// downstream event before the identity it is bound to.
//
// _principal_id is bound via SetReservedLabel, the only sanctioned writer of
// a "_"-prefixed key. RunAgentInput.Context items are written directly via
// SetLabel as general-namespace tags (Description -> key, Value -> value),
// bypassing the vetoable before:session.tag.set path entirely — this is
// already-authenticated, already-decoded trusted input, the same reasoning
// buildUserInput applies to input.messages.
//
// Every call re-binds fresh from input's own resolved principal: there is no
// "unchanged principal, skip the write" special case, so a resumed thread
// under a different principal always gets a new bind, never a stale one
// carried over from the run it continues. No-op when the plugin was
// constructed without a session (some unit tests).
func (p *Plugin) bindSessionContext(input runInput, r *run) {
	if p.session == nil {
		return
	}
	// Bound unconditionally, including when this run resolved no principal:
	// SetPrincipalID("") clears the label, so an unauthenticated run reads
	// empty rather than inheriting the last authenticated run's identity.
	// Same rule nexus.io.broker documents for the same label, and the same
	// one SetRequestHeaders already followed below. It is a no-op when auth
	// is disabled, since every run then resolves empty.
	if err := p.session.SetPrincipalID(input.principalID); err != nil {
		p.logger.Warn("binding _principal_id session label failed",
			"error", err, "principal_id", input.principalID)
	}
	// The request's X-Nexus-* headers REPLACE whatever the previous run bound,
	// including when this run carried none — SetRequestHeaders clears the
	// namespace for an empty map. That is the point: a plugin reading
	// _header.tenant must never be handed the last run's tenant because this
	// run's POST omitted the header.
	if err := p.session.SetRequestHeaders(input.headers); err != nil {
		p.logger.Warn("binding X-Nexus-* request headers failed", "error", err)
	}
	for _, item := range input.contextItems {
		if item.Description == "" {
			// A context item with no key has nothing to bind to; skip it
			// rather than write an empty-string key.
			continue
		}
		if err := p.session.SetLabel(item.Description, item.Value); err != nil {
			p.logger.Warn("writing session context tag failed",
				"error", err, "key", item.Description)
		}
	}

	// Record which run now owns the identity, AFTER the writes rather than
	// alongside the p.active registration: between registering a run and
	// binding it, the labels still hold the previous run's identity, and a
	// late agent.turn.end arriving in that window must not be told the new run
	// owns them. See handleTurnEnd.
	p.mu.Lock()
	p.identityRun = r
	// A fresh bind owns no turn yet: the next turn to start under this run is
	// the work it belongs to (see handleTurnStart), and a resume adopts the
	// parked turn explicitly (see adoptIdentityTurn). Until then an ending
	// turn belongs to an older bind and must not clear this one.
	p.identityTurn = ""
	p.mu.Unlock()
}

// adoptIdentityTurn records turnID as the turn the currently bound identity
// belongs to. resumeRun calls it for the parked turn its continuation is
// unblocking: that turn started under the interrupted run, but its remaining
// work is the resuming request's, so its agent.turn.end is what releases this
// bind. Without it a resumed turn would never clear the identity it ran under.
func (p *Plugin) adoptIdentityTurn(turnID string) {
	if turnID == "" {
		return
	}
	p.mu.Lock()
	p.identityTurn = turnID
	p.mu.Unlock()
}

// currentRun returns the active run or nil.
func (p *Plugin) currentRun() *run {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active
}

// buildUserInput maps a RunAgentInput's messages to a Nexus UserInput. The
// trailing user message becomes Content; any earlier messages ride as
// PreloadMessages so a resumed thread keeps prior context. The POST's
// X-Nexus-* headers ride along on Headers, which is the seam an io.input
// handler reads instead of the session labels bindSessionContext writes —
// same values, no metadata read.
func (p *Plugin) buildUserInput(input runInput) events.UserInput {
	sessionID := input.threadID
	if sessionID == "" {
		sessionID = p.sessionID
	}
	ui := events.UserInput{
		SchemaVersion: events.UserInputVersion,
		SessionID:     sessionID,
		Headers:       input.headers,
	}
	msgs := input.messages
	// Find the trailing user message to use as the live turn Content.
	last := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if strings.EqualFold(msgs[i].Role, "user") {
			last = i
			break
		}
	}
	for i, m := range msgs {
		if i == last {
			ui.Content = m.Content
			continue
		}
		ui.PreloadMessages = append(ui.PreloadMessages, events.Message{
			Role:       normalizeRole(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			ToolCalls:  preloadToolCalls(m.ToolCalls),
			Metadata:   preloadMessageMetadata(m),
		})
	}
	if last == -1 && len(msgs) > 0 {
		// No user message at all: fall back to the last message's content.
		ui.Content = msgs[len(msgs)-1].Content
		if len(ui.PreloadMessages) > 0 {
			ui.PreloadMessages = ui.PreloadMessages[:len(ui.PreloadMessages)-1]
		}
	}
	return ui
}

// preloadMessageMetadata rebuilds a replayed message's provider continuity
// metadata from the AG-UI fields that carried it out to the client.
//
// WHAT THIS IS FOR. A reasoning model hands back an opaque continuity token
// beside the tool calls of a turn -- Gemini's thoughtSignature, Anthropic's
// thinking blocks -- and rejects the next request that replays those calls
// without it. AG-UI is a client-owned-history protocol: the client replays the
// whole thread, so the only party able to hand the token back is the client,
// and it therefore has to survive a round trip through a browser.
//
// IT ARRIVES IN TWO PLACES BECAUSE IT LEFT IN TWO PLACES. A per-message payload
// rides `message.metadata`; a per-call one rides `toolCalls[i].metadata`,
// because the spec merges a TOOL_CALL_* event's metadata into the call rather
// than the message, and because Gemini signs one call of a parallel batch and
// not the rest. roundtrip.MergeCall folds the calls back into the message-level
// map its own ForCall split them out of.
//
// BOTH HALVES ARE ALLOWLISTED, and on this path that is a trust boundary rather
// than tidiness: the map is authored by a client, and without the filter a
// replayed message could name an engine-internal routing hint (`_source`,
// `_target_*`) and have it applied as though an internal sub-flow had produced
// the message.
//
// It is read leniently and never fails a turn. A client that preserves neither
// field leaves the message exactly as it arrives today, so the worst case of an
// unaware client is the status quo rather than a new failure.
func preloadMessageMetadata(m agui.Message) map[string]any {
	meta := roundtrip.Allowed(m.Metadata)
	for _, c := range m.ToolCalls {
		meta = roundtrip.MergeCall(meta, c.Metadata)
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

// preloadToolCalls carries a replayed assistant turn's tool calls across the
// wire boundary.
//
// A tool result is only addressable BY ITS CALL: every provider pairs the two,
// and Gemini pairs them by the call's NAME, which it reads off the matching
// functionCall rather than off the response. Dropping either half leaves the
// other unpaired, so they are carried together or not at all.
func preloadToolCalls(calls []agui.ToolCall) []events.ToolCallRequest {
	if len(calls) == 0 {
		return nil
	}
	out := make([]events.ToolCallRequest, 0, len(calls))
	for _, c := range calls {
		out = append(out, events.ToolCallRequest{
			ID:        c.ID,
			Name:      c.Function.Name,
			Arguments: c.Function.Arguments,
		})
	}
	return out
}

// normalizeRole maps AG-UI message roles onto Nexus roles.
func normalizeRole(role string) string {
	switch strings.ToLower(role) {
	case "assistant", "system", "tool", "user":
		return strings.ToLower(role)
	default:
		return "user"
	}
}

// --- bus handlers (engine -> run channel). Never touch the SSE writer. ---

func (p *Plugin) handleTurnStart(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	t, ok := e.Payload.(events.TurnInfo)
	if !ok {
		return
	}

	// Correlate the identity with the work it was bound for: the first turn to
	// start under the run that bound it. Only that run's turn counts — a turn
	// starting under some other run belongs to a bind this one has already
	// replaced. react, planexec and orchestrator all emit agent.turn.start
	// before any agent.turn.end for the same TurnID, so every turn that can
	// end here has been stamped. See handleTurnEnd.
	p.mu.Lock()
	if p.active == p.identityRun && p.identityTurn == "" {
		p.identityTurn = t.TurnID
	}
	p.mu.Unlock()

	r.onTurnStart(t)
}

// handleTurnEnd closes out a top-level turn. It is also where the identity
// bound at startRun/resumeRun is released, because agent.turn.end — not the
// HTTP request, and not endRun — is the end of the work that identity belongs
// to. react, planexec and orchestrator each emit it exactly once per
// top-level turn, on every exit including cancel, so a turn that ends at all
// ends here.
//
// The clear fires for the turn the current identity was bound for, and only
// that turn — identityTurn, stamped at agent.turn.start (or adopted on
// resume). Two cases force that precision:
//
//   - The happy path ends the turn while its OWN run is still draining SSE,
//     so "skip whenever a run is active" would never clear anything.
//   - The late-turn race ends an older turn after a newer run has bound: run
//     A's handler returns on a disconnect, run B POSTs and binds principal B,
//     and only then does turn A reach here. B is then both the active run and
//     the identity owner, so a run-pointer comparison cannot tell the two
//     apart either. The TurnID can: turn A is not B's turn, so B keeps its
//     identity.
func (p *Plugin) handleTurnEnd(e engine.Event[any]) {
	t, ok := e.Payload.(events.TurnInfo)
	if !ok {
		return
	}

	p.mu.Lock()
	r := p.active
	// The identity goes iff the turn that just ended is the one it was bound
	// for. That holds whether the run that bound it is still draining (happy
	// path), was released at a HITL park or client-tool suspend, or died on a
	// disconnect — all of which are exactly the cases where the request's
	// lifetime and the turn's diverge.
	clear := p.identityTurn != "" && p.identityTurn == t.TurnID
	if clear {
		p.identityRun = nil
		p.identityTurn = ""
	}
	p.mu.Unlock()

	if clear {
		// Cleared BEFORE r.finish() below, and that order is load-bearing:
		// finish ends the SSE drain, the handler returns, endRun frees the
		// slot, and the next POST binds its own identity. A clear landing
		// after that sequence would delete the new run's bind. Note the
		// session I/O happens outside p.mu — the tag store announces
		// session.tag.deleted on the bus, and a handler of that re-entering
		// this plugin must not meet a held lock.
		p.clearSessionIdentity()
	}
	if r == nil {
		return
	}
	// The run this turn end terminates is the run that OWNS the turn, not
	// whichever run happens to hold the slot. Same discriminator as the
	// identity clear above, and for the same race: A's handler returns on a
	// disconnect, B POSTs and takes the slot, and only then does turn A end
	// here. Before this, B's stream was closed by A's turn — RunFinished with
	// no RunStarted, HTTP 200 and silence, or A's cancellation notice read as
	// the answer to B's question.
	if !p.runOwnsTurnEnd(r, t.TurnID) {
		p.logger.Debug("agui ignoring a turn end that belongs to another run",
			"turn_id", t.TurnID,
			"run_turn_id", r.boundTurn(),
			"thread_id", r.threadID,
			"run_id", r.runID,
		)
		return
	}
	r.onTurnEnd(t)
	// A top-level turn end terminates the run and the SSE stream.
	r.finish()
}

func (p *Plugin) handleStreamChunk(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	c, ok := e.Payload.(events.StreamChunk)
	if !ok {
		return
	}
	r.onStreamChunk(c)
}

func (p *Plugin) handleStreamHold(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	h, ok := e.Payload.(events.StreamHold)
	if !ok {
		return
	}
	r.onStreamHold(h)
}

func (p *Plugin) handleStreamRetract(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	rt, ok := e.Payload.(events.StreamRetract)
	if !ok {
		return
	}
	r.onStreamRetract(rt)
}

func (p *Plugin) handleStreamEnd(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	end, ok := e.Payload.(events.StreamEnd)
	if !ok {
		return
	}
	r.onStreamEnd(end)
}

func (p *Plugin) handleOutput(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	o, ok := e.Payload.(events.AgentOutput)
	if !ok {
		return
	}
	if !p.runAcceptsTurnEvent(r, o.TurnID) {
		p.logger.Debug("agui dropping output from a turn this run does not carry",
			"turn_id", o.TurnID,
			"run_turn_id", r.boundTurn(),
			"thread_id", r.threadID,
			"run_id", r.runID,
		)
		return
	}
	r.onOutput(o)
}

// handleToolInvoke translates a tool.invoke into the ToolCallStart/Args/End
// sequence on the AG-UI stream. This fires for every real agent tool call
// (fixing the earlier tool.call vs tool.invoke mismatch, which meant no
// ToolCall* events came from real turns).
//
// When the invoked tool is a client-executed (frontend) tool advertised for
// this run via RunAgentInput.tools, there is no in-process handler to produce a
// tool.result — the CLIENT runs it. So after emitting the ToolCall* sequence the
// plugin suspends the run interrupt-style (E2-S1 machinery) and records a pending
// client-tool entry. The client's resume carries the ToolCallResult, which the
// resume path feeds back as the tool.result the parked agent is waiting on. A
// server-side Nexus catalog tool is left untouched: its own handler runs inline
// and produces the tool.result that streams via handleToolResult.
func (p *Plugin) handleToolInvoke(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	tc, ok := e.Payload.(events.ToolCall)
	if !ok {
		return
	}
	// A tool call belonging to a turn this run does not carry must not be
	// rendered onto it, and must certainly not SUSPEND it: a client-executed
	// tool invoked by the previous turn would park a run that had asked for
	// nothing, awaiting a result its client has no call to produce.
	if !p.runAcceptsTurnEvent(r, tc.TurnID) {
		p.logger.Debug("agui dropping tool invoke from a turn this run does not carry",
			"turn_id", tc.TurnID,
			"run_turn_id", r.boundTurn(),
			"tool", tc.Name,
			"run_id", r.runID,
		)
		return
	}
	// Internal sub-calls (dispatched by another tool, e.g. run_code) are not part
	// of the agent's own tool loop and must not suspend the run.
	if tc.ParentCallID != "" {
		r.onToolCall(tc)
		return
	}

	// Emit the ToolCallStart/Args/End sequence for the call regardless of origin.
	r.onToolCall(tc)

	// If this is a client-executed tool for the active run, suspend awaiting the
	// client's result. Otherwise the in-process tool handler produces the result.
	if !p.isClientTool(r, tc.Name) {
		return
	}
	p.suspendForClientTool(r, tc)
}

// handleLLMResponse records the iteration's provider continuity metadata on the
// active run, so the tool calls and text that follow can carry it to the client.
//
// It translates nothing and emits nothing. The plugin is a transport, and this
// is the one bus event it reads purely as state: AG-UI replays history from the
// client, so a token the provider will demand back on the next request has to
// leave through this stream or be lost.
func (p *Plugin) handleLLMResponse(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	resp, ok := e.Payload.(events.LLMResponse)
	if !ok {
		return
	}
	meta := roundtrip.ForwardMessageMetadata(resp.Metadata)
	r.mu.Lock()
	r.turnMeta = meta
	r.mu.Unlock()
}

func (p *Plugin) handleToolResult(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	tr, ok := e.Payload.(events.ToolResult)
	if !ok {
		return
	}
	if !p.runAcceptsTurnEvent(r, tr.TurnID) {
		p.logger.Debug("agui dropping tool result from a turn this run does not carry",
			"turn_id", tr.TurnID,
			"run_turn_id", r.boundTurn(),
			"tool", tr.Name,
			"run_id", r.runID,
		)
		return
	}
	r.onToolResult(tr)
}

func (p *Plugin) handleThinkingStep(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	s, ok := e.Payload.(events.ThinkingStep)
	if !ok {
		return
	}
	r.onThinkingStep(s)
}

// parseCORSOrigins normalizes the cors_origins config value into a slice of
// trimmed, non-empty origins. It accepts a YAML list ([]any of strings) or a
// single comma-separated string for convenience.
func parseCORSOrigins(raw any) []string {
	var out []string
	switch v := raw.(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
	case []string:
		for _, s := range v {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	case string:
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}
