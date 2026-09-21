package agui

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// This file extends run_scoping_test.go's one rule — a turn's events belong to
// the run that CARRIES that turn, never to whichever run holds the slot — over
// the two HITL handlers, which were left unscoped when the content handlers
// were done.
//
// They are the sharpest of the set on this transport. handleHITLRequested does
// not merely render onto the successor's stream: it PARKS it, ending the SSE
// with an interrupt outcome for a question that client never asked, leaving
// the successor's own turn running with no stream at all, and recording the
// interruptId → request mapping against the WRONG thread and run, so the
// resume POST that follows answers the departed turn's question.
// handleHITLCancel is worse again — cancelTerminal closes the stream of
// whatever holds the slot with a cancelled outcome it had no part in.
//
// The window is the one handleTurnEnd already guards: a disconnect frees the
// slot and only THEN asks the agent to stop, so the abandoned turn is still
// alive, still hitting gates and still able to ask a question when the next
// POST has taken the slot.

// emitHITLRequested publishes a hitl.requested carrying a specific turn id, the
// way nexus.control.hitl does for an ask_user tool call.
func emitHITLRequested(t *testing.T, h *contract.ContractHarness, requestID, turnID, prompt string) {
	t.Helper()
	if err := h.Bus().Emit("hitl.requested", events.HITLRequest{
		SchemaVersion: events.HITLRequestVersion,
		ID:            requestID,
		TurnID:        turnID,
		Prompt:        prompt,
		Mode:          events.HITLModeFreeText,
	}); err != nil {
		t.Fatalf("emit hitl.requested: %v", err)
	}
}

// emitHITLCancel publishes a hitl.cancel. Note what it CANNOT carry:
// events.HITLCancel has no TurnID field at all, which is why the handler has to
// recover the turn from the pending mapping it recorded when the question was
// asked.
func emitHITLCancel(t *testing.T, h *contract.ContractHarness, requestID string) {
	t.Helper()
	if err := h.Bus().Emit("hitl.cancel", events.HITLCancel{
		SchemaVersion: events.HITLCancelVersion,
		RequestID:     requestID,
	}); err != nil {
		t.Fatalf("emit hitl.cancel: %v", err)
	}
}

// pendingCount reports how many interrupt correlations the plugin is holding.
func pendingCount(p *Plugin) int {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	return len(p.pending)
}

// hitlPlugin builds a contract harness around the plugin and returns it typed.
func hitlPlugin(t *testing.T) (*contract.ContractHarness, *Plugin) {
	t.Helper()
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}), contract.WithSession())
	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}
	return h, p
}

// TestContract_AnAbandonedTurnsQuestionDoesNotParkTheNextRun is the reported
// defect on the request half, and it exercises the retired-turn arm: run B has
// already bound its own turn, so "this run has bound none" no longer
// discriminates and only the record endRun kept can.
//
// The assertion is not merely that B survived. It is that B's own turn RAN end
// to end afterwards — its step opened and closed, its own answer reached its
// own stream, its own turn end terminated it — because a run that is parked or
// closed satisfies neither.
func TestContract_AnAbandonedTurnsQuestionDoesNotParkTheNextRun(t *testing.T) {
	h, p := hitlPlugin(t)

	runA := startedRun(t, p, "thread-ask", "run-a")
	emitTurn(t, h, "agent.turn.start", "turn-a")
	// The disconnect: the handler returns, the slot frees, turn A does not stop.
	p.endRun(runA)

	runB := startedRun(t, p, "thread-ask", "run-b")
	emitTurn(t, h, "agent.turn.start", "turn-b")

	// The abandoned turn hits an ask_user gate and asks its question.
	emitHITLRequested(t, h, "req-a", "turn-a", "May I proceed?")

	if runB.isSuspended() {
		t.Fatal("run B was parked awaiting an answer to another turn's question; that is the defect")
	}
	if closed(runB) {
		t.Fatal("run B's stream was ended by another turn's question")
	}
	if p.currentRun() != runB {
		t.Fatal("run B lost the active slot to a question it never asked")
	}
	if n := pendingCount(p); n != 0 {
		t.Fatalf("an interrupt correlation was recorded against the wrong run: %d pending", n)
	}

	// Run B's own turn now runs exactly as it would have with no race at all.
	emitOutput(t, h, "turn-b", "forty two")
	emitTurn(t, h, "agent.turn.end", "turn-b")

	if !closed(runB) {
		t.Fatal("run B's own turn ended and did not finish its stream")
	}
	got := queued(runB)
	gotTypes := types(got)
	if len(gotTypes) == 0 || gotTypes[0] != agui.EventRunStarted {
		t.Errorf("run B's stream does not open on RunStarted: %v", gotTypes)
	}
	for _, want := range []agui.EventType{
		agui.EventRunStarted,
		agui.EventStepStarted,
		agui.EventStepFinished,
		agui.EventRunFinished,
	} {
		if !contains(gotTypes, want) {
			t.Errorf("run B's stream has no %s: %v", want, gotTypes)
		}
	}
	if !contains(texts(got), "forty two") {
		t.Errorf("run B's own answer is missing from its stream: %q", texts(got))
	}
}

// TestContract_AQuestionArrivingBeforeTheNextRunsTurnStartsIsRefused isolates
// the OTHER arm: run B holds the slot and has bound no turn yet, and the
// question names a turn that is not the retired record either — a still-live
// turn from a run that departed earlier. Only "a named turn at a run that bound
// none" can refuse this one, so the arm is load-bearing on its own.
func TestContract_AQuestionArrivingBeforeTheNextRunsTurnStartsIsRefused(t *testing.T) {
	h, p := hitlPlugin(t)

	runA := startedRun(t, p, "thread-early", "run-a")
	emitTurn(t, h, "agent.turn.start", "turn-a")
	p.endRun(runA) // retiredTurn is now turn-a, and nothing else.

	runB := startedRun(t, p, "thread-early", "run-b")
	// No agent.turn.start for B yet: its io.input has not reached an agent.

	emitHITLRequested(t, h, "req-c", "turn-c", "May I proceed?")

	if runB.isSuspended() {
		t.Fatal("run B was parked by a question from a turn it never started")
	}
	if closed(runB) {
		t.Fatal("run B's stream was ended before its own turn began")
	}
	if n := pendingCount(p); n != 0 {
		t.Fatalf("an interrupt correlation was recorded for a foreign turn: %d pending", n)
	}
}

// TestContract_ARetractedQuestionFromADepartedTurnDoesNotCancelTheNextRun is
// the cancel half, and it is the one that had to recover its discriminator:
// events.HITLCancel carries no turn id, so the handler reads the turn back out
// of the pending mapping the question recorded.
//
// Both halves are asserted, because they are independent: the successor's
// stream survives, AND the retracted request's correlation is dropped anyway —
// leaving it behind would let a resume POST answer a question that no longer
// exists.
func TestContract_ARetractedQuestionFromADepartedTurnDoesNotCancelTheNextRun(t *testing.T) {
	h, p := hitlPlugin(t)

	runA := startedRun(t, p, "thread-retract", "run-a")
	emitTurn(t, h, "agent.turn.start", "turn-a")

	// Turn A asks its question and parks run A: the stream ends with an
	// interrupt outcome and the slot is freed with the agent still blocked.
	emitHITLRequested(t, h, "req-a", "turn-a", "May I proceed?")
	if !runA.isSuspended() {
		t.Fatal("run A was not parked by its own turn's question")
	}
	if pendingCount(p) != 1 {
		t.Fatalf("run A's interrupt was not recorded: %d pending", pendingCount(p))
	}

	runB := startedRun(t, p, "thread-retract", "run-b")
	emitTurn(t, h, "agent.turn.start", "turn-b")

	// The departed turn's question is retracted while run B holds the slot.
	emitHITLCancel(t, h, "req-a")

	if closed(runB) {
		t.Fatal("run B's stream was terminated by the retraction of another turn's question; that is the defect")
	}
	if p.currentRun() != runB {
		t.Fatal("run B lost the active slot to another turn's retraction")
	}
	if n := pendingCount(p); n != 0 {
		t.Fatalf("the retracted request's correlation was not dropped: %d pending", n)
	}

	// And run B still terminates on its own turn.
	emitTurn(t, h, "agent.turn.end", "turn-b")
	if !closed(runB) {
		t.Fatal("run B's own turn ended and did not finish its stream")
	}
	if contains(types(queued(runB)), agui.EventRunError) {
		t.Error("run B was errored rather than run")
	}
}

// TestContract_AQuestionFromThisRunsOwnTurnStillParksIt is the first
// non-regression control: the whole point of the virtual-run model is that a
// question asked BY this run's turn ends this run's stream with an interrupt.
func TestContract_AQuestionFromThisRunsOwnTurnStillParksIt(t *testing.T) {
	h, p := hitlPlugin(t)

	r := startedRun(t, p, "thread-own", "run-own")
	emitTurn(t, h, "agent.turn.start", "turn-own")
	emitHITLRequested(t, h, "req-own", "turn-own", "May I proceed?")

	if !r.isSuspended() {
		t.Fatal("a run was not parked by its own turn's question")
	}
	if pendingCount(p) != 1 {
		t.Fatalf("the interrupt correlation was not recorded: %d pending", pendingCount(p))
	}
	if !contains(types(queued(r)), agui.EventRunFinished) {
		t.Error("the parked run's stream did not terminate with an interrupt outcome")
	}
}

// TestContract_AQuestionNamingNoTurnStillParksTheRun is the second, and it is
// why the content rule is used here rather than a strict equality match. Two of
// the five hitl.requested emitters set no TurnID at all —
// nexus.gate.approval_policy carries the turn in its ActionRef metadata
// instead, and plugins/memory's approval helper names none — so a strict match
// would drop the question, leave the agent blocked on an answer no client can
// give, and park the turn forever.
func TestContract_AQuestionNamingNoTurnStillParksTheRun(t *testing.T) {
	h, p := hitlPlugin(t)

	r := startedRun(t, p, "thread-anon", "run-anon")
	emitTurn(t, h, "agent.turn.start", "turn-anon")
	emitHITLRequested(t, h, "req-anon", "", "Approve this write?")

	if !r.isSuspended() {
		t.Fatal("a gate's question naming no turn was dropped and the agent is blocked on it forever")
	}
	if pendingCount(p) != 1 {
		t.Fatalf("the interrupt correlation was not recorded: %d pending", pendingCount(p))
	}
}

// TestContract_AnUncorrelatableRetractionStillCancelsTheRun is the third: a
// hitl.cancel for a request this plugin never rendered — the retraction beat
// the request to the listener, which is the case the handler was originally
// written for — recovers no turn and therefore terminates the current run
// exactly as it always did. Uncorrelatable is TAKEN, the same asymmetry
// runOwnsTurnEnd sits on: leaving a client on a stream that never ends is worse
// than ending one early.
func TestContract_AnUncorrelatableRetractionStillCancelsTheRun(t *testing.T) {
	h, p := hitlPlugin(t)

	r := startedRun(t, p, "thread-unknown", "run-unknown")
	emitTurn(t, h, "agent.turn.start", "turn-unknown")

	emitHITLCancel(t, h, "req-never-seen")

	if !closed(r) {
		t.Fatal("an uncorrelatable retraction left the client on a stream that never terminates")
	}
	if p.currentRun() != nil {
		t.Error("the cancelled run was not released from the slot")
	}
}
