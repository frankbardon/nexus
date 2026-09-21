package a2a

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// This file pins one rule, from both sides:
//
//	A turn's events belong to the TASK that carries that turn, never to
//	whichever task happens to hold the single in-flight slot when they arrive.
//
// run.onTurnEnd has always stated it for the task LIFETIME. Nothing stated it
// for the task's CONTENT, so every content handler took p.currentRun()
// unconditionally and rendered whatever arrived onto whoever held the slot.
//
// The slot and the turn have different lifetimes, and three doors open the gap:
// CancelTask settles the task — which releases the slot from the run's own
// terminal sequence — and only THEN emits cancel.request; expireInput fails the
// task and then retracts the question; a fatal core.error fails a task whose
// agent loop still has a tail to emit. In each case the turn is alive on an
// engine goroutine after the next task has taken the slot.
//
// Every test below drives the plugin through its REAL entry points — startTurn,
// cancelTask — and its real subscriptions, and asserts what a client would
// actually have read off the stream. Each one also asserts that the successor's
// turn RAN: its own answer reached the wire and it completed. "Nothing bad
// appeared" is satisfied by a task that did nothing at all.

// scopedTask starts a task through the plugin's own entry point and attaches an
// observer, failing the test on the refusals that cannot happen here.
func scopedTask(t *testing.T, p *Plugin, contextID, text string) (*run, *stream) {
	t.Helper()
	r, sub, _, protoErr := p.startTurn(turnInput{
		contextID: contextID,
		text:      text,
		messageID: "m-" + text,
	}, nexusauth.Principal{}, streamOptions{})
	if protoErr != nil {
		t.Fatalf("startTurn(%q) refused: %s", text, protoErr.Message)
	}
	return r, sub
}

// emitTo publishes one payload and fails the test if the bus refuses it.
func emitTo(t *testing.T, bus engine.EventBus, eventType string, payload any) {
	t.Helper()
	if err := bus.Emit(eventType, payload); err != nil {
		t.Fatalf("emit %s: %v", eventType, err)
	}
}

func turnStart(t *testing.T, bus engine.EventBus, turnID string) {
	t.Helper()
	emitTo(t, bus, "agent.turn.start", events.TurnInfo{
		SchemaVersion: events.TurnInfoVersion, TurnID: turnID,
	})
}

func turnOutput(t *testing.T, bus engine.EventBus, turnID, text string) {
	t.Helper()
	emitTo(t, bus, "io.output", events.AgentOutput{
		SchemaVersion: events.AgentOutputVersion,
		Content:       text, Role: "assistant", TurnID: turnID,
	})
}

func turnEnd(t *testing.T, bus engine.EventBus, turnID string) {
	t.Helper()
	emitTo(t, bus, "agent.turn.end", events.TurnInfo{
		SchemaVersion: events.TurnInfoVersion, TurnID: turnID,
	})
}

// drainObserver reads everything the run has queued for one observer WITHOUT
// closing it, so a test can read a stream that is still open.
func drainObserver(s *stream) []a2a.StreamResponse {
	var out []a2a.StreamResponse
	for {
		select {
		case f := <-s.frames:
			out = append(out, f)
		default:
			return out
		}
	}
}

// artifactTexts projects the text of every artifact a frame sequence published.
// This is the task's OUTPUT as a client reads it.
func artifactTexts(fs []a2a.StreamResponse) []string {
	var out []string
	for _, f := range fs {
		if f.Kind() != a2a.StreamPayloadArtifactUpdate || f.ArtifactUpdate == nil {
			continue
		}
		for _, part := range f.ArtifactUpdate.Artifact.Parts {
			if text, ok := part.TextValue(); ok && text != "" {
				out = append(out, text)
			}
		}
	}
	return out
}

func containsText(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// TestADepartedTurnsOutputIsNotTheNextTasksAnswer is the reported defect.
//
// Task A is canceled, which frees the slot while turn A is still alive on an
// engine goroutine — cancelTask settles first and emits cancel.request second.
// Task B takes the slot and binds its own turn. Only then does turn A's
// trailing io.output arrive. Before this fix onOutput wrote it to finalText on
// a last-write-wins field, so the cancellation's own narration was published as
// task B's response artifact: the A2A shape of answering one question with
// another's reply.
func TestADepartedTurnsOutputIsNotTheNextTasksAnswer(t *testing.T) {
	p, bus := newTestPlugin(t, nil)

	const ctx = "ctx-scoping"
	runA, _ := scopedTask(t, p, ctx, "first")
	turnStart(t, bus, "turn-A")
	if got := runA.boundTurn(); got != "turn-A" {
		t.Fatalf("task A bound turn %q, want turn-A", got)
	}
	if _, protoErr := p.cancelTask(nexusauth.Principal{}, runA.taskID); protoErr != nil {
		t.Fatalf("cancelTask refused: %s", protoErr.Message)
	}
	if r := p.currentRun(); r != nil {
		t.Fatalf("canceling task A left task %q holding the slot", r.taskID)
	}

	runB, subB := scopedTask(t, p, ctx, "second")
	turnStart(t, bus, "turn-B")
	if got := runB.boundTurn(); got != "turn-B" {
		t.Fatalf("task B bound turn %q, want turn-B", got)
	}

	// Task B answers, and only THEN does the departed turn's trailing output
	// arrive. That order is the defect and it is the real one: the cancellation
	// notice is emitted by an engine goroutine that is behind, so it lands after
	// the live turn has already spoken — and finalText is last-write-wins, so
	// the late arrival is the one that becomes the artifact.
	turnOutput(t, bus, "turn-B", "B's answer")
	turnOutput(t, bus, "turn-A", "the task was canceled by the client")
	turnEnd(t, bus, "turn-B")

	texts := artifactTexts(drainObserver(subB))
	if containsText(texts, "the task was canceled by the client") {
		t.Errorf("task B published the departed turn's text as its answer: %q", texts)
	}
	if !containsText(texts, "B's answer") {
		t.Errorf("task B's own answer never reached the client: %q", texts)
	}
	if got := runB.snapshotTask().Status.State; got != a2a.TaskStateCompleted {
		t.Errorf("task B ended %s, want COMPLETED — its own turn must still have run", got)
	}
}

// TestADepartedTurnsEventIsRefusedBeforeTheNextTaskBinds covers the same race
// one moment earlier, where the successor has taken the slot but its own
// agent.turn.start has not arrived yet. That window is the whole reason the
// rule has two halves: with no bound turn to compare against, a named turn
// reaching a task that has bound none is the only evidence available, and it is
// sufficient — a task is registered before its io.input is emitted and the
// agent loop opens every turn with agent.turn.start, so a task cannot miss its
// own.
//
// It is asserted on a tool result rather than on io.output, and that is a
// property of the two paths rather than a convenience: finalText is
// last-write-wins, so an unbound-window io.output is overwritten by the answer
// that follows it and leaves no trace a client could read. An artifact APPENDS,
// so the damage survives — which is also why this window is worth closing.
func TestADepartedTurnsEventIsRefusedBeforeTheNextTaskBinds(t *testing.T) {
	p, bus := newTestPlugin(t, nil)

	const ctx = "ctx-unbound"
	runA, _ := scopedTask(t, p, ctx, "first")
	turnStart(t, bus, "turn-A")
	if _, protoErr := p.cancelTask(nexusauth.Principal{}, runA.taskID); protoErr != nil {
		t.Fatalf("cancelTask refused: %s", protoErr.Message)
	}

	runB, subB := scopedTask(t, p, ctx, "second")
	// No turn start for B yet: the departed turn gets in first.
	emitTo(t, bus, "tool.result", events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            "call-A", Name: "search", Output: "A's tool output", TurnID: "turn-A",
	})
	if got := runB.boundTurn(); got != "" {
		t.Fatalf("task B bound turn %q before its own turn started", got)
	}

	turnStart(t, bus, "turn-B")
	turnOutput(t, bus, "turn-B", "B's answer")
	turnEnd(t, bus, "turn-B")

	texts := artifactTexts(drainObserver(subB))
	if containsText(texts, "A's tool output") {
		t.Errorf("task B published an artifact from a turn it never saw start: %q", texts)
	}
	if !containsText(texts, "B's answer") {
		t.Errorf("task B's own answer never reached the client: %q", texts)
	}
}

// TestADepartedTurnsToolResultIsNotTheNextTasksArtifact is the same rule on the
// path where the damage outlives the stream.
//
// A tool result is not telemetry here: it is published as an ARTIFACT, written
// through to the durable task store, and charged against this task's artifact
// budget. So a departed turn's result attached to the successor is a permanent
// record of work that task never did, readable by GetTask long afterwards.
func TestADepartedTurnsToolResultIsNotTheNextTasksArtifact(t *testing.T) {
	p, bus := newTestPlugin(t, nil)

	const ctx = "ctx-tools"
	runA, _ := scopedTask(t, p, ctx, "first")
	turnStart(t, bus, "turn-A")
	if _, protoErr := p.cancelTask(nexusauth.Principal{}, runA.taskID); protoErr != nil {
		t.Fatalf("cancelTask refused: %s", protoErr.Message)
	}

	runB, subB := scopedTask(t, p, ctx, "second")
	turnStart(t, bus, "turn-B")

	emitTo(t, bus, "tool.result", events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            "call-A", Name: "search", Output: "A's tool output", TurnID: "turn-A",
	})
	emitTo(t, bus, "tool.result", events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            "call-B", Name: "search", Output: "B's tool output", TurnID: "turn-B",
	})
	turnOutput(t, bus, "turn-B", "B's answer")
	turnEnd(t, bus, "turn-B")

	texts := artifactTexts(drainObserver(subB))
	if containsText(texts, "A's tool output") {
		t.Errorf("task B published a departed turn's tool result as its own artifact: %q", texts)
	}
	if !containsText(texts, "B's tool output") {
		t.Errorf("task B's own tool result never became an artifact: %q", texts)
	}
	if !containsText(texts, "B's answer") {
		t.Errorf("task B's own answer never reached the client: %q", texts)
	}
	if got := runB.snapshotTask().Status.State; got != a2a.TaskStateCompleted {
		t.Errorf("task B ended %s, want COMPLETED", got)
	}
}

// TestADepartedTurnsQuestionDoesNotParkTheNextTask is the severe case.
//
// A hitl.requested does not merely render onto the successor's stream, it
// SUSPENDS it: the task moves to INPUT_REQUIRED awaiting an answer its client
// has no call to produce, and is then failed by the input deadline. That is the
// A2A counterpart of the tool.invoke argument nexus.io.agui makes for its own
// client-executed calls.
func TestADepartedTurnsQuestionDoesNotParkTheNextTask(t *testing.T) {
	p, bus := newTestPlugin(t, nil)

	const ctx = "ctx-hitl"
	runA, _ := scopedTask(t, p, ctx, "first")
	turnStart(t, bus, "turn-A")
	if _, protoErr := p.cancelTask(nexusauth.Principal{}, runA.taskID); protoErr != nil {
		t.Fatalf("cancelTask refused: %s", protoErr.Message)
	}

	runB, subB := scopedTask(t, p, ctx, "second")
	turnStart(t, bus, "turn-B")

	emitTo(t, bus, "hitl.requested", events.HITLRequest{
		SchemaVersion: events.HITLRequestVersion,
		ID:            "hitl-A", TurnID: "turn-A",
		RequesterPlugin: "nexus.control.hitl",
		Prompt:          "which file did you mean?",
	})
	if _, parked := runB.pending(); parked {
		t.Fatal("task B was parked by a question belonging to a turn it does not carry")
	}

	turnOutput(t, bus, "turn-B", "B's answer")
	turnEnd(t, bus, "turn-B")

	fs := drainObserver(subB)
	for _, state := range states(fs) {
		if state == a2a.TaskStateInputRequired {
			t.Fatalf("task B reported INPUT_REQUIRED; states = %v", states(fs))
		}
	}
	if !containsText(artifactTexts(fs), "B's answer") {
		t.Errorf("task B's own answer never reached the client: %q", artifactTexts(fs))
	}
	if got := runB.snapshotTask().Status.State; got != a2a.TaskStateCompleted {
		t.Errorf("task B ended %s, want COMPLETED", got)
	}
}

// TestAnUncorrelatedOutputStillReachesTheTask is the other direction of the
// rule, and it is why the predicate is permissive rather than a strict equality
// match. io.output's emitters are an OPEN set: most gates publish one carrying
// no TurnID at all (a budget warning, a stop-word refusal, an endless-loop
// notice), and those must still reach the client. A strict match would have
// deleted every one of them silently.
func TestAnUncorrelatedOutputStillReachesTheTask(t *testing.T) {
	p, bus := newTestPlugin(t, nil)

	runB, subB := scopedTask(t, p, "ctx-gates", "only")
	turnStart(t, bus, "turn-B")
	turnOutput(t, bus, "", "Warning: token budget at 80%")
	turnEnd(t, bus, "turn-B")

	if !containsText(artifactTexts(drainObserver(subB)), "Warning: token budget at 80%") {
		t.Error("a gate's uncorrelated io.output was dropped; it carries no turn id and must still be published")
	}
	if got := runB.snapshotTask().Status.State; got != a2a.TaskStateCompleted {
		t.Errorf("task ended %s, want COMPLETED", got)
	}
}

// TestADelegatedRemotesNarrationStillReachesTheTask is the second half of that
// argument, and the one a strict match would have broken invisibly.
//
// nexus.agent.aguiremote and nexus.agent.a2aremote republish a delegated
// remote's narration under a SYNTHETIC sub-turn id ("agui_remote_<spawn>",
// "a2a_remote_<spawn>") on purpose, so a transport can group it apart from the
// local turn that asked for it. No agent.turn.start ever announces one, so it
// can never equal the task's bound turn — and it is not the retired turn
// either, so the rule takes it.
func TestADelegatedRemotesNarrationStillReachesTheTask(t *testing.T) {
	p, bus := newTestPlugin(t, nil)

	runB, subB := scopedTask(t, p, "ctx-remote", "only")
	turnStart(t, bus, "turn-B")
	turnOutput(t, bus, "a2a_remote_spawn-1", "the remote is working on it")
	turnEnd(t, bus, "turn-B")

	if !containsText(artifactTexts(drainObserver(subB)), "the remote is working on it") {
		t.Error("a delegated remote's republished narration was dropped")
	}
	if got := runB.snapshotTask().Status.State; got != a2a.TaskStateCompleted {
		t.Errorf("task ended %s, want COMPLETED", got)
	}
}

// TestATaskKeepsItsOwnTurnEvenWhenTheRetiredTurnSharesItsID pins the ordering
// inside the predicate, which is not a detail: the bound-turn arm is read
// BEFORE the retired-turn arm.
//
// Nothing guarantees an agent loop mints a turn id no earlier turn used — this
// transport sees only what reaches the bus — so a successor really can bind the
// same id its predecessor retired. Its own events must still be its own, or a
// collision it cannot see would make a task disown the answer it just produced.
func TestATaskKeepsItsOwnTurnEvenWhenTheRetiredTurnSharesItsID(t *testing.T) {
	p, bus := newTestPlugin(t, nil)

	const ctx, shared = "ctx-collide", "turn-shared"
	runA, _ := scopedTask(t, p, ctx, "first")
	turnStart(t, bus, shared)
	if _, protoErr := p.cancelTask(nexusauth.Principal{}, runA.taskID); protoErr != nil {
		t.Fatalf("cancelTask refused: %s", protoErr.Message)
	}

	runB, subB := scopedTask(t, p, ctx, "second")
	turnStart(t, bus, shared)
	turnOutput(t, bus, shared, "B's answer")
	turnEnd(t, bus, shared)

	if !containsText(artifactTexts(drainObserver(subB)), "B's answer") {
		t.Error("a task disowned its own answer because the retired turn shared its id")
	}
	if got := runB.snapshotTask().Status.State; got != a2a.TaskStateCompleted {
		t.Errorf("task B ended %s, want COMPLETED", got)
	}
}

// TestAReleasedTaskThatBoundNoTurnDoesNotForgetALiveOne pins the guard in
// endTurn: only a run that actually bound a turn overwrites p.retiredTurn.
//
// A task can be released without ever binding one — its io.input vetoed before
// any agent ran, its creation failing durably. Letting that release clear the
// record would forget the predecessor whose turn is still live, and the very
// next task would accept that turn's trailing output again.
func TestAReleasedTaskThatBoundNoTurnDoesNotForgetALiveOne(t *testing.T) {
	p, bus := newTestPlugin(t, nil)

	const ctx = "ctx-forget"
	runA, _ := scopedTask(t, p, ctx, "first")
	turnStart(t, bus, "turn-A")
	if _, protoErr := p.cancelTask(nexusauth.Principal{}, runA.taskID); protoErr != nil {
		t.Fatalf("cancelTask refused: %s", protoErr.Message)
	}

	// A task that never saw a turn at all, released immediately.
	runNone, _ := scopedTask(t, p, ctx, "vetoed")
	runNone.fail("input rejected before it reached the agent")
	if r := p.currentRun(); r != nil {
		t.Fatalf("the failed task left %q holding the slot", r.taskID)
	}

	runB, subB := scopedTask(t, p, ctx, "second")
	turnStart(t, bus, "turn-B")
	turnOutput(t, bus, "turn-B", "B's answer")
	turnOutput(t, bus, "turn-A", "the task was canceled by the client")
	turnEnd(t, bus, "turn-B")

	texts := artifactTexts(drainObserver(subB))
	if containsText(texts, "the task was canceled by the client") {
		t.Errorf("a turn-less release forgot the live turn it should have kept: %q", texts)
	}
	if !containsText(texts, "B's answer") {
		t.Errorf("task B's own answer never reached the client: %q", texts)
	}
	if got := runB.snapshotTask().Status.State; got != a2a.TaskStateCompleted {
		t.Errorf("task B ended %s, want COMPLETED", got)
	}
}
