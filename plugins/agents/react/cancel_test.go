package react

import (
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// discardLogger keeps handleCancelEvent's own Info line out of test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestHandleToolResult_IgnoresALateResultForACancelledTurn proves a tool that
// was in flight when the cancel landed cannot restart the turn on its way out.
//
// A tool still running when cancel.active arrives returns AFTER it by
// definition, and handleCancelEvent has by then zeroed pendingToolCalls and
// emitted agent.turn.end. Without the cancelled guard the decrement in
// handleToolResult goes to -1, allDone reads true, and sendLLMRequest fires one
// more exchange on a turn that has already ended — after nexus.io.agui's
// handleTurnEnd released the run's bound identity, so the request reaches the
// provider de-authenticated and its response is still recorded into the
// persisted conversation by nexus.memory.capped.
func TestHandleToolResult_IgnoresALateResultForACancelledTurn(t *testing.T) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = discardLogger()
	p.currentTurnID = "turn-1"
	p.pendingToolCalls = 1

	var requests atomic.Int64
	bus.Subscribe("llm.request", func(engine.Event[any]) { requests.Add(1) })

	p.handleCancelEvent(engine.Event[any]{Payload: events.CancelActive{
		SchemaVersion: events.CancelActiveVersion,
		TurnID:        "turn-1",
	}})

	// The delegate whose own LLM call the cancel aborted, returning late.
	p.handleToolResult(events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            "call_0_delegate",
		Name:          "delegate",
		Error:         "context canceled",
		TurnID:        "turn-1",
	})

	if got := requests.Load(); got != 0 {
		t.Errorf("llm.request emitted %d time(s) after the turn was cancelled, want 0", got)
	}
	if p.pendingToolCalls != 0 {
		t.Errorf("pendingToolCalls = %d, want 0 (a cancelled turn's counter must not go negative)", p.pendingToolCalls)
	}
}

// TestHandleToolResult_StillDrivesAnUncancelledTurn is the negative control:
// the guard above must not stop an ordinary result from advancing the loop.
func TestHandleToolResult_StillDrivesAnUncancelledTurn(t *testing.T) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = discardLogger()
	p.currentTurnID = "turn-1"
	p.pendingToolCalls = 1

	var requests atomic.Int64
	bus.Subscribe("llm.request", func(engine.Event[any]) { requests.Add(1) })

	p.handleToolResult(events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            "call_0_delegate",
		Name:          "delegate",
		Output:        "done",
		TurnID:        "turn-1",
	})

	if got := requests.Load(); got != 1 {
		t.Errorf("llm.request emitted %d time(s) on a live turn, want 1", got)
	}
}

// TestHandleToolResult_ResumeStillWorksAfterALateResult proves the guard reads
// the cancelled FLAG rather than clearing currentTurnID: a cancelled turn must
// still be resumable, which is what cancel.complete{Resumable: true} promises.
func TestHandleToolResult_ResumeStillWorksAfterALateResult(t *testing.T) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = discardLogger()
	p.currentTurnID = "turn-1"
	p.pendingToolCalls = 1

	var requests atomic.Int64
	bus.Subscribe("llm.request", func(engine.Event[any]) { requests.Add(1) })

	p.handleCancelEvent(engine.Event[any]{Payload: events.CancelActive{
		SchemaVersion: events.CancelActiveVersion,
		TurnID:        "turn-1",
	}})
	p.handleToolResult(events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            "call_0_delegate",
		Name:          "delegate",
		TurnID:        "turn-1",
	})
	if got := requests.Load(); got != 0 {
		t.Fatalf("llm.request emitted %d time(s) while cancelled, want 0", got)
	}

	p.handleResumeEvent(engine.Event[any]{Payload: events.CancelResume{
		SchemaVersion: events.CancelResumeVersion,
		TurnID:        "turn-1",
	}})

	if got := requests.Load(); got != 1 {
		t.Errorf("llm.request emitted %d time(s) after resume, want 1", got)
	}
}

// TestANewInputClearsTheCancelledFlag is the one that matters to a user.
//
// handleInput starts a new turn: it regenerates currentTurnID and resets the
// iteration, the pending count, the plan and the turn context. It did NOT reset
// cancelled — and the only other writer that clears it is handleResumeEvent,
// which needs a /resume the AG-UI transport has no path to.
//
// So one cancelled turn left the flag set for the life of the agent, and
// handleLLMResponse's own cancelled guard then dropped the response to EVERY
// later turn on that session. Measured end to end against the local stack: the
// turn after a client disconnect emitted RUN_STARTED and STEP_STARTED and then
// nothing at all — no tool call, no answer, no RUN_ERROR, no timeout. The
// session was dead and looked like it was thinking.
//
// Clearing it costs no resumability: handleInput has already regenerated
// currentTurnID by this point, and handleResumeEvent resumes p.currentTurnID,
// so the cancelled turn is unresumable the moment a new question is asked
// whatever this flag says.
func TestANewInputClearsTheCancelledFlag(t *testing.T) {
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = discardLogger()
	p.currentTurnID = "turn-1"
	p.pendingToolCalls = 1

	var requests atomic.Int64
	bus.Subscribe("llm.request", func(engine.Event[any]) { requests.Add(1) })

	p.handleCancelEvent(engine.Event[any]{Payload: events.CancelActive{
		SchemaVersion: events.CancelActiveVersion,
		TurnID:        "turn-1",
	}})
	if !p.cancelled {
		t.Fatal("precondition: the turn should be cancelled")
	}

	// The user asks a new question.
	p.handleInput(events.UserInput{SchemaVersion: events.UserInputVersion, Content: "a new question"})

	if p.cancelled {
		t.Error("cancelled is still set after a new input; every response on this turn will be dropped")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("llm.request emitted %d time(s) for the new turn, want 1", got)
	}

	// And the new turn's response must actually drive the loop rather than
	// being dropped by the stale flag.
	p.handleLLMResponse(events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       "an answer",
		Metadata:      map[string]any{"task_kind": "react_main"},
	})
	if p.iteration != 1 {
		t.Errorf("iteration = %d after the new turn's response, want 1 (the response was dropped)", p.iteration)
	}
}
