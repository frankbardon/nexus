package promote

import (
	"reflect"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/events"
)

// TestExtractInputs_OrderAndShape covers both the typed events.UserInput
// payload (the live shape) and the post-reload map[string]any shape (what
// the journal Reader hands back from disk).
func TestExtractInputs_OrderAndShape(t *testing.T) {
	envs := []journal.Envelope{
		{Seq: 1, Type: "io.session.start", Payload: map[string]any{"session_id": "x"}},
		{Seq: 2, Type: "io.input", Payload: events.UserInput{SchemaVersion: events.UserInputVersion, Content: "first"}},
		{Seq: 3, Type: "agent.turn.start"},
		{Seq: 4, Type: "io.input", Payload: map[string]any{"Content": "second", "Files": nil}},
		{Seq: 5, Type: "io.input", Payload: map[string]any{"content": "third-lowercase"}},
		{Seq: 6, Type: "agent.turn.end"},
	}
	got := ExtractInputs(envs)
	want := []string{"first", "second", "third-lowercase"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractInputs() = %v, want %v", got, want)
	}
}

// TestExtractInputs_IgnoresNonInput confirms only io.input events are surfaced.
func TestExtractInputs_IgnoresNonInput(t *testing.T) {
	envs := []journal.Envelope{
		{Seq: 1, Type: "agent.turn.start"},
		{Seq: 2, Type: "llm.response", Payload: map[string]any{"Content": "ignored"}},
		{Seq: 3, Type: "tool.invoke", Payload: map[string]any{"Name": "shell"}},
	}
	if got := ExtractInputs(envs); len(got) != 0 {
		t.Errorf("ExtractInputs() over non-input events = %v, want empty", got)
	}
}

// TestExtractInputs_Empty confirms an empty envelope slice yields an empty
// (not nil) slice — the YAML serializer round-trips a stable shape either
// way, but we lock the documented contract.
func TestExtractInputs_Empty(t *testing.T) {
	got := ExtractInputs(nil)
	if got == nil {
		t.Fatalf("ExtractInputs(nil) returned nil; want empty non-nil slice")
	}
	if len(got) != 0 {
		t.Fatalf("ExtractInputs(nil) = %v, want empty", got)
	}
}

// TestExtractInputs_NilPointerSafe makes sure a nil *events.UserInput
// pointer does not panic — a stale-payload edge case the live writer can
// produce in failure paths.
func TestExtractInputs_NilPointerSafe(t *testing.T) {
	envs := []journal.Envelope{{Seq: 1, Type: "io.input", Payload: (*events.UserInput)(nil)}}
	got := ExtractInputs(envs)
	if len(got) != 0 {
		t.Fatalf("nil pointer payload produced %v, want empty", got)
	}
}

// TestInputsYAMLBytes_RoundTrip runs the YAML serializer for a known input
// list and checks the result parses back to the same slice via the shape
// the loader expects.
func TestInputsYAMLBytes_RoundTrip(t *testing.T) {
	in := []string{"hello", "world"}
	data, err := inputsYAMLBytes(in)
	if err != nil {
		t.Fatalf("inputsYAMLBytes: %v", err)
	}
	var rt inputsYAML
	if err := yamlUnmarshalForTest(data, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(rt.Inputs, in) {
		t.Fatalf("round trip: got %v want %v", rt.Inputs, in)
	}
}
