package engine

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

// collectStream subscribes to the three streaming events a publisher drives
// and returns accessors for what was seen. Chunks are recorded as their text
// so assertions read as the user-visible transcript.
type streamSink struct {
	chunks   []string
	toolCall int
	holds    []events.StreamHold
	retracts []events.StreamRetract
	indexes  []int
}

func (s *streamSink) text() string { return strings.Join(s.chunks, "") }

func collectStream(t *testing.T, bus EventBus) *streamSink {
	t.Helper()
	sink := &streamSink{}
	bus.Subscribe(EventStreamChunk, func(e Event[any]) {
		c, ok := e.Payload.(events.StreamChunk)
		if !ok {
			return
		}
		sink.indexes = append(sink.indexes, c.Index)
		if c.ToolCall != nil {
			sink.toolCall++
			return
		}
		sink.chunks = append(sink.chunks, c.Content)
	})
	bus.Subscribe(EventStreamHold, func(e Event[any]) {
		if h, ok := e.Payload.(events.StreamHold); ok {
			sink.holds = append(sink.holds, h)
		}
	})
	bus.Subscribe(EventStreamRetract, func(e Event[any]) {
		if r, ok := e.Payload.(events.StreamRetract); ok {
			sink.retracts = append(sink.retracts, r)
		}
	})
	return sink
}

// gateStream subscribes a before:llm.stream.chunk handler.
func gateStream(bus EventBus, fn func(seg *events.StreamSegment, vp *VetoablePayload)) {
	bus.Subscribe(EventBeforeStreamChunk, func(e Event[any]) {
		vp, ok := e.Payload.(*VetoablePayload)
		if !ok {
			return
		}
		seg, ok := vp.Original.(*events.StreamSegment)
		if !ok {
			return
		}
		fn(seg, vp)
	})
}

// TestStreamPublisher_Ungated_PassesThroughVerbatim pins the property that
// makes this safe to turn on everywhere: with no gate subscribed, the
// publisher is a pass-through. Same text, same chunk boundaries, no hold or
// retract traffic.
func TestStreamPublisher_Ungated_PassesThroughVerbatim(t *testing.T) {
	bus := NewEventBus()
	sink := collectStream(t, bus)

	sp := NewStreamPublisher(bus, "turn-1", "req-1")
	for _, d := range []string{"Hello", ", ", "world"} {
		sp.Text(d)
	}
	sp.Close()

	if got := sink.text(); got != "Hello, world" {
		t.Fatalf("text = %q, want %q", got, "Hello, world")
	}
	if len(sink.chunks) != 3 {
		t.Fatalf("chunk count = %d, want 3 (delta boundaries must be preserved)", len(sink.chunks))
	}
	if len(sink.holds) != 0 || len(sink.retracts) != 0 {
		t.Fatalf("ungated stream emitted %d holds and %d retracts, want none",
			len(sink.holds), len(sink.retracts))
	}
}

// TestStreamPublisher_HoldDefersUntilResolved is the suspend path: a handler
// that cannot yet judge a trailing fragment withholds it, and the bytes
// surface only once the handler stops asking for them.
func TestStreamPublisher_HoldDefersUntilResolved(t *testing.T) {
	bus := NewEventBus()
	sink := collectStream(t, bus)

	// Hold any trailing run of digits and dashes — a stand-in for "this might
	// be turning into a card number".
	gateStream(bus, func(seg *events.StreamSegment, _ *VetoablePayload) {
		i := len(seg.Content)
		for i > 0 && (seg.Content[i-1] == '-' || (seg.Content[i-1] >= '0' && seg.Content[i-1] <= '9')) {
			i--
		}
		seg.HoldFrom(i)
	})

	sp := NewStreamPublisher(bus, "turn-1", "req-1")
	sp.Text("call 555")
	if got := sink.text(); got != "call " {
		t.Fatalf("after first delta text = %q, want %q (digits must be held)", got, "call ")
	}
	if len(sink.holds) != 1 || sink.holds[0].Held != 3 || sink.holds[0].Resumed {
		t.Fatalf("holds = %+v, want one entry edge holding 3 bytes", sink.holds)
	}

	// Still digit-shaped at the tail, so the hold persists and grows across
	// deltas rather than resetting each time.
	sp.Text("-1234")
	if got := sink.text(); got != "call " {
		t.Fatalf("after second delta text = %q, want the hold to persist", got)
	}
	if last := sink.holds[len(sink.holds)-1]; last.Held != len("555-1234") {
		t.Fatalf("hold = %d bytes, want the whole accumulated run (%d)", last.Held, len("555-1234"))
	}

	// A separator ends the run, so the held text is now judgeable and ships.
	sp.Text(" now")
	if got := sink.text(); got != "call 555-1234 now" {
		t.Fatalf("after the separator text = %q, want the held run released", got)
	}
	if last := sink.holds[len(sink.holds)-1]; !last.Resumed {
		t.Error("releasing a held run must emit the resume edge")
	}

	sp.Close()
	if got := sink.text(); got != "call 555-1234 now" {
		t.Fatalf("final text = %q, want %q", got, "call 555-1234 now")
	}
}

// TestStreamPublisher_HoldAnnouncesBothEdges covers the UI contract: a stream
// that goes quiet because a gate is deliberating is indistinguishable from a
// hung turn unless the pause is announced, and the resume edge is what lets a
// UI clear the indicator again.
func TestStreamPublisher_HoldAnnouncesBothEdges(t *testing.T) {
	bus := NewEventBus()
	sink := collectStream(t, bus)

	hold := true
	gateStream(bus, func(seg *events.StreamSegment, _ *VetoablePayload) {
		if hold {
			seg.HoldFrom(0)
		}
	})

	sp := NewStreamPublisher(bus, "turn-1", "req-1")
	sp.Text("secret?")
	if len(sink.chunks) != 0 {
		t.Fatalf("held stream emitted %v, want nothing", sink.chunks)
	}
	hold = false
	sp.Text(" no")
	sp.Close()

	if got := sink.text(); got != "secret? no" {
		t.Fatalf("text = %q, want %q", got, "secret? no")
	}
	if len(sink.holds) != 2 {
		t.Fatalf("holds = %+v, want exactly two (one in, one out)", sink.holds)
	}
	if sink.holds[0].Resumed {
		t.Error("first hold edge should be the entry (Resumed=false)")
	}
	if !sink.holds[1].Resumed || sink.holds[1].Held != 0 {
		t.Errorf("second hold edge = %+v, want the resume edge holding nothing", sink.holds[1])
	}
}

// TestStreamPublisher_HoldSurvivesToClose checks that a hold still standing
// when the stream ends is released rather than silently dropped. Close ignores
// Hold precisely because "wait for more context" is no longer an option.
func TestStreamPublisher_HoldSurvivesToClose(t *testing.T) {
	bus := NewEventBus()
	sink := collectStream(t, bus)

	gateStream(bus, func(seg *events.StreamSegment, _ *VetoablePayload) {
		seg.HoldFrom(0) // hold everything, forever
	})

	sp := NewStreamPublisher(bus, "turn-1", "req-1")
	sp.Text("all of it")
	if len(sink.chunks) != 0 {
		t.Fatalf("expected nothing released mid-stream, got %v", sink.chunks)
	}
	sp.Close()

	if got := sink.text(); got != "all of it" {
		t.Fatalf("text after Close = %q, want the held tail flushed", got)
	}
}

// TestStreamPublisher_VetoBlocksRestOfTurn is the core security property:
// once a handler blocks, nothing further is published for the turn — not the
// held text, not later deltas, not tool calls.
func TestStreamPublisher_VetoBlocksRestOfTurn(t *testing.T) {
	bus := NewEventBus()
	sink := collectStream(t, bus)

	gateStream(bus, func(seg *events.StreamSegment, vp *VetoablePayload) {
		if strings.Contains(seg.Full(), "BANNED") {
			vp.Veto = VetoResult{Vetoed: true, Reason: "prohibited term"}
		}
	})

	sp := NewStreamPublisher(bus, "turn-1", "req-1")
	sp.Text("safe ")
	sp.Text("BANNED")
	sp.Text(" and more")
	sp.ToolCall(&events.ToolCallRequest{ID: "t1", Name: "shell"})
	sp.Close()

	if got := sink.text(); got != "safe " {
		t.Fatalf("text = %q, want only the prefix released before the block", got)
	}
	if sink.toolCall != 0 {
		t.Errorf("blocked stream published %d tool-call chunks, want 0", sink.toolCall)
	}
	if len(sink.retracts) != 1 {
		t.Fatalf("retracts = %+v, want exactly one", sink.retracts)
	}
	r := sink.retracts[0]
	if r.Reason != "prohibited term" {
		t.Errorf("retract reason = %q, want the veto reason", r.Reason)
	}
	if r.ReleasedLen != len("safe ") {
		t.Errorf("retract ReleasedLen = %d, want %d", r.ReleasedLen, len("safe "))
	}
	if blocked, reason := sp.Blocked(); !blocked || reason != "prohibited term" {
		t.Errorf("Blocked() = %v, %q; want true and the reason", blocked, reason)
	}
}

// TestStreamPublisher_VetoClearsAStandingHold guards the UI against a stream
// blocked while suspended: without the resume edge, a "reviewing…" indicator
// would never clear.
func TestStreamPublisher_VetoClearsAStandingHold(t *testing.T) {
	bus := NewEventBus()
	sink := collectStream(t, bus)

	seen := 0
	gateStream(bus, func(seg *events.StreamSegment, vp *VetoablePayload) {
		seen++
		if seen == 1 {
			seg.HoldFrom(0)
			return
		}
		vp.Veto = VetoResult{Vetoed: true, Reason: "nope"}
	})

	sp := NewStreamPublisher(bus, "turn-1", "req-1")
	sp.Text("hold me")
	sp.Text(" then block")

	if len(sink.holds) != 2 {
		t.Fatalf("holds = %+v, want an entry edge and a resume edge", sink.holds)
	}
	if !sink.holds[1].Resumed {
		t.Error("a block while suspended must emit the resume edge so a UI can clear its indicator")
	}
	if len(sink.retracts) != 1 {
		t.Errorf("retracts = %+v, want one", sink.retracts)
	}
}

// TestStreamPublisher_RedactionShipsRewrittenText covers mutation without
// veto: the handler's rewrite is what reaches the bus, and Released reflects
// the rewrite rather than the model's original.
func TestStreamPublisher_RedactionShipsRewrittenText(t *testing.T) {
	bus := NewEventBus()
	sink := collectStream(t, bus)

	gateStream(bus, func(seg *events.StreamSegment, _ *VetoablePayload) {
		seg.Content = strings.ReplaceAll(seg.Content, "hunter2", "[REDACTED]")
	})

	sp := NewStreamPublisher(bus, "turn-1", "req-1")
	sp.Text("pw is hunter2 ok")
	sp.Close()

	if got := sink.text(); got != "pw is [REDACTED] ok" {
		t.Fatalf("text = %q, want the redacted form", got)
	}
	if got := sp.Released(); got != "pw is [REDACTED] ok" {
		t.Fatalf("Released() = %q, want the post-redaction text", got)
	}
}

// TestStreamPublisher_ReleasedIsLookbehindOnly pins that a handler sees the
// text that already shipped. Without it a pattern spanning a delta boundary
// would be invisible, which is the failure mode hold-back exists to prevent.
func TestStreamPublisher_ReleasedIsLookbehindOnly(t *testing.T) {
	bus := NewEventBus()
	collectStream(t, bus)

	var seenReleased []string
	gateStream(bus, func(seg *events.StreamSegment, _ *VetoablePayload) {
		seenReleased = append(seenReleased, seg.Released)
	})

	sp := NewStreamPublisher(bus, "turn-1", "req-1")
	sp.Text("abc")
	sp.Text("def")
	sp.Close()

	want := []string{"", "abc"}
	if len(seenReleased) < 2 || seenReleased[0] != want[0] || seenReleased[1] != want[1] {
		t.Fatalf("Released per segment = %q, want %q as the first two", seenReleased, want)
	}
}

// TestStreamPublisher_HoldSnapsToRuneBoundary guards against handing a UI a
// fragment of a multi-byte character, and against a rune straddling the
// release boundary where a scanning handler could not see it whole.
func TestStreamPublisher_HoldSnapsToRuneBoundary(t *testing.T) {
	bus := NewEventBus()
	sink := collectStream(t, bus)

	// "é" is two bytes; ask to hold one of them.
	gateStream(bus, func(seg *events.StreamSegment, _ *VetoablePayload) {
		seg.Hold = 1
	})

	sp := NewStreamPublisher(bus, "turn-1", "req-1")
	sp.Text("café")
	if got := sink.text(); got != "caf" {
		t.Fatalf("text = %q, want %q — the hold must widen to the whole rune", got, "caf")
	}
	sp.Close()
	if got := sink.text(); got != "café" {
		t.Fatalf("final text = %q, want %q", got, "café")
	}
}

// TestStreamPublisher_ChunkIndexesAreSequential keeps the ordering contract
// consumers rely on: holds and redactions change what is emitted, never the
// monotonicity of Index across text and tool-call chunks alike.
func TestStreamPublisher_ChunkIndexesAreSequential(t *testing.T) {
	bus := NewEventBus()
	sink := collectStream(t, bus)

	sp := NewStreamPublisher(bus, "turn-1", "req-1")
	sp.Text("a")
	sp.ToolCall(&events.ToolCallRequest{ID: "t1", Name: "shell"})
	sp.Text("b")
	sp.Close()

	for i, idx := range sink.indexes {
		if idx != i {
			t.Fatalf("chunk indexes = %v, want 0..n-1 in order", sink.indexes)
		}
	}
}

// TestStreamPublisher_CloseIsIdempotent matters because providers defer Close
// as an error-path backstop and also call it explicitly to order the final
// flush ahead of llm.stream.end.
func TestStreamPublisher_CloseIsIdempotent(t *testing.T) {
	bus := NewEventBus()
	sink := collectStream(t, bus)

	gateStream(bus, func(seg *events.StreamSegment, _ *VetoablePayload) {
		seg.HoldFrom(0)
	})

	sp := NewStreamPublisher(bus, "turn-1", "req-1")
	sp.Text("tail")
	sp.Close()
	sp.Close()

	if got := sink.text(); got != "tail" {
		t.Fatalf("text = %q, want the tail flushed exactly once", got)
	}
	if len(sink.chunks) != 1 {
		t.Fatalf("chunks = %v, want a single flush", sink.chunks)
	}
}

// TestStreamPublisher_NilSafe covers the provider paths that build a
// publisher lazily — OpenAI and Anthropic only learn the turn ID from the
// first SSE frame, so a stream that dies before then leaves the handle nil.
func TestStreamPublisher_NilSafe(t *testing.T) {
	var sp *StreamPublisher
	sp.Text("x")
	sp.ToolCall(&events.ToolCallRequest{ID: "t"})
	sp.Close()
	if blocked, _ := sp.Blocked(); blocked {
		t.Error("nil publisher reported blocked")
	}
	if sp.Released() != "" {
		t.Error("nil publisher reported released text")
	}
}
