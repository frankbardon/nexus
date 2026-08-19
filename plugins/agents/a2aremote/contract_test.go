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

// TestContract_ProgressIsDeclaredAndEmitted replaces the negative contract this
// test used to be. Republishing a remote run's progress IS done now, so
// Emissions() claims it — and the claim is checked against what a real
// delegation actually puts on the bus, not against intent.
//
// The negative half survives, narrowed to the things this plugin still must not
// emit. hitl.responded is the one that matters: emitting it would be Nexus
// answering a human's question on their behalf.
func TestContract_ProgressIsDeclaredAndEmitted(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{
		frames: func(*a2a.SendMessageRequest) []a2a.StreamResponse {
			return narratedRun("t1", "c1", "reading the sources", "the answer")
		},
	})
	h := contract.NewContract(t, New, contract.WithPluginConfig(oneAgent(agent.URL(), nil)))

	invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})

	h.AssertEmitted("io.output")

	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, e := range []string{"io.output", "subagent.iteration", "hitl.requested", "hitl.cancel"} {
		if !declared[e] {
			t.Errorf("Emissions() does not declare %q, which this plugin emits", e)
		}
	}
	for _, e := range []string{"hitl.responded", "llm.request", "tool.invoke", "thinking.step", "cancel.request"} {
		if declared[e] {
			t.Errorf("Emissions() declares %q, which this plugin does not emit", e)
		}
	}

	h.AssertNotEmitted("hitl.responded")
	h.AssertNoUndeclaredEmissions()
}

// TestContract_ChainedHITLStaysOnTheDeclaredSurface drives the whole chained
// human-in-the-loop round trip through a real bus and checks that every event it
// produces is one the plugin declared.
func TestContract_ChainedHITLStaysOnTheDeclaredSurface(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{frames: askThenAnswer("t1", "c1", "which year?", &resumeRecorder{})})
	h := contract.NewContract(t, New, contract.WithPluginConfig(oneAgent(agent.URL(), nil)))

	h.AssertSubscribesTo("tool.invoke", "hitl.responded", "cancel.active")
	answerHITL(t, h, "1999")

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})
	if res.Error != "" {
		t.Fatalf("chained HITL should have completed the delegation: %s", res.Error)
	}

	h.AssertEmitted("before:hitl.requested")
	h.AssertEmitted("hitl.requested")
	assertPluginNeverAnswered(t, h)
	h.AssertNoUndeclaredEmissions()
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
		cfgKeyProgress, cfgKeyHITL,
	}
	hitlKeys := []string{cfgKeyHITLEnabled, cfgKeyHITLInputTimeout, cfgKeyHITLMaxRounds}
	agentKeys := []string{
		cfgKeyName, cfgKeyBaseURL, cfgKeyJSONRPCEndpoint, cfgKeyRESTEndpoint,
		cfgKeyToolName, cfgKeyDescription, cfgKeyPosture,
	}
	retryKeys := []string{cfgKeyRetryMaxAttempts, cfgKeyRetryBaseDelay, cfgKeyRetryMaxDelay}
	credentialKeys := []string{
		cfgKeyCredentials, cfgKeyCredType,
		cfgKeyToken, cfgKeyTokenEnv, cfgKeyCredHeader, cfgKeyBearerSchem,
		cfgKeyClientID, cfgKeyClientIDEnv, cfgKeyClientSecret, cfgKeyClientSecretEnv,
		cfgKeyTokenURL, cfgKeyScopes, cfgKeyAudience, cfgKeyAuthStyle, cfgKeyRefreshLeeway,
		cfgKeyCertFile, cfgKeyKeyFile, cfgKeyCAFile, cfgKeyServerName,
	}

	all := append(append(append(append(pluginKeys, agentKeys...), retryKeys...), hitlKeys...), credentialKeys...)
	for _, key := range all {
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
	// Every credential type the parser accepts must be in the schema's enum,
	// or a valid credentials block is rejected before Init ever sees it.
	for _, kind := range credentialKinds() {
		if !strings.Contains(schema, `"`+kind+`"`) {
			t.Errorf("schema.json does not accept the credential type %q", kind)
		}
	}
	for _, style := range []string{authStyleBasic, authStyleBody} {
		if !strings.Contains(schema, `"`+style+`"`) {
			t.Errorf("schema.json does not accept the auth style %q", style)
		}
	}
}
