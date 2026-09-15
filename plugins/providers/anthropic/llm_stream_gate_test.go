package anthropic

import (
	"regexp"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/pricing"
	"github.com/frankbardon/nexus/pkg/events"
)

// ssnPattern mirrors the builtin check in nexus.gate.content_safety.
var ssnPattern = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)

// gatedStream splits the sensitive value across three text deltas, the way a
// real tokenizer would. A per-delta scan matches none of the pieces; only a
// handler seeing the cumulative turn catches it.
const gatedStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_stream_gate","model":"claude-sonnet-4-5-20250514","usage":{"input_tokens":50,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The number is "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"123-"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"45-6789"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" — keep it safe."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":12}}

event: message_stop
data: {"type":"message_stop"}

`

// streamedText concatenates the text chunks a recorder captured — what a UI
// subscribed to llm.stream.chunk would actually have rendered.
func streamedText(rec *busRecorder) string {
	var b strings.Builder
	for _, e := range rec.byType("llm.stream.chunk") {
		if c, ok := e.Payload.(events.StreamChunk); ok {
			b.WriteString(c.Content)
		}
	}
	return b.String()
}

// TestStreamGate_BlocksBeforeAnythingIsRendered drives the real SSE state
// machine, not a hand-fed publisher, and is the end-to-end form of the claim
// the streaming seam exists to make.
//
// The other tests in this package prove the provider's ungated path is
// unchanged; none of them has a handler subscribed, so none of them proves a
// gate can actually stop a provider's stream. This one does: a pattern split
// across three deltas is caught, and no part of it reaches llm.stream.chunk.
//
// Under before:llm.response alone the whole value would have been rendered
// before the veto fired, because that hook does not run until message_stop.
func TestStreamGate_BlocksBeforeAnythingIsRendered(t *testing.T) {
	rec := newBusRecorder()

	// A minimal stand-in for nexus.gate.content_safety: hold the trailing
	// non-whitespace run, block on a complete SSN.
	rec.bus.Subscribe(engine.EventBeforeStreamChunk, func(e engine.Event[any]) {
		vp, ok := e.Payload.(*engine.VetoablePayload)
		if !ok {
			return
		}
		seg, ok := vp.Original.(*events.StreamSegment)
		if !ok {
			return
		}
		i := len(seg.Content)
		for i > 0 && seg.Content[i-1] != ' ' {
			i--
		}
		seg.HoldFrom(i)

		if loc := ssnPattern.FindStringIndex(seg.Full()); loc != nil && loc[1] > seg.PendingOffset() {
			vp.Veto = engine.VetoResult{Vetoed: true, Reason: "Content safety: ssn"}
		}
	})

	p := &Plugin{
		bus:     rec.bus,
		logger:  silentLogger(),
		pricing: pricing.DefaultsFor(pricing.ProviderAnthropic),
	}

	p.handleStreamResponse(strings.NewReader(gatedStream), "test-req", nil, nil)

	// --- Nothing of the value was ever rendered.
	rendered := streamedText(rec)
	if strings.Contains(rendered, "123-45-6789") {
		t.Fatalf("streamed text %q leaked the value the gate blocked", rendered)
	}
	if strings.Contains(rendered, "123") {
		t.Errorf("streamed text %q leaked part of the value; the trailing-run "+
			"hold should have withheld it until the match completed", rendered)
	}
	if rendered != "The number is " {
		t.Errorf("streamed text = %q, want only the clean prefix", rendered)
	}

	// --- The block was announced so transports can disown the prefix.
	retracts := rec.byType("llm.stream.retract")
	if len(retracts) != 1 {
		t.Fatalf("llm.stream.retract count = %d, want 1", len(retracts))
	}
	if r := retracts[0].Payload.(events.StreamRetract); r.ReleasedLen != len("The number is ") {
		t.Errorf("retract ReleasedLen = %d, want %d", r.ReleasedLen, len("The number is "))
	}

	// --- The provider still published the model's real text to the response
	// hook, so the response-level gate has the truth to adjudicate.
	resps := rec.byType("llm.response")
	if len(resps) != 1 {
		t.Fatalf("llm.response count = %d, want 1", len(resps))
	}
	resp := resps[0].Payload.(events.LLMResponse)
	if !strings.Contains(resp.Content, "123-45-6789") {
		t.Errorf("llm.response Content = %q; the response hook must see what the "+
			"model actually said, not the truncated stream", resp.Content)
	}
}

// TestStreamGate_UngatedProviderStreamIsUnchanged pins that routing the
// provider through StreamPublisher costs an ungated deployment nothing: the
// same text, in the same chunks, with no hold or retract traffic.
func TestStreamGate_UngatedProviderStreamIsUnchanged(t *testing.T) {
	rec := newBusRecorder()

	p := &Plugin{
		bus:     rec.bus,
		logger:  silentLogger(),
		pricing: pricing.DefaultsFor(pricing.ProviderAnthropic),
	}

	p.handleStreamResponse(strings.NewReader(gatedStream), "test-req", nil, nil)

	want := "The number is 123-45-6789 — keep it safe."
	if got := streamedText(rec); got != want {
		t.Errorf("streamed text = %q, want %q", got, want)
	}
	if n := len(rec.byType("llm.stream.chunk")); n != 4 {
		t.Errorf("chunk count = %d, want 4 — delta boundaries must be preserved", n)
	}
	if n := len(rec.byType("llm.stream.hold")); n != 0 {
		t.Errorf("ungated stream emitted %d hold events, want none", n)
	}
	if n := len(rec.byType("llm.stream.retract")); n != 0 {
		t.Errorf("ungated stream emitted %d retracts, want none", n)
	}
}
