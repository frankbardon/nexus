package agui

import (
	"encoding/json"
	"sync"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/events"
)

// run holds the state of a single in-flight AG-UI run. Bus-event handlers push
// translated AG-UI events onto out; the owning HTTP handler goroutine is the
// sole reader and the sole writer to the SSE stream, so the SSEWriter is never
// touched concurrently. All mutation of the run's own bookkeeping fields goes
// through mu.
type run struct {
	threadID string
	runID    string

	// out is the single channel every translated AG-UI event flows through.
	// Buffered so a burst of bus events never blocks the emitting agent
	// goroutine on a slow client for long.
	out chan agui.Event
	// done is closed once the terminal RunFinished/RunError has been queued so
	// handlers stop translating further events for this run.
	done     chan struct{}
	closeOne sync.Once

	mu sync.Mutex
	// started guards the one-time RunStarted emission: agent.turn.start is the
	// natural trigger, but a run with no agent (or a very fast one) still needs
	// a RunStarted, so the handler emits it eagerly and this flag suppresses a
	// duplicate from the first turn.start.
	started bool
	// stepOpen tracks whether a StepStarted is currently unmatched by a
	// StepFinished, so agent.turn.end can close exactly the open step.
	stepOpen bool
	stepName string
	// activeText is the message id of the open streamed text message, empty
	// when no TextMessage is in flight.
	activeText string
	// textSeq disambiguates successive streamed messages within one run.
	textSeq int
	// textStreamed records whether any llm.stream.chunk delta was rendered as a
	// TextMessage since the last one closed. It lets onOutput distinguish a
	// genuinely-streamed final output (already rendered, skip) from one flagged
	// "streamed" by a non-streaming provider (mock / batch) where no chunk ever
	// arrived, so the text would otherwise be silently dropped.
	textStreamed bool
	// openTools tracks tool-call ids that have had a ToolCallStart but no
	// ToolCallEnd yet, so tool.result can close them before emitting the result.
	openTools map[string]bool
	// reasoningOpen tracks whether a ReasoningStart is unmatched by a
	// ReasoningEnd.
	reasoningOpen bool
}

// runInput carries the fields of a decoded RunAgentInput that the plugin needs
// to start a run, decoupling the plugin from the transport (server) package
// boundary.
type runInput struct {
	threadID string
	runID    string
	messages []agui.Message
}

// newRunStarted builds a RunStarted event for a run.
func newRunStarted(threadID, runID string) agui.RunStartedEvent {
	return agui.NewRunStarted(threadID, runID)
}

// newRun builds a run for the given thread/run identifiers.
func newRun(threadID, runID string) *run {
	return &run{
		threadID:  threadID,
		runID:     runID,
		out:       make(chan agui.Event, 256),
		done:      make(chan struct{}),
		openTools: make(map[string]bool),
	}
}

// queue pushes a translated event onto the run's channel unless the run has
// already terminated or the client has gone away. It never blocks
// indefinitely: once done is closed the event is dropped.
func (r *run) queue(e agui.Event) {
	select {
	case <-r.done:
		return
	default:
	}
	select {
	case r.out <- e:
	case <-r.done:
	}
}

// finish queues the terminal RunFinished event and closes the run exactly once.
// Subsequent bus events for this run are dropped.
func (r *run) finish() {
	r.closeOne.Do(func() {
		select {
		case r.out <- agui.NewRunFinished(r.threadID, r.runID):
		default:
		}
		close(r.done)
	})
}

// fail queues a terminal RunError event and closes the run exactly once.
func (r *run) fail(msg string) {
	r.closeOne.Do(func() {
		select {
		case r.out <- agui.NewRunError(msg):
		default:
		}
		close(r.done)
	})
}

// markStarted emits RunStarted at most once for the run and returns whether it
// performed the emission.
func (r *run) markStarted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return false
	}
	r.started = true
	return true
}

// --- bus event translation ---
//
// Each on* method translates one Nexus bus payload into zero or more AG-UI
// events and queues them. They are called from arbitrary bus goroutines; all
// shared state is guarded by r.mu and delivery is via the channel, so the
// SSEWriter is never touched here.

// onTurnStart maps agent.turn.start to a StepStarted (RunStarted is emitted
// eagerly by the handler). Nested turns (iterations) each open a step.
func (r *run) onTurnStart(t events.TurnInfo) {
	r.mu.Lock()
	if r.stepOpen {
		// An iteration boundary without an explicit turn.end: close the
		// previous step before opening the next so steps stay balanced.
		prev := r.stepName
		r.mu.Unlock()
		r.queue(agui.NewStepFinished(prev))
		r.mu.Lock()
	}
	name := stepName(t)
	r.stepOpen = true
	r.stepName = name
	r.mu.Unlock()
	r.queue(agui.NewStepStarted(name))
}

// onTurnEnd maps agent.turn.end to StepFinished. The run itself is finished by
// the handler when the top-level turn completes.
func (r *run) onTurnEnd(t events.TurnInfo) {
	r.mu.Lock()
	// Close any open streamed text before the step ends.
	openText := r.activeText
	r.activeText = ""
	name := r.stepName
	wasOpen := r.stepOpen
	r.stepOpen = false
	reasoning := r.reasoningOpen
	r.reasoningOpen = false
	r.mu.Unlock()

	if openText != "" {
		r.queue(agui.NewTextMessageEnd(openText))
	}
	if reasoning {
		r.queue(agui.NewReasoningEnd())
	}
	if wasOpen {
		if name == "" {
			name = stepName(t)
		}
		r.queue(agui.NewStepFinished(name))
	}
}

// onStreamChunk maps llm.stream.chunk text deltas to TextMessage* events. A
// TextMessageStart is emitted lazily on the first non-empty delta and closed on
// stream end / turn end.
func (r *run) onStreamChunk(c events.StreamChunk) {
	if c.Content == "" {
		return
	}
	r.mu.Lock()
	r.textStreamed = true
	if r.activeText == "" {
		r.textSeq++
		r.activeText = messageID(r.runID, r.textSeq)
		id := r.activeText
		r.mu.Unlock()
		r.queue(agui.NewTextMessageStart(id, "assistant"))
		r.queue(agui.NewTextMessageContent(id, c.Content))
		return
	}
	id := r.activeText
	r.mu.Unlock()
	r.queue(agui.NewTextMessageContent(id, c.Content))
}

// onStreamEnd closes any open streamed text message at the end of a single LLM
// stream (one iteration may stream multiple times across turns).
func (r *run) onStreamEnd(_ events.StreamEnd) {
	r.mu.Lock()
	id := r.activeText
	r.activeText = ""
	r.mu.Unlock()
	if id != "" {
		r.queue(agui.NewTextMessageEnd(id))
	}
}

// onOutput maps an io.output message to a self-contained TextMessage
// start/content/end triple, UNLESS the same content was already rendered
// incrementally via the llm.stream.chunk path (a genuinely streamed output). A
// "streamed"-flagged output whose stream produced no chunks (non-streaming
// providers such as mock or batch) is rendered here so its text is never
// silently dropped.
func (r *run) onOutput(o events.AgentOutput) {
	if o.Content == "" {
		return
	}
	// A "streamed" output was already rendered by the llm.stream.chunk path —
	// but only if a chunk actually arrived. Non-streaming providers (mock,
	// batch) still flag the output "streamed" while emitting no chunks, so fall
	// through and render a self-contained triple when nothing was streamed.
	r.mu.Lock()
	streamed, _ := o.Metadata["streamed"].(bool)
	alreadyRendered := streamed && r.textStreamed
	r.textStreamed = false
	if alreadyRendered {
		r.mu.Unlock()
		return
	}
	role := o.Role
	if role == "" {
		role = "assistant"
	}
	r.textSeq++
	id := messageID(r.runID, r.textSeq)
	r.mu.Unlock()
	r.queue(agui.NewTextMessageStart(id, role))
	r.queue(agui.NewTextMessageContent(id, o.Content))
	r.queue(agui.NewTextMessageEnd(id))
}

// onToolCall maps tool.call to ToolCallStart + ToolCallArgs + ToolCallEnd. The
// arguments are already fully resolved on the bus (not streamed), so the three
// events are emitted together and the id is tracked so tool.result can be
// correlated.
func (r *run) onToolCall(tc events.ToolCall) {
	r.mu.Lock()
	r.openTools[tc.ID] = true
	r.mu.Unlock()

	r.queue(agui.NewToolCallStart(tc.ID, tc.Name))
	if len(tc.Arguments) > 0 {
		if args, err := json.Marshal(tc.Arguments); err == nil {
			r.queue(agui.NewToolCallArgs(tc.ID, string(args)))
		}
	}
	r.queue(agui.NewToolCallEnd(tc.ID))
}

// onToolResult maps tool.result to ToolCallResult, closing an open tool call if
// tool.call did not already do so.
func (r *run) onToolResult(tr events.ToolResult) {
	r.mu.Lock()
	if r.openTools[tr.ID] {
		delete(r.openTools, tr.ID)
	}
	r.mu.Unlock()

	content := tr.Output
	if tr.Error != "" {
		content = tr.Error
	}
	msgID := messageID(r.runID, 0) + "-tool-" + tr.ID
	r.queue(agui.NewToolCallResult(msgID, tr.ID, content))
}

// onThinkingStep maps thinking.step to a Reasoning section. A ReasoningStart is
// opened lazily on the first step and closed at turn end.
func (r *run) onThinkingStep(s events.ThinkingStep) {
	if s.Content == "" {
		return
	}
	r.mu.Lock()
	openStart := !r.reasoningOpen
	r.reasoningOpen = true
	r.mu.Unlock()
	if openStart {
		r.queue(agui.NewReasoningStart())
	}
	r.queue(agui.NewReasoningMessageContent(s.Content))
}

// onCustom rides any Nexus-specific bus event that has no canonical AG-UI
// equivalent (workflow.progress, subagent.*, code.exec.stdout, ...) as a
// Custom event. The Custom name is the bus event type and the value is the
// JSON-encoded payload. This is the single, consistent superset chosen for
// non-canonical events (see docs/src/plugins/io-agui.md).
func (r *run) onCustom(eventType string, payload any) {
	value, err := json.Marshal(payload)
	if err != nil {
		value = []byte("null")
	}
	r.queue(agui.NewCustom(eventType, value))
}

// stepName derives a stable, human-readable step name from a turn.
func stepName(t events.TurnInfo) string {
	if t.TurnID != "" {
		return t.TurnID
	}
	return "turn"
}

// messageID builds a deterministic message id for a run and sequence number.
func messageID(runID string, seq int) string {
	if runID == "" {
		runID = "run"
	}
	if seq <= 0 {
		return runID + "-msg"
	}
	return runID + "-msg-" + itoa(seq)
}

// itoa is a tiny int->string helper avoiding an fmt import in the hot path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
