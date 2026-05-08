package outputlength

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract_DeclaresSubscriptionsAndEmissions(t *testing.T) {
	h := contract.NewContract(t, New)

	h.AssertSubscribesTo("before:io.output")

	emissions := h.Plugin().Emissions()
	wantEmits := map[string]bool{"llm.request": false, "io.output": false}
	for _, e := range emissions {
		if _, ok := wantEmits[e]; ok {
			wantEmits[e] = true
		}
	}
	for k, found := range wantEmits {
		if !found {
			t.Errorf("Emissions() missing %q (got %v)", k, emissions)
		}
	}
}

func TestContract_OverLimit_NoRetries_EmitsSystemWarning(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"max_chars":   10,
		"max_retries": 0,
	}))

	output := &events.AgentOutput{
		SchemaVersion: events.AgentOutputVersion,
		Role:          "assistant",
		Content:       "this is way longer than ten characters and should trip the gate",
	}
	h.InjectVetoable("before:io.output", output)

	// With max_retries=0 the gate emits a system-role warning io.output
	// instead of vetoing.
	saw := false
	for _, e := range h.PluginEmissions() {
		if e.Type == "io.output" {
			if out, ok := e.Payload.(events.AgentOutput); ok && out.Role == "system" {
				saw = true
			}
		}
	}
	if !saw {
		t.Errorf("expected system-role io.output warning; emissions: %v", h.PluginEmissions())
	}
}

func TestContract_UnderLimit_NoEmit(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"max_chars":   1000,
		"max_retries": 0,
	}))

	output := &events.AgentOutput{
		SchemaVersion: events.AgentOutputVersion,
		Role:          "assistant",
		Content:       "short",
	}
	h.InjectVetoable("before:io.output", output)

	for _, e := range h.PluginEmissions() {
		if e.Type == "io.output" {
			t.Errorf("under-limit output should not emit io.output, got %v", e)
		}
	}
}

func TestContract_NonAssistantRole_PassesThrough(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"max_chars":   10,
		"max_retries": 0,
	}))

	output := &events.AgentOutput{
		SchemaVersion: events.AgentOutputVersion,
		Role:          "system",
		Content:       "this is way longer than ten characters but is system role",
	}
	h.InjectVetoable("before:io.output", output)

	for _, e := range h.PluginEmissions() {
		if e.Type == "io.output" {
			t.Errorf("system-role outputs should not be gated; got emit %+v", e)
		}
	}
}

// silence unused import linting if engine becomes unused after refactors.
var _ engine.EventBus = nil
