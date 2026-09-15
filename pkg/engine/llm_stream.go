package engine

import (
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/frankbardon/nexus/pkg/events"
)

// Bus event types owned by the streaming seam. Declared here rather than as
// string literals at the emit sites so the provider Emissions() lists and the
// transports that subscribe agree on one spelling.
const (
	// EventStreamChunk is the per-delta event every streaming provider
	// publishes. Emitted only via StreamPublisher, so that the
	// before:llm.stream.chunk hook cannot be skipped at one provider's emit
	// site.
	EventStreamChunk = "llm.stream.chunk"
	// EventBeforeStreamChunk is the vetoable hook offered before any text is
	// released. Payload is *events.StreamSegment.
	EventBeforeStreamChunk = "before:llm.stream.chunk"
	// EventStreamHold announces both edges of a suspended stream.
	EventStreamHold = "llm.stream.hold"
	// EventStreamRetract announces that a mid-flight block disowned the text
	// already released for a turn.
	EventStreamRetract = "llm.stream.retract"
)

// StreamPublisher is the single path from a provider's SSE loop to the bus,
// and the streaming half of the response-veto contract that
// PublishLLMResponse implements for whole responses.
//
// The problem it solves: before:llm.response fires once, at end of stream,
// by which point every token has already been rendered. A veto there governs
// what enters history and what drives tools, but it cannot unsay text. The
// standing advice was therefore to run disclosure-preventing gates with
// streaming off — which, on top of costing time-to-first-token, was advice an
// operator could not actually follow, since the agent loops hardcode
// Stream: true.
//
// So gating moves into the stream. Every text delta is offered to
// before:llm.stream.chunk as an *events.StreamSegment before any of it
// reaches the bus, and a handler can pass, redact, suspend, or block it. The
// guarantee that buys is categorical rather than best-effort: withheld text
// was never emitted, so it is not a question of whether a given transport can
// retract. It never had anything to retract.
//
// Three properties make that hold in practice:
//
//   - Suspension composes with detection latency. A handler that cannot yet
//     judge a trailing fragment holds it (StreamSegment.Hold) instead of
//     gambling. Held bytes are re-offered, prepended to the next delta, so a
//     handler holding at least its longest pattern can never be beaten by a
//     pattern split across two deltas.
//
//   - A blocked stream stays blocked. Once a handler vetoes, the publisher
//     drops what it was holding and swallows every later delta for the turn.
//     Nothing re-opens it; the substitute arrives through
//     PublishLLMResponse, as it does for the non-streaming path.
//
//   - The unguarded path is unchanged. With nothing subscribed to
//     before:llm.stream.chunk, Text emits exactly the delta it was handed,
//     with no buffering, no extra events, and no hook dispatch.
//
// What it deliberately does not do is make the residue vanish. Bytes released
// before a pattern was detectable are gone; llm.stream.retract asks
// transports to disown them and most can, but a human who already read them
// has read them. Hold is the knob that shrinks that window, and a gate that
// holds its full pattern width shrinks it to nothing.
//
// A publisher belongs to one turn. Providers create one at stream start and
// Close it when the SSE loop ends, on every path including error returns.
// Methods are safe to call from one goroutine; providers drive their SSE loop
// from one, and the mutex guards against a Close racing a late delta rather
// than against genuine concurrent production.
type StreamPublisher struct {
	bus       EventBus
	turnID    string
	requestID string

	mu sync.Mutex
	// released is every byte emitted as llm.stream.chunk so far, after any
	// handler redaction. Handed to handlers as StreamSegment.Released.
	released strings.Builder
	// pending is text a handler asked to withhold, waiting to be re-offered
	// with the next delta.
	pending string
	// index is the next llm.stream.chunk sequence number. Shared with
	// tool-call chunks so a consumer ordering by Index sees one sequence.
	index int
	// holding tracks whether the last segment left bytes withheld, so the
	// resume edge of llm.stream.hold is emitted exactly once.
	holding bool
	// blocked latches on a veto. Terminal for the turn.
	blocked bool
	reason  string
	// gated caches whether anything subscribes to the hook. Sampled per
	// segment rather than once per turn so a gate that subscribes mid-turn
	// still takes effect; the check is a read-locked map lookup.
	closed bool
}

// NewStreamPublisher returns a publisher for one turn's stream. requestID may
// be empty for providers that do not correlate, but passing it lets a gate
// tie a held or blocked stream back to the request that produced it.
func NewStreamPublisher(bus EventBus, turnID, requestID string) *StreamPublisher {
	return &StreamPublisher{bus: bus, turnID: turnID, requestID: requestID}
}

// Text offers a text delta to the gate and emits whatever it allows.
//
// The fast path — no before:llm.stream.chunk subscriber, nothing held, not
// blocked — emits delta verbatim and allocates nothing beyond the chunk
// itself, so an ungated deployment pays for none of this.
func (sp *StreamPublisher) Text(delta string) {
	if sp == nil || sp.bus == nil || delta == "" {
		return
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.offer(delta, false)
}

// ToolCall publishes a tool-call chunk. Tool calls bypass the text gate: they
// are not disclosure — nothing of a tool call is shown to a user as prose —
// and the authority over whether they may run belongs to before:llm.response,
// which clears ToolCalls on every substitution, and to before:tool.invoke,
// which sees parsed arguments. Gating a partial JSON fragment here would
// duplicate that with strictly less information.
//
// A blocked stream still suppresses them, because a turn whose text was
// blocked is a turn whose response is about to be substituted anyway.
func (sp *StreamPublisher) ToolCall(tc *events.ToolCallRequest) {
	if sp == nil || sp.bus == nil || tc == nil {
		return
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.blocked {
		return
	}
	_ = sp.bus.Emit(EventStreamChunk, events.StreamChunk{
		SchemaVersion: events.StreamChunkVersion,
		ToolCall:      tc,
		Index:         sp.index,
		TurnID:        sp.turnID,
	})
	sp.index++
}

// Close flushes whatever is still held and settles any outstanding hold.
//
// The final flush still runs the hook — a handler gets one last look at the
// tail it was deferring — but Hold is ignored on it: the stream is over, so
// "wait for more context" is no longer an option and the handler must decide.
// A handler that would rather block than release an unresolved tail vetoes
// here, which is the same move it has on any other segment.
//
// Idempotent. Providers should defer it so an error path cannot leave a
// stream suspended forever.
func (sp *StreamPublisher) Close() {
	if sp == nil || sp.bus == nil {
		return
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.closed {
		return
	}
	sp.closed = true
	if sp.blocked {
		return
	}
	sp.offer("", true)
}

// Blocked reports whether a handler cut this stream short, and why. Providers
// have no obligation to act on it — the response-level veto is what
// substitutes the answer — but it is useful for logging, and for a provider
// that wants to stop reading an SSE body it can no longer publish.
func (sp *StreamPublisher) Blocked() (bool, string) {
	if sp == nil {
		return false, ""
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.blocked, sp.reason
}

// Released returns the text actually emitted so far, post-redaction. Provided
// for tests and for providers that want to log the divergence between what
// the model produced and what the user saw.
func (sp *StreamPublisher) Released() string {
	if sp == nil {
		return ""
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.released.String()
}

// offer runs one delta through the hook and emits what survives. Caller holds
// the mutex. final suppresses Hold, because no further delta is coming.
func (sp *StreamPublisher) offer(delta string, final bool) {
	if sp.blocked {
		return
	}

	candidate := sp.pending + delta
	if candidate == "" {
		// Nothing withheld and nothing new. Only reachable from Close on a
		// stream that produced no text.
		return
	}

	// Ungated fast path. Checked per call so a late subscriber still takes
	// effect, and skipped entirely when something is already held — a
	// publisher with pending bytes has a gate that asked for them, and
	// dropping to the fast path would release them unexamined.
	if sp.pending == "" && !sp.bus.HasSubscribers(EventBeforeStreamChunk) {
		sp.emit(delta)
		return
	}

	seg := &events.StreamSegment{
		SchemaVersion: events.StreamSegmentVersion,
		TurnID:        sp.turnID,
		RequestID:     sp.requestID,
		Index:         sp.index,
		Released:      sp.released.String(),
		Content:       candidate,
	}

	veto, err := sp.bus.EmitVetoable(EventBeforeStreamChunk, seg)
	if err == nil && veto.Vetoed {
		sp.block(veto.Reason)
		return
	}

	// Handlers may have rewritten Content; that rewrite is what ships.
	out := seg.Content
	hold := 0
	if !final {
		hold = clampHold(out, seg.Hold)
	}

	sp.pending = out[len(out)-hold:]
	release := out[:len(out)-hold]

	if release != "" {
		sp.emit(release)
	}
	sp.noteHold(hold, veto.Reason)
}

// emit publishes one llm.stream.chunk and records the text as released.
// Caller holds the mutex.
func (sp *StreamPublisher) emit(text string) {
	sp.released.WriteString(text)
	_ = sp.bus.Emit(EventStreamChunk, events.StreamChunk{
		SchemaVersion: events.StreamChunkVersion,
		Content:       text,
		Index:         sp.index,
		TurnID:        sp.turnID,
	})
	sp.index++
}

// noteHold emits the hold edges. A stream that goes quiet because a gate is
// deliberating looks exactly like a hung turn from a UI's side, so both the
// entry and the exit are announced rather than left to be inferred.
// Caller holds the mutex.
func (sp *StreamPublisher) noteHold(hold int, reason string) {
	switch {
	case hold > 0:
		// Re-announce on every segment while the hold grows or persists, so
		// a UI that missed the first edge still learns the stream is paused.
		sp.holding = true
		_ = sp.bus.Emit(EventStreamHold, events.StreamHold{
			SchemaVersion: events.StreamHoldVersion,
			TurnID:        sp.turnID,
			RequestID:     sp.requestID,
			Held:          hold,
			Reason:        reason,
		})
	case sp.holding:
		sp.holding = false
		_ = sp.bus.Emit(EventStreamHold, events.StreamHold{
			SchemaVersion: events.StreamHoldVersion,
			TurnID:        sp.turnID,
			RequestID:     sp.requestID,
			Resumed:       true,
		})
	}
}

// block latches the terminal blocked state and tells transports to disown the
// prefix that did get out. Caller holds the mutex.
func (sp *StreamPublisher) block(reason string) {
	sp.blocked = true
	sp.reason = reason
	sp.pending = ""
	released := sp.released.Len()

	// A stream that was suspended when the block landed leaves a UI showing
	// a "reviewing" state that nothing else would ever clear.
	if sp.holding {
		sp.holding = false
		_ = sp.bus.Emit(EventStreamHold, events.StreamHold{
			SchemaVersion: events.StreamHoldVersion,
			TurnID:        sp.turnID,
			RequestID:     sp.requestID,
			Resumed:       true,
		})
	}

	_ = sp.bus.Emit(EventStreamRetract, events.StreamRetract{
		SchemaVersion: events.StreamRetractVersion,
		TurnID:        sp.turnID,
		RequestID:     sp.requestID,
		ReleasedLen:   released,
		Reason:        reason,
	})
}

// clampHold bounds a handler-requested hold to the available text and snaps
// it back to a rune boundary. Splitting mid-rune would hand a UI an invalid
// fragment and, worse, would let a multi-byte character straddle the release
// boundary where a handler scanning Full() could not see it whole.
func clampHold(content string, hold int) int {
	if hold <= 0 {
		return 0
	}
	if hold >= len(content) {
		return len(content)
	}
	split := len(content) - hold
	for split > 0 && !utf8.RuneStart(content[split]) {
		split--
	}
	return len(content) - split
}
