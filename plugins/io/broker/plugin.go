// Package broker implements the nexus.io.broker plugin: a dial-back IO
// transport for spawned Nexus instances managed by the session broker
// (cmd/nexus-broker).
//
// Unlike nexus.io.browser and nexus.io.realtime — which LISTEN for inbound
// connections — this plugin DIALS OUT to the broker's instance gateway. The
// broker is the only listening socket; each spawned instance reaches back to
// it. On boot the plugin reads its broker address and lease id from config
// (falling back to the NEXUS_BROKER_ADDR / NEXUS_BROKER_LEASE_ID environment
// variables the broker injects at spawn), dials the gateway, sends a register
// frame keyed by the lease id, announces readiness, and reports the engine's
// session id back so the broker can persist it for later -recall resume.
//
// Thereafter it bridges the engine IO event bus to broker frames in both
// directions — engine output events (io.output, llm.stream.chunk, etc.)
// become outbound SignalIO frames; inbound SignalIO frames become io.input
// (and friends) on the bus — the same role the listener transports play for
// their connections. See docs/src/configuration/reference.md for the
// nexus.io.broker entry.
package broker

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/brokerframe"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/nexusheaders"
)

const pluginID = "nexus.io.broker"

// Plugin is the broker dial-back IO plugin. It bridges a small set of engine
// IO events to broker frames over a single outbound WebSocket.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger
	client *client

	brokerAddr string
	leaseID    string
	sessionID  string

	// session is held so a turn's forwarded X-Nexus-* request headers can be
	// bound into the reserved "_header.*" labels, the read seam for a plugin
	// that never sees io.input. Nil when the engine runs without a session;
	// every use is guarded.
	session *engine.SessionWorkspace

	// clientMu guards clientHeaders and clientHeadersKnown, written from the
	// read pump and read by the input path.
	clientMu sync.Mutex
	// clientHeaders are the X-Nexus-* request headers of the client connection
	// currently attached at the broker, as last reported by a `client.headers`
	// payload. clientHeadersKnown distinguishes "the broker has told us, and
	// the answer was none" from "the broker has never told us" — the second is
	// the A2A path, where the headers ride on the input message itself.
	clientHeaders      map[string]string
	clientHeadersKnown bool
	// clientPrincipalID is the identity the broker's own credential validator
	// resolved for that connection, announced alongside the headers. Empty when
	// the broker runs with authentication disabled, which is the honest answer:
	// there is then no verified identity to report.
	clientPrincipalID string

	// spawnSecret is the per-spawn second factor the broker handed this process
	// at exec. It is echoed in the register frame and is NEVER logged: unlike
	// brokerAddr and leaseID (both of which appear in the init record below and
	// on the broker's own surfaces), this value's only job is to be unguessable.
	spawnSecret string

	unsubs []func()
}

// New creates a new broker IO plugin. Used by the plugin registry.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "Broker IO" }
func (p *Plugin) Version() string                   { return "0.1.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

// Subscriptions declares the engine output events forwarded to the broker as
// outbound IO frames. Mirrors the core transport surface of io/browser and
// io/realtime so parity holds (see .claude/docs/io-transport.md).
func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "io.output", Priority: 50},
		{EventType: "llm.stream.chunk", Priority: 50},
		{EventType: "llm.stream.end", Priority: 50},
		{EventType: "io.status", Priority: 50},
		{EventType: "io.approval.request", Priority: 50},
		{EventType: "hitl.requested", Priority: 50},
		{EventType: "cancel.complete", Priority: 50},
		{EventType: "core.ready", Priority: 50},
	}
}

// Emissions declares the bus events injected from inbound broker IO frames,
// plus io.session.end which the plugin emits when the broker requests a
// graceful shutdown of this instance.
func (p *Plugin) Emissions() []string {
	return []string{
		"io.input",
		"before:io.input",
		"io.approval.response",
		"hitl.responded",
		"cancel.request",
		"io.session.end",
	}
}

// Init reads config/env and wires the bus handlers. It constructs (but does
// not dial) the client — dialing happens in handleCoreReady, gated on the
// engine-wide "core.ready" event, so the engine is fully up before the
// broker is told the instance is ready. See handleCoreReady's own doc
// comment for why this plugin's own Ready() is not where that dial happens.
func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	// Config takes precedence; fall back to the broker-injected env vars.
	p.brokerAddr = configString(ctx.Config, "broker_addr")
	if p.brokerAddr == "" {
		p.brokerAddr = os.Getenv(brokerframe.EnvBrokerAddr)
	}
	p.leaseID = configString(ctx.Config, "lease_id")
	if p.leaseID == "" {
		p.leaseID = os.Getenv(brokerframe.EnvLeaseID)
	}
	// Same config-then-env shape as the two above, so all three spawn inputs are
	// resolved one way. Absent is a supported state: an unauthenticated broker
	// does not check the secret, so a missing value must produce an empty
	// register field rather than a boot failure.
	p.spawnSecret = configString(ctx.Config, "spawn_secret")
	if p.spawnSecret == "" {
		p.spawnSecret = os.Getenv(brokerframe.EnvSpawnSecret)
	}
	if ctx.Session != nil {
		p.sessionID = ctx.Session.ID
		p.session = ctx.Session
	}

	p.client = newClient(p.logger, clientConfig{
		addr:        p.brokerAddr,
		leaseID:     p.leaseID,
		spawnSecret: p.spawnSecret,
		sessionID:   p.sessionID,
	}, p.handleInbound, p.handleShutdown)

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("io.output", p.handleOutput, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.stream.chunk", p.handleStreamChunk, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.stream.end", p.handleStreamEnd, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.status", p.handleStatus, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.approval.request", p.handleApprovalRequest, engine.WithSource(pluginID)),
		p.bus.Subscribe("hitl.requested", p.handleHITLRequest, engine.WithSource(pluginID)),
		p.bus.Subscribe("cancel.complete", p.handleCancelComplete, engine.WithSource(pluginID)),
		p.bus.Subscribe("core.ready", p.handleCoreReady, engine.WithSource(pluginID)),
	)

	// spawn_secret_present records WHETHER a secret was resolved, never its
	// value. The boolean is worth a field: if the broker refuses this instance's
	// registration, the first question is whether the secret ever reached the
	// process, and that is answerable from this line alone.
	p.logger.Info("broker IO plugin initialized",
		"broker_addr", p.brokerAddr, "lease_id", p.leaseID, "session_id", p.sessionID,
		"spawn_secret_present", p.spawnSecret != "")
	return nil
}

// Ready is a no-op. Dialing the broker and starting the reconnect loop is
// deferred to handleCoreReady, gated on the engine-wide "core.ready" event
// (pkg/engine/lifecycle.go's own Boot: emitted once EVERY plugin's own
// Ready() has returned) rather than done directly here — see
// handleCoreReady's own doc comment for why. Kept as a real method (not
// removed) only because engine.Plugin requires one.
func (p *Plugin) Ready() error {
	return nil
}

// handleCoreReady starts the broker dial-back once the whole engine has
// finished its Ready phase. lifecycle.go's own Boot runs every plugin's
// Ready() IN PARALLEL ("Ready phase (parallel, per PRD 4.5)") with no
// ordering guarantee between this plugin and any other — in particular
// nexus.io.agui, whose own Ready() calls Start(), which returns once its
// listener's net.Listen has succeeded but BEFORE its Serve goroutine has
// necessarily reached its first Accept. Dialing directly from THIS plugin's
// old Ready() raced that: the broker dial-back is a fast, synchronous
// loopback round trip, so it routinely completed — and told cmd/nexus-broker
// "ready to accept IO" — before nexus.io.agui's own listener had begun
// serving, sometimes by tens of milliseconds under a real engine boot (21
// plugins, confirmed by direct reproduction), not the sub-millisecond gap
// the original "Announce readiness to accept IO" design assumed. A caller
// that reverse-proxies the instant the broker's own /claim answers can land
// squarely in that window — indistinguishable from a dead instance to the
// caller, since the connection is refused or reset. Deferring to
// "core.ready" (emitted only after readyWg.Wait() — i.e., after every
// plugin's Ready() has returned, io.agui's included) closes that window down
// to the same narrow, sub-millisecond gap oneshot.Ready()'s own doc comment
// already documents for the identical class of race against
// mcp.client.Ready().
//
// The dormant-when-unconfigured guard moves here unchanged from the old
// Ready() — a broker-less config (unit/contract tests, or a config that
// activates this plugin without broker wiring) still logs the identical WARN
// and never dials, just later than before.
func (p *Plugin) handleCoreReady(engine.Event[any]) {
	if p.brokerAddr == "" {
		p.logger.Warn("broker IO plugin has no broker_addr; staying dormant")
		return
	}
	if p.leaseID == "" {
		p.logger.Warn("broker IO plugin has no lease_id; staying dormant")
		return
	}
	p.client.Start()
}

// Shutdown unsubscribes and closes the dial-back connection cleanly.
func (p *Plugin) Shutdown(ctx context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	if p.client == nil {
		return nil
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.client.Stop(stopCtx)
	return nil
}

// --- outbound (bus -> broker frames) ---

func (p *Plugin) handleOutput(e engine.Event[any]) {
	out, ok := e.Payload.(events.AgentOutput)
	if !ok {
		return
	}
	// Streamed output already reached the client via stream.delta frames.
	if streamed, _ := out.Metadata["streamed"].(bool); streamed {
		return
	}
	p.client.SendIO(ioMessage{
		Type:    "output",
		Content: out.Content,
		Role:    out.Role,
		TurnID:  out.TurnID,
	})
}

func (p *Plugin) handleStreamChunk(e engine.Event[any]) {
	chunk, ok := e.Payload.(events.StreamChunk)
	if !ok || chunk.Content == "" {
		return
	}
	p.client.SendIO(ioMessage{
		Type:    "stream.delta",
		Content: chunk.Content,
		TurnID:  chunk.TurnID,
	})
}

func (p *Plugin) handleStreamEnd(e engine.Event[any]) {
	end, ok := e.Payload.(events.StreamEnd)
	if !ok {
		return
	}
	p.client.SendIO(ioMessage{
		Type:         "stream.end",
		TurnID:       end.TurnID,
		FinishReason: end.FinishReason,
	})
}

func (p *Plugin) handleStatus(e engine.Event[any]) {
	status, ok := e.Payload.(events.StatusUpdate)
	if !ok {
		return
	}
	p.client.SendIO(ioMessage{
		Type:   "status",
		State:  status.State,
		Detail: status.Detail,
	})
}

func (p *Plugin) handleApprovalRequest(e engine.Event[any]) {
	req, ok := e.Payload.(events.ApprovalRequest)
	if !ok {
		return
	}
	p.client.SendIO(ioMessage{
		Type:        "approval.request",
		PromptID:    req.PromptID,
		Description: req.Description,
		ToolCall:    req.ToolCall,
		Risk:        req.Risk,
	})
}

func (p *Plugin) handleHITLRequest(e engine.Event[any]) {
	req, ok := e.Payload.(events.HITLRequest)
	if !ok {
		return
	}
	// Mode and Choices ride along for the same reason nexus.io.browser sends
	// them: a responder answers a multiple-choice question with a choice id, and
	// a payload carrying only the prompt gives it no way to know what the ids
	// are. Mode is normalized here rather than downstream so every consumer sees
	// one spelling of the default.
	mode := string(req.Mode)
	if mode == "" {
		mode = string(events.HITLModeFreeText)
	}
	choices := make([]ioChoice, 0, len(req.Choices))
	for _, c := range req.Choices {
		choices = append(choices, ioChoice{ID: c.ID, Label: c.Label})
	}
	if len(choices) == 0 {
		// nil rather than an empty slice, so omitempty keeps a free-text
		// question's frame byte-identical to what it was before choices existed.
		choices = nil
	}
	p.client.SendIO(ioMessage{
		Type:      "hitl.request",
		RequestID: req.ID,
		Prompt:    req.Prompt,
		Mode:      mode,
		Choices:   choices,
		TurnID:    req.TurnID,
	})
}

func (p *Plugin) handleCancelComplete(e engine.Event[any]) {
	cc, ok := e.Payload.(events.CancelComplete)
	if !ok {
		return
	}
	resumable := cc.Resumable
	p.client.SendIO(ioMessage{
		Type:      "cancel.complete",
		TurnID:    cc.TurnID,
		Resumable: &resumable,
	})
}

// inputHeaders resolves which X-Nexus-* headers an inbound `input` belongs to,
// by merging the two sources a turn can have.
//
// # The two sources, and why neither alone is enough
//
// A client connection's headers are fixed at the WebSocket handshake — that is
// the only request a WebSocket has — and the broker announces them ahead of
// every IO frame. That suits the values describing the CONVERSATION: the
// tenant, the caller's subject, the locale. It cannot carry a value that
// changes from one turn to the next, because there is no new handshake to put
// one on.
//
// So a client may also attach headers to the `input` payload itself, which is
// per-turn by construction. Those fill in AROUND the connection's rather than
// replacing them.
//
// # Connection-scoped wins, per key
//
// Where both name the same header the announcement wins, and that is the
// security-relevant half of this function rather than a tie break. The
// announcement comes from the HTTP handshake — the hop an operator's reverse
// proxy controls and injects into — while the message field is whatever the
// client typed on a pipe the broker forwards verbatim and cannot filter. If
// the message won, anything a trusted proxy asserted about a caller could be
// overridden by that caller.
//
// The rule this gives an integrator is simple: put fixed values on the
// handshake, varying values on the turn, and never the same key on both. A
// value that must change per turn simply must not be pinned on the handshake.
//
// Identity is NOT merged — see inputPrincipalID, which keeps the announcement
// outright. A verified identity has no per-turn variant worth the risk of a
// client being able to contribute one.
//
// With no announcement ever made the message's own field stands entirely: that
// is the A2A path, where the broker built the payload from the request itself,
// so the field is the broker's and not a client's.
func (p *Plugin) inputHeaders(msg ioMessage) map[string]string {
	p.clientMu.Lock()
	connection := p.clientHeaders
	known := p.clientHeadersKnown
	p.clientMu.Unlock()

	if !known {
		return msg.Headers
	}
	// Client-authored, so it gets the same normalization and the same bounds
	// the handshake path already applies. Without this a caller could name a
	// header anything, or send a megabyte of them, on a path that bypasses
	// nexusheaders.Extract entirely.
	perTurn := nexusheaders.Sanitize(msg.Headers)
	if len(perTurn) == 0 {
		return connection
	}
	if len(connection) == 0 {
		return perTurn
	}
	out := make(map[string]string, len(perTurn)+len(connection))
	for k, v := range perTurn {
		out[k] = v
	}
	for k, v := range connection {
		out[k] = v // last write wins: the handshake is authoritative
	}
	return out
}

// inputPrincipalID resolves the verified identity an inbound `input` belongs
// to, by the same precedence inputHeaders applies and for a sharper reason.
//
// On the opaque client-stream pipe a client authors its own `input` payload, so
// it could set principal_id on it. An announcement therefore wins outright
// where one has been made: it is broker-originated, and the broker only ever
// emits it from the principal its own validator resolved. Without that rule a
// caller could name itself anyone simply by typing the field.
//
// With no announcement ever made — the A2A path, where the broker builds the
// payload from the request itself — the message's own field is the broker's,
// not a client's, and stands.
func (p *Plugin) inputPrincipalID(msg ioMessage) string {
	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	if p.clientHeadersKnown {
		return p.clientPrincipalID
	}
	return msg.PrincipalID
}

// --- inbound (broker frames -> bus) ---
//
// handleInbound runs on the client's read pump goroutine. Bus dispatch is
// synchronous, so io.input (which may drive an agent loop that blocks on a
// HITL response) is offloaded to a goroutine to avoid deadlocking the read
// pump — mirroring io/browser and io/realtime.
func (p *Plugin) handleInbound(msg ioMessage) {
	switch msg.Type {
	case "client.headers":
		// Broker-originated: it reports the X-Nexus-* headers of the client
		// connection whose frames follow. Recorded rather than acted on; the
		// next input is what consumes it. An absent map clears the record,
		// which is how a new client connection that sends no header avoids
		// inheriting the previous one's values.
		p.clientMu.Lock()
		p.clientHeaders = msg.Headers
		p.clientHeadersKnown = true
		p.clientPrincipalID = msg.PrincipalID
		p.clientMu.Unlock()

	case "input":
		go func(content string, headers map[string]string, principalID string) {
			// The headers are the broker's report of the client HTTP request
			// it translated — this process never saw that request. Bound
			// before the emit so a plugin handling io.input cannot observe
			// the turn ahead of its context; a message the broker forwarded
			// without headers clears the namespace rather than leaving the
			// previous turn's values standing.
			if p.session != nil {
				if err := p.session.SetRequestHeaders(headers); err != nil {
					p.logger.Warn("binding X-Nexus-* request headers failed", "error", err)
				}
				// Bound alongside the headers and never conditionally: a turn
				// whose request carried no verified identity must CLEAR the
				// previous turn's, or a plugin gating on _principal_id would
				// read the last caller's identity as this one's — the one
				// failure here that is worse than having no identity at all.
				if err := p.session.SetPrincipalID(principalID); err != nil {
					p.logger.Warn("binding _principal_id session label failed", "error", err)
				}
			}
			input := events.UserInput{
				SchemaVersion: events.UserInputVersion,
				Content:       content,
				Headers:       headers,
			}
			if veto, err := p.bus.EmitVetoable("before:io.input", &input); err == nil && veto.Vetoed {
				return
			}
			_ = p.bus.Emit("io.input", input)
		}(msg.Content, p.inputHeaders(msg), p.inputPrincipalID(msg))

	case "approval.response":
		_ = p.bus.Emit("io.approval.response", events.ApprovalResponse{
			SchemaVersion: events.ApprovalResponseVersion,
			PromptID:      msg.PromptID,
			Approved:      msg.Approved,
			Always:        msg.Always,
		})

	case "hitl.response":
		_ = p.bus.Emit("hitl.responded", events.HITLResponse{
			SchemaVersion: events.HITLResponseVersion,
			RequestID:     msg.RequestID,
			ChoiceID:      msg.ChoiceID,
			FreeText:      msg.FreeText,
		})

	case "cancel":
		_ = p.bus.Emit("cancel.request", events.CancelRequest{
			SchemaVersion: events.CancelRequestVersion,
			TurnID:        msg.TurnID,
			Source:        "broker",
		})

	default:
		p.logger.Debug("unknown inbound broker io message", "type", msg.Type)
	}
}

// handleShutdown runs when the broker sends a SignalShutdown frame. It emits
// io.session.end on the bus, which the engine's run loop observes to begin a
// graceful Stop — flushing and persisting the session before the process
// exits. It does NOT hard-exit mid-write; the engine owns teardown ordering.
// The broker bounds how long it waits for the process to exit and force-kills
// it if this graceful path overruns.
func (p *Plugin) handleShutdown() {
	p.logger.Info("broker requested instance shutdown; ending session",
		"lease_id", p.leaseID, "session_id", p.sessionID)
	_ = p.bus.Emit("io.session.end", events.SessionInfo{
		SchemaVersion: events.SessionInfoVersion,
		ID:            p.sessionID,
		Transport:     "broker",
	})
}

// configString reads a string config key, returning "" when absent/empty.
func configString(cfg map[string]any, key string) string {
	if v, ok := cfg[key].(string); ok {
		return v
	}
	return ""
}
