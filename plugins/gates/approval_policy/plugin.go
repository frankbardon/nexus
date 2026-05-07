// Package approvalpolicy implements the policy-driven approval gate. It
// subscribes to before:tool.invoke and before:llm.request, evaluates a
// config-supplied rule list against the action payload, and on first
// match emits a hitl.requested event and blocks waiting on the
// matching hitl.responded. The operator's choice resolves to either
// passthrough (allow), veto (reject), or passthrough-with-edits — the
// last lets the operator hand-edit tool args (or LLM model/temperature)
// before the action proceeds.
//
// The gate is the central enforcement point for approvals on agent
// actions; per-plugin require_approval shortcuts exist for inline
// cases (e.g. memory writes) where wiring a rule is overkill.
package approvalpolicy

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.gate.approval_policy"

// Default choices used when a rule selects mode=choices but does not
// supply an explicit choices list. Mirrors the canonical allow/reject
// pair documented in the HITL design doc.
var defaultChoices = []choiceCfg{
	{id: "allow", label: "Approve", kind: string(events.ChoiceAllow)},
	{id: "reject", label: "Reject", kind: string(events.ChoiceReject)},
}

// New constructs the approval policy gate.
func New() engine.Plugin {
	return &Plugin{
		pending: make(map[string]chan events.HITLResponse),
	}
}

// Plugin gates before:tool.invoke and before:llm.request by translating
// matching actions into a hitl.requested round-trip.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	rules  []rule
	unsubs []func()

	mu      sync.Mutex
	pending map[string]chan events.HITLResponse

	requestCounter atomic.Uint64
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "Approval Policy Gate" }
func (p *Plugin) Version() string                   { return "0.1.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	rules, err := parseRules(ctx.Config["rules"])
	if err != nil {
		return fmt.Errorf("approval_policy: parse rules: %w", err)
	}
	p.rules = rules

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("before:tool.invoke", p.handleBeforeToolInvoke,
			engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("hitl.responded", p.handleHITLResponse,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	p.logger.Info("approval policy gate initialized", "rules", len(p.rules))
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "before:tool.invoke", Priority: 10},
		{EventType: "before:llm.request", Priority: 10},
		{EventType: "hitl.responded", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{"before:hitl.requested", "hitl.requested"}
}

// handleBeforeToolInvoke evaluates rules against pending tool-invoke
// payloads and either lets them pass, vetoes them, or mutates their
// arguments in place (for "edit" responses).
func (p *Plugin) handleBeforeToolInvoke(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	tc, ok := vp.Original.(*events.ToolCall)
	if !ok {
		return
	}

	payload := toolCallActionPayload(tc)
	matched := p.firstMatch(payload)
	if matched == nil {
		return
	}

	resp := p.requestApproval(*matched, payload, "tool.invoke", tc.Name)
	if resp.rejected {
		vp.Veto = engine.VetoResult{
			Vetoed: true,
			Reason: resp.reason,
		}
		return
	}
	if resp.editedPayload != nil {
		applyToolEdit(tc, resp.editedPayload)
	}
}

// handleBeforeLLMRequest evaluates rules against pending llm.request
// payloads. Same dispatch as tool.invoke; edits land on the model and
// temperature fields, with leftover keys merged into Metadata.
func (p *Plugin) handleBeforeLLMRequest(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	req, ok := vp.Original.(*events.LLMRequest)
	if !ok {
		return
	}

	// Skip gate-originated and planner-tagged requests so we never gate
	// our own follow-up emissions or planner-generated calls.
	if src, _ := req.Metadata["_source"].(string); src != "" {
		return
	}

	payload := llmRequestActionPayload(req)
	matched := p.firstMatch(payload)
	if matched == nil {
		return
	}

	resp := p.requestApproval(*matched, payload, "llm.request", req.Model)
	if resp.rejected {
		vp.Veto = engine.VetoResult{
			Vetoed: true,
			Reason: resp.reason,
		}
		return
	}
	if resp.editedPayload != nil {
		applyLLMEdit(req, resp.editedPayload)
	}
}

// handleHITLResponse routes operator decisions back to the goroutine
// that issued the corresponding hitl.requested. Same channel-handoff
// pattern as the control/hitl plugin uses for its own requests.
func (p *Plugin) handleHITLResponse(event engine.Event[any]) {
	resp, ok := toHITLResponse(event.Payload)
	if !ok {
		return
	}

	p.mu.Lock()
	ch, exists := p.pending[resp.RequestID]
	p.mu.Unlock()
	if !exists {
		return
	}
	select {
	case ch <- resp:
	default:
		p.logger.Warn("hitl.responded dropped — channel full", "request_id", resp.RequestID)
	}
}

// approvalOutcome is the gate-internal interpretation of a HITL
// response: did the operator allow it, with optional edits, or reject?
type approvalOutcome struct {
	rejected      bool
	reason        string
	editedPayload map[string]any
}

// requestApproval emits a hitl.requested for the matched rule and
// blocks until the corresponding hitl.responded arrives or (when set)
// the rule's timeout elapses. Returns the gate-internal interpretation.
func (p *Plugin) requestApproval(r rule, payload map[string]any, actionKind, target string) approvalOutcome {
	choices := buildChoices(r)
	defaultID := r.defaultChoice
	prompt := renderPrompt(r, payload, actionKind, target)

	requestID := fmt.Sprintf("approval-%d-%d", time.Now().UnixNano(), p.requestCounter.Add(1))

	req := events.HITLRequest{SchemaVersion: events.HITLRequestVersion, ID: requestID,
		RequesterPlugin:   pluginID,
		ActionKind:        actionKind,
		ActionRef:         payload,
		Mode:              events.HITLMode(r.mode),
		Choices:           choices,
		DefaultChoiceID:   defaultID,
		Prompt:            prompt,
		PromptSynthesizer: r.promptSynthesizer,
	}
	if r.timeoutSeconds > 0 {
		req.Deadline = time.Now().Add(time.Duration(r.timeoutSeconds) * time.Second)
	}

	ch := make(chan events.HITLResponse, 1)
	p.mu.Lock()
	p.pending[requestID] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, requestID)
		p.mu.Unlock()
	}()

	// Canonical Option B emission: vetoable pointer-payload first so
	// before:hitl.requested subscribers (notably nexus.control.hitl_synthesizer)
	// can mutate Prompt or veto the request, then the non-vetoable value
	// emission consumed by IO plugins.
	if veto, err := p.bus.EmitVetoable("before:hitl.requested", &req); err != nil {
		p.logger.Error("emit before:hitl.requested failed", "error", err, "action", actionKind)
		return approvalOutcome{rejected: true, reason: fmt.Sprintf("approval gate emit failed: %v", err)}
	} else if veto.Vetoed {
		reason := veto.Reason
		if reason == "" {
			reason = "approval vetoed by before:hitl.requested handler"
		}
		return approvalOutcome{rejected: true, reason: reason}
	}

	if err := p.bus.Emit("hitl.requested", req); err != nil {
		p.logger.Error("emit hitl.requested failed", "error", err, "action", actionKind)
		return approvalOutcome{rejected: true, reason: fmt.Sprintf("approval gate emit failed: %v", err)}
	}

	var resp events.HITLResponse
	if r.timeoutSeconds > 0 {
		select {
		case resp = <-ch:
		case <-time.After(time.Duration(r.timeoutSeconds) * time.Second):
			if defaultID != "" {
				resp = events.HITLResponse{SchemaVersion: events.HITLResponseVersion, RequestID: requestID, ChoiceID: defaultID}
				p.logger.Info("approval timed out, applying default", "default", defaultID, "action", actionKind)
			} else {
				p.logger.Warn("approval timed out with no default", "action", actionKind)
				return approvalOutcome{rejected: true, reason: "approval timed out with no default"}
			}
		}
	} else {
		resp = <-ch
	}

	return interpretResponse(resp, choices)
}

// interpretResponse maps a HITLResponse onto our internal allow/reject
// view, honoring ChoiceKind when the rule supplied one and falling back
// to the canonical "reject" id otherwise.
func interpretResponse(resp events.HITLResponse, choices []events.HITLChoice) approvalOutcome {
	if resp.Cancelled {
		reason := resp.CancelReason
		if reason == "" {
			reason = "approval cancelled"
		}
		return approvalOutcome{rejected: true, reason: reason}
	}

	choiceID := resp.ChoiceID
	if choiceID == "" {
		// Mode=free_text response with no explicit choice id. Treat as
		// allow — the gate owner asked for free-form input and got it.
		return approvalOutcome{editedPayload: resp.EditedPayload}
	}

	for _, c := range choices {
		if c.ID != choiceID {
			continue
		}
		if c.Kind == events.ChoiceReject {
			return approvalOutcome{rejected: true, reason: fmt.Sprintf("operator rejected (%s)", c.ID)}
		}
		// Allow / edit / custom all pass through with optional edits.
		edits := resp.EditedPayload
		if edits == nil {
			edits = c.EditedPayload
		}
		return approvalOutcome{editedPayload: edits}
	}

	// Unknown choice id. Fall back to the canonical reject id name to
	// catch the common case where a rule omits Kind annotations.
	if choiceID == "reject" {
		return approvalOutcome{rejected: true, reason: "operator rejected"}
	}
	return approvalOutcome{editedPayload: resp.EditedPayload}
}

// firstMatch returns a pointer to the first rule whose match map is
// satisfied by payload, or nil if none match.
func (p *Plugin) firstMatch(payload map[string]any) *rule {
	for i := range p.rules {
		if p.rules[i].matches(payload) {
			return &p.rules[i]
		}
	}
	return nil
}

// toolCallActionPayload flattens a ToolCall into the map shape rules
// match against. The runtime payload mirrors the YAML config's match
// keys: action_kind, tool, args, plus passthrough id/turn_id.
func toolCallActionPayload(tc *events.ToolCall) map[string]any {
	args := tc.Arguments
	if args == nil {
		args = map[string]any{}
	}
	return map[string]any{
		"action_kind": "tool.invoke",
		"tool":        tc.Name,
		"args":        args,
		"id":          tc.ID,
		"turn_id":     tc.TurnID,
	}
}

// llmRequestActionPayload flattens an LLMRequest into the rule-match
// shape. Tools list is exposed as a name slice so rules can match e.g.
// `tools: "shell"` if needed.
func llmRequestActionPayload(req *events.LLMRequest) map[string]any {
	toolNames := make([]any, 0, len(req.Tools))
	for _, t := range req.Tools {
		toolNames = append(toolNames, t.Name)
	}
	args := map[string]any{
		"role":  req.Role,
		"model": req.Model,
		"tools": toolNames,
	}
	if req.Temperature != nil {
		args["temperature"] = *req.Temperature
	}
	if req.MaxTokens > 0 {
		args["max_tokens"] = req.MaxTokens
	}
	return map[string]any{
		"action_kind": "llm.request",
		"model":       req.Model,
		"role":        req.Role,
		"args":        args,
	}
}

// applyToolEdit overlays an EditedPayload onto a ToolCall's Arguments.
// Top-level keys win over the original args; nested edits replace
// wholesale (intentional: "edit" is an operator override, not a deep
// merge).
func applyToolEdit(tc *events.ToolCall, edits map[string]any) {
	if tc.Arguments == nil {
		tc.Arguments = map[string]any{}
	}
	// Nested edits under "args" key take precedence — lets HITLChoice
	// authors supply either a flat overlay or the full ActionRef shape.
	if argEdits, ok := edits["args"].(map[string]any); ok {
		for k, v := range argEdits {
			tc.Arguments[k] = v
		}
		return
	}
	for k, v := range edits {
		tc.Arguments[k] = v
	}
}

// applyLLMEdit overlays known LLMRequest fields from an EditedPayload.
// Recognized keys: model, temperature. Anything else lands in Metadata
// so future fields don't silently disappear.
func applyLLMEdit(req *events.LLMRequest, edits map[string]any) {
	for k, v := range edits {
		switch k {
		case "model":
			if s, ok := v.(string); ok {
				req.Model = s
			}
		case "temperature":
			switch t := v.(type) {
			case float64:
				req.Temperature = &t
			case float32:
				ft := float64(t)
				req.Temperature = &ft
			case int:
				ft := float64(t)
				req.Temperature = &ft
			}
		default:
			if req.Metadata == nil {
				req.Metadata = map[string]any{}
			}
			req.Metadata[k] = v
		}
	}
}

// renderPrompt returns the rendered approval prompt. Falls back to a
// generic "Approve <kind>: <target>" message when no template was set
// or the template fails to parse/execute. When the rule names a
// prompt_synthesizer, an empty prompt template stays empty so the
// synthesizer can fill it in via the before:hitl.requested handler.
func renderPrompt(r rule, payload map[string]any, actionKind, target string) string {
	if strings.TrimSpace(r.prompt) == "" {
		if r.promptSynthesizer != "" {
			return ""
		}
		return fmt.Sprintf("Approve %s: %s", actionKind, target)
	}
	tmpl, err := template.New("approval").Option("missingkey=zero").Parse(r.prompt)
	if err != nil {
		return r.prompt
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, payload); err != nil {
		return r.prompt
	}
	return buf.String()
}

// buildChoices converts a rule's choice config into the wire-shape
// HITLChoice slice. Honors mode=free_text by returning nil; defaults
// to allow/reject when no choices were configured for choice modes.
func buildChoices(r rule) []events.HITLChoice {
	if r.mode == string(events.HITLModeFreeText) {
		return nil
	}
	src := r.choices
	if len(src) == 0 {
		src = defaultChoices
	}
	out := make([]events.HITLChoice, 0, len(src))
	for _, c := range src {
		kind := events.ChoiceKind(c.kind)
		if kind == "" {
			switch c.id {
			case "allow":
				kind = events.ChoiceAllow
			case "reject":
				kind = events.ChoiceReject
			case "edit":
				kind = events.ChoiceEdit
			default:
				kind = events.ChoiceCustom
			}
		}
		label := c.label
		if label == "" {
			label = c.id
		}
		out = append(out, events.HITLChoice{
			ID:    c.id,
			Label: label,
			Kind:  kind,
		})
	}
	return out
}

// parseRules unpacks the YAML-parsed []any (each item map[string]any)
// into rule values. Returns a friendly error on malformed input so boot
// fails loud instead of silently skipping rules.
func parseRules(raw any) ([]rule, error) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("rules must be a list, got %T", raw)
	}
	out := make([]rule, 0, len(list))
	for i, entry := range list {
		obj, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("rules[%d] must be a map, got %T", i, entry)
		}
		r, err := parseRule(obj)
		if err != nil {
			return nil, fmt.Errorf("rules[%d]: %w", i, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// parseRule pulls a single rule out of a map[string]any. Defaults are
// applied here so the runtime path can rely on r.mode being set.
func parseRule(obj map[string]any) (rule, error) {
	r := rule{
		mode: string(events.HITLModeChoices),
	}

	if v, ok := obj["match"].(map[string]any); ok {
		r.match = v
	} else if obj["match"] != nil {
		return r, fmt.Errorf("match must be a map")
	}

	if v, ok := obj["mode"].(string); ok && v != "" {
		switch events.HITLMode(v) {
		case events.HITLModeFreeText, events.HITLModeChoices, events.HITLModeBoth:
			r.mode = v
		default:
			return r, fmt.Errorf("invalid mode %q (want free_text, choices, both)", v)
		}
	}

	if v, ok := obj["choices"]; ok && v != nil {
		choices, err := parseChoices(v)
		if err != nil {
			return r, err
		}
		r.choices = choices
	}

	if v, ok := obj["default_choice"].(string); ok {
		r.defaultChoice = v
	}

	if v, ok := obj["prompt"].(string); ok {
		r.prompt = v
	}

	if v, ok := obj["prompt_synthesizer"].(string); ok {
		r.promptSynthesizer = v
	}

	if v, ok := obj["timeout"].(string); ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return r, fmt.Errorf("invalid timeout %q: %w", v, err)
		}
		r.timeoutSeconds = int(d.Seconds())
	}

	return r, nil
}

// parseChoices accepts the YAML choices list. Each entry is either a
// bare string (treated as id, with the id-derived default kind) or a
// map with id/label/kind.
func parseChoices(raw any) ([]choiceCfg, error) {
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("choices must be a list")
	}
	out := make([]choiceCfg, 0, len(list))
	for i, entry := range list {
		switch e := entry.(type) {
		case string:
			if e == "" {
				return nil, fmt.Errorf("choices[%d] empty id", i)
			}
			out = append(out, choiceCfg{id: e})
		case map[string]any:
			id, _ := e["id"].(string)
			if id == "" {
				return nil, fmt.Errorf("choices[%d] missing id", i)
			}
			label, _ := e["label"].(string)
			kind, _ := e["kind"].(string)
			out = append(out, choiceCfg{id: id, label: label, kind: kind})
		default:
			return nil, fmt.Errorf("choices[%d] must be a string or map, got %T", i, entry)
		}
	}
	return out, nil
}

// toHITLResponse mirrors the converter in plugins/control/hitl. We
// duplicate it here rather than depending on the hitl plugin to keep
// the gate's dependency surface flat (gates are policy modules, not
// downstream consumers of other plugins).
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
