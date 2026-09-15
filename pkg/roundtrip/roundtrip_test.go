package roundtrip

import "testing"

func TestForwardMessageMetadata_KeepsOnlyContinuityKeys(t *testing.T) {
	out := ForwardMessageMetadata(map[string]any{
		"_source":                   "delegate.abc",
		"task_kind":                 "delegate",
		"thinking_blocks":           []any{"block"},
		"gemini_thought_signatures": map[string]any{"call_0": "sig"},
	})
	if len(out) != 2 {
		t.Fatalf("out = %v, want exactly the two continuity keys", out)
	}
	if _, ok := out["_source"]; ok {
		t.Errorf("request-scoped routing hint _source leaked into a message")
	}
	if got := out["gemini_thought_signatures"].(map[string]any)["call_0"]; got != "sig" {
		t.Errorf("gemini_thought_signatures = %v, want the captured signature", got)
	}
}

func TestForwardMessageMetadata_NilWhenNothingToCarry(t *testing.T) {
	if got := ForwardMessageMetadata(nil); got != nil {
		t.Errorf("ForwardMessageMetadata(nil) = %v, want nil", got)
	}
	if got := ForwardMessageMetadata(map[string]any{"_source": "x"}); got != nil {
		t.Errorf("out = %v, want nil so the message serialises without an empty object", got)
	}
}
