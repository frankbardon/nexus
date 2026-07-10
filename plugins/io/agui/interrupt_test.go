package agui

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// newInterruptTestPlugin builds a Plugin with just the fields the HITL handlers
// touch, plus a single active run so handleHITLRequested has a stream to end.
func newInterruptTestPlugin(t *testing.T, threadID, runID string) (*Plugin, *run) {
	t.Helper()
	p := &Plugin{
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		pending: make(map[string]pendingInterrupt),
	}
	r := newRun(threadID, runID)
	p.active = r
	return p, r
}

// drainAll reads every queued event off a run's channel without closing it
// again (the run is expected to already be finished by the caller).
func drainAll(r *run) []agui.Event {
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

// TestHITLRequested_EmitsInterruptSequence is the core acceptance test: a
// hitl.requested during an active run must emit StateSnapshot, then
// MessagesSnapshot, then RunFinished(interrupt) — in that order — close the SSE
// stream, record the pending interrupt, and NOT unblock the in-process agent
// (no hitl.responded is emitted; this test drives the handler directly and
// asserts the run terminated as an interrupt rather than a normal finish).
func TestHITLRequested_EmitsInterruptSequence(t *testing.T) {
	p, r := newInterruptTestPlugin(t, "thread-1", "run-1")

	// Give the run some rendered conversation so MessagesSnapshot is non-trivial.
	r.onOutput(events.AgentOutput{Content: "working on it", Role: "assistant"})

	req := events.HITLRequest{
		SchemaVersion:   events.HITLRequestVersion,
		ID:              "req-abc",
		SessionID:       "sess-9",
		TurnID:          "turn-7",
		Mode:            events.HITLModeChoices,
		Prompt:          "Approve deploy?",
		DefaultChoiceID: "reject",
		Choices: []events.HITLChoice{
			{ID: "allow", Label: "Allow", Kind: events.ChoiceAllow},
			{ID: "reject", Label: "Reject", Kind: events.ChoiceReject},
		},
	}
	p.handleHITLRequested(engine.Event[any]{Payload: req})

	// Stream must be closed.
	select {
	case <-r.done:
	default:
		t.Fatal("run not closed after interrupt")
	}

	// Active run pointer cleared (stream done) but agent still blocked.
	if p.currentRun() != nil {
		t.Fatal("active run should be cleared after interrupt")
	}

	events := drainAll(r)

	// Locate the three load-bearing events and assert their relative order.
	var stateIdx, msgIdx, finIdx = -1, -1, -1
	var fin agui.RunFinishedEvent
	for i, e := range events {
		switch ev := e.(type) {
		case agui.StateSnapshotEvent:
			if stateIdx == -1 {
				stateIdx = i
			}
		case agui.MessagesSnapshotEvent:
			if msgIdx == -1 {
				msgIdx = i
			}
			if len(ev.Messages) == 0 {
				t.Error("MessagesSnapshot is empty; want rendered assistant message")
			}
		case agui.RunFinishedEvent:
			finIdx = i
			fin = ev
		}
	}
	if stateIdx == -1 || msgIdx == -1 || finIdx == -1 {
		t.Fatalf("missing events: state=%d messages=%d finished=%d (all=%v)", stateIdx, msgIdx, finIdx, eventTypes(events))
	}
	if !(stateIdx < msgIdx && msgIdx < finIdx) {
		t.Fatalf("order wrong: state=%d messages=%d finished=%d; want state < messages < finished", stateIdx, msgIdx, finIdx)
	}

	// RunFinished must carry the interrupt outcome and a decodable payload.
	if fin.Outcome != agui.OutcomeInterrupt {
		t.Fatalf("outcome = %q, want %q", fin.Outcome, agui.OutcomeInterrupt)
	}
	var payload agui.Interrupt
	if err := json.Unmarshal(fin.Result, &payload); err != nil {
		t.Fatalf("decode interrupt result: %v", err)
	}
	if payload.InterruptID == "" {
		t.Fatal("interrupt payload missing interruptId")
	}
	if payload.Prompt != "Approve deploy?" {
		t.Errorf("prompt = %q, want %q", payload.Prompt, "Approve deploy?")
	}
	if payload.Mode != agui.InterruptModeChoices {
		t.Errorf("mode = %q, want choices", payload.Mode)
	}
	if payload.DefaultChoiceID != "reject" {
		t.Errorf("defaultChoiceId = %q, want reject", payload.DefaultChoiceID)
	}
	if len(payload.Choices) != 2 || payload.Choices[0].ID != "allow" || payload.Choices[1].ID != "reject" {
		t.Errorf("choices = %+v, want allow,reject", payload.Choices)
	}

	// Pending-interrupt mapping recorded and correlatable on resume.
	p.pendingMu.Lock()
	pi, ok := p.pending[payload.InterruptID]
	p.pendingMu.Unlock()
	if !ok {
		t.Fatalf("pending interrupt %q not recorded", payload.InterruptID)
	}
	if pi.RequestID != "req-abc" || pi.SessionID != "sess-9" || pi.TurnID != "turn-7" {
		t.Errorf("pending = %+v; want request/session/turn correlated", pi)
	}
	if pi.ThreadID != "thread-1" || pi.RunID != "run-1" {
		t.Errorf("pending thread/run = %q/%q; want thread-1/run-1", pi.ThreadID, pi.RunID)
	}
}

// TestHITLRequested_NoActiveRunIgnored asserts a hitl.requested with no run in
// flight is a no-op: nothing is recorded and no panic occurs.
func TestHITLRequested_NoActiveRunIgnored(t *testing.T) {
	p := &Plugin{
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		pending: make(map[string]pendingInterrupt),
	}
	p.handleHITLRequested(engine.Event[any]{Payload: events.HITLRequest{ID: "x"}})
	p.pendingMu.Lock()
	n := len(p.pending)
	p.pendingMu.Unlock()
	if n != 0 {
		t.Fatalf("pending len = %d, want 0 with no active run", n)
	}
}

// TestHITLCancel_EndsRunAndDropsPending asserts hitl.cancel terminates an active
// run with a cancelled outcome and removes any recorded pending mapping.
func TestHITLCancel_EndsRunAndDropsPending(t *testing.T) {
	p, r := newInterruptTestPlugin(t, "thread-2", "run-2")

	// Pre-record a pending interrupt for the request being cancelled.
	p.pending["int-xyz"] = pendingInterrupt{InterruptID: "int-xyz", RequestID: "req-c"}

	p.handleHITLCancel(engine.Event[any]{Payload: events.HITLCancel{
		SchemaVersion: events.HITLCancelVersion,
		RequestID:     "req-c",
		Reason:        "stage aborted",
	}})

	select {
	case <-r.done:
	default:
		t.Fatal("run not closed after cancel")
	}
	if p.currentRun() != nil {
		t.Fatal("active run should be cleared after cancel")
	}

	var finished *agui.RunFinishedEvent
	for _, e := range drainAll(r) {
		if fe, ok := e.(agui.RunFinishedEvent); ok {
			finished = &fe
		}
	}
	if finished == nil {
		t.Fatal("no RunFinished emitted on cancel")
	}
	if finished.Outcome != agui.OutcomeCancelled {
		t.Fatalf("outcome = %q, want %q", finished.Outcome, agui.OutcomeCancelled)
	}

	p.pendingMu.Lock()
	_, stillThere := p.pending["int-xyz"]
	p.pendingMu.Unlock()
	if stillThere {
		t.Fatal("pending interrupt not dropped on cancel")
	}
}

func eventTypes(evs []agui.Event) []agui.EventType {
	out := make([]agui.EventType, len(evs))
	for i, e := range evs {
		out[i] = e.EventType()
	}
	return out
}
