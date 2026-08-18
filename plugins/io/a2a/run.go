package a2a

import (
	"sync"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// runQueueDepth is the buffer on a run's frame channel. It is generous on
// purpose: the producer side is an engine goroutine in the middle of dispatching
// a bus event, and a full channel would stall the agent loop behind a slow HTTP
// client. A turn produces a handful of frames today, so the buffer is never
// approached; it exists so that a future story adding per-delta artifact chunks
// does not have to revisit the concurrency model.
const runQueueDepth = 256

// artifactSuffix names the one artifact a turn produces: the final assistant
// text. Artifact ids must be unique within a task, and a task is one turn, so a
// fixed suffix on the task id is unique by construction.
const artifactSuffix = "-response"

// artifactName is the human-readable label on that artifact.
const artifactName = "response"

// run is one in-flight A2A task: exactly one Nexus agent turn, rendered as one
// Task moving SUBMITTED -> WORKING -> COMPLETED (or FAILED).
//
// # Concurrency
//
// This is nexus.io.agui's model, deliberately unchanged. Bus handlers run on
// arbitrary engine goroutines and NEVER touch the SSE writer: each handler
// translates its payload into A2A stream frames and pushes them onto out. The
// HTTP handler goroutine is the sole reader of out and the sole writer to the
// response, whether it is rendering an SSE stream or folding the frames into a
// blocking Task reply. Every field below is guarded by mu; delivery is by
// channel. That is what keeps the race detector quiet.
type run struct {
	// taskID and contextID are fixed at construction and read without the lock.
	taskID    string
	contextID string

	// out carries every translated frame. See the concurrency note above.
	out chan a2a.StreamResponse
	// done is closed once a terminal frame has been queued, so later bus
	// events for this run are dropped rather than queued behind a stream that
	// is already finished.
	done     chan struct{}
	closeOne sync.Once

	mu sync.Mutex
	// turnID is the Nexus turn this run bound to, taken from the first
	// agent.turn.start observed. It exists so a turn.end belonging to some
	// other agent loop cannot terminate this task early.
	turnID string
	// working records that the WORKING status update has been queued, so
	// repeated turn starts (a resumed turn) do not restate it.
	working bool
	// finalText is the assistant text the turn will publish as its artifact.
	// Last write wins: llm.response supplies it, and io.output overwrites with
	// whatever the output gates actually let through.
	finalText string
}

// newRun builds a run for one task.
func newRun(taskID, contextID string) *run {
	return &run{
		taskID:    taskID,
		contextID: contextID,
		out:       make(chan a2a.StreamResponse, runQueueDepth),
		done:      make(chan struct{}),
	}
}

// openingTask is the Task snapshot that opens the response: a stream's first
// frame, and the seed a blocking reply folds updates into. A2A requires a task
// to exist before any update event may name it, and SUBMITTED is the state a
// task is created in (specification section 4.1).
func (r *run) openingTask() a2a.Task {
	return a2a.NewTask(r.taskID, r.contextID)
}

// queue pushes a frame unless the run has already terminated. It blocks only
// while the buffer is full and the run is live, so a burst never drops a frame
// silently; once done is closed the frame is discarded, which is correct because
// nothing may follow a terminal state on the wire.
func (r *run) queue(frame a2a.StreamResponse) {
	select {
	case <-r.done:
		return
	default:
	}
	select {
	case r.out <- frame:
	case <-r.done:
	}
}

// push is the terminal-path enqueue: non-blocking, used from inside closeOne
// where blocking on a full buffer would deadlock a bus goroutine against a
// reader that has gone away. Dropping is unreachable in practice (a turn queues
// three frames into a 256-slot buffer) and is preferable to a stuck engine.
func (r *run) push(frame a2a.StreamResponse) {
	select {
	case r.out <- frame:
	default:
	}
}

// --- bus event translation ---
//
// Each on* method translates one Nexus bus payload into zero or more A2A stream
// frames and queues them. They run on arbitrary bus goroutines.

// onTurnStart maps agent.turn.start to the WORKING status update, and binds the
// run to the turn so a foreign turn.end cannot terminate it.
func (r *run) onTurnStart(t events.TurnInfo) {
	r.mu.Lock()
	if r.turnID == "" {
		r.turnID = t.TurnID
	}
	if r.working {
		r.mu.Unlock()
		return
	}
	r.working = true
	r.mu.Unlock()

	r.queue(a2a.StreamStatusUpdate(a2a.NewStatusUpdate(r.taskID, r.contextID,
		a2a.NewTaskStatus(a2a.TaskStateWorking))))
}

// onLLMResponse records the terminal assistant text. A response carrying tool
// calls is an intermediate step of the agent loop, not the turn's answer, so
// only a tool-call-free response contributes.
func (r *run) onLLMResponse(resp events.LLMResponse) {
	if len(resp.ToolCalls) > 0 || resp.Content == "" {
		return
	}
	r.mu.Lock()
	r.finalText = resp.Content
	r.mu.Unlock()
}

// onOutput records the assistant text the transport layer actually published.
// It overwrites what llm.response supplied because an output gate may have
// rewritten, redacted or replaced it between the two events, and the artifact
// must carry what Nexus decided to say — not what the model first proposed.
func (r *run) onOutput(o events.AgentOutput) {
	if o.Content == "" {
		return
	}
	r.mu.Lock()
	r.finalText = o.Content
	r.mu.Unlock()
}

// onTurnEnd terminates the task when the turn this run bound to ends. It
// reports whether it claimed the event, so an unrelated agent's turn.end is
// visibly ignored rather than silently swallowed.
func (r *run) onTurnEnd(t events.TurnInfo) bool {
	r.mu.Lock()
	bound := r.turnID
	r.mu.Unlock()
	// An empty TurnID on either side means "not correlatable"; the run takes it,
	// because a transport that ignores an uncorrelatable turn end leaves the
	// client waiting forever, which is the worse failure.
	if bound != "" && t.TurnID != "" && bound != t.TurnID {
		return false
	}
	r.complete()
	return true
}

// complete emits the turn's artifact and the COMPLETED status, exactly once.
//
// Ordering is load-bearing: the artifact MUST precede the terminal status.
// A2A closes a stream the moment a frame reports a terminal state
// (specification section 11.7), so an artifact queued after COMPLETED would be
// refused by SSEWriter and the client would see a completed task with no output.
func (r *run) complete() {
	r.closeOne.Do(func() {
		r.mu.Lock()
		text := r.finalText
		r.mu.Unlock()

		if text != "" {
			r.push(a2a.StreamArtifactUpdate(a2a.NewArtifactUpdate(r.taskID, r.contextID,
				a2a.NewTextArtifact(r.taskID+artifactSuffix, artifactName, text))))
		}
		r.push(a2a.StreamStatusUpdate(a2a.NewStatusUpdate(r.taskID, r.contextID,
			a2a.NewTaskStatus(a2a.TaskStateCompleted))))
		close(r.done)
	})
}

// fail terminates the task with FAILED, carrying the reason as the status
// message.
//
// FAILED is a task state, not a transport error: the work failed, the protocol
// did not. Section 11.7's terminal-close rule then ends the stream exactly as a
// success would, so a client handles one shape of ending rather than two.
func (r *run) fail(reason string) {
	r.closeOne.Do(func() {
		if reason == "" {
			reason = "the agent turn failed"
		}
		message := a2a.NewAgentMessage(newMessageID(), reason).
			InContext(r.contextID).
			ForTask(r.taskID)
		r.push(a2a.StreamStatusUpdate(a2a.NewStatusUpdate(r.taskID, r.contextID,
			a2a.NewTaskStatus(a2a.TaskStateFailed).WithMessage(message))))
		close(r.done)
	})
}

// applyFrame folds one stream frame into a Task snapshot, for the blocking
// SendMessage reply. It reports whether the frame put the task in a terminal
// state, which is what ends the wait.
//
// It is the non-streaming renderer of the same frames the SSE path writes, so
// both bindings answer from one translation rather than two that can drift.
func applyFrame(task *a2a.Task, frame a2a.StreamResponse) bool {
	switch frame.Kind() {
	case a2a.StreamPayloadStatusUpdate:
		task.Status = frame.StatusUpdate.Status
		return task.Status.State.IsTerminal()
	case a2a.StreamPayloadArtifactUpdate:
		task.Artifacts = append(task.Artifacts, frame.ArtifactUpdate.Artifact)
	}
	return false
}

// newTaskID mints a server-assigned task id. A2A task ids are server-generated
// and opaque (specification section 4.1); the "task-" prefix is for humans
// reading logs, not for clients to parse.
func newTaskID() string { return "task-" + engine.GenerateID() }

// newMessageID mints an id for a message this agent originates.
func newMessageID() string { return "msg-" + engine.GenerateID() }
