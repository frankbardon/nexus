package approvalpolicy

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// scriptedResponder is a tiny test double for an IO plugin: it
// subscribes to hitl.requested and re-emits a scripted hitl.responded.
// The bus dispatches handlers synchronously, so the responder must be
// driven from a separate goroutine to unblock the gate's wait.
type scriptedResponder struct {
	bus     engine.EventBus
	mu      sync.Mutex
	scripts []events.HITLResponse
	seen    []events.HITLRequest
}

func newScriptedResponder(bus engine.EventBus, scripts ...events.HITLResponse) *scriptedResponder {
	r := &scriptedResponder{bus: bus, scripts: scripts}
	bus.Subscribe("hitl.requested", r.handle)
	return r
}

func (r *scriptedResponder) handle(e engine.Event[any]) {
	req, ok := e.Payload.(events.HITLRequest)
	if !ok {
		return
	}
	r.mu.Lock()
	r.seen = append(r.seen, req)
	var resp events.HITLResponse
	if len(r.scripts) > 0 {
		resp = r.scripts[0]
		if len(r.scripts) > 1 {
			r.scripts = r.scripts[1:]
		}
	}
	r.mu.Unlock()

	resp.RequestID = req.ID
	// Emit asynchronously so the gate's blocking <-ch unblocks. Same
	// pattern as a real IO handler: hitl.responded comes from a
	// different goroutine than the agent's tool dispatch.
	go func() {
		_ = r.bus.Emit("hitl.responded", resp)
	}()
}

func newTestPlugin(t *testing.T, rules []rule) (*Plugin, engine.EventBus) {
	t.Helper()
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = slog.Default()
	p.rules = rules

	bus.Subscribe("before:tool.invoke", p.handleBeforeToolInvoke,
		engine.WithPriority(10))
	bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
		engine.WithPriority(10))
	bus.Subscribe("hitl.responded", p.handleHITLResponse,
		engine.WithPriority(50))
	return p, bus
}

func TestApprovalPolicy_NoMatch_PassThrough(t *testing.T) {
	rules := []rule{{
		match: map[string]any{"action_kind": "tool.invoke", "tool": "shell"},
		mode:  string(events.HITLModeChoices),
	}}
	_, bus := newTestPlugin(t, rules)
	// No responder needed — the rule shouldn't match, so no hitl.requested.

	tc := events.ToolCall{
		ID:        "t1",
		Name:      "fileio.read", // not "shell"
		Arguments: map[string]any{"path": "/tmp/x"},
	}
	veto, err := bus.EmitVetoable("before:tool.invoke", &tc)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if veto.Vetoed {
		t.Fatalf("expected passthrough, got veto: %s", veto.Reason)
	}
}

func TestApprovalPolicy_Match_Allow(t *testing.T) {
	rules := []rule{{
		match: map[string]any{"action_kind": "tool.invoke", "tool": "shell"},
		mode:  string(events.HITLModeChoices),
	}}
	_, bus := newTestPlugin(t, rules)
	resp := newScriptedResponder(bus, events.HITLResponse{ChoiceID: "allow"})

	tc := events.ToolCall{
		ID:        "t1",
		Name:      "shell",
		Arguments: map[string]any{"command": "ls"},
	}
	veto, err := bus.EmitVetoable("before:tool.invoke", &tc)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if veto.Vetoed {
		t.Fatalf("expected allow, got veto: %s", veto.Reason)
	}
	if len(resp.seen) != 1 {
		t.Fatalf("expected 1 hitl.requested, got %d", len(resp.seen))
	}
	if resp.seen[0].ActionKind != "tool.invoke" {
		t.Errorf("ActionKind = %q, want tool.invoke", resp.seen[0].ActionKind)
	}
	if len(resp.seen[0].Choices) != 2 {
		t.Errorf("expected default 2 choices, got %d", len(resp.seen[0].Choices))
	}
}

func TestApprovalPolicy_Match_Reject_Vetoes(t *testing.T) {
	rules := []rule{{
		match: map[string]any{"action_kind": "tool.invoke", "tool": "shell"},
		mode:  string(events.HITLModeChoices),
	}}
	_, bus := newTestPlugin(t, rules)
	newScriptedResponder(bus, events.HITLResponse{ChoiceID: "reject"})

	tc := events.ToolCall{
		ID:        "t1",
		Name:      "shell",
		Arguments: map[string]any{"command": "rm -rf /"},
	}
	veto, err := bus.EmitVetoable("before:tool.invoke", &tc)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !veto.Vetoed {
		t.Fatal("expected reject to veto, got passthrough")
	}
}

func TestApprovalPolicy_Match_Edit_MutatesPayload(t *testing.T) {
	rules := []rule{{
		match: map[string]any{"action_kind": "tool.invoke", "tool": "shell"},
		mode:  string(events.HITLModeChoices),
		choices: []choiceCfg{
			{id: "allow", kind: string(events.ChoiceAllow)},
			{id: "edit", kind: string(events.ChoiceEdit)},
			{id: "reject", kind: string(events.ChoiceReject)},
		},
	}}
	_, bus := newTestPlugin(t, rules)
	newScriptedResponder(bus, events.HITLResponse{
		ChoiceID:      "edit",
		EditedPayload: map[string]any{"command": "ls -la"},
	})

	tc := events.ToolCall{
		ID:        "t1",
		Name:      "shell",
		Arguments: map[string]any{"command": "rm -rf /"},
	}
	veto, err := bus.EmitVetoable("before:tool.invoke", &tc)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if veto.Vetoed {
		t.Fatalf("edit should pass through, got veto: %s", veto.Reason)
	}
	if got := tc.Arguments["command"]; got != "ls -la" {
		t.Errorf("Arguments[command] = %v, want %q", got, "ls -la")
	}
}

func TestApprovalPolicy_GlobMatch(t *testing.T) {
	rules := []rule{{
		match: map[string]any{"action_kind": "tool.invoke", "tool": "shell", "args.command": "rm*"},
		mode:  string(events.HITLModeChoices),
	}}
	_, bus := newTestPlugin(t, rules)
	newScriptedResponder(bus, events.HITLResponse{ChoiceID: "reject"})

	// "ls" doesn't match "rm*" — should pass through silently.
	safeCall := events.ToolCall{
		Name:      "shell",
		Arguments: map[string]any{"command": "ls"},
	}
	if veto, _ := bus.EmitVetoable("before:tool.invoke", &safeCall); veto.Vetoed {
		t.Fatal("ls should not match rm* — unexpected veto")
	}

	// "rm -rf /" matches "rm*" — should be vetoed.
	dangerous := events.ToolCall{
		Name:      "shell",
		Arguments: map[string]any{"command": "rm -rf /"},
	}
	veto, _ := bus.EmitVetoable("before:tool.invoke", &dangerous)
	if !veto.Vetoed {
		t.Fatal("rm command should match and reject")
	}
}

func TestApprovalPolicy_DefaultChoice_OnTimeout(t *testing.T) {
	rules := []rule{{
		match:          map[string]any{"action_kind": "tool.invoke", "tool": "shell"},
		mode:           string(events.HITLModeChoices),
		defaultChoice:  "reject",
		timeoutSeconds: 1,
	}}
	_, bus := newTestPlugin(t, rules)
	// No responder — gate must time out.

	tc := events.ToolCall{
		Name:      "shell",
		Arguments: map[string]any{"command": "ls"},
	}
	start := time.Now()
	veto, _ := bus.EmitVetoable("before:tool.invoke", &tc)
	elapsed := time.Since(start)

	if !veto.Vetoed {
		t.Fatal("expected default reject on timeout")
	}
	if elapsed < 500*time.Millisecond {
		t.Errorf("returned too fast (%v) — should wait for timeout", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("returned too slow (%v) — timeout misconfigured", elapsed)
	}
}

func TestApprovalPolicy_LLMRequest_Match(t *testing.T) {
	rules := []rule{{
		match: map[string]any{"action_kind": "llm.request", "model": "claude-opus-*"},
		mode:  string(events.HITLModeChoices),
	}}
	_, bus := newTestPlugin(t, rules)
	newScriptedResponder(bus, events.HITLResponse{ChoiceID: "reject"})

	req := events.LLMRequest{Model: "claude-opus-4-7"}
	veto, _ := bus.EmitVetoable("before:llm.request", &req)
	if !veto.Vetoed {
		t.Fatal("expected veto for matching expensive model")
	}

	cheaper := events.LLMRequest{Model: "claude-haiku-4-5"}
	veto2, _ := bus.EmitVetoable("before:llm.request", &cheaper)
	if veto2.Vetoed {
		t.Fatal("haiku should not match opus glob")
	}
}

func TestApprovalPolicy_LLMRequest_SkipsSourcedRequests(t *testing.T) {
	rules := []rule{{
		match: map[string]any{"action_kind": "llm.request"},
		mode:  string(events.HITLModeChoices),
	}}
	_, bus := newTestPlugin(t, rules)
	// No responder — if the gate tried to request approval the test
	// would deadlock. The skip path must short-circuit before that.

	req := events.LLMRequest{
		Model:    "x",
		Metadata: map[string]any{"_source": "nexus.planner.dynamic"},
	}
	done := make(chan struct{})
	go func() {
		bus.EmitVetoable("before:llm.request", &req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sourced llm.request should be skipped, not blocked")
	}
}

func TestApprovalPolicy_FirstMatchWins(t *testing.T) {
	rules := []rule{
		{
			match: map[string]any{"action_kind": "tool.invoke", "tool": "shell"},
			mode:  string(events.HITLModeChoices),
		},
		{
			// Broader rule — would match anything if first didn't catch it.
			match: map[string]any{"action_kind": "tool.invoke"},
			mode:  string(events.HITLModeChoices),
		},
	}
	_, bus := newTestPlugin(t, rules)
	resp := newScriptedResponder(bus, events.HITLResponse{ChoiceID: "allow"})

	tc := events.ToolCall{Name: "shell", Arguments: map[string]any{"command": "ls"}}
	bus.EmitVetoable("before:tool.invoke", &tc)

	if len(resp.seen) != 1 {
		t.Fatalf("expected exactly 1 approval request (first match wins), got %d", len(resp.seen))
	}
}

func TestApprovalPolicy_PromptTemplate(t *testing.T) {
	rules := []rule{{
		match:  map[string]any{"action_kind": "tool.invoke", "tool": "shell"},
		mode:   string(events.HITLModeChoices),
		prompt: "Run shell {{ index .args \"command\" }}?",
	}}
	_, bus := newTestPlugin(t, rules)
	resp := newScriptedResponder(bus, events.HITLResponse{ChoiceID: "allow"})

	tc := events.ToolCall{Name: "shell", Arguments: map[string]any{"command": "ls /etc"}}
	bus.EmitVetoable("before:tool.invoke", &tc)

	if len(resp.seen) != 1 {
		t.Fatalf("expected 1 request, got %d", len(resp.seen))
	}
	want := "Run shell ls /etc?"
	if resp.seen[0].Prompt != want {
		t.Errorf("Prompt = %q, want %q", resp.seen[0].Prompt, want)
	}
}

func TestApprovalPolicy_DefaultPrompt_WhenUnset(t *testing.T) {
	rules := []rule{{
		match: map[string]any{"action_kind": "tool.invoke", "tool": "shell"},
		mode:  string(events.HITLModeChoices),
	}}
	_, bus := newTestPlugin(t, rules)
	resp := newScriptedResponder(bus, events.HITLResponse{ChoiceID: "allow"})

	tc := events.ToolCall{Name: "shell", Arguments: map[string]any{"command": "ls"}}
	bus.EmitVetoable("before:tool.invoke", &tc)

	if len(resp.seen) != 1 || resp.seen[0].Prompt == "" {
		t.Fatalf("expected non-empty default prompt, got %+v", resp.seen)
	}
}

func TestParseRules_RoundTrip(t *testing.T) {
	raw := []any{
		map[string]any{
			"match":          map[string]any{"action_kind": "tool.invoke", "tool": "shell"},
			"mode":           "choices",
			"timeout":        "5m",
			"default_choice": "reject",
			"choices": []any{
				map[string]any{"id": "allow", "label": "Approve", "kind": "allow"},
				"reject",
			},
		},
	}
	rules, err := parseRules(raw)
	if err != nil {
		t.Fatalf("parseRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	r := rules[0]
	if r.mode != "choices" {
		t.Errorf("mode = %q, want choices", r.mode)
	}
	if r.timeoutSeconds != 300 {
		t.Errorf("timeoutSeconds = %d, want 300", r.timeoutSeconds)
	}
	if r.defaultChoice != "reject" {
		t.Errorf("defaultChoice = %q, want reject", r.defaultChoice)
	}
	if len(r.choices) != 2 {
		t.Errorf("choices = %d, want 2", len(r.choices))
	}
	if r.choices[1].id != "reject" {
		t.Errorf("string-form choice id = %q, want reject", r.choices[1].id)
	}
}

// TestApprovalPolicy_BeforeHandlerSynthesizesPrompt verifies the gate
// emits before:hitl.requested with a pointer payload first, so the
// HITL prompt synthesizer (or any other before:hitl.requested handler)
// can mutate Prompt before IO sees the request.
func TestApprovalPolicy_BeforeHandlerSynthesizesPrompt(t *testing.T) {
	rules := []rule{{
		match: map[string]any{"action_kind": "tool.invoke", "tool": "shell"},
		mode:  string(events.HITLModeChoices),
	}}
	_, bus := newTestPlugin(t, rules)

	bus.Subscribe("before:hitl.requested", func(ev engine.Event[any]) {
		vp, ok := ev.Payload.(*engine.VetoablePayload)
		if !ok {
			return
		}
		req, ok := vp.Original.(*events.HITLRequest)
		if !ok {
			return
		}
		req.Prompt = "synthesized: " + req.ActionKind
	}, engine.WithPriority(-100))

	resp := newScriptedResponder(bus, events.HITLResponse{ChoiceID: "allow"})

	tc := events.ToolCall{
		ID:        "t1",
		Name:      "shell",
		Arguments: map[string]any{"command": "ls"},
	}
	veto, err := bus.EmitVetoable("before:tool.invoke", &tc)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if veto.Vetoed {
		t.Fatalf("expected allow, got veto: %s", veto.Reason)
	}
	if len(resp.seen) != 1 {
		t.Fatalf("expected 1 hitl.requested, got %d", len(resp.seen))
	}
	if resp.seen[0].Prompt != "synthesized: tool.invoke" {
		t.Errorf("expected synthesized prompt to reach IO, got %q", resp.seen[0].Prompt)
	}
}

// TestApprovalPolicy_PromptSynthesizerOptIn asserts a rule with the
// prompt_synthesizer key set propagates the capability ID onto the
// emitted HITLRequest and leaves Prompt empty so the
// before:hitl.requested subscriber can render it.
func TestApprovalPolicy_PromptSynthesizerOptIn(t *testing.T) {
	rules := []rule{{
		match:             map[string]any{"action_kind": "tool.invoke", "tool": "shell"},
		mode:              string(events.HITLModeChoices),
		promptSynthesizer: "hitl.prompt_synthesizer",
	}}
	_, bus := newTestPlugin(t, rules)
	resp := newScriptedResponder(bus, events.HITLResponse{ChoiceID: "allow"})

	tc := events.ToolCall{
		ID:        "t1",
		Name:      "shell",
		Arguments: map[string]any{"command": "rm -rf /tmp"},
	}
	if _, err := bus.EmitVetoable("before:tool.invoke", &tc); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if len(resp.seen) != 1 {
		t.Fatalf("expected 1 hitl.requested, got %d", len(resp.seen))
	}
	if resp.seen[0].PromptSynthesizer != "hitl.prompt_synthesizer" {
		t.Errorf("PromptSynthesizer not propagated, got %q", resp.seen[0].PromptSynthesizer)
	}
	if resp.seen[0].Prompt != "" {
		t.Errorf("expected empty Prompt to leave synthesis room, got %q", resp.seen[0].Prompt)
	}
}

// TestApprovalPolicy_BeforeHandlerVetoRejects asserts a veto on the
// canonical before:hitl.requested entry point translates into a veto
// on the original before:tool.invoke without ever firing IO.
func TestApprovalPolicy_BeforeHandlerVetoRejects(t *testing.T) {
	rules := []rule{{
		match: map[string]any{"action_kind": "tool.invoke", "tool": "shell"},
		mode:  string(events.HITLModeChoices),
	}}
	_, bus := newTestPlugin(t, rules)

	bus.Subscribe("before:hitl.requested", func(ev engine.Event[any]) {
		vp, ok := ev.Payload.(*engine.VetoablePayload)
		if !ok {
			return
		}
		vp.Veto = engine.VetoResult{Vetoed: true, Reason: "approval auditor rejected"}
	}, engine.WithPriority(-100))

	var ioCalls int
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		if _, ok := ev.Payload.(events.HITLRequest); ok {
			ioCalls++
		}
	}, engine.WithPriority(50))

	tc := events.ToolCall{
		ID:        "t1",
		Name:      "shell",
		Arguments: map[string]any{"command": "ls"},
	}
	veto, err := bus.EmitVetoable("before:tool.invoke", &tc)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !veto.Vetoed {
		t.Fatal("expected gate to veto tool.invoke when before:hitl.requested vetoes")
	}
	if veto.Reason != "approval auditor rejected" {
		t.Errorf("expected veto reason forwarded, got %q", veto.Reason)
	}
	if ioCalls != 0 {
		t.Errorf("vetoed approval must not emit hitl.requested to IO, got %d", ioCalls)
	}
}

func TestParseRules_InvalidMode(t *testing.T) {
	raw := []any{
		map[string]any{"mode": "bogus"},
	}
	_, err := parseRules(raw)
	if err == nil {
		t.Fatal("expected error for bogus mode")
	}
}
