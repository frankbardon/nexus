// Package hitl is the human-in-the-loop primitive: any plugin that needs
// an operator's input — clarifying a question, approving a destructive
// tool call, picking among curated plans, or signing off on a memory
// write — emits hitl.requested and waits on hitl.responded.
//
// The plugin owns the LLM-facing ask_user tool, which is the LLM's direct
// path to the same machinery: the tool accepts an extended schema that
// lets the model present a freeform question, a multi-choice prompt, or
// both. Approval gates and memory plugins emit hitl.requested directly,
// without going through the tool.
package hitl

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.control.hitl"
	pluginName = "Human-in-the-Loop"
	version    = "0.2.0"
)

// Plugin owns the hitl.requested/hitl.responded registry and the LLM-facing
// ask_user tool.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger
	replay *engine.ReplayState
	unsubs []func()

	mu      sync.Mutex
	pending map[string]chan events.HITLResponse

	// reg is the optional filesystem-backed registry (config-gated by
	// registry.enabled). When non-nil, every hitl.requested is mirrored to
	// disk and an fsnotify watcher dispatches matching response files back
	// onto the bus. The synchronous IO-driven response path is unaffected.
	reg *registry

	liveCalls atomic.Uint64
}

// LiveCalls returns the count of ask_user invocations that survived the
// replay short-circuit. Tests assert zero during replay — the user is
// never re-prompted; the journaled answer is replayed.
func (p *Plugin) LiveCalls() uint64 { return p.liveCalls.Load() }

// New creates a new hitl plugin.
func New() engine.Plugin {
	return &Plugin{
		pending: make(map[string]chan events.HITLResponse),
	}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return pluginName }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.replay = ctx.Replay

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.handleToolInvoke,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("hitl.responded", p.handleResponse,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	if cfg, ok := ctx.Config["registry"].(map[string]any); ok {
		enabled, _ := cfg["enabled"].(bool)
		if enabled {
			dir, _ := cfg["dir"].(string)
			if dir == "" {
				dir = "~/.nexus/hitl"
			}
			reg, err := newRegistry(dir, p.logger, p.bus)
			if err != nil {
				return fmt.Errorf("hitl registry: %w", err)
			}
			p.reg = reg
			p.unsubs = append(p.unsubs,
				p.bus.Subscribe("hitl.requested", p.handleRequest,
					engine.WithPriority(50), engine.WithSource(pluginID)),
			)
			p.logger.Info("hitl registry enabled", "dir", reg.Dir())
		}
	}

	return nil
}

func (p *Plugin) Ready() error {
	_ = p.bus.Emit("tool.register", events.ToolDef{
		Name: "ask_user",
		Description: "Ask the user something and wait for their response. " +
			"Use this when you need clarification, confirmation, or a decision before proceeding. " +
			"Set mode=\"choices\" with a choices array to present options; mode=\"free_text\" (default) accepts a freeform answer; mode=\"both\" accepts either.",
		Class: "communication",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{
					"type":        "string",
					"description": "The question or approval text to show the user.",
				},
				"mode": map[string]any{
					"type":        "string",
					"enum":        []string{"free_text", "choices", "both"},
					"default":     "free_text",
					"description": "Response shape. Defaults to free_text (single string). Use choices for multiple-choice; both lets the user either pick or type.",
				},
				"choices": map[string]any{
					"type":        "array",
					"description": "Required when mode=choices or both. Each choice has an id (machine-stable) and label (human-readable).",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":    map[string]any{"type": "string"},
							"label": map[string]any{"type": "string"},
						},
						"required": []string{"id", "label"},
					},
				},
				"default_choice_id": map[string]any{
					"type":        "string",
					"description": "Optional. Choice id picked when the deadline elapses with no response. Must reference a choice in choices.",
				},
				"deadline_seconds": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Optional. Seconds before the request auto-resolves to default_choice_id (or cancels if unset).",
				},
			},
			"required": []string{"prompt"},
		},
	})
	return nil
}

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	if p.reg != nil {
		p.reg.Close()
		p.reg = nil
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	subs := []engine.EventSubscription{
		{EventType: "tool.invoke", Priority: 50},
		{EventType: "hitl.responded", Priority: 50},
	}
	if p.reg != nil {
		subs = append(subs, engine.EventSubscription{EventType: "hitl.requested", Priority: 50})
	}
	return subs
}

func (p *Plugin) Emissions() []string {
	return []string{
		"before:tool.result",
		"tool.result",
		"tool.register",
		"before:hitl.requested",
		"hitl.requested",
	}
}

func (p *Plugin) handleToolInvoke(event engine.Event[any]) {
	tc, ok := event.Payload.(events.ToolCall)
	if !ok || tc.Name != "ask_user" {
		return
	}

	// Replay short-circuit: pop the next journaled tool.result. The user is
	// never re-prompted during replay — hitl.requested would emit to a UI
	// that may not exist (or worse, prompt the live user with stale
	// questions).
	if engine.ReplayToolShortCircuit(p.replay, p.bus, tc, p.logger) {
		return
	}
	p.liveCalls.Add(1)

	req, errMsg := buildRequestFromToolCall(tc)
	if errMsg != "" {
		p.emitResult(tc, "", errMsg)
		return
	}

	ch := make(chan events.HITLResponse, 1)
	p.mu.Lock()
	p.pending[req.ID] = ch
	p.mu.Unlock()

	// Canonical Option B emission: vetoable pointer-payload first so
	// before:hitl.requested subscribers (notably nexus.control.hitl_synthesizer)
	// see the request and can mutate Prompt or veto. The non-vetoable value
	// emission below is what IO plugins consume.
	veto, vetoErr := p.bus.EmitVetoable("before:hitl.requested", &req)
	if vetoErr != nil {
		p.logger.Warn("emit before:hitl.requested failed", "error", vetoErr, "request_id", req.ID)
	}
	if veto.Vetoed {
		p.mu.Lock()
		delete(p.pending, req.ID)
		p.mu.Unlock()
		reason := veto.Reason
		if reason == "" {
			reason = "ask_user vetoed by before:hitl.requested handler"
		}
		p.emitResult(tc, "", reason)
		return
	}

	_ = p.bus.Emit("hitl.requested", req)

	resp := <-ch

	p.mu.Lock()
	delete(p.pending, req.ID)
	p.mu.Unlock()

	output, errOut := encodeResponseForLLM(resp)
	p.emitResult(tc, output, errOut)
}

func (p *Plugin) handleResponse(event engine.Event[any]) {
	resp, ok := toHITLResponse(event.Payload)
	if !ok {
		return
	}

	p.mu.Lock()
	ch, exists := p.pending[resp.RequestID]
	p.mu.Unlock()

	// Clean up the registry file for this request — covers the case where
	// an IO plugin answered before any out-of-band channel did, leaving the
	// pending request file orphaned in the registry directory.
	if p.reg != nil {
		p.reg.removeRequest(resp.RequestID)
	}

	if !exists {
		// No pending channel: either the request was already drained (this
		// is a duplicate response from a second source) or it never existed
		// (stale fsnotify event, replay artifact). Drop silently.
		return
	}
	select {
	case ch <- resp:
	default:
		// Channel already drained — first-response-wins. The duplicate is a
		// no-op rather than a warning to keep the async-channel path quiet.
	}
}

// handleRequest mirrors hitl.requested to the on-disk registry. Only wired
// when registry.enabled is true.
func (p *Plugin) handleRequest(event engine.Event[any]) {
	req, ok := event.Payload.(events.HITLRequest)
	if !ok {
		return
	}
	if p.reg == nil {
		return
	}
	if err := p.reg.persistRequest(req); err != nil {
		p.logger.Warn("hitl registry: persist failed", "request_id", req.ID, "err", err)
	}
}

func (p *Plugin) emitResult(tc events.ToolCall, output, errMsg string) {
	result := events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: tc.ID,
		Name:   tc.Name,
		Output: output,
		Error:  errMsg,
		TurnID: tc.TurnID,
	}
	if veto, err := p.bus.EmitVetoable("before:tool.result", &result); err == nil && veto.Vetoed {
		p.logger.Info("tool.result vetoed", "tool", tc.Name, "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("tool.result", result)
}

// buildRequestFromToolCall turns ask_user tool args into a HITLRequest.
// Returns ("", errMsg) when the args are malformed.
func buildRequestFromToolCall(tc events.ToolCall) (events.HITLRequest, string) {
	prompt, _ := tc.Arguments["prompt"].(string)
	if prompt == "" {
		// Backward-compatibility nicety: accept the legacy "question" key
		// so prompts that already train the LLM on the old name keep
		// working. The schema only documents prompt going forward.
		prompt, _ = tc.Arguments["question"].(string)
	}
	if prompt == "" {
		return events.HITLRequest{SchemaVersion: events.HITLRequestVersion}, "prompt argument is required"
	}

	mode := events.HITLMode(stringArg(tc.Arguments, "mode", string(events.HITLModeFreeText)))
	switch mode {
	case events.HITLModeFreeText, events.HITLModeChoices, events.HITLModeBoth:
	default:
		return events.HITLRequest{SchemaVersion: events.HITLRequestVersion}, fmt.Sprintf("invalid mode %q (want free_text, choices, or both)", mode)
	}

	choices, err := parseChoices(tc.Arguments["choices"])
	if err != nil {
		return events.HITLRequest{SchemaVersion: events.HITLRequestVersion}, err.Error()
	}
	if (mode == events.HITLModeChoices || mode == events.HITLModeBoth) && len(choices) == 0 {
		return events.HITLRequest{SchemaVersion: events.HITLRequestVersion}, "choices is required when mode is choices or both"
	}

	defaultChoiceID, _ := tc.Arguments["default_choice_id"].(string)
	if defaultChoiceID != "" {
		found := false
		for _, c := range choices {
			if c.ID == defaultChoiceID {
				found = true
				break
			}
		}
		if !found {
			return events.HITLRequest{SchemaVersion: events.HITLRequestVersion}, fmt.Sprintf("default_choice_id %q does not match any choice id", defaultChoiceID)
		}
	}

	req := events.HITLRequest{SchemaVersion: events.HITLRequestVersion, ID: fmt.Sprintf("hitl-%s-%s", tc.TurnID, tc.ID),
		TurnID:          tc.TurnID,
		RequesterPlugin: pluginID,
		ActionKind:      "free_text",
		Mode:            mode,
		Choices:         choices,
		DefaultChoiceID: defaultChoiceID,
		Prompt:          prompt,
	}
	if mode != events.HITLModeFreeText {
		req.ActionKind = "ask_user.choices"
	}
	return req, ""
}

func parseChoices(raw any) ([]events.HITLChoice, error) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("choices must be an array of {id,label} objects")
	}
	out := make([]events.HITLChoice, 0, len(list))
	seen := make(map[string]struct{}, len(list))
	for i, entry := range list {
		obj, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("choices[%d] must be an object", i)
		}
		id, _ := obj["id"].(string)
		label, _ := obj["label"].(string)
		if id == "" || label == "" {
			return nil, fmt.Errorf("choices[%d] requires non-empty id and label", i)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("choices[%d] duplicate id %q", i, id)
		}
		seen[id] = struct{}{}
		out = append(out, events.HITLChoice{
			ID:    id,
			Label: label,
			Kind:  events.ChoiceCustom,
		})
	}
	return out, nil
}

func stringArg(args map[string]any, key, def string) string {
	if v, ok := args[key].(string); ok && v != "" {
		return v
	}
	return def
}

// encodeResponseForLLM renders a HITLResponse into the structured JSON
// the model consumes as the ask_user tool result. Always returns a JSON
// object so the LLM sees a stable shape regardless of mode.
func encodeResponseForLLM(resp events.HITLResponse) (string, string) {
	if resp.Cancelled {
		errMsg := resp.CancelReason
		if errMsg == "" {
			errMsg = "cancelled"
		}
		return "", errMsg
	}
	out := map[string]string{}
	if resp.ChoiceID != "" {
		out["choice_id"] = resp.ChoiceID
	}
	if resp.FreeText != "" || resp.ChoiceID == "" {
		out["free_text"] = resp.FreeText
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return "", err.Error()
	}
	return string(encoded), ""
}

// toHITLResponse converts a bus payload (which may be a typed
// HITLResponse from a live emitter or a map[string]any from journal
// replay) into a typed value.
func toHITLResponse(payload any) (events.HITLResponse, bool) {
	switch v := payload.(type) {
	case events.HITLResponse:
		return v, true
	case *events.HITLResponse:
		if v == nil {
			return events.HITLResponse{SchemaVersion: events.HITLResponseVersion}, false
		}
		return *v, true
	case map[string]any:
		resp := events.HITLResponse{SchemaVersion: events.HITLResponseVersion}
		resp.RequestID, _ = v["request_id"].(string)
		resp.ChoiceID, _ = v["choice_id"].(string)
		resp.FreeText, _ = v["free_text"].(string)
		resp.Cancelled, _ = v["cancelled"].(bool)
		resp.CancelReason, _ = v["cancel_reason"].(string)
		if ep, ok := v["edited_payload"].(map[string]any); ok {
			resp.EditedPayload = ep
		}
		if resp.RequestID == "" {
			return resp, false
		}
		return resp, true
	default:
		return events.HITLResponse{SchemaVersion: events.HITLResponseVersion}, false
	}
}
