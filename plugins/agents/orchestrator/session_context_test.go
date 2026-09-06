package orchestrator

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// findLLMRequestBySource returns the events.LLMRequest payload of the first
// captured "llm.request" plugin emission whose Metadata["_source"] matches
// source, failing the test if none was observed.
func findLLMRequestBySource(t *testing.T, h *contract.ContractHarness, source string) events.LLMRequest {
	t.Helper()
	for _, e := range h.PluginEmissions() {
		if e.Type != "llm.request" {
			continue
		}
		req, ok := e.Payload.(events.LLMRequest)
		if !ok {
			t.Fatalf("llm.request payload type = %T, want events.LLMRequest", e.Payload)
		}
		if s, _ := req.Metadata["_source"].(string); s == source {
			return req
		}
	}
	t.Fatalf("no llm.request emission with _source=%q observed", source)
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

// TestSessionContext_DecomposePromptRendersNonReservedLabels asserts the
// worker-prompt (decompose) path's system prompt carries a <session_context>
// block reflecting the current session's non-reserved Labels, and that a
// reserved ("_"-prefixed) key present in the same Labels map never reaches
// the prompt.
func TestSessionContext_DecomposePromptRendersNonReservedLabels(t *testing.T) {
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

	req := findLLMRequestBySource(t, h, decomposeSource)
	sys := systemMessage(t, req)

	if !strings.Contains(sys, "<session_context>") {
		t.Errorf("decompose system prompt missing <session_context>: %s", sys)
	}
	if !strings.Contains(sys, "team: acme") {
		t.Errorf("decompose system prompt missing rendered team label: %s", sys)
	}
	if strings.Contains(sys, "_principal_id") || strings.Contains(sys, "user-42") {
		t.Errorf("decompose system prompt leaked reserved label: %s", sys)
	}
}

// TestSessionContext_DecomposePromptAbsentWhenNoNonReservedLabels asserts the
// <session_context> section is omitted entirely from the decompose prompt
// when the session has no non-reserved Labels.
func TestSessionContext_DecomposePromptAbsentWhenNoNonReservedLabels(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(), contract.WithPluginConfig(map[string]any{
		"system_prompt": "Base system prompt.",
	}))
	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	if err := p.session.SetReservedLabel("_principal_id", "user-42"); err != nil {
		t.Fatalf("SetReservedLabel: %v", err)
	}

	h.Inject("io.input", events.UserInput{
		SchemaVersion: events.UserInputVersion,
		Content:       "hello",
		SessionID:     "sess-1",
	})

	req := findLLMRequestBySource(t, h, decomposeSource)
	sys := systemMessage(t, req)

	if strings.Contains(sys, "session_context") {
		t.Errorf("decompose system prompt should omit session_context entirely: %s", sys)
	}
}

// TestSessionContext_SynthesisPromptRendersNonReservedLabels asserts the
// synthesis path's system prompt carries the same <session_context> block,
// built directly by calling sendSynthesizeRequest (the synthesis phase is
// normally reached only after a full decompose/dispatch/complete cycle,
// which is orthogonal to this prompt-content assertion).
func TestSessionContext_SynthesisPromptRendersNonReservedLabels(t *testing.T) {
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

	p.mu.Lock()
	p.originalInput = "do the thing"
	p.mu.Unlock()
	p.sendSynthesizeRequest()

	req := findLLMRequestBySource(t, h, synthesizeSource)
	sys := systemMessage(t, req)

	if !strings.Contains(sys, "<session_context>") {
		t.Errorf("synthesis system prompt missing <session_context>: %s", sys)
	}
	if !strings.Contains(sys, "team: acme") {
		t.Errorf("synthesis system prompt missing rendered team label: %s", sys)
	}
	if strings.Contains(sys, "_principal_id") || strings.Contains(sys, "user-42") {
		t.Errorf("synthesis system prompt leaked reserved label: %s", sys)
	}
}

// TestSessionContext_SynthesisPromptAbsentWhenNoNonReservedLabels asserts the
// <session_context> section is omitted entirely from the synthesis prompt
// when the session has no non-reserved Labels.
func TestSessionContext_SynthesisPromptAbsentWhenNoNonReservedLabels(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(), contract.WithPluginConfig(map[string]any{
		"system_prompt": "Base system prompt.",
	}))
	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	if err := p.session.SetReservedLabel("_principal_id", "user-42"); err != nil {
		t.Fatalf("SetReservedLabel: %v", err)
	}

	p.mu.Lock()
	p.originalInput = "do the thing"
	p.mu.Unlock()
	p.sendSynthesizeRequest()

	req := findLLMRequestBySource(t, h, synthesizeSource)
	sys := systemMessage(t, req)

	if strings.Contains(sys, "session_context") {
		t.Errorf("synthesis system prompt should omit session_context entirely: %s", sys)
	}
}
