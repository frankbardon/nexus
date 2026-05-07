// Package realtime implements the nexus.io.realtime plugin: a WebSocket
// transport that streams LLM token deltas, tool previews, voice audio, and
// cancellation envelopes to connected clients, and accepts text input,
// audio chunks, cancel, and HITL responses going the other way.
//
// This is Phase 3 of Idea 18 (multimodal & voice IO). Phase 4 will land
// nexus.io.voice (VAD + ASR + TTS); Phase 5 may eventually fold the
// envelope shape into io/browser and io/wails. Until then the realtime
// transport stands alongside those plugins as a third, lower-latency
// option for clients that want raw stream.delta deltas without the
// browser UI's hub state machine.
//
// Security: there is **no auth in v1**. Origin checks and bearer-token
// validation are tracked as follow-ups; operators running this on a
// public network must front it with a reverse proxy that does its own
// authentication. See docs/src/configuration/reference.md for the
// nexus.io.realtime entry.
package realtime

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.io.realtime"

// Plugin is the realtime IO plugin. It hosts a WebSocket server and
// bridges a small set of bus events into JSON envelopes for clients.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger
	server *Server

	listenAddr string
	path       string
	maxClients int

	unsubs []func()
}

// New creates a new realtime IO plugin. Used by the plugin registry.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "Realtime IO" }
func (p *Plugin) Version() string                   { return "0.1.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

// Subscriptions declares the events forwarded to clients. We attach a
// vetoable subscription for before:tool.invoke for read-only observation —
// the handler never sets Veto. The remainder are post-event observers.
func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "llm.stream.chunk", Priority: 50},
		{EventType: "llm.stream.end", Priority: 50},
		{EventType: "before:tool.invoke", Priority: 100}, // read-only, low priority
		{EventType: "voice.audio.output.chunk", Priority: 50},
		{EventType: "cancel.complete", Priority: 50},
		{EventType: "hitl.requested", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"io.input",
		"before:io.input",
		"voice.audio.input.chunk",
		"cancel.request",
		"hitl.responded",
	}
}

// Init reads config and constructs (but does not start) the server.
func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	p.listenAddr = ":7676"
	if v, ok := ctx.Config["listen_addr"].(string); ok && v != "" {
		p.listenAddr = v
	}
	p.path = "/ws"
	if v, ok := ctx.Config["path"].(string); ok && v != "" {
		p.path = v
	}
	p.maxClients = 16
	if v, ok := ctx.Config["max_clients"].(int); ok && v > 0 {
		p.maxClients = v
	}

	p.server = NewServer(p.logger, p.listenAddr, p.path, p.maxClients, p.handleInbound)

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("llm.stream.chunk", p.handleStreamChunk, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.stream.end", p.handleStreamEnd, engine.WithSource(pluginID)),
		p.bus.Subscribe("before:tool.invoke", p.handleBeforeToolInvoke,
			engine.WithSource(pluginID), engine.WithPriority(100)),
		p.bus.Subscribe("voice.audio.output.chunk", p.handleVoiceAudioOutput, engine.WithSource(pluginID)),
		p.bus.Subscribe("cancel.complete", p.handleCancelComplete, engine.WithSource(pluginID)),
		p.bus.Subscribe("hitl.requested", p.handleHITLRequest, engine.WithSource(pluginID)),
	)

	p.logger.Info("realtime IO plugin initialized",
		"listen_addr", p.listenAddr, "path", p.path, "max_clients", p.maxClients)
	return nil
}

// Ready starts the HTTP server.
func (p *Plugin) Ready() error {
	if err := p.server.Start(); err != nil {
		return fmt.Errorf("starting realtime server: %w", err)
	}
	return nil
}

// Shutdown unsubscribes and closes all client connections.
func (p *Plugin) Shutdown(ctx context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	if p.server == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.server.Shutdown(shutdownCtx)
}

// --- outbound (bus -> clients) ---

func (p *Plugin) handleStreamChunk(e engine.Event[any]) {
	chunk, ok := e.Payload.(events.StreamChunk)
	if !ok || chunk.Content == "" {
		return
	}
	p.server.Broadcast(envelope{
		Type:    "stream.delta",
		TurnID:  chunk.TurnID,
		Content: chunk.Content,
	})
}

func (p *Plugin) handleStreamEnd(e engine.Event[any]) {
	end, ok := e.Payload.(events.StreamEnd)
	if !ok {
		return
	}
	p.server.Broadcast(envelope{
		Type:         "stream.end",
		TurnID:       end.TurnID,
		FinishReason: end.FinishReason,
	})
}

// handleBeforeToolInvoke is a read-only observer: it inspects the wrapped
// ToolCall, forwards a tool.preview envelope to clients, and never sets
// Veto. Subscribing to before:* (rather than tool.invoke) lets clients see
// a tool call before any potential gate veto suppresses it.
func (p *Plugin) handleBeforeToolInvoke(e engine.Event[any]) {
	vp, ok := e.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	tc, ok := vp.Original.(*events.ToolCall)
	if !ok {
		return
	}
	p.server.Broadcast(envelope{
		Type: "tool.preview",
		ID:   tc.ID,
		Name: tc.Name,
		Args: tc.Arguments,
	})
}

func (p *Plugin) handleVoiceAudioOutput(e engine.Event[any]) {
	chunk, ok := e.Payload.(events.VoiceAudioOutputChunk)
	if !ok {
		return
	}
	p.server.Broadcast(envelope{
		Type:        "audio.chunk",
		TurnID:      chunk.TurnID,
		Sequence:    chunk.Sequence,
		AudioBase64: chunk.AudioBase64,
		MimeType:    chunk.MimeType,
		Final:       chunk.Final,
	})
}

func (p *Plugin) handleCancelComplete(e engine.Event[any]) {
	cc, ok := e.Payload.(events.CancelComplete)
	if !ok {
		return
	}
	p.server.Broadcast(envelope{
		Type:      "cancel.complete",
		TurnID:    cc.TurnID,
		Resumable: &cc.Resumable,
	})
}

func (p *Plugin) handleHITLRequest(e engine.Event[any]) {
	req, ok := e.Payload.(events.HITLRequest)
	if !ok {
		return
	}
	p.server.Broadcast(envelope{
		Type:   "hitl.request",
		ID:     req.ID,
		Prompt: req.Prompt,
	})
}

// --- inbound (clients -> bus) ---
//
// handleInbound runs on the server's read pump goroutine. The bus dispatch
// is synchronous, so any handler chain that might block (e.g. the HITL
// path, where another plugin waits on the response) must not run inline:
// we'd deadlock the read pump and never see further frames from the same
// client. Following io/browser's pattern, we offload to a goroutine for
// io.input; the lighter cancel/audio/approval emits are short-circuit
// dispatches that complete quickly enough to ride the read pump.
func (p *Plugin) handleInbound(env envelope) {
	switch env.Type {
	case "input":
		go func(content string) {
			input := events.UserInput{
				SchemaVersion: events.UserInputVersion,
				Content:       content,
			}
			if veto, err := p.bus.EmitVetoable("before:io.input", &input); err == nil && veto.Vetoed {
				return
			}
			_ = p.bus.Emit("io.input", input)
		}(env.Content)

	case "audio.chunk":
		_ = p.bus.Emit("voice.audio.input.chunk", events.VoiceAudioInputChunk{
			SchemaVersion: events.VoiceAudioInputChunkVersion,
			TurnID:        env.TurnID,
			Sequence:      env.Sequence,
			AudioBase64:   env.AudioBase64,
			MimeType:      env.MimeType,
			Final:         env.Final,
		})

	case "cancel":
		_ = p.bus.Emit("cancel.request", events.CancelRequest{
			SchemaVersion: events.CancelRequestVersion,
			TurnID:        env.TurnID,
			Source:        "realtime",
		})

	case "approval":
		// Map the realtime "approval" envelope onto hitl.responded — the
		// modern unified human-in-the-loop path. ChoiceID carries the
		// decision verbatim ("approve" / "reject" / arbitrary plugin id);
		// we deliberately do not interpret it here.
		_ = p.bus.Emit("hitl.responded", events.HITLResponse{
			SchemaVersion: events.HITLResponseVersion,
			RequestID:     env.RequestID,
			ChoiceID:      env.Decision,
		})

	default:
		p.logger.Debug("unknown realtime envelope type", "type", env.Type)
	}
}
