package wails

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/ui"
)

const pluginID = "nexus.io.wails"

// Compile-time assertion that Plugin satisfies engine.Plugin.
var _ engine.Plugin = (*Plugin)(nil)

// Plugin is the Wails desktop-shell IO transport for Nexus.
//
// Scope and lifetime are intentionally different from nexus.io.browser:
// Wails is process-scoped, owned by the host Wails main.go. The host
// installs a Runtime implementation via SetRuntime before calling
// engine.Boot, and tears down via engine.Stop from OnShutdown.
//
// The plugin supports two modes:
//
//   - Legacy (no subscribe/accept config): hardcoded chat-event
//     subscriptions with typed handlers. Backward compatible with
//     existing configs that don't set these keys.
//   - Config-driven (subscribe/accept lists in YAML): generic
//     passthrough bridging for arbitrary domain events. The developer
//     controls exactly which events cross the bus-to-frontend boundary.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	adapter *Adapter
	hub     *Hub
	unsubs  []func()

	// Config-driven event bridging. When configDriven is true, the
	// plugin uses subscribeList/acceptList instead of the hardcoded
	// chat-event handler set.
	configDriven  bool
	subscribeList []string
	acceptList    []string
}

// New creates a new Wails IO plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "Wails IO" }
func (p *Plugin) Version() string                   { return "0.1.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

// Hub exposes the plugin's Hub so the embedder can call SetRuntime on it
// before engine.Boot. This is the seam the build plan calls out:
// "accept the Wails runtime context via a setter the embedder calls
// before Boot".
//
// Usage from the Wails app's main.go:
//
//	p := wailsplugin.New().(*wailsplugin.Plugin)
//	eng.Registry.Register("nexus.io.wails", func() engine.Plugin { return p })
//	// inside OnStartup(ctx):
//	p.Hub().SetRuntime(myWailsRuntimeAdapter{ctx: ctx})
//	eng.Boot(ctx)
//
// The hub is created lazily in Init because Init is where the logger
// becomes available; the embedder must therefore call Init (by booting
// the lifecycle) or register a pre-built plugin instance.
func (p *Plugin) Hub() *Hub {
	if p.hub == nil {
		// Allow the embedder to grab the hub before Init runs. We
		// create it with a no-op logger that will be replaced in Init.
		p.hub = NewHub(slog.Default())
	}
	return p.hub
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	// Parity with nexus.io.browser: every in-session event the browser
	// plugin renders also needs to reach the Wails webview. Per the
	// parity rule in CLAUDE.md §7a, in-session UX changes must land in
	// both plugins — this list tracks that of browser.Plugin.Subscriptions
	// one-for-one. Wrapper features (file dialogs, session list, etc.)
	// are emitted separately and live only in the wails plugin.
	return []engine.EventSubscription{
		{EventType: "io.output", Priority: 50},
		{EventType: "llm.stream.chunk", Priority: 50},
		{EventType: "llm.stream.end", Priority: 50},
		{EventType: "io.output.clear", Priority: 50},
		{EventType: "io.status", Priority: 50},
		{EventType: "io.approval.request", Priority: 50},
		{EventType: "hitl.requested", Priority: 50},
		{EventType: "thinking.step", Priority: 50},
		{EventType: "code.exec.stdout", Priority: 50},
		{EventType: "plan.approval.request", Priority: 50},
		{EventType: "plan.created", Priority: 50},
		{EventType: "agent.plan", Priority: 50},
		{EventType: "provider.fallback", Priority: 50},
		{EventType: "session.file.created", Priority: 50},
		{EventType: "session.file.updated", Priority: 50},
		{EventType: "io.history.replay", Priority: 50},
		{EventType: "cancel.complete", Priority: 50},

		// Wrapper features (CLAUDE.md §7a). nexus.io.browser does not
		// subscribe to these — a session-scoped browser transport has
		// no business popping OS-native dialogs.
		{EventType: "io.file.open.request", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"io.input",
		"before:io.input",
		"io.approval.response",
		"hitl.responded",
		"plan.approval.response",
		"io.session.start",
		"io.session.end",
		"cancel.request",
		"cancel.resume",
		"io.file.open.response",
	}
}

// Init creates the hub (if the embedder has not already done so via
// Hub()) and the adapter, then wires event bridging.
//
// When the plugin config contains "subscribe" and/or "accept" keys,
// the plugin operates in config-driven mode: only the listed events
// are bridged, using a generic passthrough handler. When those keys
// are absent, the plugin falls back to its legacy hardcoded chat-event
// handler set for backward compatibility.
func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	// Parse config-driven event lists.
	if subs, ok := ctx.Config["subscribe"].([]any); ok {
		p.configDriven = true
		for _, s := range subs {
			if str, ok := s.(string); ok {
				p.subscribeList = append(p.subscribeList, str)
			}
		}
	}
	if accs, ok := ctx.Config["accept"].([]any); ok {
		for _, a := range accs {
			if str, ok := a.(string); ok {
				p.acceptList = append(p.acceptList, str)
			}
		}
	}

	if p.hub == nil {
		p.hub = NewHub(p.logger)
	} else {
		// Replace the placeholder logger from the pre-Init Hub() call.
		p.hub.logger = p.logger
	}

	sessionID := ""
	if ctx.Session != nil {
		sessionID = ctx.Session.ID
	}
	p.adapter = NewAdapter(p.hub, sessionID, p.bus, p.acceptList)

	if p.configDriven {
		p.initConfigDriven()
	} else {
		p.initLegacy()
	}

	p.logger.Info("wails IO plugin initialized", "config_driven", p.configDriven)
	return nil
}

// initConfigDriven wires the generic passthrough bridge for
// config-driven mode. Each event in subscribeList gets a generic
// outbound handler; inbound events matching acceptList are handled
// by the adapter's default case.
func (p *Plugin) initConfigDriven() {
	for _, eventType := range p.subscribeList {
		et := eventType // capture for closure
		p.unsubs = append(p.unsubs,
			p.bus.Subscribe(et, func(e engine.Event[any]) {
				p.handleGenericOutbound(e)
			}, engine.WithSource(pluginID)),
		)
	}
}

// handleGenericOutbound serializes any event payload into a ui.Envelope
// and sends it to the frontend through the hub.
func (p *Plugin) handleGenericOutbound(e engine.Event[any]) {
	raw, err := json.Marshal(e.Payload)
	if err != nil {
		p.logger.Warn("generic outbound: marshal failed", "type", e.Type, "error", err)
		return
	}
	env := ui.Envelope{
		Type:      e.Type,
		ID:        fmt.Sprintf("env-gen-%s", e.ID),
		Timestamp: e.Timestamp,
		Payload:   raw,
	}
	if err := p.hub.BroadcastEnvelope(env); err != nil {
		p.logger.Warn("generic outbound: broadcast failed", "type", e.Type, "error", err)
	}
}

// initLegacy wires the hardcoded chat-event handlers for backward
// compatibility with configs that don't specify subscribe/accept lists.
func (p *Plugin) initLegacy() {
	// Inbound: webview -> engine bus.
	//
	// As with the browser plugin, we emit io.input from a goroutine
	// because the event bus dispatches synchronously and the agent
	// loop may block waiting for a later ask-user response. Running
	// the emit on the Wails runtime callback goroutine would deadlock
	// if the callback chain later tried to send back to the webview.
	p.adapter.OnInput(func(msg ui.InputMessage) {
		if msg.Content == "/quit" || msg.Content == "/exit" {
			_ = p.bus.Emit("io.session.end", events.SessionInfo{
				Transport: "wails",
			})
			return
		}
		go func(content string) {
			input := events.UserInput{Content: content}
			if veto, err := p.bus.EmitVetoable("before:io.input", &input); err == nil && veto.Vetoed {
				return
			}
			_ = p.bus.Emit("io.input", input)
		}(msg.Content)
	})

	p.adapter.OnApprovalResponse(func(msg ui.ApprovalResponseMessage) {
		_ = p.bus.Emit("io.approval.response", events.ApprovalResponse{
			PromptID: msg.PromptID,
			Approved: msg.Approved,
			Always:   msg.Always,
		})
	})

	p.adapter.OnCancel(func() {
		_ = p.bus.Emit("cancel.request", events.CancelRequest{
			Source: "wails",
		})
	})

	p.adapter.OnResume(func() {
		_ = p.bus.Emit("cancel.resume", events.CancelResume{})
	})

	// Outbound: engine bus -> webview. Mirrors nexus.io.browser's
	// subscription set one-for-one (parity rule in CLAUDE.md §7a).
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("io.output", p.handleOutput, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.stream.chunk", p.handleStreamChunk, engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.stream.end", p.handleStreamEnd, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.output.clear", p.handleOutputClear, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.status", p.handleStatus, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.approval.request", p.handleApprovalRequest, engine.WithSource(pluginID)),
		p.bus.Subscribe("hitl.requested", p.handleHITLRequest, engine.WithSource(pluginID)),
		p.bus.Subscribe("thinking.step", p.handleThinkingStep, engine.WithSource(pluginID)),
		p.bus.Subscribe("code.exec.stdout", p.handleCodeExecStdout, engine.WithSource(pluginID)),
		p.bus.Subscribe("plan.approval.request", p.handlePlanApprovalRequest, engine.WithSource(pluginID)),
		p.bus.Subscribe("plan.created", p.handlePlanCreated, engine.WithSource(pluginID)),
		p.bus.Subscribe("agent.plan", p.handleAgentPlan, engine.WithSource(pluginID)),
		p.bus.Subscribe("provider.fallback", p.handleProviderFallback, engine.WithSource(pluginID)),
		p.bus.Subscribe("session.file.created", p.handleFileChanged, engine.WithSource(pluginID)),
		p.bus.Subscribe("session.file.updated", p.handleFileChanged, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.history.replay", p.handleHistoryReplay, engine.WithSource(pluginID)),
		p.bus.Subscribe("cancel.complete", p.handleCancelComplete, engine.WithSource(pluginID)),
		p.bus.Subscribe("io.file.open.request", p.handleFileOpenRequest, engine.WithSource(pluginID)),
	)
}

// Ready emits the session-start event. Unlike the browser plugin, there
// is no HTTP server to start here — the Wails host process already owns
// the webview, and the Runtime was installed by the embedder before Boot.
func (p *Plugin) Ready() error {
	_ = p.bus.Emit("io.session.start", events.SessionInfo{
		Transport: "wails",
	})
	return nil
}

// Shutdown unsubscribes from the bus. The webview itself is torn down
// by the Wails host process, not the plugin.
func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	p.unsubs = nil
	p.hub.Close()
	return nil
}

func (p *Plugin) handleOutputClear(_ engine.Event[any]) {
	_ = p.adapter.broadcast("output_clear", nil)
}

func (p *Plugin) handleProviderFallback(e engine.Event[any]) {
	fb, ok := e.Payload.(events.ProviderFallback)
	if !ok {
		return
	}
	_ = p.adapter.broadcast("provider_fallback", fb)
}

func (p *Plugin) handleOutput(e engine.Event[any]) {
	out, ok := e.Payload.(events.AgentOutput)
	if !ok {
		return
	}
	if streamed, _ := out.Metadata["streamed"].(bool); streamed {
		return
	}
	_ = p.adapter.SendOutput(ui.OutputMessage{
		Content:  out.Content,
		Role:     out.Role,
		Metadata: out.Metadata,
		TurnID:   out.TurnID,
	})
}

func (p *Plugin) handleStreamChunk(e engine.Event[any]) {
	chunk, ok := e.Payload.(events.StreamChunk)
	if !ok || chunk.Content == "" {
		return
	}
	_ = p.adapter.SendStreamChunk(ui.StreamChunkMessage{
		Content: chunk.Content,
		TurnID:  chunk.TurnID,
		Index:   chunk.Index,
	})
}

func (p *Plugin) handleStreamEnd(e engine.Event[any]) {
	end, ok := e.Payload.(events.StreamEnd)
	if !ok {
		return
	}
	_ = p.adapter.SendStreamEnd(ui.StreamEndMessage{
		TurnID: end.TurnID,
	})
}

func (p *Plugin) handleStatus(e engine.Event[any]) {
	status, ok := e.Payload.(events.StatusUpdate)
	if !ok {
		return
	}
	_ = p.adapter.SendStatus(ui.StatusMessage{
		State:  status.State,
		Detail: status.Detail,
		ToolID: status.ToolID,
	})
}

func (p *Plugin) handleApprovalRequest(e engine.Event[any]) {
	req, ok := e.Payload.(events.ApprovalRequest)
	if !ok {
		return
	}
	_, err := p.adapter.RequestApproval(ui.ApprovalRequestMessage{
		PromptID:    req.PromptID,
		Description: req.Description,
		ToolCall:    req.ToolCall,
		Risk:        req.Risk,
	})
	if err != nil {
		p.logger.Error("approval request failed", "error", err)
	}
}

func (p *Plugin) handleHITLRequest(e engine.Event[any]) {
	req, ok := e.Payload.(events.HITLRequest)
	if !ok {
		return
	}
	mode := string(req.Mode)
	if mode == "" {
		mode = string(events.HITLModeFreeText)
	}
	choices := make([]ui.HITLChoiceMessage, 0, len(req.Choices))
	for _, c := range req.Choices {
		choices = append(choices, ui.HITLChoiceMessage{ID: c.ID, Label: c.Label})
	}
	resp, err := p.adapter.RequestHumanInput(ui.HITLRequestMessage{
		RequestID: req.ID,
		Prompt:    req.Prompt,
		Mode:      mode,
		Choices:   choices,
		TurnID:    req.TurnID,
	})
	if err != nil {
		p.logger.Error("hitl request failed", "error", err)
		return
	}
	_ = p.bus.Emit("hitl.responded", events.HITLResponse{
		RequestID: resp.RequestID,
		ChoiceID:  resp.ChoiceID,
		FreeText:  resp.FreeText,
	})
}

func (p *Plugin) handleThinkingStep(e engine.Event[any]) {
	step, ok := e.Payload.(events.ThinkingStep)
	if !ok {
		return
	}
	_ = p.adapter.SendThinking(ui.ThinkingMessage{
		Content: step.Content,
		Phase:   step.Phase,
		Source:  step.Source,
		TurnID:  step.TurnID,
	})
}

func (p *Plugin) handleCodeExecStdout(e engine.Event[any]) {
	out, ok := e.Payload.(events.CodeExecStdout)
	if !ok {
		return
	}
	_ = p.adapter.SendCodeExecStdout(ui.CodeExecStdoutMessage{
		CallID:    out.CallID,
		TurnID:    out.TurnID,
		Chunk:     out.Chunk,
		Final:     out.Final,
		Truncated: out.Truncated,
	})
}

func (p *Plugin) handlePlanApprovalRequest(e engine.Event[any]) {
	req, ok := e.Payload.(events.ApprovalRequest)
	if !ok {
		return
	}
	resp, err := p.adapter.RequestApproval(ui.ApprovalRequestMessage{
		PromptID:    req.PromptID,
		Description: req.Description,
		ToolCall:    req.ToolCall,
		Risk:        req.Risk,
	})
	if err != nil {
		p.logger.Error("plan approval request failed", "error", err)
		return
	}
	_ = p.bus.Emit("plan.approval.response", events.ApprovalResponse{
		PromptID: resp.PromptID,
		Approved: resp.Approved,
		Always:   resp.Always,
	})
}

func (p *Plugin) handlePlanCreated(e engine.Event[any]) {
	result, ok := e.Payload.(events.PlanResult)
	if !ok {
		return
	}
	steps := make([]ui.PlanDisplayStep, len(result.Steps))
	for i, s := range result.Steps {
		steps[i] = ui.PlanDisplayStep{
			ID:          s.ID,
			Description: s.Description,
			Status:      s.Status,
			Order:       s.Order,
		}
	}
	_ = p.adapter.SendPlanDisplay(ui.PlanDisplayMessage{
		PlanID:  result.PlanID,
		Summary: result.Summary,
		Steps:   steps,
		Source:  result.Source,
		TurnID:  result.TurnID,
	})
}

func (p *Plugin) handleAgentPlan(e engine.Event[any]) {
	plan, ok := e.Payload.(events.Plan)
	if !ok {
		return
	}
	steps := make([]ui.PlanDisplayStep, len(plan.Steps))
	for i, s := range plan.Steps {
		steps[i] = ui.PlanDisplayStep{
			ID:          fmt.Sprintf("step_%d", i+1),
			Description: s.Description,
			Status:      s.Status,
			Order:       i + 1,
		}
	}
	_ = p.adapter.SendPlanUpdate(ui.PlanDisplayMessage{
		Steps:  steps,
		TurnID: plan.TurnID,
		Source: "agent",
	})
}

func (p *Plugin) handleFileChanged(e engine.Event[any]) {
	data, ok := e.Payload.(map[string]any)
	if !ok {
		return
	}
	path, _ := data["path"].(string)
	action := "updated"
	if e.Type == "session.file.created" {
		action = "created"
	}
	_ = p.adapter.SendFileChanged(path, action)
}

func (p *Plugin) handleCancelComplete(e engine.Event[any]) {
	cc, ok := e.Payload.(events.CancelComplete)
	if !ok {
		return
	}
	_ = p.adapter.SendCancelComplete(cc.TurnID, cc.Resumable)
}

// handleFileOpenRequest presents a native single-file open dialog via
// the attached FileDialogRuntime and emits io.file.open.response with
// the result. The dialog runs on a background goroutine so the bus
// dispatcher is not blocked waiting on user interaction, matching the
// same pattern as OnInput above.
func (p *Plugin) handleFileOpenRequest(e engine.Event[any]) {
	req, ok := e.Payload.(events.FileOpenRequest)
	if !ok {
		return
	}

	filters := make([]FileDialogFilter, len(req.Filters))
	for i, f := range req.Filters {
		filters[i] = FileDialogFilter{
			DisplayName: f.DisplayName,
			Pattern:     f.Pattern,
		}
	}
	opts := FileDialogOptions{
		Title:            req.Title,
		DefaultDirectory: req.DefaultDirectory,
		Filters:          filters,
	}

	go func() {
		resp := events.FileOpenResponse{RequestID: req.RequestID}
		path, err := p.hub.OpenFileDialog(opts)
		switch {
		case err != nil && errors.Is(err, ErrFileDialogUnavailable):
			resp.Error = err.Error()
			p.logger.Warn("file dialog unavailable", "request_id", req.RequestID)
		case err != nil:
			resp.Error = err.Error()
			p.logger.Error("file dialog failed", "error", err, "request_id", req.RequestID)
		case path == "":
			resp.Cancelled = true
		default:
			resp.Path = path
		}
		_ = p.bus.Emit("io.file.open.response", resp)
	}()
}

func (p *Plugin) handleHistoryReplay(e engine.Event[any]) {
	replay, ok := e.Payload.(events.HistoryReplay)
	if !ok {
		return
	}
	for _, msg := range replay.Messages {
		role := msg.Role
		switch role {
		case "user", "assistant", "system", "error":
			// Directly displayable.
		case "tool_invoke", "tool_result":
			continue
		default:
			continue
		}
		_ = p.adapter.SendOutput(ui.OutputMessage{
			Content: msg.Content,
			Role:    role,
		})
	}
}
