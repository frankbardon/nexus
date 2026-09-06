package subagent

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// wireMockLLM subscribes to llm.request on the harness bus and synchronously
// replies with a matching llm.response carrying a fixed, tool-call-free
// content — enough to let runSubagent's blocking delegate.SyncLLM call
// return immediately with no further iterations. Returns a function that
// reads back the last captured request's system-role message.
func wireMockLLM(h *contract.ContractHarness) func() events.LLMRequest {
	var last events.LLMRequest
	h.Bus().Subscribe("llm.request", func(ev engine.Event[any]) {
		req, ok := ev.Payload.(events.LLMRequest)
		if !ok {
			return
		}
		last = req
		_ = h.Bus().Emit("llm.response", events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			RequestID:     req.RequestID,
			Content:       "done",
		})
	}, engine.WithPriority(1))
	return func() events.LLMRequest { return last }
}

func subagentSystemMessage(t *testing.T, req events.LLMRequest) (string, bool) {
	t.Helper()
	for _, m := range req.Messages {
		if m.Role == "system" {
			return m.Content, true
		}
	}
	return "", false
}

func TestContract_DeclaredSubscriptions(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(), contract.WithPluginConfig(map[string]any{
		"name":           "researcher",
		"system_prompt":  "You are a researcher.",
		"max_iterations": 3,
	}))
	h.AssertSubscribesTo("tool.invoke", "tool.register")
}

// TestSessionContext_PrependedAheadOfSystemPrompt asserts a subagent's
// system-role message starts with a <session_context> block built from the
// current session's non-reserved Labels, followed by the configured
// systemPrompt — and that a reserved ("_"-prefixed) key never reaches the
// prompt. Unlike react/orchestrator, subagent has no pre-existing
// XMLWrap-based prompt builder, so this is new mechanism, not an insertion
// into an existing one (see E3-S3).
func TestSessionContext_PrependedAheadOfSystemPrompt(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(), contract.WithPluginConfig(map[string]any{
		"system_prompt": "Base system prompt.",
	}))
	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	if err := p.session.SetLabel("team", "acme"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	if err := p.session.SetReservedLabel("_principal_id", "user-42"); err != nil {
		t.Fatalf("SetReservedLabel: %v", err)
	}

	lastReq := wireMockLLM(h)

	h.Inject("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-1",
		Name:          p.toolName,
		Arguments:     map[string]any{"task": "investigate something"},
	})

	sys, ok := subagentSystemMessage(t, lastReq())
	if !ok {
		t.Fatal("no system-role message in llm.request")
	}

	if !strings.HasPrefix(sys, "<session_context>") {
		t.Errorf("system prompt does not start with <session_context>: %s", sys)
	}
	if !strings.Contains(sys, "team: acme") {
		t.Errorf("system prompt missing rendered team label: %s", sys)
	}
	if !strings.Contains(sys, "Base system prompt.") {
		t.Errorf("system prompt missing original systemPrompt content: %s", sys)
	}
	if strings.Index(sys, "</session_context>") > strings.Index(sys, "Base system prompt.") {
		t.Errorf("session_context block not ahead of systemPrompt: %s", sys)
	}
	if strings.Contains(sys, "_principal_id") || strings.Contains(sys, "user-42") {
		t.Errorf("system prompt leaked reserved label: %s", sys)
	}
}

// TestSessionContext_AbsentWhenNoNonReservedLabels asserts the system-role
// message is byte-identical to the plain configured systemPrompt (no
// wrapper, no whitespace change) when the session has no non-reserved
// Labels — including when only a reserved label is present.
func TestSessionContext_AbsentWhenNoNonReservedLabels(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(), contract.WithPluginConfig(map[string]any{
		"system_prompt": "Base system prompt.",
	}))
	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	// Only a reserved label present — should still render nothing.
	if err := p.session.SetReservedLabel("_principal_id", "user-42"); err != nil {
		t.Fatalf("SetReservedLabel: %v", err)
	}

	lastReq := wireMockLLM(h)

	h.Inject("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-1",
		Name:          p.toolName,
		Arguments:     map[string]any{"task": "investigate something"},
	})

	sys, ok := subagentSystemMessage(t, lastReq())
	if !ok {
		t.Fatal("no system-role message in llm.request")
	}

	if sys != "Base system prompt." {
		t.Errorf("system prompt should be byte-identical to configured systemPrompt, got: %q", sys)
	}
}

func TestContract_DeclaredEmissions(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(), contract.WithPluginConfig(map[string]any{
		"name":          "researcher",
		"system_prompt": "research",
	}))
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{
		"tool.register",
		"tool.result",
		"llm.request",
		"subagent.started",
		"subagent.complete",
	} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
