package hitl

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// TestToolTimeoutCancelsPendingRequestsForThatCall pins #219: a request
// raised on or for a tool call that timed out is cancelled (synthetic
// hitl.responded{Cancelled}) instead of staying open until shutdown. Two
// requests name call X — this plugin's own ask_user and one raised by
// another plugin (an approval gate shape) — and one names call Y. A
// tool.timeout for X cancels both X requests, unblocks ask_user so the
// pending set empties and the bus drains, and leaves Y open.
func TestToolTimeoutCancelsPendingRequestsForThatCall(t *testing.T) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{Bus: bus, Logger: newTestLogger(), Config: map[string]any{}}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	var mu sync.Mutex
	requested := map[string]bool{}
	responded := map[string]events.HITLResponse{}
	var results []events.ToolResult
	bus.Subscribe("hitl.requested", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.HITLRequest); ok {
			mu.Lock()
			requested[r.ID] = true
			mu.Unlock()
		}
	})
	bus.Subscribe("hitl.responded", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.HITLResponse); ok {
			mu.Lock()
			responded[r.RequestID] = r
			mu.Unlock()
		}
	})
	bus.Subscribe("tool.result", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.ToolResult); ok {
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		}
	})

	// ask_user on call X blocks inside its tool.invoke dispatch.
	askDone := make(chan struct{})
	go func() {
		defer close(askDone)
		_ = bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion,
			ID: "call-X", Name: "ask_user", TurnID: "turn-1",
			Arguments: map[string]any{"prompt": "still there?"}})
	}()
	askID := "hitl-turn-1-call-X"
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return requested[askID] })

	// A request some other plugin raised for call X, and one for call Y.
	_ = bus.Emit("hitl.requested", events.HITLRequest{SchemaVersion: events.HITLRequestVersion,
		ID: "approval-X", CallID: "call-X", Prompt: "approve X?"})
	_ = bus.Emit("hitl.requested", events.HITLRequest{SchemaVersion: events.HITLRequestVersion,
		ID: "approval-Y", CallID: "call-Y", Prompt: "approve Y?"})

	_ = bus.Emit("tool.timeout", events.ToolTimeout{SchemaVersion: events.ToolTimeoutVersion,
		ToolName: "ask_user", CallID: "call-X", Timeout: time.Second, TurnID: "turn-1"})

	select {
	case <-askDone:
	case <-time.After(2 * time.Second):
		t.Fatal("ask_user still blocked after tool.timeout for its call")
	}

	mu.Lock()
	for _, id := range []string{askID, "approval-X"} {
		r, ok := responded[id]
		if !ok || !r.Cancelled || r.CancelReason == "" {
			t.Errorf("request %s: response = %+v (present=%v); want cancelled with a reason", id, r, ok)
		}
	}
	if r, ok := responded["approval-Y"]; ok {
		t.Errorf("request for call Y was touched: %+v", r)
	}
	if len(results) != 1 || results[0].ID != "call-X" || results[0].Error == "" {
		t.Errorf("tool.result = %+v; want one error result for call-X", results)
	}
	mu.Unlock()

	p.mu.Lock()
	if len(p.pending) != 0 {
		t.Errorf("pending = %v; want empty", p.pending)
	}
	if _, ok := p.byCall["approval-Y"]; !ok || len(p.byCall) != 1 {
		t.Errorf("byCall = %v; want only approval-Y", p.byCall)
	}
	p.mu.Unlock()

	// Shutdown's drain must not wait on the cancelled call.
	drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := bus.Drain(drainCtx); err != nil {
		t.Fatalf("bus drain after timeout: %v", err)
	}
}

// TestToolTimeoutIgnoresRequestsWithoutCallID: a request naming no call is
// never matched, even by a tool.timeout with an empty CallID.
func TestToolTimeoutIgnoresRequestsWithoutCallID(t *testing.T) {
	bus := engine.NewEventBus()
	defer func() { _ = bus.Drain(context.TODO()) }()
	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{Bus: bus, Logger: newTestLogger(), Config: map[string]any{}}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	got := make(chan events.HITLResponse, 4)
	bus.Subscribe("hitl.responded", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.HITLResponse); ok {
			got <- r
		}
	})
	_ = bus.Emit("hitl.requested", events.HITLRequest{SchemaVersion: events.HITLRequestVersion,
		ID: "free-1", Prompt: "no call"})
	_ = bus.Emit("tool.timeout", events.ToolTimeout{SchemaVersion: events.ToolTimeoutVersion, ToolName: "x"})

	select {
	case r := <-got:
		t.Fatalf("unexpected hitl.responded: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
