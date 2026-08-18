package a2a

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

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

// artifactSuffix names the artifact carrying the turn's final assistant text.
// Artifact ids must be unique within a task, and a task is one turn, so a fixed
// suffix on the task id is unique by construction. Tool results, written files
// and the suppression notice carry their own suffixes — see artifacts.go.
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
	// nexusExtension records that this observer opted into the Nexus extension
	// with the A2A-Extensions service parameter. It is fixed at attach time and
	// read without the lock. Telemetry frames are copied ONLY into the channels
	// of observers that set it — see run.emitTelemetry — so a client that did
	// not ask for the extension is not force-fed it (specification section 8.4).
	nexusExtension bool
	// frames carries the run's frames to this observer, in run order.
	frames chan a2a.StreamResponse
	// dropped closes when the run will send this observer nothing further: it
	// fell too far behind, or its owner detached. It exists so a reader parks on
	// a channel receive rather than polling, and so a dropped observer ends its
	// response instead of hanging on a channel nobody will write to again.
	dropped chan struct{}
	dropOne sync.Once
}

// streamOptions is what an attaching observer declares about itself. It is a
// struct rather than a bare bool so a later service parameter has somewhere to
// land without touching every call site.
type streamOptions struct {
	// nexusExtension is the client's A2A-Extensions opt-in for the Nexus
	// extension.
	nexusExtension bool
}

func newStream(opts streamOptions) *stream {
	return &stream{
		nexusExtension: opts.nexusExtension,
		frames:         make(chan a2a.StreamResponse, runQueueDepth),
		dropped:        make(chan struct{}),
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
	// onTerminal releases the plugin's active-task slot. See newRun.
	onTerminal func()
	// policy bounds what this task may publish as artifacts. Immutable after
	// construction.
	policy artifactPolicy
	// textOnly records that the requesting client restricted itself to text
	// output modes, so this task publishes no JSON or inline-file parts. See
	// runConfig.textOnly.
	textOnly bool

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
	// parked is the human-in-the-loop question this task is currently waiting
	// on, or the zero value when it is not parked. It is what makes a resuming
	// message routable: the client names the task, and this names the pending
	// hitl.requested the answer belongs to.
	parked parkedInput
	// inputTimer bounds that wait. It is armed when the task parks and stopped
	// on resume and on any terminal transition, so a task cannot be terminated
	// twice by a timer that outlived the question it was watching.
	inputTimer *time.Timer
	// budget is this task's artifact allowance. See artifacts.go: unconditional
	// tool-result artifacts times inline file parts times a disk-persisted store
	// is an unbounded product, and this is what bounds it.
	budget *artifactBudget
	// seq numbers Nexus extension events within the task, so a client can
	// reassemble their order across the two carriers the extension defines.
	seq int
	// artifactSeq is the fallback ordinal for an artifact whose natural id is
	// missing (a tool result with no call id). It is separate from seq on
	// purpose: the extension's sequence numbers are a client-visible ordering
	// and must not be consumed by an id this transport minted for itself.
	artifactSeq int
	// extSubs counts attached observers that opted into the Nexus extension. It
	// is what lets a translator skip building telemetry nobody asked for, which
	// matters on a long tool-heavy turn.
	extSubs int
	// structuredSchema names the JSON schema the turn's output was constrained
	// to, when an llm.request declared one. It only labels the artifact; the
	// JSON part is emitted on the strength of the text parsing as JSON.
	structuredSchema string
}

// parkedInput is the human-in-the-loop question a task is parked on.
//
// It carries the choice ids as well as the request id because a client
// answering over A2A can only send text: matching that text against a choice id
// is what lets a multiple-choice ask_user be answered by a remote agent at all,
// rather than degrading every question to free text.
type parkedInput struct {
	requestID string
	choices   []string
}

// live reports whether this is a real parked question rather than the zero
// value.
func (p parkedInput) live() bool { return p.requestID != "" }

// newRun builds a run for one task, bound to the durable record it writes
// through to. It starts with no observers; the request that created the task
// attaches one before the turn is allowed to emit anything.
//
// onTerminal is called exactly once, from inside the one-shot terminal
// sequence, and is how the plugin's single active-task slot is returned. Wiring
// it HERE rather than in the HTTP handler is the whole of the detached-lifetime
// change: a run now ends when its TASK ends, not when the request that started
// it returns.
func newRun(cfg runConfig) *run {
	return &run{
		taskID:     cfg.taskID,
		contextID:  cfg.contextID,
		sink:       cfg.sink,
		logger:     cfg.logger,
		policy:     cfg.artifacts,
		textOnly:   cfg.textOnly,
		onTerminal: cfg.onTerminal,
		done:       make(chan struct{}),
		subs:       make(map[*stream]struct{}),
		snapshot:   a2a.NewTask(cfg.taskID, cfg.contextID),
		budget:     newArtifactBudget(cfg.artifacts.maxTaskBytes),
	}
}

// runConfig is everything a run is constructed from. It is a struct rather than
// a parameter list because the list had already reached the length where a
// caller can transpose two arguments of the same type without the compiler
// noticing.
type runConfig struct {
	taskID    string
	contextID string
	sink      taskSink
	logger    *slog.Logger
	// artifacts bounds what the task may publish.
	artifacts artifactPolicy
	// textOnly is set when the creating request's acceptedOutputModes named only
	// text types. A2A lets a client state what it can render (section 3.2.2), and
	// posting base64 file parts to a client that said "text/plain" would answer a
	// question it did not ask. Non-text parts are then omitted, and a file
	// degrades to the same metadata note an oversized one gets.
	textOnly   bool
	onTerminal func()
}

// attach registers a new observer and returns it alongside the task snapshot
// that observer must open on.
//
// The pair is produced under one lock acquisition on purpose: the snapshot
// accounts for every frame already emitted, and the stream receives every frame
// emitted from here on. Taking them separately would either lose a frame emitted
// in between or replay one the snapshot already reflects.
func (r *run) attach(opts streamOptions) (*stream, a2a.Task) {
	s := newStream(opts)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subs[s] = struct{}{}
	if s.nexusExtension {
		r.extSubs++
	}
	return s, cloneTask(r.snapshot)
}

// detach removes an observer. The owning HTTP handler calls it on return,
// whether the response ended normally or the client vanished.
func (r *run) detach(s *stream) {
	if s == nil {
		return
	}
	r.mu.Lock()
	r.removeLocked(s)
	r.mu.Unlock()
	s.drop()
}

// removeLocked forgets an observer and keeps the extension-subscriber count in
// step. Every removal path goes through it, so the count cannot drift from the
// map it summarizes. The caller holds r.mu.
func (r *run) removeLocked(s *stream) {
	if _, present := r.subs[s]; !present {
		return
	}
	delete(r.subs, s)
	if s.nexusExtension {
		r.extSubs--
	}
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
			r.removeLocked(s)
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

// emitTelemetry delivers one Nexus extension frame to the observers that asked
// for the extension, and to nobody else.
//
// It is the deliberate exception to emit's "every frame is persisted first"
// rule, and the exception is the point: telemetry is not task state. Persisting
// it would write a WORKING transition into task_status_history for every
// thinking step and tool call, so GetTask would replay a turn's reasoning as a
// sequence of state changes the task never made — and a long turn would bloat
// the store with them. It is also not folded into the snapshot, for the same
// reason: a client attaching mid-turn opens on what the task IS, not on the last
// thing it was told about.
func (r *run) emitTelemetry(frame a2a.StreamResponse) {
	if r.terminated() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for s := range r.subs {
		if !s.nexusExtension {
			continue
		}
		select {
		case s.frames <- frame:
		default:
			r.removeLocked(s)
			s.drop()
			if r.logger != nil {
				r.logger.Warn("a2a stream fell behind and was dropped",
					"task_id", r.taskID, "queue_depth", runQueueDepth)
			}
		}
	}
}

// emitNexus builds and delivers one Nexus extension event.
//
// The event is built LAZILY and only when at least one attached observer opted
// in, which is what keeps the extension free for the common case: a tool-heavy
// turn with no extension client marshals nothing at all.
//
// The status the frame carries is the task's CURRENT state rather than a
// hardcoded WORKING. A telemetry frame emitted while the task is parked at
// INPUT_REQUIRED must not tell a client the task went back to work; re-entering
// an active state is exactly what the transition table permits for an agent
// re-emitting a state with new information attached.
func (r *run) emitNexus(build func() a2a.NexusEvent) {
	if r.terminated() {
		return
	}
	r.mu.Lock()
	if r.extSubs == 0 {
		r.mu.Unlock()
		return
	}
	r.seq++
	seq := r.seq
	state := r.snapshot.Status.State
	r.mu.Unlock()

	if !state.Known() || state == a2a.TaskStateUnspecified {
		state = a2a.TaskStateSubmitted
	}
	frame, err := nexusEventFrame(r.taskID, r.contextID, state, build().Seq(seq))
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("a2a nexus extension event could not be encoded",
				"task_id", r.taskID, "error", err)
		}
		return
	}
	r.emitTelemetry(frame)
}

// publishArtifact charges an artifact against the task's budget and emits it, or
// records that it was suppressed.
//
// The budget is the whole of the bound this story exists under: tool results are
// artifacts unconditionally, files ride inline, and everything lands in a
// disk-persisted store, so without a per-task ceiling the store grows with how
// chatty a turn was rather than with how many turns there were. A refused
// artifact is COUNTED and the first refusal mints one notice artifact saying so,
// because the failure mode worth avoiding is not "the client got less" — it is
// "the client got less and was not told".
func (r *run) publishArtifact(art a2a.Artifact) {
	if r.terminated() {
		return
	}
	size := artifactSize(art)

	r.mu.Lock()
	admitted := r.budget.charge(size)
	notice := false
	suppressed := 0
	if !admitted {
		notice = r.budget.needsNotice()
		suppressed = r.budget.suppressed
	}
	r.mu.Unlock()

	if admitted {
		r.emit(a2a.StreamArtifactUpdate(a2a.NewArtifactUpdate(r.taskID, r.contextID, art)))
		return
	}
	if notice {
		r.emit(a2a.StreamArtifactUpdate(a2a.NewArtifactUpdate(r.taskID, r.contextID,
			suppressionNotice(r.taskID, suppressed))))
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
	// Token accounting is reported for EVERY response, including the
	// intermediate tool-calling ones: the cost of a turn is the sum of the calls
	// it made, and reporting only the last would understate a ten-iteration turn
	// by an order of magnitude.
	if hasUsage(resp.Usage) {
		r.emitNexus(func() a2a.NexusEvent { return usageEvent(r.taskID, r.contextID, resp) })
	}
	if len(resp.ToolCalls) > 0 || resp.Content == "" {
		return
	}
	r.mu.Lock()
	r.finalText = resp.Content
	r.mu.Unlock()
}

// onLLMRequest records the JSON schema the turn's output is constrained to, when
// the request declared one.
//
// It only LABELS the response artifact. Whether a JSON part is emitted is
// decided by whether the final text actually parses as JSON, because an output
// schema can be enforced several ways in Nexus — a provider's native structured
// output, a tool-use-as-schema simulation, or nexus.gate.json_schema validating
// after the fact — and only the first of those is visible here. Keying the part
// off the parse covers all three; keying the label off the request means the
// label is only claimed when it is known to be true.
//
// A request tagged with _source belongs to a planner or another plugin's own
// model call rather than to the turn's answer, and is ignored for the same
// reason the ReAct agent ignores its response.
func (r *run) onLLMRequest(req events.LLMRequest) {
	if req.ResponseFormat == nil {
		return
	}
	if _, internal := req.Metadata["_source"]; internal {
		return
	}
	switch req.ResponseFormat.Type {
	case "json_schema", "json_object":
	default:
		return
	}
	name := req.ResponseFormat.Name
	if name == "" {
		name = req.ResponseFormat.Type
	}
	r.mu.Lock()
	r.structuredSchema = name
	r.mu.Unlock()
}

// onThinking reports one reasoning step as extension telemetry. It is telemetry
// and not an artifact on purpose: a reasoning step is how the agent reached its
// answer, not part of the answer.
func (r *run) onThinking(step events.ThinkingStep) {
	if step.Content == "" {
		return
	}
	r.emitNexus(func() a2a.NexusEvent { return thinkingEvent(r.taskID, r.contextID, step) })
}

// onToolCall reports a tool invocation as extension telemetry. The RESULT
// becomes an artifact; the call does not, because a call is an intention and an
// artifact is output.
func (r *run) onToolCall(call events.ToolCall) {
	if call.Name == "" || call.ID == "" {
		return
	}
	r.emitNexus(func() a2a.NexusEvent { return toolCallEvent(r.taskID, r.contextID, call) })
}

// onToolResult publishes a tool outcome: the live extension signal first, then
// the artifact that outlives the stream, then an artifact per file the result
// reported writing.
//
// The tool-result artifact is UNCONDITIONAL. It is not behind a config flag, and
// deliberately so: an interop transport whose observability depends on the
// operator having switched it on is one a partner cannot rely on. The volume
// that buys is answered by the budget rather than by a flag — see
// publishArtifact.
func (r *run) onToolResult(res events.ToolResult) {
	if res.Name == "" {
		return
	}
	r.emitNexus(func() a2a.NexusEvent {
		return toolResultEvent(r.taskID, r.contextID, res, r.policy.maxToolOutputBytes)
	})

	r.mu.Lock()
	r.artifactSeq++
	seq := r.artifactSeq
	r.mu.Unlock()
	r.publishArtifact(toolResultArtifact(r.taskID, seq, res, r.policy, !r.textOnly))

	for _, f := range detectWrittenFiles(res, r.policy) {
		art, ok := fileArtifact(r.taskID, f, r.policy, !r.textOnly)
		if !ok {
			continue
		}
		r.publishArtifact(art)
	}
}

// onSubagent reports delegated-work progress as extension telemetry.
func (r *run) onSubagent(build func() a2a.NexusEvent) { r.emitNexus(build) }

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
	// A turn end naming a turn is only this task's if this task saw that turn
	// start. Both halves matter:
	//
	//   - A DIFFERENT id is somebody else's turn, and always was.
	//   - An id when this run has bound NONE is also somebody else's: a run is
	//     registered before its io.input is emitted, so it cannot miss its own
	//     turn start. This case became reachable when task lifetime detached
	//     from the request — a cancellation releases the slot and then asks the
	//     agent to stop, and the turn end that follows must not be able to
	//     complete whichever task started next.
	//
	// An EMPTY id on the event is not correlatable at all, and the run takes it:
	// ignoring it would leave the client waiting forever, which is worse.
	if t.TurnID != "" && bound != t.TurnID {
		return false
	}
	r.complete()
	return true
}

// --- interruption: INPUT_REQUIRED and back ---

// park moves the task to TASK_STATE_INPUT_REQUIRED, carrying the agent's
// question on the attached status message, and arms the input deadline.
//
// INPUT_REQUIRED is an INTERRUPTION, not a termination (specification section
// 3.1.1): the task stays live, any open stream stays open, and the client is
// expected to resume by sending a new message naming the same taskId and
// contextId. Everything about the parked state is therefore recoverable —
// the transition is written through to the store like any other, so a client
// that reconnects with GetTask or SubscribeToTask sees the question it missed.
//
// timeout bounds the wait; zero leaves the task parked indefinitely. onExpired
// runs on the timer's own goroutine when it elapses. It reports whether the
// task actually parked, so a question arriving after the task ended is visibly
// ignored.
func (r *run) park(in parkedInput, question string, timeout time.Duration, onExpired func()) bool {
	if r.terminated() {
		return false
	}
	r.mu.Lock()
	// A second question replaces the first. The state graph permits
	// INPUT_REQUIRED -> INPUT_REQUIRED precisely so an agent may restate what it
	// is waiting for, and tracking only the newest request id is the honest
	// reading: it is the one whose answer will unblock the agent loop.
	r.parked = in
	r.stopTimerLocked()
	if timeout > 0 && onExpired != nil {
		r.inputTimer = time.AfterFunc(timeout, onExpired)
	}
	r.mu.Unlock()

	if question == "" {
		question = "the agent is waiting for input"
	}
	message := a2a.NewAgentMessage(newMessageID(), question).
		InContext(r.contextID).
		ForTask(r.taskID)
	if in.requestID != "" {
		// The request id is carried as message metadata rather than being
		// required of the client: A2A's resume mechanism is the taskId, and a
		// conforming client needs nothing else. It is exposed because a Nexus
		// client that also watches the bus can correlate the two views.
		message.Metadata = map[string]any{metadataHITLRequestID: in.requestID}
	}
	// The question is recorded as a message reference as well as riding the
	// status: history is what a reconnecting client reads, and a question that
	// existed only on a status nobody was watching would be lost to it.
	r.recordMessage(messageRef{MessageID: message.MessageID, Role: a2a.RoleAgent, Text: question})
	r.emit(a2a.StreamStatusUpdate(a2a.NewStatusUpdate(r.taskID, r.contextID,
		a2a.NewTaskStatus(a2a.TaskStateInputRequired).WithMessage(message))))
	return true
}

// boundTurn returns the Nexus turn id this run bound to, empty when no turn
// start has been observed.
func (r *run) boundTurn() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turnID
}

// pending returns the question this task is parked on, if any.
func (r *run) pending() (parkedInput, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.parked, r.parked.live()
}

// unpark clears the parked question and stops its deadline, reporting the
// question it cleared. It is the state half of resuming, split from the frame
// half so a caller that must not emit (a cancellation) can still disarm.
func (r *run) unpark(requestID string) (parkedInput, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.parked.live() {
		return parkedInput{}, false
	}
	// An empty requestID matches whatever is parked: a response with no
	// correlation id can only plausibly be for the one question outstanding.
	if requestID != "" && requestID != r.parked.requestID {
		return parkedInput{}, false
	}
	was := r.parked
	r.parked = parkedInput{}
	r.stopTimerLocked()
	return was, true
}

// resume returns a parked task to WORKING. It reports whether it did, so a
// response for a question this task is not waiting on is visibly ignored rather
// than silently restating a state.
func (r *run) resume(requestID string) bool {
	if r.terminated() {
		return false
	}
	if _, ok := r.unpark(requestID); !ok {
		return false
	}
	r.emit(a2a.StreamStatusUpdate(a2a.NewStatusUpdate(r.taskID, r.contextID,
		a2a.NewTaskStatus(a2a.TaskStateWorking))))
	return true
}

// stopTimerLocked disarms the input deadline. The caller holds r.mu.
func (r *run) stopTimerLocked() {
	if r.inputTimer != nil {
		r.inputTimer.Stop()
		r.inputTimer = nil
	}
}

// --- terminal transitions ---

// terminate runs the one-shot terminal sequence: disarm the input deadline,
// emit whatever frames describe the ending, close done so nothing further is
// queued, and release the plugin's active-task slot.
//
// Every terminal path goes through it, which is what makes "the slot is
// returned when the task ends" true by construction rather than by each caller
// remembering. Ordering matters: the frames are emitted BEFORE done closes,
// because emit drops anything queued after a terminal state.
func (r *run) terminate(frames func()) bool {
	fired := false
	r.closeOne.Do(func() {
		fired = true
		r.mu.Lock()
		r.parked = parkedInput{}
		r.stopTimerLocked()
		r.mu.Unlock()

		frames()
		close(r.done)
		if r.onTerminal != nil {
			r.onTerminal()
		}
	})
	return fired
}

// complete emits the turn's artifact and the COMPLETED status, exactly once.
//
// Ordering is load-bearing: the artifact MUST precede the terminal status.
// A2A closes a stream the moment a frame reports a terminal state
// (specification section 11.7), so an artifact queued after COMPLETED would be
// refused by SSEWriter and the client would see a completed task with no output.
func (r *run) complete() {
	r.terminate(func() {
		r.mu.Lock()
		text := r.finalText
		schema := r.structuredSchema
		suppressed := r.budget.suppressed
		noticed := r.budget.noticed
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
				r.responseArtifact(text, schema))))
		}
		// The suppression notice is restated with the FINAL count on the same
		// artifact id it was minted under. The store upserts by id, so the record
		// ends with the true total rather than the count at the moment of the
		// first refusal, and the wire carries exactly two frames for it however
		// many artifacts were suppressed.
		if suppressed > 0 && noticed {
			r.emit(a2a.StreamArtifactUpdate(a2a.NewArtifactUpdate(r.taskID, r.contextID,
				suppressionNotice(r.taskID, suppressed))))
		}
		r.emit(a2a.StreamStatusUpdate(a2a.NewStatusUpdate(r.taskID, r.contextID,
			a2a.NewTaskStatus(a2a.TaskStateCompleted))))
	})
}

// responseArtifact renders the turn's answer.
//
// The text part is always there, and always first: it is what every A2A client
// can render, and demoting it would break clients that read parts[0]. When the
// answer IS a JSON document, a second part carries it as real
// application/json — a client that wants the structured value gets a document to
// decode rather than a string to re-parse, which is the whole of "not buried in
// text". Both parts describe the same answer; a client picks the one it can use.
//
// The schema name, when the turn's llm.request declared one, rides the artifact
// metadata rather than a part, because it describes the content rather than
// being content.
func (r *run) responseArtifact(text, schema string) a2a.Artifact {
	art := a2a.NewTextArtifact(r.taskID+artifactSuffix, artifactName, text)
	if r.textOnly {
		return art
	}
	raw, ok := structuredJSON(text)
	if !ok {
		return art
	}
	art.Parts = append(art.Parts, a2a.Part{Data: raw, MediaType: mediaTypeJSON})
	if schema != "" {
		art.Metadata = map[string]any{metadataJSONSchema: schema}
	}
	return art
}

// structuredJSON reports whether the turn's answer is a JSON document, returning
// it verbatim so the client decodes exactly what the agent produced.
//
// Only an object or an array counts. A bare JSON scalar — 42, "yes", true — is
// valid JSON and is also what an ordinary prose answer looks like to a parser,
// so treating those as structured output would attach a redundant JSON part to
// half the plain-text turns this transport serves.
//
// One fenced code block is unwrapped first, because a model told to answer in
// JSON very often answers in a fence, and refusing to see through it would make
// the feature depend on the model's formatting habits.
func structuredJSON(text string) (json.RawMessage, bool) {
	candidate := strings.TrimSpace(unfence(text))
	if candidate == "" {
		return nil, false
	}
	switch candidate[0] {
	case '{', '[':
	default:
		return nil, false
	}
	if !json.Valid([]byte(candidate)) {
		return nil, false
	}
	return json.RawMessage(candidate), true
}

// unfence strips a single surrounding markdown code fence, with or without a
// language tag. Anything else is returned unchanged.
func unfence(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "```") || !strings.HasSuffix(trimmed, "```") {
		return text
	}
	body := strings.TrimPrefix(trimmed, "```")
	body = strings.TrimSuffix(body, "```")
	// The opening fence may carry a language tag on its own line.
	if idx := strings.IndexByte(body, '\n'); idx >= 0 {
		if tag := strings.TrimSpace(body[:idx]); tag == "" || !strings.ContainsAny(tag, " \t{[") {
			body = body[idx+1:]
		}
	}
	return body
}

// fail terminates the task with FAILED, carrying the reason as the status
// message.
//
// FAILED is a task state, not a transport error: the work failed, the protocol
// did not. Section 11.7's terminal-close rule then ends the stream exactly as a
// success would, so a client handles one shape of ending rather than two.
// It reports whether THIS call settled the task, so a caller that must only act
// on a real transition (the input-deadline timer) can tell.
func (r *run) fail(reason string) bool {
	if reason == "" {
		reason = "the agent turn failed"
	}
	return r.endWith(a2a.TaskStateFailed, reason)
}

// cancel terminates the task with CANCELED. It reports whether THIS call was
// the one that settled the task, so a CancelTask racing the turn's own ending
// can tell the client which of the two happened.
func (r *run) cancel(reason string) bool {
	if reason == "" {
		reason = "the task was canceled"
	}
	return r.endWith(a2a.TaskStateCanceled, reason)
}

// endWith settles the task in a terminal state carrying reason as the status
// message.
func (r *run) endWith(state a2a.TaskState, reason string) bool {
	return r.terminate(func() {
		message := a2a.NewAgentMessage(newMessageID(), reason).
			InContext(r.contextID).
			ForTask(r.taskID)
		r.emit(a2a.StreamStatusUpdate(a2a.NewStatusUpdate(r.taskID, r.contextID,
			a2a.NewTaskStatus(state).WithMessage(message))))
	})
}

// snapshotTask returns the task as every frame emitted so far leaves it. It
// carries no history: the store owns that, and a caller that needs the whole
// record reads it back from there.
func (r *run) snapshotTask() a2a.Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneTask(r.snapshot)
}

// describeQuestion renders a HITL request as the text an A2A client is asked.
//
// A2A messages on this transport are text, so a multiple-choice question has to
// be flattened into one: the prompt, then the options by id. The ids are what
// the client echoes back, so they are rendered verbatim rather than numbered —
// a numbered list would invite an answer this agent cannot map onto a choice.
func describeQuestion(prompt string, choices []events.HITLChoice) string {
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
