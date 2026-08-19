package main

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// This file is the broker's A2A MAPPING: the translation between a leased
// instance's flat IO envelope and the A2A frames a client reads. It is the
// second of the two mappings in this repository that produce A2A output, and
// the only thing it shares with the first (plugins/io/a2a) is pkg/a2a — no
// mapping logic at all.
//
// Because nothing in the type system makes the two agree, both are judged by
// the shared corpus in pkg/a2a/a2aconform: same abstract scenarios, same
// expected frames. a2aconformance_test.go is this mapping's driver for it.
//
// CONCURRENCY. Exactly the model nexus.io.a2a uses, and for the same reason.
// Payloads arrive on the gateway's instance read-pump goroutine; every one of
// them only ever APPENDS to a per-observer buffered channel. The HTTP goroutine
// serving a stream is the sole reader of that channel and the sole writer of
// the socket, so no SSEWriter and no ResponseWriter is ever touched by two
// goroutines. Everything below runs under t.mu; nothing below writes a socket.

// a2aStreamQueueDepth is the buffer on one attached observer's frame channel.
// It is generous on purpose: the producer is a read pump in the middle of
// forwarding an instance frame, and a shallow buffer would put a slow HTTP
// client between the broker and its next payload.
const a2aStreamQueueDepth = 256

// The artifact carrying a turn's final text. Artifact ids must be unique within
// a task and a task is one turn, so a fixed suffix on the task id is unique by
// construction. Both the suffix and the name match nexus.io.a2a exactly: a
// client must not be able to tell which Nexus deployment answered it.
const (
	a2aResponseArtifactSuffix = "-response"
	a2aResponseArtifactName   = "response"
)

// a2aMetadataHITLRequestID is the message-metadata key an INPUT_REQUIRED
// question carries its originating hitl.request id under. Same key as
// nexus.io.a2a's, for the same reason: a Nexus-aware client correlating the two
// views must not need to know which mapping produced the frame.
const a2aMetadataHITLRequestID = "nexus.hitl.requestId"

// The reasons this mapping settles a task with. They are the mapping's own
// prose, not the client's: A2A's CancelTask carries no client reason, and an
// instance that died supplies none either.
const (
	a2aCancelReason       = "the task was canceled by the client"
	a2aDefaultFailReason  = "the agent turn failed"
	a2aDefaultQuestion    = "the agent is waiting for input"
	a2aInstanceGoneReason = "the agent instance stopped responding before the turn finished"
)

// a2aStream is one attached observer of a task: a SendStreamingMessage
// response, or a blocking SendMessage waiting for a frame it must answer on.
//
// Each observer gets its OWN channel rather than sharing one. A shared channel
// is a queue — the first reader to wake takes the frame and the others never
// see it — and two clients watching one task must see the same sequence.
type a2aStream struct {
	frames  chan a2a.StreamResponse
	dropped chan struct{}
	dropOne sync.Once
}

func newA2AStream() *a2aStream {
	return &a2aStream{
		frames:  make(chan a2a.StreamResponse, a2aStreamQueueDepth),
		dropped: make(chan struct{}),
	}
}

// drop marks this observer as no longer being fed. It is idempotent because
// both the task (when the observer falls behind) and the handler (on return)
// reach it.
func (s *a2aStream) drop() { s.dropOne.Do(func() { close(s.dropped) }) }

// a2aParked is the question a task is currently interrupted on.
//
// It carries the choice ids as well as the request id because a client
// answering over A2A can only send text: matching that text against a choice id
// is what lets a multiple-choice question be answered by its id rather than
// degrading every answer to free text.
type a2aParked struct {
	requestID string
	choices   []string
}

func (p a2aParked) live() bool { return p.requestID != "" }

// a2aTask is one A2A task running on one leased Nexus instance.
//
// The Nexus TURN is what a task is anchored to: every payload the instance
// sends carries the turn id it belongs to, the first one binds this task to
// that turn, and a payload naming a different turn is not this task's. The turn
// ending is what completes the task.
type a2aTask struct {
	taskID    string
	contextID string
	// profile is the agent profile this task was addressed to, for logging.
	profile string
	// owner is the principal the creating request authenticated as. Every
	// lookup of this task is scoped by it, so another caller's task is
	// indistinguishable from one that does not exist.
	owner  nexusauth.Principal
	logger *slog.Logger

	// instance is the leased instance this task drives. Payloads go out through
	// it; payloads come back through deliver.
	instance a2aInstance

	// onTerminal is called exactly once, from inside the terminal sequence. It
	// is how the ingress forgets a finished task and releases its lease, so
	// "the lease is returned when the task ends" holds by construction rather
	// than by each caller remembering.
	onTerminal func()

	done     chan struct{}
	closeOne sync.Once

	mu       sync.Mutex
	snapshot a2a.Task
	subs     map[*a2aStream]struct{}
	// working records that the WORKING transition has been emitted, so the
	// first sign of life from the instance moves the task and later ones do
	// not restate it.
	working bool
	// turnID is the Nexus turn this task bound to, empty until the first
	// payload carrying one arrives.
	turnID string
	// segment accumulates stream.delta content for the model response
	// currently in flight; stream.end closes it into lastSegment and resets it.
	//
	// The split matters on a tool-using turn: an intermediate response's prose
	// would otherwise be concatenated onto the answer. Only the LAST segment is
	// a candidate for the response artifact.
	segment     strings.Builder
	lastSegment string
	// finalText is the text an `output` payload published. It WINS over the
	// accumulated stream text, because output is what the instance's output
	// gates let through and the deltas are what the model first proposed.
	finalText string
	parked    a2aParked
}

// a2aTaskConfig is everything a task is constructed from. A struct rather than
// a parameter list because the list had reached the length where two same-typed
// arguments can be transposed without the compiler noticing.
type a2aTaskConfig struct {
	taskID     string
	contextID  string
	profile    string
	owner      nexusauth.Principal
	logger     *slog.Logger
	instance   a2aInstance
	onTerminal func()
}

func newA2ATask(cfg a2aTaskConfig) *a2aTask {
	if cfg.logger == nil {
		cfg.logger = slog.Default()
	}
	return &a2aTask{
		taskID:     cfg.taskID,
		contextID:  cfg.contextID,
		profile:    cfg.profile,
		owner:      cfg.owner,
		logger:     cfg.logger,
		instance:   cfg.instance,
		onTerminal: cfg.onTerminal,
		done:       make(chan struct{}),
		subs:       make(map[*a2aStream]struct{}),
		snapshot:   a2a.NewTask(cfg.taskID, cfg.contextID),
	}
}

// ---- observers ----

// attach registers a new observer and returns it with the snapshot it must open
// on.
//
// The pair is produced under ONE lock acquisition: the snapshot accounts for
// every frame already emitted and the channel carries every frame emitted from
// here on, so nothing is lost between them and nothing is delivered twice.
func (t *a2aTask) attach() (*a2aStream, a2a.Task) {
	s := newA2AStream()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.subs[s] = struct{}{}
	return s, cloneA2ATask(t.snapshot)
}

// detach removes an observer. The owning handler calls it on return, whether
// the response ended normally or the client vanished. It does NOT end the task:
// a task's lifetime is the turn's, not one request's.
func (t *a2aTask) detach(s *a2aStream) {
	if s == nil {
		return
	}
	t.mu.Lock()
	delete(t.subs, s)
	t.mu.Unlock()
	s.drop()
}

// terminated reports whether a terminal frame has been queued.
func (t *a2aTask) terminated() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// emit folds one frame into the snapshot and delivers it to every observer.
//
// It is the ONLY path a frame takes, so a frame reaching one observer but not
// another, or reaching the wire without reaching the snapshot, is not
// expressible.
func (t *a2aTask) emit(frame a2a.StreamResponse) {
	if t.terminated() {
		// Nothing may follow a terminal state on an A2A stream, so a late frame
		// is discarded rather than queued behind a finished one.
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	applyA2AFrame(&t.snapshot, frame)
	for s := range t.subs {
		select {
		case s.frames <- frame:
		default:
			// This observer is too far behind to be given a coherent sequence.
			// Deleting during range is defined in Go and affects only this entry.
			delete(t.subs, s)
			s.drop()
			t.logger.Warn("a2a stream fell behind and was dropped",
				"profile", t.profile, "task_id", t.taskID, "queue_depth", a2aStreamQueueDepth)
		}
	}
}

// snapshotTask returns the task as every frame emitted so far leaves it. It is
// what a non-streaming client is answered with.
func (t *a2aTask) snapshotTask() a2a.Task {
	t.mu.Lock()
	defer t.mu.Unlock()
	return cloneA2ATask(t.snapshot)
}

// ---- instance payloads -> A2A frames ----

// deliver translates one payload from the leased instance.
//
// A payload whose type this broker does not handle is LOGGED AND IGNORED, never
// a task failure: the IO envelope is shared with every other broker client, so
// an instance may legitimately send a type this ingress has no A2A meaning for
// (approval.request today), and an instance newer than its broker must keep
// working.
//
// It runs on the gateway's instance read-pump goroutine and touches no socket.
func (t *a2aTask) deliver(msg brokerIOMessage) {
	if t.terminated() {
		return
	}
	switch msg.Type {
	case ioTypeStreamDelta:
		t.bindTurn(msg.TurnID)
		t.ensureWorking()
		t.appendDelta(msg.Content)

	case ioTypeStreamEnd:
		// A model response ending is NOT the turn ending. Completing here would
		// publish the model's draft: the instance runs its output gates and
		// publishes io.output afterwards, and on a tool-using turn there are
		// several stream ends before the turn is over. It closes the current
		// text segment and nothing else.
		t.bindTurn(msg.TurnID)
		t.ensureWorking()
		t.closeSegment()

	case ioTypeOutput:
		t.bindTurn(msg.TurnID)
		t.ensureWorking()
		t.setFinalText(msg.Content)

	case ioTypeStatus:
		t.onStatus(msg.State)

	case ioTypeHITLRequest:
		t.bindTurn(msg.TurnID)
		t.ensureWorking()
		t.park(a2aParked{requestID: msg.RequestID, choices: msg.choiceIDs()},
			describeA2AQuestion(msg.Prompt, msg.Choices))

	case ioTypeCancelComplete:
		// A cancellation the INSTANCE settled — a /cancel typed into another
		// transport, a gate stopping the turn — reaches A2A as the terminal
		// state it is. A cancellation this ingress started has already settled
		// the task, so this is a no-op for it.
		t.bindTurn(msg.TurnID)
		t.cancel(a2aCancelReason)

	case ioTypeApprovalRequest:
		// Deliberately unmapped. An approval is a question, so INPUT_REQUIRED is
		// the obvious A2A rendering — but nexus.io.a2a does not map approvals
		// either, and pkg/a2a/a2aconform holds no vector for one. Implementing
		// it in the SECOND mapping first is precisely the drift that corpus
		// exists to prevent, so it is recorded and ignored until a vector says
		// what the frames must be.
		t.logger.Warn("a2a ingress received a tool approval request it cannot express; "+
			"the turn will wait for an approval no A2A client can send",
			"profile", t.profile, "task_id", t.taskID, "prompt_id", msg.PromptID)

	default:
		t.logger.Debug("ignoring an instance io payload this a2a mapping does not map",
			"profile", t.profile, "task_id", t.taskID, "type", msg.Type)
	}
}

// bindTurn binds this task to the first Nexus turn it sees, and ignores
// payloads belonging to any other.
//
// A task is one turn. A payload naming a DIFFERENT turn is another turn's, and
// folding it in would attribute one turn's output to another's task. A payload
// naming NO turn is not correlatable at all and is taken, because dropping it
// would leave a client waiting for output that exists.
func (t *a2aTask) bindTurn(turnID string) {
	if turnID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.turnID == "" {
		t.turnID = turnID
	}
}

// boundTurn reports the Nexus turn this task bound to, empty when none has been
// seen. It is what a cancellation names.
func (t *a2aTask) boundTurn() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.turnID
}

// ensureWorking emits the SUBMITTED -> WORKING transition once.
//
// ANY sign of life from the instance drives it, because the IO envelope carries
// no "the turn started" payload: agent.turn.start is not forwarded. The honest
// reading is that the task is working from the moment the instance says
// anything about it at all.
func (t *a2aTask) ensureWorking() {
	t.mu.Lock()
	if t.working {
		t.mu.Unlock()
		return
	}
	t.working = true
	t.mu.Unlock()

	t.emit(a2a.StreamStatusUpdate(a2a.NewStatusUpdate(t.taskID, t.contextID,
		a2a.NewTaskStatus(a2a.TaskStateWorking))))
}

// onStatus maps an io.status payload.
//
// `idle` is the only end-of-turn signal the envelope carries, so it is what
// completes the task. Every other state is progress: it keeps the task WORKING
// and mints no frame of its own, because the detail text describes HOW the
// agent is working rather than a state an A2A client can act on — and a status
// frame per progress line would put frames on the wire that the other mapping
// never produces.
func (t *a2aTask) onStatus(state string) {
	if strings.EqualFold(strings.TrimSpace(state), ioStateIdle) {
		// An idle before the instance has done anything is the instance
		// reporting that it is quiet, not a turn ending. Completing on it would
		// settle a task that never ran.
		if !t.hasWorked() {
			return
		}
		t.complete()
		return
	}
	if strings.TrimSpace(state) == "" {
		return
	}
	t.ensureWorking()
}

// hasWorked reports whether the WORKING transition has been emitted.
func (t *a2aTask) hasWorked() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.working
}

// appendDelta accumulates one streamed text chunk.
func (t *a2aTask) appendDelta(content string) {
	if content == "" {
		return
	}
	t.mu.Lock()
	t.segment.WriteString(content)
	t.mu.Unlock()
}

// closeSegment ends the current model response's text segment.
func (t *a2aTask) closeSegment() {
	t.mu.Lock()
	if text := t.segment.String(); text != "" {
		t.lastSegment = text
		t.segment.Reset()
	}
	t.mu.Unlock()
}

// setFinalText records the text the instance actually published.
func (t *a2aTask) setFinalText(content string) {
	if content == "" {
		return
	}
	t.mu.Lock()
	t.finalText = content
	t.mu.Unlock()
}

// answerText is the text the response artifact carries: what the instance
// published if it published anything, and otherwise the last streamed segment.
//
// The fallback is not a nicety. Every shipped Nexus agent loop tags its
// io.output with metadata streamed=true, and nexus.io.broker drops those, so on
// a streaming turn the ONLY text the envelope carries is the deltas.
func (t *a2aTask) answerText() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finalText != "" {
		return t.finalText
	}
	if t.lastSegment != "" {
		return t.lastSegment
	}
	return t.segment.String()
}

// ---- interruption: INPUT_REQUIRED and back ----

// park moves the task to TASK_STATE_INPUT_REQUIRED carrying the agent's
// question on the status message.
//
// INPUT_REQUIRED is an INTERRUPTION, not a termination (specification section
// 3.1.1): the task stays live and any open stream stays OPEN. Closing on a
// non-terminal state is indistinguishable client-side from a dropped
// connection, which is why pkg/a2a/a2aconform pins the parked stream open.
//
// It reports whether the task actually parked, so a question arriving after the
// task ended is visibly ignored.
func (t *a2aTask) park(in a2aParked, question string) bool {
	if t.terminated() {
		return false
	}
	t.mu.Lock()
	// A second question replaces the first: the state graph permits
	// INPUT_REQUIRED -> INPUT_REQUIRED precisely so an agent may restate what it
	// is waiting for, and only the newest request id will unblock it.
	t.parked = in
	t.mu.Unlock()

	if question == "" {
		question = a2aDefaultQuestion
	}
	message := a2a.NewAgentMessage(newA2AMessageID(), question).
		InContext(t.contextID).
		ForTask(t.taskID)
	if in.requestID != "" {
		// Carried as metadata rather than required of the client: A2A's resume
		// mechanism is the taskId and a conforming client needs nothing else.
		// It is exposed so a Nexus-aware client can correlate the two views.
		message.Metadata = map[string]any{a2aMetadataHITLRequestID: in.requestID}
	}
	t.emit(a2a.StreamStatusUpdate(a2a.NewStatusUpdate(t.taskID, t.contextID,
		a2a.NewTaskStatus(a2a.TaskStateInputRequired).WithMessage(message))))
	return true
}

// pending returns the question this task is parked on, if any.
func (t *a2aTask) pending() (a2aParked, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.parked, t.parked.live()
}

// unpark clears the parked question, reporting the one it cleared. It is the
// state half of resuming, split from the frame half so a caller that must not
// emit (a cancellation) can still disarm.
func (t *a2aTask) unpark(requestID string) (a2aParked, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.parked.live() {
		return a2aParked{}, false
	}
	// An empty requestID matches whatever is parked: an answer with no
	// correlation id can only plausibly be for the one question outstanding.
	if requestID != "" && requestID != t.parked.requestID {
		return a2aParked{}, false
	}
	was := t.parked
	t.parked = a2aParked{}
	return was, true
}

// resume returns a parked task to WORKING, reporting whether it did.
func (t *a2aTask) resume(requestID string) bool {
	if t.terminated() {
		return false
	}
	if _, ok := t.unpark(requestID); !ok {
		return false
	}
	t.emit(a2a.StreamStatusUpdate(a2a.NewStatusUpdate(t.taskID, t.contextID,
		a2a.NewTaskStatus(a2a.TaskStateWorking))))
	return true
}

// ---- terminal transitions ----

// terminate runs the one-shot terminal sequence: clear any parked question,
// emit whatever frames describe the ending, close done so nothing further is
// queued, and release the lease.
//
// Ordering is load-bearing: the frames are emitted BEFORE done closes, because
// emit drops anything queued after a terminal state.
func (t *a2aTask) terminate(frames func()) bool {
	fired := false
	t.closeOne.Do(func() {
		fired = true
		t.mu.Lock()
		t.parked = a2aParked{}
		t.mu.Unlock()

		frames()
		close(t.done)
		if t.onTerminal != nil {
			t.onTerminal()
		}
	})
	return fired
}

// complete publishes the turn's answer and settles the task at COMPLETED,
// exactly once.
//
// Ordering is load-bearing: the artifact MUST precede the terminal status. A2A
// closes a stream the moment a frame reports a terminal state (specification
// section 11.7), so an artifact queued after COMPLETED would be refused and the
// client would see a finished task with no output.
func (t *a2aTask) complete() bool {
	return t.terminate(func() {
		if text := t.answerText(); text != "" {
			t.emit(a2a.StreamArtifactUpdate(a2a.NewArtifactUpdate(t.taskID, t.contextID,
				a2a.NewTextArtifact(t.taskID+a2aResponseArtifactSuffix, a2aResponseArtifactName, text))))
		}
		t.emit(a2a.StreamStatusUpdate(a2a.NewStatusUpdate(t.taskID, t.contextID,
			a2a.NewTaskStatus(a2a.TaskStateCompleted))))
	})
}

// fail settles the task at FAILED carrying reason as the status message.
//
// FAILED is a TASK STATE, not a transport error: the work failed, the protocol
// did not. The terminal-close rule then ends the stream exactly as a success
// would, so a client handles one shape of ending rather than two — which is
// what turns an instance dying mid-turn into an answer rather than a hang.
func (t *a2aTask) fail(reason string) bool {
	if strings.TrimSpace(reason) == "" {
		reason = a2aDefaultFailReason
	}
	return t.endWith(a2a.TaskStateFailed, reason)
}

// cancel settles the task at CANCELED. It reports whether THIS call settled it,
// so a cancellation racing the turn's own ending can tell which won.
func (t *a2aTask) cancel(reason string) bool {
	if strings.TrimSpace(reason) == "" {
		reason = a2aCancelReason
	}
	return t.endWith(a2a.TaskStateCanceled, reason)
}

// endWith settles the task in a terminal state carrying reason as the status
// message.
func (t *a2aTask) endWith(state a2a.TaskState, reason string) bool {
	return t.terminate(func() {
		message := a2a.NewAgentMessage(newA2AMessageID(), reason).
			InContext(t.contextID).
			ForTask(t.taskID)
		t.emit(a2a.StreamStatusUpdate(a2a.NewStatusUpdate(t.taskID, t.contextID,
			a2a.NewTaskStatus(state).WithMessage(message))))
	})
}

// instanceGone settles a task whose leased instance is no longer there.
//
// This is the crash path, and it is why a broker-side mapping can express
// FAILED at all: the instance IO envelope carries no error payload — core.error
// is not forwarded by nexus.io.broker — so the broker learns a turn failed by
// the dial-back socket going away. Rendering that as FAILED with the reason on
// the status message is what stops a crashed instance becoming a stream that
// never ends.
func (t *a2aTask) instanceGone(reason string) bool {
	if strings.TrimSpace(reason) == "" {
		reason = a2aInstanceGoneReason
	}
	settled := t.fail(reason)
	if settled {
		t.logger.Warn("a2a task failed because its agent instance went away",
			"profile", t.profile, "task_id", t.taskID, "reason", reason)
	}
	return settled
}

// ---- broker -> instance ----

// send delivers one IO payload to the leased instance.
//
// A send failure is reported rather than swallowed, because every caller has a
// different right answer: refusing a message that never reached the agent is a
// protocol error the client must see, whereas a cancellation that could not be
// delivered has already settled the task and only needs recording.
func (t *a2aTask) send(msg brokerIOMessage) error {
	if t.instance == nil {
		return fmt.Errorf("task %s has no leased instance", t.taskID)
	}
	return t.instance.SendIO(msg)
}

// ---- helpers ----

// describeA2AQuestion renders a HITL question as the text an A2A client is
// asked.
//
// A2A messages on this transport are text, so a multiple-choice question has to
// be flattened into one: the prompt, then the options by id. The ids are what
// the client echoes back, so they are rendered verbatim rather than numbered — a
// numbered list would invite an answer this mapping cannot map onto a choice.
//
// The rendering is character-for-character nexus.io.a2a's (see
// describeQuestion there). Two Nexus deployments must not ask one question two
// ways.
func describeA2AQuestion(prompt string, choices []brokerIOChoice) string {
	if len(choices) == 0 {
		return prompt
	}
	out := prompt + "\n\nAnswer with one of these option ids:"
	for _, c := range choices {
		label := c.Label
		if label == "" {
			label = c.ID
		}
		out += fmt.Sprintf("\n  %s — %s", c.ID, label)
	}
	return out
}

// matchA2AChoice resolves an answer's text against a parked question's option
// ids, case-insensitively and ignoring surrounding whitespace. It reports the
// canonical id, so the instance sees the id it published rather than the
// client's spelling of it.
func matchA2AChoice(choices []string, text string) (string, bool) {
	answer := strings.TrimSpace(text)
	for _, id := range choices {
		if strings.EqualFold(id, answer) {
			return id, true
		}
	}
	return "", false
}

// applyA2AFrame folds one stream frame into a Task snapshot, reporting whether
// it put the task in a terminal state.
//
// It is the non-streaming renderer of the frames the SSE path writes, and it
// also maintains the task's own snapshot, so a blocking reply and a streaming
// reply describe one task rather than two translations that can drift.
func applyA2AFrame(task *a2a.Task, frame a2a.StreamResponse) bool {
	switch frame.Kind() {
	case a2a.StreamPayloadStatusUpdate:
		task.Status = frame.StatusUpdate.Status
		return task.Status.State.IsTerminal()
	case a2a.StreamPayloadArtifactUpdate:
		task.Artifacts = append(task.Artifacts, frame.ArtifactUpdate.Artifact)
	}
	return false
}

// cloneA2ATask copies a task's slices so a snapshot handed to one observer
// cannot alias the task's own, which keeps a later append off a slice another
// goroutine is reading.
func cloneA2ATask(t a2a.Task) a2a.Task {
	if len(t.Artifacts) > 0 {
		t.Artifacts = append([]a2a.Artifact(nil), t.Artifacts...)
	}
	if len(t.History) > 0 {
		t.History = append([]a2a.Message(nil), t.History...)
	}
	return t
}

// newA2ATaskID mints a server-assigned task id. A2A task ids are
// server-generated and opaque (specification section 4.1); the prefix is for
// humans reading logs, not for clients to parse.
func newA2ATaskID() string { return "task-" + engine.GenerateID() }

// newA2AMessageID mints an id for a message this agent originates.
func newA2AMessageID() string { return "msg-" + engine.GenerateID() }

// newA2AContextID mints a context for a client that named none.
func newA2AContextID() string { return "ctx-" + engine.GenerateID() }
