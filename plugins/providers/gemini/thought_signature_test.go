package gemini

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

// Gemini 3.x attaches an opaque thoughtSignature to model parts in a
// function-calling exchange and rejects any follow-up request that does not
// echo each signature back on the part it arrived with (HTTP 400 "Function
// call is missing a thought_signature"). These tests pin the round trip:
// capture on convert, echo on the next request, and silence for 2.5-era
// turns that carry no signatures.

func TestConvertAPIResponse_CapturesThoughtSignatures(t *testing.T) {
	resp := (&Plugin{}).convertAPIResponse(apiResponse{
		Candidates: []apiCandidate{{
			Content: apiContent{Parts: []apiPart{
				{FunctionCall: &apiFunctionCall{Name: "search", Args: map[string]any{"q": "x"}}, ThoughtSignature: "sig-fc-0"},
				{FunctionCall: &apiFunctionCall{Name: "predict", Args: map[string]any{}}, ThoughtSignature: "sig-fc-1"},
				{Text: "done", ThoughtSignature: "sig-text"},
			}},
		}},
	}, "")

	sigs, ok := resp.Metadata[thoughtSigMetaKey].(map[string]string)
	if !ok {
		t.Fatalf("Metadata[%s] is %T, want map[string]string", thoughtSigMetaKey, resp.Metadata[thoughtSigMetaKey])
	}
	want := map[string]string{
		"call_0_search":  "sig-fc-0",
		"call_1_predict": "sig-fc-1",
		textSigKey:       "sig-text",
	}
	if len(sigs) != len(want) {
		t.Fatalf("got %d signatures %v, want %d", len(sigs), sigs, len(want))
	}
	for k, v := range want {
		if sigs[k] != v {
			t.Fatalf("sigs[%q] = %q, want %q", k, sigs[k], v)
		}
	}
	// The captured keys must match the tool-call IDs the same conversion
	// synthesized, or the echo can never find them again.
	for _, tc := range resp.ToolCalls {
		if _, ok := sigs[tc.ID]; !ok {
			t.Fatalf("no signature captured under synthesized tool-call ID %q", tc.ID)
		}
	}
}

func TestConvertAPIResponse_NoSignaturesNoMetadata(t *testing.T) {
	resp := (&Plugin{}).convertAPIResponse(apiResponse{
		Candidates: []apiCandidate{{
			Content: apiContent{Parts: []apiPart{
				{FunctionCall: &apiFunctionCall{Name: "search", Args: map[string]any{}}},
			}},
		}},
	}, "")
	if resp.Metadata != nil {
		t.Fatalf("2.5-style response must carry no signature metadata, got %v", resp.Metadata)
	}
}

// assistantParts converts a single assistant message and returns its parts.
func assistantParts(t *testing.T, msg events.Message) []map[string]any {
	t.Helper()
	_, contents, err := (&Plugin{}).convertMessages([]events.Message{
		{Role: "user", Content: "hi"},
		msg,
	})
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	if len(contents) != 2 {
		t.Fatalf("want 2 contents, got %d", len(contents))
	}
	parts, ok := contents[1]["parts"].([]map[string]any)
	if !ok || len(parts) == 0 {
		t.Fatalf("no parts on assistant content: %#v", contents[1])
	}
	return parts
}

func TestConvertMessages_EchoesThoughtSignatures(t *testing.T) {
	// Both metadata shapes must work: map[string]string as stashed in-process,
	// and map[string]any as it comes back from JSON session persistence.
	shapes := map[string]any{
		"in-process": map[string]string{
			"call_0_search": "sig-fc-0",
			textSigKey:      "sig-text",
		},
		"json-round-trip": map[string]any{
			"call_0_search": "sig-fc-0",
			textSigKey:      "sig-text",
		},
	}
	for name, sigs := range shapes {
		t.Run(name, func(t *testing.T) {
			parts := assistantParts(t, events.Message{
				Role:      "assistant",
				Content:   "calling search",
				ToolCalls: []events.ToolCallRequest{{ID: "call_0_search", Name: "search", Arguments: `{"q":"x"}`}},
				Metadata:  map[string]any{thoughtSigMetaKey: sigs},
			})
			if len(parts) != 2 {
				t.Fatalf("want text + functionCall parts, got %d: %#v", len(parts), parts)
			}
			if got := parts[0]["thoughtSignature"]; got != "sig-text" {
				t.Fatalf("text part signature = %v, want sig-text", got)
			}
			if got := parts[1]["thoughtSignature"]; got != "sig-fc-0" {
				t.Fatalf("functionCall part signature = %v, want sig-fc-0", got)
			}
		})
	}
}

func TestConvertMessages_NoSignatureNoKey(t *testing.T) {
	parts := assistantParts(t, events.Message{
		Role:      "assistant",
		Content:   "plain 2.5-era turn",
		ToolCalls: []events.ToolCallRequest{{ID: "call_0_search", Name: "search", Arguments: "{}"}},
	})
	for i, part := range parts {
		if _, present := part["thoughtSignature"]; present {
			t.Fatalf("part %d must not carry thoughtSignature: %#v", i, part)
		}
	}
}

func TestMergeMetadata(t *testing.T) {
	if got := mergeMetadata(nil, nil); got != nil {
		t.Fatalf("nil+nil = %v, want nil", got)
	}
	base := map[string]any{thoughtSigMetaKey: "x", "keep": 1}
	if got := mergeMetadata(base, nil); got["keep"] != 1 {
		t.Fatalf("base must survive nil overlay: %v", got)
	}
	merged := mergeMetadata(base, map[string]any{"keep": 2, "extra": true})
	if merged["keep"] != 2 || merged["extra"] != true || merged[thoughtSigMetaKey] != "x" {
		t.Fatalf("overlay must win, base keys survive: %v", merged)
	}
}
