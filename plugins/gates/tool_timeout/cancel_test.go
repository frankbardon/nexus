package tooltimeout

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// A handler that runs its work under the engine's per-call context and blocks
// on ctx.Done() must be released when the gate times the call out, and its
// late result must still be dropped.
func TestToolTimeout_CancelsHandlerContext(t *testing.T) {
	_, bus := newTestPlugin(t, 50*time.Millisecond, nil)
	calls := engine.NewToolCallContexts()
	t.Cleanup(calls.Attach(bus))

	var (
		mu      sync.Mutex
		results []events.ToolResult
	)
	bus.Subscribe("tool.result", func(e engine.Event[any]) {
		if r, ok := e.Payload.(events.ToolResult); ok {
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		}
	})

	handlerDone := make(chan error, 1)
	bus.Subscribe("tool.invoke", func(e engine.Event[any]) {
		tc := e.Payload.(events.ToolCall)
		ctx, release := calls.Begin(context.Background(), tc.ID)
		defer release()
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
		// Late real result, the way a tool answers after cancellation.
		res := events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: tc.ID, Name: tc.Name, Error: "cancelled"}
		if veto, err := bus.EmitVetoable("before:tool.result", &res); err == nil && !veto.Vetoed {
			_ = bus.Emit("tool.result", res)
		}
		handlerDone <- context.Cause(ctx)
	}, engine.WithPriority(50))

	start := time.Now()
	go func() {
		_ = bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion, ID: "slow-1", Name: "slow"})
	}()

	select {
	case cause := <-handlerDone:
		if !errors.Is(cause, engine.ErrToolCallTimedOut) {
			t.Fatalf("handler cause = %v, want ErrToolCallTimedOut", cause)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("handler took %s to return after a 50ms timeout", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler blocked on ctx.Done() was never released by the timeout")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(results) != 1 {
		t.Fatalf("got %d tool.result events, want exactly the synthetic one: %+v", len(results), results)
	}
	if results[0].Error == "cancelled" {
		t.Fatal("the late real result got through instead of the synthetic timeout result")
	}
}
