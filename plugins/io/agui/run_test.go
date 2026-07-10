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

// drainEvents reads every queued AG-UI event from a finished run.
func drainEvents(r *run) []agui.Event {
	r.finish()
	var out []agui.Event
	for {
		select {
		case e := <-r.out:
			out = append(out, e)
		default:
			return out
		}
	}
}

// validAGUIRoles is the closed set the @ag-ui/client schema permits on a
// Message / TextMessageStart. Any other value breaks the reference client.
var validAGUIRoles = map[string]bool{
	"developer": true,
	"system":    true,
	"assistant": true,
	"user":      true,
	"tool":      true,
}

// TestOnOutput_ErrorRoleMapsToAssistant is the regression guard for the AG-UI
// spec-compliance bug: a Nexus io.output carrying Role "error" (used for failed
// LLM calls / guardrails) must never reach the wire with role "error" — the
// reference client rejects it. It must render as an "assistant" message with the
// error content preserved.
func TestOnOutput_ErrorRoleMapsToAssistant(t *testing.T) {
	r := newRun("t", "r", nil)
	r.onOutput(events.AgentOutput{Content: "boom: llm failed", Role: "error"})

	// The rendered message must be assistant with content preserved.
	msgs := r.snapshotMessages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	if msgs[0].Role != "assistant" {
		t.Fatalf("message role = %q, want assistant", msgs[0].Role)
	}
	if msgs[0].Content != "boom: llm failed" {
		t.Fatalf("message content = %q, want error text preserved", msgs[0].Content)
	}

	// The wire TextMessageStart must carry role assistant, never error.
	evs := drainEvents(r)
	sawStart := false
	for _, e := range evs {
		if s, ok := e.(agui.TextMessageStartEvent); ok {
			sawStart = true
			if s.Role != "assistant" {
				t.Fatalf("TextMessageStart role = %q, want assistant", s.Role)
			}
		}
	}
	if !sawStart {
		t.Fatal("no TextMessageStart emitted for error output")
	}
	assertOnlyValidRoles(t, evs)
}

// TestOnOutput_UnknownRoleMapsToAssistant confirms empty/unknown roles also fall
// back to assistant rather than leaking an invalid literal.
func TestOnOutput_UnknownRoleMapsToAssistant(t *testing.T) {
	for _, role := range []string{"", "guardrail", "ERROR", "developer-note"} {
		r := newRun("t", "r", nil)
		r.onOutput(events.AgentOutput{Content: "x", Role: role})
		msgs := r.snapshotMessages()
		if len(msgs) != 1 || msgs[0].Role != "assistant" {
			t.Fatalf("role %q: got %+v, want single assistant message", role, msgs)
		}
	}
}

// TestSanitizeRole exercises the helper directly: valid roles pass through,
// everything else collapses to assistant.
func TestSanitizeRole(t *testing.T) {
	for _, role := range []string{"developer", "system", "assistant", "user", "tool"} {
		if got := sanitizeRole(role); got != role {
			t.Fatalf("sanitizeRole(%q) = %q, want passthrough", role, got)
		}
	}
	for _, role := range []string{"error", "", "Assistant", "unknown"} {
		if got := sanitizeRole(role); got != "assistant" {
			t.Fatalf("sanitizeRole(%q) = %q, want assistant", role, got)
		}
	}
}

// assertOnlyValidRoles asserts that no emitted TextMessageStart or Message
// carries a role outside the AG-UI-permitted set.
func assertOnlyValidRoles(t *testing.T, evs []agui.Event) {
	t.Helper()
	for _, e := range evs {
		if s, ok := e.(agui.TextMessageStartEvent); ok && !validAGUIRoles[s.Role] {
			t.Fatalf("TextMessageStart carries invalid AG-UI role %q", s.Role)
		}
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
