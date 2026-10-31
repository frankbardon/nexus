package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
)

func TestToolCallContexts_TimeoutEventCancelsWithCause(t *testing.T) {
	bus := NewEventBus()
	r := NewToolCallContexts()
	unsub := r.Attach(bus)
	defer unsub()

	ctx, release := r.Begin(context.Background(), "c1")
	defer release()
	other, releaseOther := r.Begin(context.Background(), "c2")
	defer releaseOther()

	_ = bus.Emit("tool.timeout", events.ToolTimeout{SchemaVersion: events.ToolTimeoutVersion,
		ToolName: "slow", CallID: "c1", Timeout: time.Second})

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("timed-out call's context was not cancelled")
	}
	if cause := context.Cause(ctx); !errors.Is(cause, ErrToolCallTimedOut) {
		t.Fatalf("cause = %v, want ErrToolCallTimedOut", cause)
	}
	if other.Err() != nil {
		t.Fatal("a tool.timeout for c1 cancelled c2")
	}
}

func TestToolCallContexts_ReleaseUnregisters(t *testing.T) {
	r := NewToolCallContexts()
	ctx, release := r.Begin(context.Background(), "c1")
	if !r.Live("c1") {
		t.Fatal("Begin did not register the call")
	}
	release()
	release() // idempotent
	if r.Live("c1") {
		t.Fatal("release left the call registered")
	}
	if ctx.Err() == nil {
		t.Fatal("release did not release the context")
	}
	if r.Cancel("c1", ErrToolCallTimedOut) {
		t.Fatal("Cancel reported a live call after release")
	}
}

func TestToolCallContexts_DuplicateIDsAllCancelled(t *testing.T) {
	r := NewToolCallContexts()
	a, ra := r.Begin(context.Background(), "dup")
	b, rb := r.Begin(context.Background(), "dup")
	ra()
	if !r.Live("dup") {
		t.Fatal("releasing one entry dropped the other")
	}
	if !r.Cancel("dup", nil) {
		t.Fatal("Cancel found no live call")
	}
	if b.Err() == nil || a.Err() == nil {
		t.Fatal("expected both contexts done")
	}
	rb()
}

func TestToolCallContexts_CancelAll(t *testing.T) {
	r := NewToolCallContexts()
	a, ra := r.Begin(context.Background(), "a")
	defer ra()
	b, rb := r.Begin(context.Background(), "b")
	defer rb()
	r.CancelAll(ErrToolCallsStopped)
	for _, c := range []context.Context{a, b} {
		if !errors.Is(context.Cause(c), ErrToolCallsStopped) {
			t.Fatalf("cause = %v, want ErrToolCallsStopped", context.Cause(c))
		}
	}
}

func TestToolCallContexts_NilSafe(t *testing.T) {
	var r *ToolCallContexts
	ctx, release := r.Begin(context.Background(), "c1")
	if ctx.Err() != nil {
		t.Fatal("nil registry returned a done context")
	}
	if r.Cancel("c1", nil) || r.Live("c1") {
		t.Fatal("nil registry reported a live call")
	}
	r.CancelAll(nil)
	r.Attach(NewEventBus())()
	release()
	if ctx.Err() == nil {
		t.Fatal("release did not release the context")
	}
}

func TestEngine_ToolCallsWiredIntoBootAndStop(t *testing.T) {
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = filepath.Join(root, "sessions")
	cfg.Core.Storage.Root = root
	eng := newFromConfig(cfg)
	if eng.ToolCalls == nil || eng.Lifecycle.toolCalls != eng.ToolCalls {
		t.Fatal("engine and lifecycle must share one ToolCalls registry")
	}
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}

	timedOut, releaseTimedOut := eng.ToolCalls.Begin(context.Background(), "t1")
	defer releaseTimedOut()
	_ = eng.Bus.Emit("tool.timeout", events.ToolTimeout{SchemaVersion: events.ToolTimeoutVersion,
		ToolName: "slow", CallID: "t1", Timeout: time.Second})
	if !errors.Is(context.Cause(timedOut), ErrToolCallTimedOut) {
		t.Fatalf("booted engine did not cancel on tool.timeout: cause %v", context.Cause(timedOut))
	}

	live, releaseLive := eng.ToolCalls.Begin(context.Background(), "t2")
	defer releaseLive()
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !errors.Is(context.Cause(live), ErrToolCallsStopped) {
		t.Fatalf("Stop did not cancel the live call: cause %v", context.Cause(live))
	}
}
