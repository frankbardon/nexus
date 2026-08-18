package a2a

import (
	"log/slog"
	"sync"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// runQueueDepth is the buffer on one attached stream's frame channel. It is
// generous on purpose: the producer side is an engine goroutine in the middle of
// dispatching a bus event, and a shallow buffer would put a slow HTTP client
// between the agent loop and its next event. A turn produces a handful of frames
// today, so the buffer is never approached; it exists so that a future story
// adding per-delta artifact chunks does not have to revisit the concurrency
// model.
const runQueueDepth = 256

// artifactSuffix names the one artifact a turn produces: the final assistant
// text. Artifact ids must be unique within a task, and a task is one turn, so a
// fixed suffix on the task id is unique by construction.
const artifactSuffix = "-response"

// artifactName is the human-readable label on that artifact.
const artifactName = "response"

// stream is one attached observer of a run: a SendStreamingMessage response, a
// blocking SendMessage waiting for a terminal frame, or a SubscribeToTask
// stream that joined mid-turn.
//
// Each observer gets its OWN buffered channel rather than sharing one, because a
// shared channel is a queue — the first reader to wake takes the frame and the
// others never see it — and the whole point of SubscribeToTask is that every
// attached stream sees the same sequence.
type stream struct {
	// frames carries the run's frames to this observer, in run order.
	frames chan a2a.StreamResponse
	// dropped closes when the run will send this observer nothing further: it
	// fell too far behind, or its owner detached. It exists so a reader parks on
	// a channel receive rather than polling, and so a dropped observer ends its
	// response instead of hanging on a channel nobody will write to again.
	dropped chan struct{}
	dropOne sync.Once
}

func newStream() *stream {
	return &stream{
		frames:  make(chan a2a.StreamResponse, runQueueDepth),
		dropped: make(chan struct{}),
	}
}

// drop marks the observer finished. It is idempotent: the run drops a stream
// that overflowed, and the owning HTTP handler drops the same stream when it
// returns.
func (s *stream) drop() { s.dropOne.Do(func() { close(s.dropped) }) }

// run is one in-flight A2A task: exactly one Nexus agent turn, rendered as one
// Task moving SUBMITTED -> WORKING -> COMPLETED (or FAILED).
//
// # Concurrency
//
// This is nexus.io.agui's model, extended in exactly one direction: from one
// observer to many. Bus handlers run on arbitrary engine goroutines and NEVER
// touch an SSE writer. Each handler translates its payload into A2A stream
// frames and hands them to emit, which — under r.mu — folds the frame into the
// task snapshot and copies it into every attached stream's buffered channel.
// Each attached stream is read by exactly ONE HTTP handler goroutine, which is
// the sole writer of that goroutine's response. So a frame crosses exactly one
// channel between the bus and the wire, no matter how many clients are watching.
//
// Delivery is non-blocking. Fanning out to N observers under a lock must not
// admit a case where one wedged client stalls the agent loop, so a stream whose
// buffer is full is dropped rather than waited on. Dropping ENDS that observer's
// response: a stream that skipped a frame would deliver a gapped sequence, and a
// conforming client validates transitions and would reject it. Every other
// observer is unaffected.
//
// The snapshot is what makes attaching race-free. attach registers the new
// stream and copies the snapshot under one acquisition of r.mu, so the snapshot
// accounts for exactly the frames emitted before registration and the channel
// carries exactly the frames emitted after: no gap, no duplicate.
type run struct {
	// taskID and contextID are fixed at construction and read without the lock.
	taskID    string
	contextID string

	// sink is the durable record of this task, already scoped to the principal
	// that created it. Every frame the run produces is written through it
	// before it is queued for the wire — see record. Nil only in transport-only
	// tests that construct a run directly.
	sink taskSink
	// logger reports a store write that failed. A failed write does NOT fail
	// the turn: the client is owed the answer the agent produced, and a store
	// that cannot record it is an operator problem, not a protocol one.
	logger *slog.Logger

	// done is closed once a terminal frame has been queued, so later bus
	// events for this run are dropped rather than queued behind a stream that
	// is already finished.
	done     chan struct{}
	closeOne sync.Once

	mu sync.Mutex
	// subs is every attached observer. See the concurrency note above.
	subs map[*stream]struct{}
	// snapshot is the task as every frame emitted so far leaves it. It is what a
	// stream attaching mid-turn opens on.
	snapshot a2a.Task
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

// newRun builds a run for one task, bound to the durable record it writes
// through to. It starts with no observers; the request that created the task
// attaches one before the turn is allowed to emit anything.
func newRun(taskID, contextID string, sink taskSink, logger *slog.Logger) *run {
	return &run{
		taskID:    taskID,
		contextID: contextID,
		sink:      sink,
		logger:    logger,
		done:      make(chan struct{}),
		subs:      make(map[*stream]struct{}),
		snapshot:  a2a.NewTask(taskID, contextID),
	}
}

// attach registers a new observer and returns it alongside the task snapshot
// that observer must open on.
//
// The pair is produced under one lock acquisition on purpose: the snapshot
// accounts for every frame already emitted, and the stream receives every frame
// emitted from here on. Taking them separately would either lose a frame emitted
// in between or replay one the snapshot already reflects.
func (r *run) attach() (*stream, a2a.Task) {
	s := newStream()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subs[s] = struct{}{}
	return s, cloneTask(r.snapshot)
}

// detach removes an observer. The owning HTTP handler calls it on return,
// whether the response ended normally or the client vanished.
func (r *run) detach(s *stream) {
	if s == nil {
		return
	}
	r.mu.Lock()
	delete(r.subs, s)
	r.mu.Unlock()
	s.drop()
}

// terminated reports whether a terminal frame has been queued.
func (r *run) terminated() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// emit persists one frame, folds it into the snapshot and delivers it to every
// attached observer.
//
// It is the ONLY path a frame takes. A frame reaching the wire without reaching
// the store, or reaching one observer but not another, is not expressible: both
// happen here or neither does.
func (r *run) emit(frame a2a.StreamResponse) {
	if r.terminated() {
		// Nothing may follow a terminal state on the wire, so a late frame is
		// discarded rather than queued behind a finished stream.
		return
	}
	// Persisted BEFORE delivery, and outside r.mu because this touches SQLite.
	r.record(frame)

	r.mu.Lock()
	defer r.mu.Unlock()
	applyFrame(&r.snapshot, frame)
	for s := range r.subs {
		select {
		case s.frames <- frame:
		default:
			// This observer is too far behind to be given a coherent sequence.
			// Deleting during range is defined behaviour in Go and only affects
			// this one entry.
			delete(r.subs, s)
			s.drop()
			if r.logger != nil {
				r.logger.Warn("a2a stream fell behind and was dropped",
					"task_id", r.taskID, "queue_depth", runQueueDepth)
			}
		}
	}
}

// record persists one frame to the durable task record.
//
// This is the ONLY write-through point, and it sits on the single enqueue path
// rather than on each translator, so a transition cannot reach the wire without
// reaching the store: adding a frame type to a translator persists it by
// construction. It runs BEFORE the delivery so the store never lags what a
// client has already been told.
//
// Callers must not hold r.mu: this touches SQLite.
func (r *run) record(frame a2a.StreamResponse) {
	if r.sink == nil {
		return
	}
	var err error
	switch frame.Kind() {
	case a2a.StreamPayloadStatusUpdate:
		err = r.sink.RecordStatus(r.taskID, frame.StatusUpdate.Status)
	case a2a.StreamPayloadArtifactUpdate:
		err = r.sink.RecordArtifact(r.taskID, frame.ArtifactUpdate.Artifact)
	default:
		return
	}
	if err != nil && r.logger != nil {
		r.logger.Warn("a2a task store write failed",
			"task_id", r.taskID, "frame", string(frame.Kind()), "error", err)
	}
}

// recordMessage persists one message reference, logging rather than failing the
// turn if the store refuses it. Callers must not hold r.mu.
func (r *run) recordMessage(ref messageRef) {
	if r.sink == nil {
		return
	}
	if err := r.sink.RecordMessage(r.taskID, ref); err != nil && r.logger != nil {
		r.logger.Warn("a2a task store message write failed", "task_id", r.taskID, "error", err)
	}
}

// --- bus event translation ---
//
// Each on* method translates one Nexus bus payload into zero or more A2A stream
// frames and emits them. They run on arbitrary bus goroutines.

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

	r.emit(a2a.StreamStatusUpdate(a2a.NewStatusUpdate(r.taskID, r.contextID,
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
			// The reply is recorded as a message reference as well as an
			// artifact. The artifact is the OUTPUT; the reference is the other
			// half of the exchange, so a stored task can answer "what was said"
			// rather than only "what was produced". Both are bounded by the
			// same retention policy.
			r.recordMessage(messageRef{
				MessageID: newMessageID(),
				Role:      a2a.RoleAgent,
				Text:      text,
			})
			r.emit(a2a.StreamArtifactUpdate(a2a.NewArtifactUpdate(r.taskID, r.contextID,
				a2a.NewTextArtifact(r.taskID+artifactSuffix, artifactName, text))))
		}
		r.emit(a2a.StreamStatusUpdate(a2a.NewStatusUpdate(r.taskID, r.contextID,
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
		r.emit(a2a.StreamStatusUpdate(a2a.NewStatusUpdate(r.taskID, r.contextID,
			a2a.NewTaskStatus(a2a.TaskStateFailed).WithMessage(message))))
		close(r.done)
	})
}

// applyFrame folds one stream frame into a Task snapshot. It reports whether the
// frame put the task in a terminal state, which is what ends a blocking wait.
//
// It is the non-streaming renderer of the same frames the SSE path writes, and
// it is also what maintains the run's own snapshot, so a blocking reply, a
// streaming reply and a mid-turn subscription all describe one task rather than
// three translations that can drift.
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

// cloneTask copies a task's slices so a snapshot handed to one observer cannot
// alias the run's own, which keeps a later append off a slice another goroutine
// is reading.
func cloneTask(t a2a.Task) a2a.Task {
	if len(t.Artifacts) > 0 {
		t.Artifacts = append([]a2a.Artifact(nil), t.Artifacts...)
	}
	if len(t.History) > 0 {
		t.History = append([]a2a.Message(nil), t.History...)
	}
	return t
}

// newTaskID mints a server-assigned task id. A2A task ids are server-generated
// and opaque (specification section 4.1); the "task-" prefix is for humans
// reading logs, not for clients to parse.
func newTaskID() string { return "task-" + engine.GenerateID() }

// newMessageID mints an id for a message this agent originates.
func newMessageID() string { return "msg-" + engine.GenerateID() }
