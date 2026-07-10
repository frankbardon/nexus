package agui

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// TestResumeCycle is the core acceptance test for E2-S2: a single interrupted
// Nexus turn spans two AG-UI runs. Run 1 ends with an interrupt (hitl.requested
// during the turn); a resume-POST (run 2, same threadId, new runId) emits
// hitl.responded to unblock the still-parked agent and streams the continuation
// through to RunFinished. It exercises the full inbound->bus->outbound path over
// real HTTP + a real engine bus.
func TestResumeCycle(t *testing.T) {
	p, bus, url := newTestPlugin(t)

	// A single long-lived "agent" goroutine models the in-process agent that
	// blocks on hitl.responded across two runs. It starts its turn on io.input,
	// asks for input (hitl.requested), waits for the resolution, then finishes.
	responded := make(chan events.HITLResponse, 1)
	bus.Subscribe("hitl.responded", func(e engine.Event[any]) {
		if r, ok := e.Payload.(events.HITLResponse); ok {
			select {
			case responded <- r:
			default:
			}
		}
	})

	inputSeen := make(chan events.UserInput, 1)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		if in, ok := e.Payload.(events.UserInput); ok {
			select {
			case inputSeen <- in:
			default:
			}
		}
	})

	go func() {
		<-inputSeen
		turn := events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1"}
		_ = bus.Emit("agent.turn.start", turn)
		// Ask the user — this ends run 1 with an interrupt at the transport.
		_ = bus.Emit("hitl.requested", events.HITLRequest{
			SchemaVersion: events.HITLRequestVersion,
			ID:            "req-1",
			SessionID:     "thread-1",
			TurnID:        "turn-1",
			Mode:          events.HITLModeChoices,
			Prompt:        "Approve deploy?",
			Choices: []events.HITLChoice{
				{ID: "allow", Label: "Allow", Kind: events.ChoiceAllow},
				{ID: "reject", Label: "Reject", Kind: events.ChoiceReject},
			},
		})

		// The in-process agent stays blocked until the resume resolves it.
		resp := <-responded
		if resp.RequestID != "req-1" {
			t.Errorf("hitl.responded request_id = %q, want req-1", resp.RequestID)
		}
		if resp.ChoiceID != "allow" {
			t.Errorf("hitl.responded choice_id = %q, want allow", resp.ChoiceID)
		}

		// Continuation of the SAME turn streams on run 2.
		out := events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "deploying now", Role: "assistant"}
		_ = bus.Emit("io.output", out)
		_ = bus.Emit("agent.turn.end", turn)
	}()

	// --- Run 1: POST, read to the interrupt, extract the interruptId. ---
	body1 := `{"threadId":"thread-1","runId":"run-1","messages":[{"id":"m1","role":"user","content":"deploy prod"}]}`
	resp1 := post(t, url, "", "", body1)
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("run1 status = %d, want 200", resp1.StatusCode)
	}
	evs1, err := agui.NewSSEReader(resp1.Body).ReadAll()
	if err != nil {
		t.Fatalf("read run1 sse: %v", err)
	}

	interruptID := ""
	var fin1 *agui.RunFinishedEvent
	for _, e := range evs1 {
		if fe, ok := e.(*agui.RunFinishedEvent); ok {
			fin1 = fe
		}
	}
	if fin1 == nil {
		t.Fatalf("run1 produced no RunFinished (events=%v)", eventTypes(evs1))
	}
	if fin1.Outcome != agui.OutcomeInterrupt {
		t.Fatalf("run1 outcome = %q, want interrupt", fin1.Outcome)
	}
	var ip agui.Interrupt
	if err := json.Unmarshal(fin1.Result, &ip); err != nil {
		t.Fatalf("decode run1 interrupt: %v", err)
	}
	interruptID = ip.InterruptID
	if interruptID == "" {
		t.Fatal("run1 interrupt missing interruptId")
	}

	// Run 1's HTTP handler frees the run slot via a deferred endRun as it
	// returns; the SSE read above completing means the terminal event was seen,
	// but the defer may not have run yet. Wait for the slot to actually free so
	// the resume run can register (rather than racing the second POST).
	waitFor(t, func() bool { return p.currentRun() == nil })

	// --- Run 2: resume the interrupt with "allow"; read continuation. ---
	body2 := `{"threadId":"thread-1","runId":"run-2","resume":[{"interruptId":"` + interruptID +
		`","status":"resolved","payload":{"choiceId":"allow"}}]}`
	resp2 := post(t, url, "", "", body2)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("run2 status = %d, want 200", resp2.StatusCode)
	}
	evs2, err := agui.NewSSEReader(resp2.Body).ReadAll()
	if err != nil {
		t.Fatalf("read run2 sse: %v", err)
	}

	if len(evs2) == 0 || evs2[0].EventType() != agui.EventRunStarted {
		t.Fatalf("run2 event[0] = %v, want RunStarted", eventTypes(evs2))
	}
	last := evs2[len(evs2)-1]
	fe2, ok := last.(*agui.RunFinishedEvent)
	if !ok {
		t.Fatalf("run2 last event = %s, want RunFinished", last.EventType())
	}
	if fe2.Outcome == agui.OutcomeInterrupt {
		t.Fatal("run2 should complete normally, not re-interrupt")
	}
	if fe2.RunID != "run-2" || fe2.ThreadID != "thread-1" {
		t.Errorf("run2 RunFinished thread/run = %q/%q, want thread-1/run-2", fe2.ThreadID, fe2.RunID)
	}

	// The continuation content streamed on run 2.
	sawContent := false
	for _, e := range evs2 {
		if c, ok := e.(*agui.TextMessageContentEvent); ok && c.Delta == "deploying now" {
			sawContent = true
		}
	}
	if !sawContent {
		t.Errorf("run2 did not stream the continuation content (events=%v)", eventTypes(evs2))
	}

	// Pending map must be empty after the interrupt is resolved.
	waitFor(t, func() bool {
		p.pendingMu.Lock()
		defer p.pendingMu.Unlock()
		return len(p.pending) == 0
	})
	p.pendingMu.Lock()
	nPending := len(p.pending)
	p.pendingMu.Unlock()
	if nPending != 0 {
		t.Fatalf("pending len = %d after resume, want 0", nPending)
	}
}

// waitFor polls cond until true or a short deadline; it is a tiny helper for the
// asynchronous handoffs in the resume cycle.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// --- unit tests for resume validation (no HTTP / no bus round-trip) ---

// newResumeTestPlugin builds a Plugin with a bus and a pre-seeded pending map so
// resumeRun/matchResume can be driven directly.
func newResumeTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	return &Plugin{
		bus:     engine.NewEventBus(),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		pending: make(map[string]pendingInterrupt),
	}
}

// TestResume_UnknownInterrupt asserts an unknown/expired interruptId is rejected
// (the server turns the error into a RunError) and no run is registered.
func TestResume_UnknownInterrupt(t *testing.T) {
	p := newResumeTestPlugin(t)
	r, err := p.resumeRun(runInput{
		threadID: "thread-1",
		runID:    "run-2",
		resume:   []agui.ResumeItem{{InterruptID: "int-nope", Status: agui.ResumeResolved}},
	})
	if err == nil {
		t.Fatal("resume of unknown interrupt should error")
	}
	if r != nil {
		t.Fatal("no run should be registered on a rejected resume")
	}
	if p.currentRun() != nil {
		t.Fatal("active run slot must stay empty on a rejected resume")
	}
}

// TestResume_PartialRejected asserts that leaving an open interrupt on the
// thread unaddressed is rejected — AG-UI forbids partial resumes.
func TestResume_PartialRejected(t *testing.T) {
	p := newResumeTestPlugin(t)
	p.pending["int-a"] = pendingInterrupt{InterruptID: "int-a", RequestID: "req-a", ThreadID: "thread-1"}
	p.pending["int-b"] = pendingInterrupt{InterruptID: "int-b", RequestID: "req-b", ThreadID: "thread-1"}

	_, err := p.resumeRun(runInput{
		threadID: "thread-1",
		runID:    "run-2",
		resume:   []agui.ResumeItem{{InterruptID: "int-a", Status: agui.ResumeResolved}},
	})
	if err == nil {
		t.Fatal("partial resume should be rejected")
	}
	// Both pending interrupts remain — the agent stays parked for a full resume.
	p.pendingMu.Lock()
	n := len(p.pending)
	p.pendingMu.Unlock()
	if n != 2 {
		t.Fatalf("pending len = %d, want 2 (nothing resolved on rejection)", n)
	}
}

// TestResume_CancelledPath asserts the cancelled status emits a hitl.responded
// carrying Cancelled:true and clears the pending entry.
func TestResume_CancelledPath(t *testing.T) {
	p := newResumeTestPlugin(t)
	p.pending["int-x"] = pendingInterrupt{
		InterruptID: "int-x", RequestID: "req-x", ThreadID: "thread-9", Mode: events.HITLModeFreeText,
	}

	got := make(chan events.HITLResponse, 1)
	p.bus.Subscribe("hitl.responded", func(e engine.Event[any]) {
		if r, ok := e.Payload.(events.HITLResponse); ok {
			got <- r
		}
	})

	r, err := p.resumeRun(runInput{
		threadID: "thread-9",
		runID:    "run-2",
		resume:   []agui.ResumeItem{{InterruptID: "int-x", Status: agui.ResumeCancelled}},
	})
	if err != nil {
		t.Fatalf("cancelled resume errored: %v", err)
	}
	if r == nil {
		t.Fatal("cancelled resume should still open a continuation run")
	}
	t.Cleanup(func() { r.finish(); p.endRun(r) })

	select {
	case resp := <-got:
		if resp.RequestID != "req-x" {
			t.Errorf("request_id = %q, want req-x", resp.RequestID)
		}
		if !resp.Cancelled {
			t.Error("hitl.responded should carry Cancelled=true on cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no hitl.responded emitted on cancelled resume")
	}

	waitFor(t, func() bool {
		p.pendingMu.Lock()
		defer p.pendingMu.Unlock()
		_, still := p.pending["int-x"]
		return !still
	})
	p.pendingMu.Lock()
	_, still := p.pending["int-x"]
	p.pendingMu.Unlock()
	if still {
		t.Fatal("pending interrupt not cleared after cancelled resume")
	}
}

// TestBuildHITLResponse_ModeMapping asserts payload->HITLResponse mapping honors
// the interrupt Mode: choices-only drops stray free text; both keeps both.
func TestBuildHITLResponse_ModeMapping(t *testing.T) {
	choices := buildHITLResponse(
		pendingInterrupt{RequestID: "r1", Mode: events.HITLModeChoices},
		agui.ResumeItem{Status: agui.ResumeResolved, Payload: json.RawMessage(`{"choiceId":"allow","freeText":"ignore me"}`)},
	)
	if choices.ChoiceID != "allow" {
		t.Errorf("choice_id = %q, want allow", choices.ChoiceID)
	}
	if choices.FreeText != "" {
		t.Errorf("free_text = %q, want dropped for choices mode", choices.FreeText)
	}

	both := buildHITLResponse(
		pendingInterrupt{RequestID: "r2", Mode: events.HITLModeBoth},
		agui.ResumeItem{Status: agui.ResumeResolved, Payload: json.RawMessage(`{"choiceId":"allow","freeText":"with a note"}`)},
	)
	if both.ChoiceID != "allow" || both.FreeText != "with a note" {
		t.Errorf("both mode = %+v, want choice+text preserved", both)
	}
}
