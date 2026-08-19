package a2aremote

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// TestContract_DeclarationMatchesBehaviour drives the plugin's whole event
// surface through a real bus and checks the declaration against what actually
// happened, rather than against intent.
//
// One successful delegation exercises every emission this plugin claims:
// tool.register at boot, the card-driven re-register, the subagent lifecycle
// pair, the vetoable before:tool.result and the tool.result itself.
func TestContract_DeclarationMatchesBehaviour(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{})
	h := contract.NewContract(t, New, contract.WithPluginConfig(
		oneAgent(agent.URL(), map[string]any{"description": "placeholder"})))

	h.AssertSubscribesTo("tool.invoke")

	// tool.register fires during Ready, before any event is injected.
	h.AssertEmitted("tool.register")

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "find things out"})
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}

	// The card-derived re-registration is part of the contract too: it is a
	// second tool.register, emitted from the worker goroutine.
	waitFor(t, "the card-derived tool.register", func() bool { return len(toolDefs(h)) >= 2 })

	h.AssertEmitted("subagent.started")
	h.AssertEmitted("subagent.complete")
	h.AssertEmitted("before:tool.result")
	h.AssertEmitted("tool.result")

	h.AssertNoUndeclaredEmissions()
}

// TestContract_FailureStaysOnTheDeclaredSurface pins the failure path to the
// same declaration. A remote that is down must produce a tool result, not a new
// event type and not silence.
func TestContract_FailureStaysOnTheDeclaredSurface(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(
		oneAgent(deadURL(t), nil)))

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})
	if res.Error == "" {
		t.Fatal("an unreachable remote must produce a tool error")
	}

	h.AssertEmitted("subagent.started")
	h.AssertEmitted("subagent.complete")
	h.AssertEmitted("tool.result")
	h.AssertNoUndeclaredEmissions()
}

// TestContract_NoProgressEventsYet is a negative contract. Republishing a
// remote run's incremental progress onto the local bus is a separate piece of
// work, and Emissions() must not claim it before the code does — an over-broad
// declaration is exactly the drift this harness exists to catch.
func TestContract_NoProgressEventsYet(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{})
	h := contract.NewContract(t, New, contract.WithPluginConfig(oneAgent(agent.URL(), nil)))

	invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})

	h.AssertNotEmitted("io.output")
	h.AssertNotEmitted("subagent.iteration")

	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, e := range []string{"io.output", "subagent.iteration", "llm.request", "tool.invoke"} {
		if declared[e] {
			t.Errorf("Emissions() declares %q, which this plugin does not emit", e)
		}
	}
}

// TestConfigSchemaCoversEveryParsedKey guards the boot-breaking drift the
// engine's `additionalProperties: false` turns a missing schema key into: a key
// parseConfig reads but the schema does not list aborts boot for every config
// that uses it.
func TestConfigSchemaCoversEveryParsedKey(t *testing.T) {
	schema := string(configSchemaBytes)

	pluginKeys := []string{
		cfgKeyAgents, cfgKeyCache, cfgKeyCacheSize, cfgKeyMaxDepth,
		cfgKeyBinding, cfgKeyValidateCard, cfgKeyStream, cfgKeyTimeout,
		cfgKeyRequestTimeout, cfgKeyMessageTimeout, cfgKeyStreamOpenTimeout,
		cfgKeyStreamIdleTimeout, cfgKeyExtensions, cfgKeyRetry,
	}
	agentKeys := []string{
		cfgKeyName, cfgKeyBaseURL, cfgKeyJSONRPCEndpoint, cfgKeyRESTEndpoint,
		cfgKeyToolName, cfgKeyDescription, cfgKeyPosture,
	}
	retryKeys := []string{cfgKeyRetryMaxAttempts, cfgKeyRetryBaseDelay, cfgKeyRetryMaxDelay}

	for _, key := range append(append(pluginKeys, agentKeys...), retryKeys...) {
		if !strings.Contains(schema, `"`+key+`"`) {
			t.Errorf("schema.json does not declare the %q key that parseConfig reads", key)
		}
	}
	// Every accepted binding spelling must be in the enum, or a valid config is
	// rejected at boot.
	for _, spelling := range bindingSpellings() {
		if !strings.Contains(schema, `"`+spelling+`"`) {
			t.Errorf("schema.json does not accept the binding spelling %q", spelling)
		}
	}
	if _, ok := bindingNames["jsonrpc"]; !ok {
		t.Error("jsonrpc must remain an accepted binding spelling")
	}
	if bindingNames["jsonrpc"] != a2a.BindingJSONRPC {
		t.Error("the jsonrpc spelling must map to the JSONRPC binding")
	}
}
