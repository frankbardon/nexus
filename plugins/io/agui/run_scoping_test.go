package agui

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// This file pins one rule, from both sides:
//
//	A turn's events belong to the run that CARRIES that turn, never to
//	whichever run happens to hold the single active-run slot when they arrive.
//
// The slot and the turn have different lifetimes — endRun frees the slot the
// moment the SSE stream is done, while the turn runs detached on engine
// goroutines and routinely outlives it (a disconnect frees the slot and only
// THEN asks the agent to cancel; a HITL park frees it with the agent still
// blocked). So a cancelled turn's trailing io.output and agent.turn.end arrive
// after the next POST has already taken the slot.
//
// Untreated, that had two symptoms and the tests below are named for them:
// the successor's stream was FINISHED by a turn it never ran (RunFinished with
// no RunStarted — HTTP 200 and silence), and the cancelled turn's own notice
// was rendered as the ANSWER to the next question.
//
// Every assertion here reads what the client would actually have received, and
// asserts the successor's turn RAN — its step opened, its own text arrived,
// its own end closed it. "A stream started" is not the property: a run refused
// with RunStarted + RunError satisfies that while no work happened at all.

// startedRun is startRun with the "another run in flight" rejection turned into
// a fatal, which is never the case under test here.
func startedRun(t *testing.T, p *Plugin, threadID, runID string) *run {
	t.Helper()
	r, ok := p.startRun(runInput{threadID: threadID, runID: runID})
	if !ok {
		t.Fatalf("startRun(%s) rejected: the slot should have been free", runID)
	}
	return r
}

// emitOutput publishes an io.output carrying a specific turn id, the way an
// agent loop does.
func emitOutput(t *testing.T, h *contract.ContractHarness, turnID, content string) {
	t.Helper()
	if err := h.Bus().Emit("io.output", events.AgentOutput{
		SchemaVersion: events.AgentOutputVersion,
		Content:       content,
		Role:          "assistant",
		TurnID:        turnID,
	}); err != nil {
		t.Fatalf("emit io.output: %v", err)
	}
}

// queued reads everything currently sitting on a run's channel WITHOUT
// finishing it, so a test can assert on a stream that is still open. It is
// drainTypes' non-destructive sibling.
func queued(r *run) []agui.Event {
	var out []agui.Event
	for {
		select {
		case e := <-r.out:
			out = append(out, e)
		default:
			return out
		}
	}
}

// types projects the discriminators of a captured stream.
func types(evs []agui.Event) []agui.EventType {
	out := make([]agui.EventType, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.EventType())
	}
	return out
}

// texts projects every rendered text delta of a captured stream. This is what
// a reader of the AG-UI stream sees as the assistant's words.
func texts(evs []agui.Event) []string {
	var out []string
	for _, e := range evs {
		if c, ok := e.(agui.TextMessageContentEvent); ok {
			out = append(out, c.Delta)
		}
	}
	return out
}

func contains[T comparable](hay []T, needle T) bool {
	for _, v := range hay {
		if v == needle {
			return true
		}
	}
	return false
}

func closed(r *run) bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// TestContract_AnAbandonedTurnDoesNotFinishTheNextRun is the reported defect.
//
// Run A's client disconnects; endRun frees the slot while turn A is still
// alive on an engine goroutine. Run B POSTs and takes the slot. Only then does
// the cancellation of turn A reach the bus — its io.output, then its
// agent.turn.end. Before this fix that turn end called finish() on whatever
// held the slot, which was B: B's RunStarted was discarded by queue (done was
// already closed) and the client got HTTP 200 on a stream that never started.
//
// The assertion is deliberately NOT "a stream started". It is that run B's own
// turn RAN end to end on its own stream: RunStarted, its step, its own answer,
// its own RunFinished — and that turn A's notice is nowhere in it.
func TestContract_AnAbandonedTurnDoesNotFinishTheNextRun(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}), contract.WithSession())

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	runA := startedRun(t, p, "thread-race", "run-a")
	emitTurn(t, h, "agent.turn.start", "turn-a")
	// The disconnect: the handler returns, the slot frees, the turn does not
	// stop. (The real path additionally emits cancel.request; what matters
	// here is only that the slot is free while turn A is still emitting.)
	p.endRun(runA)

	runB := startedRun(t, p, "thread-race", "run-b")

	// Turn A's cancellation lands on the listener while run B holds the slot.
	emitOutput(t, h, "turn-a", "_Operation cancelled. Type /resume or press the resume button to continue._")
	emitTurn(t, h, "agent.turn.end", "turn-a")

	if closed(runB) {
		t.Fatal("run B's stream was closed by turn A's end; that is the defect")
	}
	if p.currentRun() != runB {
		t.Fatal("run B lost the active slot to a turn it never ran")
	}

	// Run B's own turn now runs, exactly as it would have with no race at all.
	emitTurn(t, h, "agent.turn.start", "turn-b")
	emitOutput(t, h, "turn-b", "forty two")
	emitTurn(t, h, "agent.turn.end", "turn-b")

	if !closed(runB) {
		t.Fatal("run B's own turn ended and did not finish its stream")
	}

	got := queued(runB)
	gotTypes := types(got)

	// RunStarted is FIRST, and that is the half startRun's ordering owns:
	// it is queued before the run is published as p.active, so nothing can
	// close the run between publication and its own first event. The dropped
	// RunStarted was the reported "HTTP 200 and silence".
	if len(gotTypes) == 0 || gotTypes[0] != agui.EventRunStarted {
		t.Errorf("run B's stream does not open on RunStarted: %v", gotTypes)
	}

	// The turn RAN: its lifecycle opened, its step opened and closed, and it
	// terminated on its own turn's end.
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
	if contains(gotTypes, agui.EventRunError) {
		t.Errorf("run B's turn was refused rather than run: %v", gotTypes)
	}

	// And it answered its OWN question.
	gotTexts := texts(got)
	if !contains(gotTexts, "forty two") {
		t.Errorf("run B's own answer is missing from its stream: %q", gotTexts)
	}
	for _, s := range gotTexts {
		if s != "forty two" {
			t.Errorf("run B's stream carries text from another turn: %q", s)
		}
	}
}

// TestContract_ACancelledTurnsOutputIsNotTheNextQuestionsAnswer is the same
// race one beat later, and it is the half that produces a WRONG ANSWER rather
// than silence: run B's own turn has already started by the time the abandoned
// turn's io.output arrives, so "this run has bound no turn yet" no longer
// discriminates. The turn the released run was carrying does — endRun records
// it, and an event naming it belongs to the run that left.
func TestContract_ACancelledTurnsOutputIsNotTheNextQuestionsAnswer(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}), contract.WithSession())

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	runA := startedRun(t, p, "thread-late", "run-a")
	emitTurn(t, h, "agent.turn.start", "turn-a")
	p.endRun(runA)

	runB := startedRun(t, p, "thread-late", "run-b")
	emitTurn(t, h, "agent.turn.start", "turn-b")

	// Turn A's cancellation notice arrives with run B already working.
	emitOutput(t, h, "turn-a", "_Operation cancelled. Type /resume or press the resume button to continue._")
	emitOutput(t, h, "turn-b", "forty two")
	emitTurn(t, h, "agent.turn.end", "turn-a")

	if closed(runB) {
		t.Fatal("run B's stream was closed by turn A's end")
	}
	emitTurn(t, h, "agent.turn.end", "turn-b")

	gotTexts := texts(queued(runB))
	if !contains(gotTexts, "forty two") {
		t.Errorf("run B's own answer is missing: %q", gotTexts)
	}
	for _, s := range gotTexts {
		if s != "forty two" {
			t.Errorf("the abandoned turn's notice was rendered as run B's answer: %q", gotTexts)
		}
	}
}

// TestContract_OutputWithNoTurnAndFromASubTurnStillReaches is the
// non-regression half, and it is why the content rule is not a strict TurnID
// match. io.output's emitters are an open set:
//
//   - most gates emit one carrying no TurnID at all (a budget warning, a
//     stop-word refusal, the engine's own error output), and
//   - nexus.agent.aguiremote / nexus.agent.a2aremote republish a delegated
//     remote's narration under a synthetic sub-turn id no agent.turn.start
//     ever announces.
//
// Both must still reach the client. A rule that dropped everything not
// matching the run's own turn would delete them silently.
func TestContract_OutputWithNoTurnAndFromASubTurnStillReaches(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}), contract.WithSession())

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	runB := startedRun(t, p, "thread-open", "run-b")
	emitTurn(t, h, "agent.turn.start", "turn-b")

	emitOutput(t, h, "", "a gate said something")
	emitOutput(t, h, "agui_remote_spawn-1", "a delegated remote narrated")
	emitOutput(t, h, "turn-b", "forty two")

	gotTexts := texts(queued(runB))
	for _, want := range []string{"a gate said something", "a delegated remote narrated", "forty two"} {
		if !contains(gotTexts, want) {
			t.Errorf("legitimate output %q was dropped: %q", want, gotTexts)
		}
	}
}

// TestContract_TurnEndWithNoTurnIDStillFinishesTheRun pins the one case the
// turn-end rule deliberately takes rather than refuses. An agent loop that
// names no turn is not correlatable, and leaving the client on a stream that
// never terminates is worse than terminating the wrong one — the same reading
// nexus.io.a2a states for its own task lifetime.
func TestContract_TurnEndWithNoTurnIDStillFinishesTheRun(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}), contract.WithSession())

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	r := startedRun(t, p, "thread-anon", "run-anon")
	emitTurn(t, h, "agent.turn.start", "turn-anon")
	emitTurn(t, h, "agent.turn.end", "")

	if !closed(r) {
		t.Fatal("an uncorrelatable turn end left the stream open forever")
	}
}

// TestContract_AnAbandonedTurnDoesNotSuspendTheNextRun covers tool.invoke,
// which carries the same turn id and is the sharpest of the content handlers:
// a client-executed tool invoked by the departed turn would not merely render
// onto the successor's stream, it would SUSPEND it — parking a run that asked
// for nothing, awaiting a result its client has no call to produce.
func TestContract_AnAbandonedTurnDoesNotSuspendTheNextRun(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}), contract.WithSession())

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	runA := startedRun(t, p, "thread-tool", "run-a")
	emitTurn(t, h, "agent.turn.start", "turn-a")
	p.endRun(runA)

	// Run B advertises a client-executed tool, as a frontend does.
	runB, started := p.startRun(runInput{
		threadID: "thread-tool",
		runID:    "run-b",
		tools:    []agui.Tool{{Name: "client_confirm"}},
	})
	if !started {
		t.Fatal("startRun rejected after run A was released")
	}
	emitTurn(t, h, "agent.turn.start", "turn-b")

	// The departed turn invokes that tool.
	if err := h.Bus().Emit("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-from-turn-a",
		Name:          "client_confirm",
		TurnID:        "turn-a",
	}); err != nil {
		t.Fatalf("emit tool.invoke: %v", err)
	}

	if runB.isSuspended() {
		t.Fatal("run B was parked awaiting a client tool result for another turn's call")
	}
	if closed(runB) {
		t.Fatal("run B's stream was terminated by another turn's tool call")
	}
	gotTypes := types(queued(runB))
	if contains(gotTypes, agui.EventToolCallStart) {
		t.Errorf("another turn's tool call was rendered onto run B: %v", gotTypes)
	}
}

// TestUnit_RunAcceptsTurnEventRefusesOnlyWhatItCanProveForeign states the
// content rule directly, so the reasoning behind each clause is pinned rather
// than only its effect through the handlers.
func TestUnit_RunAcceptsTurnEventRefusesOnlyWhatItCanProveForeign(t *testing.T) {
	p := &Plugin{retiredTurn: "turn-a"}

	unbound := newRun("t", "r-unbound", nil)
	bound := newRun("t", "r-bound", nil)
	bound.adoptTurn("turn-b")
	continuation := newRun("t", "r-cont", nil)
	continuation.adoptTurn("turn-a") // resumed the parked turn endRun recorded

	cases := []struct {
		name   string
		r      *run
		turnID string
		want   bool
	}{
		{"no turn id is uncorrelatable and taken", bound, "", true},
		{"its own turn", bound, "turn-b", true},
		{"a named turn at a run that has bound none", unbound, "turn-a", false},
		{"the turn the previous run was carrying", bound, "turn-a", false},
		{"a synthetic delegated sub-turn", bound, "agui_remote_spawn-1", true},
		{"a resumed run's own adopted turn beats the retired record", continuation, "turn-a", true},
	}
	for _, tc := range cases {
		if got := p.runAcceptsTurnEvent(tc.r, tc.turnID); got != tc.want {
			t.Errorf("%s: runAcceptsTurnEvent(%q) = %v, want %v", tc.name, tc.turnID, got, tc.want)
		}
	}
}

// TestUnit_RunOwnsTurnEndIsStricterThanTheContentRule states the other rule,
// and the relation between them: every turn end the content rule would refuse
// is also refused here, and the reverse does not hold. Terminating the wrong
// stream is the worse failure, so the terminating rule is the stricter one.
func TestUnit_RunOwnsTurnEndIsStricterThanTheContentRule(t *testing.T) {
	p := &Plugin{retiredTurn: "turn-a"}

	bound := newRun("t", "r-bound", nil)
	bound.adoptTurn("turn-b")
	unbound := newRun("t", "r-unbound", nil)

	if !p.runOwnsTurnEnd(bound, "turn-b") {
		t.Error("a run must own the end of the turn it bound")
	}
	if !p.runOwnsTurnEnd(bound, "") {
		t.Error("an uncorrelatable turn end must still terminate the run")
	}
	if p.runOwnsTurnEnd(unbound, "turn-a") {
		t.Error("a run that bound no turn cannot own a named turn's end")
	}
	// Stricter: a foreign top-level turn that is not the retired one is
	// delivered as content but must not terminate.
	if p.runOwnsTurnEnd(bound, "turn-c") {
		t.Error("a turn this run never bound must not terminate it")
	}
	if !p.runAcceptsTurnEvent(bound, "turn-c") {
		t.Error("the content rule must stay the more permissive of the two")
	}

	// And the subset relation, over the whole vocabulary the two share.
	for _, turnID := range []string{"", "turn-a", "turn-b", "turn-c", "agui_remote_x"} {
		for _, r := range []*run{bound, unbound} {
			if !p.runAcceptsTurnEvent(r, turnID) && p.runOwnsTurnEnd(r, turnID) {
				t.Errorf("turn %q on run %s: content refused it but termination allowed it",
					turnID, r.runID)
			}
		}
	}
}
