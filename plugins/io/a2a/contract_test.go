package a2a

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// settled blocks until every event in flight on the harness bus has finished
// dispatching. Every emission assertion about a TURN must go through it, and
// deleting it will not fail the suite on the next run — it will fail it once
// every few dozen runs of the package on a CPU-contended machine (E3-S1 saw one
// in sixteen under a full `make test-race`), which is worse.
//
// The window it closes is not a product defect. startTurn emits before:io.input
// and io.input from a GOROUTINE on purpose (documented at that call site: the
// whole turn runs inside the dispatch, so the HTTP goroutine has to be draining
// frames already or a streaming client sees nothing until the turn is over). The
// harness, meanwhile, records events from a SubscribeAll wildcard, and the bus
// dispatches wildcards only AFTER every typed subscriber has returned. The
// scripted agent that answers the turn — and by ending it releases the HTTP
// response these tests read — IS one of those typed subscribers. So the response
// can be complete while io.input has not yet been appended to the captured
// stream: every event the agent produced downstream is on record, but not the
// io.input that caused them. That is the exact shape of the failure this
// synchronisation removes.
//
// Drain is the seam the harness already has (NewContract's own cleanup uses it),
// and it is a barrier on the emission being asserted rather than a sleep: it
// waits on the bus's in-flight count, and the io.input emit is by construction
// still in flight when the response completes, because the agent that completed
// it ran inside that emit. The deadline is generous but finite so a genuinely
// wedged turn is a test failure and not a hung package.
func settled(t *testing.T, h *contract.ContractHarness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.Bus().Drain(ctx); err != nil {
		t.Fatalf("harness bus did not settle before asserting emissions: %v", err)
	}
}

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

	h.AssertSubscribesTo("agent.turn.start", "agent.turn.end", "llm.request", "llm.response",
		"io.output", "core.error", "tool.invoke", "tool.result", "thinking.step",
		"subagent.started", "subagent.iteration", "subagent.complete")

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

	settled(t, h)
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
		// CancelTask is implemented now, and answers TaskNotFoundError for an id
		// nothing minted — which is exactly the point here: refusing a task must
		// not reach the bus either.
		{a2a.MethodCancelTask, map[string]any{"id": "task-1"}},
		// Still unimplemented, so this one covers the not-implemented refusal.
		{a2a.MethodCreateTaskPushNotificationConfig, map[string]any{
			"parent": "tasks/task-1",
			"config": map[string]any{"name": "tasks/task-1/pushNotificationConfigs/1"},
		}},
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

// TestContract_ObservingATurnStaysOffTheBus pins the direction of the artifact
// and telemetry work: the plugin RENDERS what it sees on the bus and publishes
// nothing new because of it. A tool-heavy turn produces artifacts and extension
// telemetry on the wire and exactly one emission — the io.input that started it.
func TestContract_ObservingATurnStaysOffTheBus(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(),
		contract.WithPluginConfig(testConfig(t, nil)))

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}
	h.Bus().Subscribe("io.input", func(e engine.Event[any]) {
		in, ok := e.Payload.(events.UserInput)
		if !ok {
			return
		}
		telemetryTurn("contract answer")(h.Bus(), in)
	}, engine.WithSource("test.agent"))

	rec := do(t, p.server, http.MethodPost, "/a2a",
		withVersion("1.0"), withExtensions(a2a.NexusExtensionURI),
		jsonrpcBody(t, a2a.MethodSendStreamingMessage,
			sendMessageParams("drive a tool turn", "ctx-contract-tools")))
	if rec.Code != http.StatusOK {
		t.Fatalf("SendStreamingMessage status = %d: %s", rec.Code, rec.Body)
	}

	fs := frames(t, rec.Body.Bytes())
	if len(nexusEvents(t, fs)) == 0 {
		t.Error("an opted-in stream carried no extension telemetry")
	}
	artifacts := 0
	for _, f := range fs {
		if f.Kind() == a2a.StreamPayloadArtifactUpdate {
			artifacts++
		}
	}
	if artifacts < 2 {
		t.Errorf("artifact frames = %d, want the tool result and the response", artifacts)
	}

	settled(t, h)
	h.AssertEmitted("io.input")
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
