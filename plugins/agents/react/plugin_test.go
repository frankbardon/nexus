package react

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// findLLMRequest returns the events.LLMRequest payload of the first captured
// "llm.request" plugin emission, failing the test if none was observed.
func findLLMRequest(t *testing.T, h *contract.ContractHarness) events.LLMRequest {
	t.Helper()
	for _, e := range h.PluginEmissions() {
		if e.Type != "llm.request" {
			continue
		}
		req, ok := e.Payload.(events.LLMRequest)
		if !ok {
			t.Fatalf("llm.request payload type = %T, want events.LLMRequest", e.Payload)
		}
		return req
	}
	t.Fatal("no llm.request emission observed")
	return events.LLMRequest{}
}

func systemMessage(t *testing.T, req events.LLMRequest) string {
	t.Helper()
	for _, m := range req.Messages {
		if m.Role == "system" {
			return m.Content
		}
	}
	t.Fatal("no system-role message in llm.request")
	return ""
}

// TestSessionContext_RendersNonReservedLabels asserts react's system prompt
// carries a <session_context> block reflecting the current session's
// non-reserved Labels, and that a reserved ("_"-prefixed) key present in the
// same Labels map — e.g. _principal_id, as AG-UI binds it — never reaches the
// prompt. This is the security-relevant half of E3-S2: a future change that
// lets a reserved key leak into the prompt breaks this test.
func TestSessionContext_RendersNonReservedLabels(t *testing.T) {
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

	h.Inject("io.input", events.UserInput{
		SchemaVersion: events.UserInputVersion,
		Content:       "hello",
		SessionID:     "sess-1",
	})

	req := findLLMRequest(t, h)
	sys := systemMessage(t, req)

	if !strings.Contains(sys, "<session_context>") {
		t.Errorf("system prompt missing <session_context>: %s", sys)
	}
	if !strings.Contains(sys, "team: acme") {
		t.Errorf("system prompt missing rendered team label: %s", sys)
	}
	if strings.Contains(sys, "_principal_id") || strings.Contains(sys, "user-42") {
		t.Errorf("system prompt leaked reserved label: %s", sys)
	}
}

// TestSessionContext_AbsentWhenNoNonReservedLabels asserts the
// <session_context> section is omitted entirely (not emitted as an empty
// element) when the session has no non-reserved Labels — mirroring how the
// neighboring skill_context/execution_plan/current_task sections are already
// conditionally included.
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

	h.Inject("io.input", events.UserInput{
		SchemaVersion: events.UserInputVersion,
		Content:       "hello",
		SessionID:     "sess-1",
	})

	req := findLLMRequest(t, h)
	sys := systemMessage(t, req)

	if strings.Contains(sys, "session_context") {
		t.Errorf("system prompt should omit session_context entirely: %s", sys)
	}
}
