package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/journal"
	evalcase "github.com/frankbardon/nexus/pkg/eval/case"
	"github.com/frankbardon/nexus/pkg/events"
)

// fixtureTranscriptEvents is a known fixture session covering every payload
// shape RenderTranscript has to handle: a map[string]any payload (the
// common case, off a loaded journal), a typed event struct payload (the
// wildcard-collector fallback shape), a nil payload (agent.turn.start/end
// carry none), and one deliberately long payload value, to prove v1 does
// not truncate.
func fixtureTranscriptEvents() []evalcase.ObservedEvent {
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	longContent := strings.Repeat("this reply is long on purpose. ", 200) // ~6.4KB
	return []evalcase.ObservedEvent{
		{Type: "io.session.start", Timestamp: t0, Payload: map[string]any{"session_id": "fixture"}},
		{Type: "io.input", Timestamp: t0.Add(10 * time.Millisecond), Payload: events.UserInput{
			SchemaVersion: events.UserInputVersion,
			Content:       "hi there",
		}},
		{Type: "agent.turn.start", Timestamp: t0.Add(20 * time.Millisecond), Payload: nil},
		{Type: "tool.invoke", Timestamp: t0.Add(30 * time.Millisecond), Payload: map[string]any{
			"id":   "tc_0",
			"name": "read_file",
			"args": map[string]any{"path": "README.md"},
		}},
		{Type: "tool.result", Timestamp: t0.Add(40 * time.Millisecond), Payload: map[string]any{
			"id":   "tc_0",
			"name": "read_file",
			"ok":   true,
		}},
		{Type: "llm.response", Timestamp: t0.Add(50 * time.Millisecond), Payload: events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Model:         "mock",
			FinishReason:  "end_turn",
			Usage:         events.Usage{PromptTokens: 50, CompletionTokens: 10, TotalTokens: 60},
			Content:       longContent,
		}},
		{Type: "agent.turn.end", Timestamp: t0.Add(60 * time.Millisecond), Payload: nil},
	}
}

// TestRenderTranscript_IncludesEveryEventTypeUnfiltered proves the v1
// contract: every event type present in a known fixture session appears in
// the rendered output, in order, with its full (untruncated) payload
// content — no event dropped, no payload trimmed.
func TestRenderTranscript_IncludesEveryEventTypeUnfiltered(t *testing.T) {
	evts := fixtureTranscriptEvents()
	out := RenderTranscript(evts)

	if out == "" {
		t.Fatal("expected non-empty transcript")
	}

	// Every event type present in the fixture must appear in the output.
	wantTypes := []string{
		"io.session.start", "io.input", "agent.turn.start",
		"tool.invoke", "tool.result", "llm.response", "agent.turn.end",
	}
	for _, ty := range wantTypes {
		if !strings.Contains(out, ty) {
			t.Errorf("transcript missing event type %q\n---\n%s", ty, out)
		}
	}

	// Order is preserved: each type's header must appear strictly after
	// the previous type's header.
	lastIdx := -1
	for _, ty := range wantTypes {
		idx := strings.Index(out, ty)
		if idx <= lastIdx {
			t.Errorf("event type %q out of order (idx=%d, previous=%d)\n---\n%s", ty, idx, lastIdx, out)
		}
		lastIdx = idx
	}

	// Relevant payload fields surface legibly, not just event type names.
	for _, want := range []string{"hi there", "read_file", "README.md", "tc_0", `"ok": true`} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript missing expected payload content %q\n---\n%s", want, out)
		}
	}

	// No truncation: the full long payload value must be present verbatim.
	longContent := strings.Repeat("this reply is long on purpose. ", 200)
	if !strings.Contains(out, longContent) {
		t.Error("transcript truncated a long payload value; v1 must render it in full")
	}

	// Events with a nil payload (agent.turn.start/end) still get a header
	// line, just no payload body.
	if !strings.Contains(out, "agent.turn.start\n") {
		t.Errorf("expected a bare header line for the nil-payload agent.turn.start event\n---\n%s", out)
	}
}

// TestRenderTranscript_Empty confirms an empty observed stream renders to
// an empty string rather than panicking or emitting stray formatting.
func TestRenderTranscript_Empty(t *testing.T) {
	if out := RenderTranscript(nil); out != "" {
		t.Errorf("expected empty transcript for nil input, got %q", out)
	}
}

// TestRunLive_ResultCarriesTranscript proves the acceptance criterion end
// to end on the real RunLive path (not just the unit-level RenderTranscript
// call): a live session's Result.Transcript is non-empty and contains every
// event type actually observed, per res.Counts.
func TestRunLive_ResultCarriesTranscript(t *testing.T) {
	assertionsYAML := `deterministic:
  - kind: event_emitted
    type: io.input
    count: { min: 1, max: 1 }
`
	golden := []journal.Envelope{
		{Seq: 1, Type: "io.session.start", Payload: map[string]any{"session_id": "golden"}},
	}
	c, sessionsRoot := buildLiveCase(t, "Hello from live mock", golden, assertionsYAML)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := RunLive(ctx, c, Options{SessionsRoot: sessionsRoot})
	if err != nil {
		t.Fatalf("RunLive: %v", err)
	}
	if res.Transcript == "" {
		t.Fatal("expected non-empty Result.Transcript")
	}
	for ty, n := range res.Counts {
		if n == 0 {
			continue
		}
		if !strings.Contains(res.Transcript, ty) {
			t.Errorf("Result.Transcript missing observed event type %q (count=%d)\n---\n%s", ty, n, res.Transcript)
		}
	}
}
