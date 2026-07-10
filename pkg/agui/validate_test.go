package agui

import "testing"

// decodeSeq encodes then decodes each event so the sequence is composed of the
// pointer types ValidateSequence expects (matching what SSEReader produces).
func decodeSeq(t *testing.T, events ...Event) []Event {
	t.Helper()
	out := make([]Event, 0, len(events))
	for _, e := range events {
		data, err := EncodeEvent(e)
		if err != nil {
			t.Fatalf("encode %s: %v", e.EventType(), err)
		}
		d, err := DecodeEvent(data)
		if err != nil {
			t.Fatalf("decode %s: %v", e.EventType(), err)
		}
		out = append(out, d)
	}
	return out
}

func TestValidateSequenceWellFormed(t *testing.T) {
	seq := decodeSeq(t,
		NewRunStarted("t", "r"),
		NewStepStarted("plan"),
		NewTextMessageStart("m1", "assistant"),
		NewTextMessageContent("m1", "hi"),
		NewTextMessageEnd("m1"),
		NewToolCallStart("tc1", "search"),
		NewToolCallArgs("tc1", "{}"),
		NewToolCallEnd("tc1"),
		NewStepFinished("plan"),
		NewRunFinished("t", "r"),
	)
	if err := ValidateSequence(seq); err != nil {
		t.Fatalf("expected well-formed sequence, got: %v", err)
	}
}

func TestValidateSequenceErrorTerminal(t *testing.T) {
	seq := decodeSeq(t, NewRunStarted("t", "r"), NewRunError("boom"))
	if err := ValidateSequence(seq); err != nil {
		t.Fatalf("RunError should be a valid terminal: %v", err)
	}
}

func TestValidateSequenceFailures(t *testing.T) {
	cases := map[string][]Event{
		"empty": {},
		"no RunStarted": {
			NewTextMessageStart("m1", "assistant"),
			NewRunFinished("t", "r"),
		},
		"no terminal": {
			NewRunStarted("t", "r"),
			NewStepStarted("plan"),
			NewStepFinished("plan"),
		},
		"event after terminal": {
			NewRunStarted("t", "r"),
			NewRunFinished("t", "r"),
			NewStepStarted("late"),
		},
		"unclosed message": {
			NewRunStarted("t", "r"),
			NewTextMessageStart("m1", "assistant"),
			NewRunFinished("t", "r"),
		},
		"content for unopened message": {
			NewRunStarted("t", "r"),
			NewTextMessageContent("ghost", "hi"),
			NewRunFinished("t", "r"),
		},
		"unmatched step finished": {
			NewRunStarted("t", "r"),
			NewStepFinished("plan"),
			NewRunFinished("t", "r"),
		},
		"unclosed tool call": {
			NewRunStarted("t", "r"),
			NewToolCallStart("tc1", "search"),
			NewRunFinished("t", "r"),
		},
	}
	for name, evs := range cases {
		t.Run(name, func(t *testing.T) {
			var seq []Event
			if len(evs) > 0 {
				seq = decodeSeq(t, evs...)
			}
			if err := ValidateSequence(seq); err == nil {
				t.Fatalf("expected validation error for %q", name)
			}
		})
	}
}
