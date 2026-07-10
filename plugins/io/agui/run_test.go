package agui

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/events"
)

// drainTypes reads every queued AG-UI event from a finished run and returns
// their type discriminators in order.
func drainTypes(r *run) []agui.EventType {
	r.finish()
	var out []agui.EventType
	for {
		select {
		case e := <-r.out:
			out = append(out, e.EventType())
		default:
			return out
		}
	}
}

// TestOnOutput_NonStreamedRendersTriple covers the common non-streamed output:
// it must produce a TextMessage start/content/end triple.
func TestOnOutput_NonStreamedRendersTriple(t *testing.T) {
	r := newRun("t", "r", nil)
	r.onOutput(events.AgentOutput{Content: "hello", Role: "assistant"})

	types := drainTypes(r)
	assertContainsTriple(t, types)
}

// TestOnOutput_StreamedWithoutChunksRendersTriple is the regression guard for
// non-streaming providers (mock/batch): the output is flagged "streamed" but no
// llm.stream.chunk ever arrived, so the text must still be rendered rather than
// dropped. Without the textStreamed guard this triple would be silently lost.
func TestOnOutput_StreamedWithoutChunksRendersTriple(t *testing.T) {
	r := newRun("t", "r", nil)
	r.onOutput(events.AgentOutput{
		Content:  "high noon",
		Role:     "assistant",
		Metadata: map[string]any{"streamed": true},
	})

	types := drainTypes(r)
	assertContainsTriple(t, types)
}

// TestOnOutput_StreamedWithChunksSkips confirms a genuinely streamed output
// (chunks already rendered) is NOT re-rendered by onOutput, avoiding duplicate
// text.
func TestOnOutput_StreamedWithChunksSkips(t *testing.T) {
	r := newRun("t", "r", nil)
	// Simulate the streamed path.
	r.onStreamChunk(events.StreamChunk{Content: "high "})
	r.onStreamChunk(events.StreamChunk{Content: "noon"})
	r.onStreamEnd(events.StreamEnd{})
	// The final io.output echoes the same content flagged streamed.
	r.onOutput(events.AgentOutput{
		Content:  "high noon",
		Role:     "assistant",
		Metadata: map[string]any{"streamed": true},
	})

	types := drainTypes(r)
	starts := 0
	for _, ty := range types {
		if ty == agui.EventTextMessageStart {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("TextMessageStart count = %d, want 1 (no duplicate render)", starts)
	}
}

func assertContainsTriple(t *testing.T, types []agui.EventType) {
	t.Helper()
	want := []agui.EventType{
		agui.EventTextMessageStart,
		agui.EventTextMessageContent,
		agui.EventTextMessageEnd,
	}
	idx := 0
	for _, ty := range types {
		if idx < len(want) && ty == want[idx] {
			idx++
		}
	}
	if idx != len(want) {
		t.Fatalf("stream %v missing ordered text triple %v", types, want)
	}
}
