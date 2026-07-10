package agui

import (
	"encoding/json"
	"reflect"
	"testing"
)

// allEventSamples returns one representative value per canonical event type,
// each with its discriminator set via the constructor or literal.
func allEventSamples(t *testing.T) []Event {
	t.Helper()
	return []Event{
		NewRunStarted("thread-1", "run-1"),
		NewRunFinished("thread-1", "run-1"),
		NewRunError("boom"),
		NewStepStarted("plan"),
		NewStepFinished("plan"),
		NewTextMessageStart("msg-1", "assistant"),
		NewTextMessageContent("msg-1", "hello"),
		NewTextMessageEnd("msg-1"),
		NewTextMessageChunk("msg-1", "hi"),
		NewToolCallStart("tc-1", "search"),
		NewToolCallArgs("tc-1", `{"q":`),
		NewToolCallEnd("tc-1"),
		NewToolCallResult("msg-2", "tc-1", "result body"),
		NewToolCallChunk("tc-1"),
		NewStateSnapshot(json.RawMessage(`{"count":1}`)),
		NewStateDelta(JSONPatch{{Op: "replace", Path: "/count", Value: json.RawMessage(`2`)}}),
		NewMessagesSnapshot([]Message{{ID: "m1", Role: "user", Content: "hey"}}),
		ActivitySnapshotEvent{BaseEvent: newBase(EventActivitySnapshot), MessageID: "m1", ActivityType: "typing", Content: json.RawMessage(`"..."`)},
		ActivityDeltaEvent{BaseEvent: newBase(EventActivityDelta), MessageID: "m1", ActivityType: "typing", Patch: JSONPatch{{Op: "add", Path: "/x", Value: json.RawMessage(`1`)}}},
		NewReasoningStart(),
		ReasoningMessageStartEvent{BaseEvent: newBase(EventReasoningMessageStart), MessageID: "r1"},
		NewReasoningMessageContent("thinking..."),
		ReasoningMessageEndEvent{BaseEvent: newBase(EventReasoningMessageEnd), MessageID: "r1"},
		ReasoningMessageChunkEvent{BaseEvent: newBase(EventReasoningMessageChunk), Delta: "t"},
		NewReasoningEnd(),
		ReasoningEncryptedValueEvent{BaseEvent: newBase(EventReasoningEncryptedValue), Value: "enc"},
		NewRaw(json.RawMessage(`{"provider":"x"}`)),
		NewCustom("workflow.progress", json.RawMessage(`{"pct":50}`)),
		MetaEvent{BaseEvent: newBase(EventMeta), Name: "meta", Value: json.RawMessage(`{}`)},
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	for _, sample := range allEventSamples(t) {
		sample := sample
		t.Run(string(sample.EventType()), func(t *testing.T) {
			data, err := EncodeEvent(sample)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}

			// The decoded value is always a pointer; compare against a pointer
			// to the original sample for deep equality.
			decoded, err := DecodeEvent(data)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if decoded.EventType() != sample.EventType() {
				t.Fatalf("type mismatch: got %s want %s", decoded.EventType(), sample.EventType())
			}

			want := reflect.New(reflect.TypeOf(sample))
			want.Elem().Set(reflect.ValueOf(sample))
			if !reflect.DeepEqual(decoded, want.Interface()) {
				t.Fatalf("round-trip mismatch\n got: %#v\nwant: %#v", decoded, want.Interface())
			}
		})
	}
}

func TestDecodeUnknownType(t *testing.T) {
	if _, err := DecodeEvent([]byte(`{"type":"NotAThing"}`)); err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestDecodeMissingType(t *testing.T) {
	if _, err := DecodeEvent([]byte(`{"messageId":"x"}`)); err == nil {
		t.Fatal("expected error for missing type")
	}
}

func TestEncodeEmptyType(t *testing.T) {
	if _, err := EncodeEvent(RunStartedEvent{}); err == nil {
		t.Fatal("expected error encoding event with empty type")
	}
}

func TestStateDeltaIsJSONPatchArray(t *testing.T) {
	ev := NewStateDelta(JSONPatch{
		{Op: "add", Path: "/items/-", Value: json.RawMessage(`"x"`)},
		{Op: "remove", Path: "/old"},
		{Op: "move", From: "/a", Path: "/b"},
	})
	data, err := EncodeEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	// delta must serialize as a JSON array.
	var probe struct {
		Delta json.RawMessage `json:"delta"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatal(err)
	}
	if len(probe.Delta) == 0 || probe.Delta[0] != '[' {
		t.Fatalf("delta is not a JSON array: %s", probe.Delta)
	}
}
