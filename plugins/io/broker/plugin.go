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
	"time"

	"github.com/frankbardon/nexus/pkg/brokerframe"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
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
	}
}

// Emissions declares the bus events injected from inbound broker IO frames.
func (p *Plugin) Emissions() []string {
	return []string{
		"io.input",
		"before:io.input",
		"io.approval.response",
		"hitl.responded",
		"cancel.request",
	}
}

// Init reads config/env and wires the bus handlers. It constructs (but does
// not dial) the client — dialing happens in Ready so the engine is fully up
// before the broker is told the instance is ready.
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
	if ctx.Session != nil {
		p.sessionID = ctx.Session.ID
	}

	p.client = newClient(p.logger, p.brokerAddr, p.leaseID, p.sessionID, p.handleInbound)

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("io.output", p.handleOutput, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.stream.chunk", p.handleStreamChunk, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.stream.end", p.handleStreamEnd, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.status", p.handleStatus, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.approval.request", p.handleApprovalRequest, engine.WithSource(pluginID)),
		p.bus.Subscribe("hitl.requested", p.handleHITLRequest, engine.WithSource(pluginID)),
		p.bus.Subscribe("cancel.complete", p.handleCancelComplete, engine.WithSource(pluginID)),
	)

	p.logger.Info("broker IO plugin initialized",
		"broker_addr", p.brokerAddr, "lease_id", p.leaseID, "session_id", p.sessionID)
	return nil
}

// Ready dials the broker and starts the reconnect loop. When no broker
// address is configured (e.g. unit/contract tests, or a config that activates
// the plugin without broker wiring) it stays dormant rather than erroring so
// the engine still boots cleanly.
func (p *Plugin) Ready() error {
	if p.brokerAddr == "" {
		p.logger.Warn("broker IO plugin has no broker_addr; staying dormant")
		return nil
	}
	if p.leaseID == "" {
		p.logger.Warn("broker IO plugin has no lease_id; staying dormant")
		return nil
	}
	p.client.Start()
	return nil
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
	p.client.SendIO(ioMessage{
		Type:      "hitl.request",
		RequestID: req.ID,
		Prompt:    req.Prompt,
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

// --- inbound (broker frames -> bus) ---
//
// handleInbound runs on the client's read pump goroutine. Bus dispatch is
// synchronous, so io.input (which may drive an agent loop that blocks on a
// HITL response) is offloaded to a goroutine to avoid deadlocking the read
// pump — mirroring io/browser and io/realtime.
func (p *Plugin) handleInbound(msg ioMessage) {
	switch msg.Type {
	case "input":
		go func(content string) {
			input := events.UserInput{SchemaVersion: events.UserInputVersion, Content: content}
			if veto, err := p.bus.EmitVetoable("before:io.input", &input); err == nil && veto.Vetoed {
				return
			}
			_ = p.bus.Emit("io.input", input)
		}(msg.Content)

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

// configString reads a string config key, returning "" when absent/empty.
func configString(cfg map[string]any, key string) string {
	if v, ok := cfg[key].(string); ok {
		return v
	}
	return ""
}
