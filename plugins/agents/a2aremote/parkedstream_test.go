package a2aremote

import (
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/events"
)

// Chained human-in-the-loop against a remote that HOLDS the stream open.
//
// A2A leaves it to the server whether an INPUT_REQUIRED park closes the SSE
// stream, and both readings are legal. nexus.io.a2a deliberately holds it open
// — keep-alive comments, no terminal frame — because closing on a non-terminal
// state would be indistinguishable, client side, from a dropped connection.
//
// Every test here therefore runs at the shipped default, stream: true, against
// a test agent configured with parkOpen. Before exchange learned to stop on the
// interruption these all deadlocked: the reader waited for an end of stream the
// remote was never going to send, and the remote waited for a resuming message
// the reader was never going to get around to sending. The question reached
// nobody and the delegation died on whichever deadline fired first.

// parkedAgent builds a remote that parks on a question and holds the stream
// open, answering the resumption on a fresh connection.
func parkedAgent(t *testing.T, question string, rec *resumeRecorder) *testAgent {
	t.Helper()
	return newTestAgent(t, testAgentConfig{
		parkOpen: true,
		frames:   askThenAnswer("held-task", "held-ctx", question, rec),
	})
}

// TestHeldOpenParkedStreamStillReachesTheHuman is the defect, as a test.
//
// It is the one that fails on the old code: with stream: true the read loop
// never returned, so no hitl.requested was ever emitted and the tool result was
// the call budget's expiry rather than the remote's answer.
func TestHeldOpenParkedStreamStillReachesTheHuman(t *testing.T) {
	rec := &resumeRecorder{}
	agent := parkedAgent(t, "which fiscal year?", rec)
	// A budget tight enough that a deadlock ends as a timeout inside the test's
	// own patience rather than hanging until the harness gives up.
	h := boot(t, oneAgent(agent.URL(), map[string]any{"timeout": "5s"}))
	human := answerHITL(t, h, "FY2026")

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "summarize the numbers"})

	if res.Error != "" {
		t.Fatalf("a question from a remote holding its stream open must still reach a human: %s", res.Error)
	}
	if !strings.Contains(res.Output, "the answer was FY2026") {
		t.Errorf("the resumed task's output should reach the model: %s", res.Output)
	}

	questions := human.questions()
	if len(questions) != 1 {
		t.Fatalf("the human was asked %d times, want 1", len(questions))
	}
	if !strings.Contains(questions[0].Prompt, "which fiscal year?") {
		t.Errorf("the human's prompt should carry the remote's own question: %q", questions[0].Prompt)
	}

	// The continuation named the SAME task: a park is resumed, not restarted.
	resumes := rec.all()
	if len(resumes) != 1 || resumes[0].taskID != "held-task" || resumes[0].contextID != "held-ctx" {
		t.Fatalf("the remote saw %+v, want one resumption of held-task/held-ctx", resumes)
	}

	// And the parked connection was let go rather than sat on. A client that
	// kept it open would be holding a socket for a task it has already moved on
	// from, on top of the remote's own.
	waitFor(t, "the parked stream to be abandoned", func() bool {
		return agent.releasedParks() > 0
	})
}

// TestHeldOpenParkedStreamRepublishesProgressBeforeThePark: stopping at the
// interruption must not cost the live progress that arrived before it.
func TestHeldOpenParkedStreamRepublishesProgressBeforeThePark(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{
		parkOpen: true,
		frames: func(req *a2a.SendMessageRequest) []a2a.StreamResponse {
			if req.Message.TaskID != "" {
				return completedRun("held-task", "held-ctx", "done")
			}
			working := a2a.NewTaskStatus(a2a.TaskStateWorking).
				WithMessage(a2a.NewAgentMessage("m-progress", "reading the ledger"))
			asking := a2a.NewTaskStatus(a2a.TaskStateInputRequired).
				WithMessage(a2a.NewAgentMessage("m-ask", "which fiscal year?"))
			return []a2a.StreamResponse{
				a2a.StreamTask(a2a.NewTask("held-task", "held-ctx")),
				a2a.StreamStatusUpdate(a2a.NewStatusUpdate("held-task", "held-ctx", working)),
				a2a.StreamStatusUpdate(a2a.NewStatusUpdate("held-task", "held-ctx", asking)),
			}
		},
	})
	h := boot(t, oneAgent(agent.URL(), map[string]any{"timeout": "5s"}))
	answerHITL(t, h, "FY2026")

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}

	var narrated bool
	for _, content := range republished(h) {
		if strings.Contains(content, "reading the ledger") {
			narrated = true
		}
		// The question itself is not progress: it goes to a human, never into
		// the delegating model's transcript.
		if strings.Contains(content, "which fiscal year?") {
			t.Errorf("the remote's question was republished as output: %q", content)
		}
	}
	if !narrated {
		t.Error("frames seen before the park must still reach the local bus as io.output")
	}
}

// TestResumedStreamOpeningOnTheParkDoesNotReAsk guards the other half of the
// rule: a continuation stream OPENS on a snapshot of the task it continues,
// which is the very interruption being answered. Treating that as a new
// question would put the same question in front of the human round after round
// until the round cap stopped it.
func TestResumedStreamOpeningOnTheParkDoesNotReAsk(t *testing.T) {
	rec := &resumeRecorder{}
	agent := newTestAgent(t, testAgentConfig{
		parkOpen: true,
		frames:   askThenAnswerFromSnapshot("held-task", "held-ctx", "which fiscal year?", rec),
	})
	h := boot(t, oneAgent(agent.URL(), map[string]any{"timeout": "5s"}))
	human := answerHITL(t, h, "FY2026")

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})

	if res.Error != "" {
		t.Fatalf("the delegation should have completed once the human answered: %s", res.Error)
	}
	if got := len(human.questions()); got != 1 {
		t.Fatalf("the human was asked %d times, want 1 — the continuation's opening snapshot is not a new question", got)
	}
	if got := len(rec.all()); got != 1 {
		t.Errorf("the remote saw %d resumptions, want 1", got)
	}
	if !strings.Contains(res.Output, "the answer was FY2026") {
		t.Errorf("the resumed task's output should reach the model: %s", res.Output)
	}
}

// TestHeldOpenParkedStreamWithChainingOffStillReturns: with chaining switched
// off there is no human to wait for, so the parked stream is the end of the
// exchange and the calling model gets the old, honest tool error — not a
// timeout.
func TestHeldOpenParkedStreamWithChainingOffStillReturns(t *testing.T) {
	agent := parkedAgent(t, "which fiscal year?", &resumeRecorder{})
	h := boot(t, oneAgent(agent.URL(), map[string]any{
		"timeout": "5s",
		"hitl":    map[string]any{"enabled": false},
	}))
	human := watchHITL(t, h)

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})

	if !strings.Contains(res.Error, "waiting for input") {
		t.Errorf("tool error = %q, want the parked-task advice rather than a deadline", res.Error)
	}
	if strings.Contains(res.Error, "did not finish within") {
		t.Errorf("the delegation waited for a stream the remote was holding open: %s", res.Error)
	}
	if len(human.questions()) != 0 {
		t.Error("no human should be asked when chaining is disabled")
	}
}

// TestHeldOpenParkedStreamCancellationStillPropagates: abandoning the stream at
// the park must not cost local cancellation. The turn is cancelled with the
// question still up, and the remote is told to stop.
func TestHeldOpenParkedStreamCancellationStillPropagates(t *testing.T) {
	agent := parkedAgent(t, "which fiscal year?", &resumeRecorder{})
	h := boot(t, oneAgent(agent.URL(), nil))
	human := watchHITL(t, h)

	h.Inject("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-1",
		Name:          "delegate_a2a_researcher",
		Arguments:     map[string]any{"task": "anything"},
		TurnID:        "turn-1",
	})

	waitFor(t, "the question to reach the human", func() bool {
		return len(human.questions()) > 0
	})

	h.Inject("cancel.active", events.CancelActive{
		SchemaVersion: events.CancelActiveVersion,
		TurnID:        "turn-1",
	})

	waitFor(t, "the question to be retracted", func() bool {
		return countEmitted(h, "hitl.cancel") > 0
	})
	waitFor(t, "CancelTask to reach the remote", func() bool {
		for _, id := range agent.cancelledTasks() {
			if id == "held-task" {
				return true
			}
		}
		return false
	})
	waitFor(t, "the cancelled delegation to publish a tool result", func() bool {
		return len(toolResults(h)) > 0
	})

	if res := toolResults(h)[0]; !strings.Contains(res.Error, "do NOT answer it on their behalf") {
		t.Errorf("even a cancelled question must not be handed to the model: %s", res.Error)
	}
}

// TestHeldOpenParkedStreamHonoursTheInputDeadline: the dedicated question
// deadline still frees a delegation nobody answers, and the parked connection
// is released with it.
func TestHeldOpenParkedStreamHonoursTheInputDeadline(t *testing.T) {
	agent := parkedAgent(t, "which fiscal year?", &resumeRecorder{})
	h := boot(t, oneAgent(agent.URL(), map[string]any{
		"hitl": map[string]any{"input_timeout": "150ms"},
	}))
	human := watchHITL(t, h)

	start := time.Now()
	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})
	elapsed := time.Since(start)

	if !strings.Contains(res.Error, "no answer arrived within 150ms") {
		t.Errorf("the error should name the deadline that fired: %s", res.Error)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the delegation took %s; the input deadline should have freed it promptly", elapsed)
	}
	if len(human.questions()) != 1 {
		t.Errorf("the human should have been asked exactly once")
	}
	waitFor(t, "CancelTask on the abandoned remote task", func() bool {
		return len(agent.cancelledTasks()) > 0
	})
}
