package agui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestSSERoundTripAllEvents(t *testing.T) {
	samples := allEventSamples(t)

	var buf bytes.Buffer
	w := NewSSEWriter(&buf)
	for _, e := range samples {
		if err := w.Write(e); err != nil {
			t.Fatalf("write %s: %v", e.EventType(), err)
		}
	}

	r := NewSSEReader(&buf)
	got, err := r.ReadAll()
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if len(got) != len(samples) {
		t.Fatalf("event count: got %d want %d", len(got), len(samples))
	}
	for i, e := range got {
		want := reflect.New(reflect.TypeOf(samples[i]))
		want.Elem().Set(reflect.ValueOf(samples[i]))
		if !reflect.DeepEqual(e, want.Interface()) {
			t.Fatalf("event %d mismatch\n got: %#v\nwant: %#v", i, e, want.Interface())
		}
	}
}

func TestSSEWriterRecordFormat(t *testing.T) {
	var buf bytes.Buffer
	w := NewSSEWriter(&buf)
	if err := w.Write(NewTextMessageContent("m1", "hi")); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "data: ") {
		t.Fatalf("record must start with 'data: ', got %q", out)
	}
	if !strings.HasSuffix(out, "\n\n") {
		t.Fatalf("record must end with blank line, got %q", out)
	}
}

func TestSSEReaderSkipsCommentsAndBlanks(t *testing.T) {
	stream := ": heartbeat\n" +
		"\n" +
		"data: {\"type\":\"StepStarted\",\"stepName\":\"a\"}\n" +
		"\n" +
		": another comment\n" +
		"event: ignored\n" +
		"data: {\"type\":\"StepFinished\",\"stepName\":\"a\"}\n" +
		"\n"

	r := NewSSEReader(strings.NewReader(stream))
	events, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events want 2", len(events))
	}
	if events[0].EventType() != EventStepStarted || events[1].EventType() != EventStepFinished {
		t.Fatalf("unexpected event types: %s, %s", events[0].EventType(), events[1].EventType())
	}
}

func TestSSEReaderTrailingRecordNoBlankLine(t *testing.T) {
	stream := "data: {\"type\":\"RunError\",\"message\":\"x\"}"
	r := NewSSEReader(strings.NewReader(stream))
	e, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if e.EventType() != EventRunError {
		t.Fatalf("got %s", e.EventType())
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestWriteHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteHeaders(rec.Header())
	if got := rec.Header().Get("Content-Type"); got != ContentType {
		t.Fatalf("content-type: got %q want %q", got, ContentType)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("cache-control: got %q", got)
	}
}

func TestSSEWriterFlushes(t *testing.T) {
	rec := httptest.NewRecorder()
	w := NewSSEWriter(rec)
	if err := w.Write(NewRunStarted("t", "r")); err != nil {
		t.Fatal(err)
	}
	if !rec.Flushed {
		t.Fatal("expected recorder to be flushed")
	}
}

func TestRunAgentInputRoundTrip(t *testing.T) {
	in := RunAgentInput{
		ThreadID: "thread-1",
		RunID:    "run-2",
		Messages: []Message{
			{ID: "m1", Role: "user", Content: "hello"},
			{ID: "m2", Role: "assistant", ToolCalls: []ToolCall{{ID: "tc1", Type: "function", Function: ToolCallFunction{Name: "search", Arguments: `{"q":"x"}`}}}},
		},
		State:          json.RawMessage(`{"k":"v"}`),
		Tools:          []Tool{{Name: "search", Description: "web search", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Context:        []ContextItem{{Description: "user tz", Value: "UTC"}},
		ForwardedProps: json.RawMessage(`{"ui":"dark"}`),
		Resume: []ResumeItem{
			{InterruptID: "int-1", Status: ResumeResolved, Payload: json.RawMessage(`{"answer":"yes"}`)},
			{InterruptID: "int-2", Status: ResumeCancelled},
		},
	}

	data, err := in.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRunAgentInput(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round-trip mismatch\n got: %#v\nwant: %#v", got, in)
	}
}

func TestRunAgentInputStatePresent(t *testing.T) {
	// A client-authored shared-state document must decode into State off the
	// "state" wire tag (E3-S2 inbound state).
	raw := `{"threadId":"t","runId":"r","state":{"scene_1":{"title":"draft"}}}`
	in, err := DecodeRunAgentInput([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(in.State) == 0 {
		t.Fatal("State empty; expected client state document")
	}
	var m map[string]any
	if err := json.Unmarshal(in.State, &m); err != nil {
		t.Fatalf("State is not a JSON object: %v", err)
	}
	s1, ok := m["scene_1"].(map[string]any)
	if !ok || s1["title"] != "draft" {
		t.Fatalf("State = %s, want scene_1.title=draft", in.State)
	}
}

func TestRunAgentInputResumePresent(t *testing.T) {
	// resume[] must survive JSON with the exact field names.
	raw := `{"threadId":"t","runId":"r","resume":[{"interruptId":"i1","status":"resolved","payload":{"a":1}}]}`
	in, err := DecodeRunAgentInput([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Resume) != 1 {
		t.Fatalf("got %d resume items", len(in.Resume))
	}
	if in.Resume[0].InterruptID != "i1" || in.Resume[0].Status != ResumeResolved {
		t.Fatalf("bad resume item: %#v", in.Resume[0])
	}
}
