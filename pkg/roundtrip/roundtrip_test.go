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

// OpenAI's Responses surface issues its continuity token as an ordered list of
// whole `reasoning` Items, each carrying an opaque encrypted_content blob. The
// forwarder must carry the list through untouched — including the summary that
// rides alongside the blob, which is the combination several other SDKs drop.
func TestForwardMessageMetadata_CarriesOpenAIReasoningItems(t *testing.T) {
	items := []map[string]any{
		{"type": "reasoning", "id": "rs_1", "encrypted_content": "blob-1",
			"summary": []any{map[string]any{"type": "summary_text", "text": "Checking units."}}},
		{"type": "reasoning", "id": "rs_2", "encrypted_content": "blob-2"},
	}
	out := ForwardMessageMetadata(map[string]any{
		"_source":                  "delegate.abc",
		"openai_reasoning_items":   items,
		"openai_reasoning_summary": []string{"Checking units."},
	})

	got, ok := out["openai_reasoning_items"].([]map[string]any)
	if !ok {
		t.Fatalf("openai_reasoning_items = %T, want the captured Items", out["openai_reasoning_items"])
	}
	if len(got) != 2 || got[0]["encrypted_content"] != "blob-1" || got[1]["encrypted_content"] != "blob-2" {
		t.Errorf("Items = %v, want both blobs in arrival order", got)
	}
	if got[0]["summary"] == nil {
		t.Error("the summary riding the first Item was dropped — the exact bug other SDKs shipped")
	}

	// The summary *text* key is display material, not continuity state: a
	// later request has no use for it and history should not carry a second
	// copy of every summary.
	if _, ok := out["openai_reasoning_summary"]; ok {
		t.Error("openai_reasoning_summary must not be forwarded onto a stored Message")
	}
}

// The Items are a per-message payload, not a per-call map: they are not
// addressed by tool-call ID at all, so a transport that splits a turn across
// its calls carries them whole on each one.
func TestOpenAIReasoningItemsAreAPerMessageKey(t *testing.T) {
	if perCallKeys["openai_reasoning_items"] {
		t.Fatal("openai_reasoning_items must not be a per-call key")
	}
	items := []map[string]any{{"type": "reasoning", "id": "rs_1", "encrypted_content": "blob"}}
	meta := map[string]any{"openai_reasoning_items": items}

	one := ForCall(meta, "call_a")
	if got, ok := one["openai_reasoning_items"].([]map[string]any); !ok || len(got) != 1 {
		t.Fatalf("ForCall carried %v, want the whole list", one["openai_reasoning_items"])
	}

	back := MergeCall(nil, one)
	if got, ok := back["openai_reasoning_items"].([]map[string]any); !ok || got[0]["encrypted_content"] != "blob" {
		t.Errorf("MergeCall reassembled %v, want the blob intact", back["openai_reasoning_items"])
	}
}
