package a2aremote

import (
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/events"
)

// TestLocalCancellationIssuesCancelTaskToTheRemote is the propagation
// criterion: cancelling the local turn must reach the remote, not just the
// goroutine waiting on it. A remote left working for a turn the user abandoned
// burns somebody else's tokens on an answer nobody will read.
func TestLocalCancellationIssuesCancelTaskToTheRemote(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{
		frameDelay: 50 * time.Millisecond,
		frames: func(*a2a.SendMessageRequest) []a2a.StreamResponse {
			// A long run: the opening task snapshot, then narration that keeps
			// arriving until somebody stops it.
			frames := []a2a.StreamResponse{a2a.StreamTask(a2a.NewTask("slow-task", "c1"))}
			for i := 0; i < 200; i++ {
				status := a2a.NewTaskStatus(a2a.TaskStateWorking).
					WithMessage(a2a.NewAgentMessage("m", "still working"))
				frames = append(frames, a2a.StreamStatusUpdate(
					a2a.NewStatusUpdate("slow-task", "c1", status)))
			}
			return frames
		},
	})
	h := boot(t, oneAgent(agent.URL(), nil))

	h.Inject("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-1",
		Name:          "delegate_a2a_researcher",
		Arguments:     map[string]any{"task": "something long"},
		TurnID:        "turn-1",
	})

	// Wait until the remote task's id has actually been learned; cancelling
	// before the first frame would prove nothing about propagation.
	waitFor(t, "the remote task to start", func() bool {
		for _, ev := range h.PluginEmissions() {
			if ev.Type == "io.output" {
				return true
			}
		}
		return false
	})

	h.Inject("cancel.active", events.CancelActive{
		SchemaVersion: events.CancelActiveVersion,
		TurnID:        "turn-1",
	})

	waitFor(t, "CancelTask to reach the remote", func() bool {
		for _, id := range agent.cancelledTasks() {
			if id == "slow-task" {
				return true
			}
		}
		return false
	})

	// And the delegation itself ends rather than hanging on the stream.
	waitFor(t, "the cancelled delegation to publish a tool result", func() bool {
		return len(toolResults(h)) > 0
	})
	res := toolResults(h)[0]
	if res.Error == "" {
		t.Errorf("a cancelled delegation should report why it stopped: %+v", res)
	}
}

// TestCancellationRetractsAPendingQuestion: a question raised for a turn the
// user has cancelled must not be left sitting in front of them.
func TestCancellationRetractsAPendingQuestion(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{
		frames: askThenAnswer("parked-task", "c1", "which fiscal year?", &resumeRecorder{}),
	})
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
		return len(agent.cancelledTasks()) > 0
	})
	waitFor(t, "the cancelled delegation to publish a tool result", func() bool {
		return len(toolResults(h)) > 0
	})

	res := toolResults(h)[0]
	if !strings.Contains(res.Error, "do NOT answer it on their behalf") {
		t.Errorf("even a cancelled question must not be handed to the model: %s", res.Error)
	}
}

// TestCompletedTaskIsNotCancelled: abandonment is for work this instance walked
// away from, and a task that finished is not that. Sending CancelTask anyway
// would be noise on the remote and a lie in its task history.
func TestCompletedTaskIsNotCancelled(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{})
	h := boot(t, oneAgent(agent.URL(), nil))

	if res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"}); res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}
	// Give any stray cancel a chance to land before asserting its absence.
	time.Sleep(100 * time.Millisecond)

	if got := agent.cancelledTasks(); len(got) != 0 {
		t.Errorf("a completed task was cancelled anyway: %v", got)
	}
}
