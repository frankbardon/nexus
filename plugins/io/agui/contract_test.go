package agui

import (
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// TestContract_Subscriptions asserts the plugin's declared Subscriptions()
// cover every bus event the outbound translator wires in Init. The list is the
// canonical set plus the non-canonical events bridged as AG-UI Custom.
func TestContract_Subscriptions(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}))

	h.AssertSubscribesTo(
		"agent.turn.start",
		"agent.turn.end",
		"llm.stream.chunk",
		"llm.stream.end",
		"io.output",
		// Real agent tool calls ride tool.invoke (not tool.call); client-executed
		// tools ride the same event and additionally suspend the run.
		"tool.invoke",
		"tool.result",
		"thinking.step",
		// Virtual-run interrupt/cancel at the transport boundary.
		"hitl.requested",
		"hitl.cancel",
		// Per-run client tools are appended to this synchronous catalog snapshot.
		"tool.catalog.query",
	)
	// Non-canonical bus events ride the AG-UI Custom event and must be declared.
	h.AssertSubscribesTo(customBridgedEvents...)
}

// TestContract_EmitsInputOnRun asserts the runtime emission contract: starting a
// run publishes before:io.input then io.input, and no undeclared event types
// escape. startRun is the inbound seam the HTTP handler uses; driving it here
// exercises the same bus path without a socket round-trip.
func TestContract_EmitsInputOnRun(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}))

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	// Observe the emitted UserInput to confirm the inbound mapping is real.
	seen := make(chan events.UserInput, 1)
	h.Bus().Subscribe("io.input", func(e engine.Event[any]) {
		if in, ok := e.Payload.(events.UserInput); ok {
			select {
			case seen <- in:
			default:
			}
		}
	})

	run, started := p.startRun(runInput{
		threadID: "thread-contract",
		runID:    "run-contract",
		messages: []agui.Message{{ID: "m1", Role: "user", Content: "ping"}},
	})
	if !started {
		t.Fatal("startRun rejected on a fresh plugin")
	}
	t.Cleanup(func() {
		run.finish()
		p.endRun(run)
	})

	select {
	case in := <-seen:
		if in.Content != "ping" {
			t.Errorf("io.input content = %q, want ping", in.Content)
		}
		if in.SessionID != "thread-contract" {
			t.Errorf("io.input session = %q, want thread-contract", in.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for io.input emission")
	}

	// Declared emissions actually fired (before:io.input rides the veto path;
	// io.input is a plain emit captured by the harness).
	h.AssertEmitted("io.input")
	h.AssertNoUndeclaredEmissions()
}
