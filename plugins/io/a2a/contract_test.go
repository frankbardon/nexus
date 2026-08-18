package a2a

import (
	"net/http"
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// TestContract_TurnMapping asserts the declared event contract is the one the
// plugin actually exercises: an A2A message emits io.input (gated first), and
// nothing else reaches the bus.
//
// The turn is driven through a real listener and a real bus, so the declaration
// is checked against behaviour rather than against intent — which is the whole
// reason this harness exists.
func TestContract_TurnMapping(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(),
		contract.WithPluginConfig(testConfig(t, nil)))

	h.AssertSubscribesTo("agent.turn.start", "agent.turn.end", "llm.response", "io.output", "core.error")

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	// A scripted agent answers the turn so the task reaches a terminal state and
	// the request returns.
	h.Bus().Subscribe("io.input", func(e engine.Event[any]) {
		in, ok := e.Payload.(events.UserInput)
		if !ok {
			return
		}
		scriptedTurn("contract answer")(h.Bus(), in)
	}, engine.WithSource("test.agent"))

	rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
		jsonrpcBody(t, a2a.MethodSendMessage, sendMessageParams("drive a turn", "ctx-contract")))
	if rec.Code != http.StatusOK {
		t.Fatalf("SendMessage status = %d: %s", rec.Code, rec.Body)
	}

	h.AssertEmitted("io.input")
	h.AssertNoUndeclaredEmissions()
}

// TestContract_NonTurnOperationsStayOffTheBus pins the other half of the
// contract: only a message operation may reach the bus. An operation that is not
// implemented must not acquire a side effect through its refusal, and the task
// READ operations must not emit either — they answer from the store, and a read
// that emitted io.input would start a turn nobody asked for.
func TestContract_NonTurnOperationsStayOffTheBus(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(),
		contract.WithPluginConfig(testConfig(t, nil)))

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}
	calls := []struct {
		method string
		params map[string]any
	}{
		{a2a.MethodGetTask, map[string]any{"id": "task-1"}},
		{a2a.MethodListTasks, map[string]any{}},
		{a2a.MethodSubscribeToTask, map[string]any{"id": "task-1"}},
		// Still unimplemented, so this one also covers the refusal path.
		{a2a.MethodCancelTask, map[string]any{"id": "task-1"}},
	}
	for _, c := range calls {
		rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
			jsonrpcBody(t, c.method, c.params))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", c.method, rec.Code, rec.Body)
		}
	}

	h.AssertNotEmitted("io.input")
	h.AssertNoUndeclaredEmissions()
}

// TestContract_BootsThroughTheHarness asserts Init and Ready succeed under the
// harness's PluginContext, which is what the engine hands every plugin.
func TestContract_BootsThroughTheHarness(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(),
		contract.WithPluginConfig(testConfig(t, map[string]any{
			"bearer_token": "s3cret",
		})))

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}
	card := p.Card()
	if err := a2a.ValidateAgentCard(&card); err != nil {
		t.Fatalf("the plugin booted with an unservable card: %v", err)
	}
	if len(card.SecuritySchemes) == 0 {
		t.Error("a guarded listener published no securitySchemes for a client to discover")
	}
	// The card advertises streaming now that both streaming operations —
	// SendStreamingMessage and SubscribeToTask — are wired.
	if !card.Capabilities.Streaming {
		t.Error("capabilities.streaming is false while the streaming operations are implemented")
	}
}
