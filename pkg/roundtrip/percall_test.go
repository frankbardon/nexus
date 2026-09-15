package roundtrip

import (
	"encoding/json"
	"reflect"
	"testing"
)

// A transport whose metadata slot is per-call has to split a turn's metadata
// across its calls and put it back together. The pair is only correct if the
// reassembled map is the one the provider published, so that is what this
// asserts rather than either half in isolation.
func TestSplittingATurnAcrossItsCallsAndFoldingItBackIsLossless(t *testing.T) {
	published := map[string]any{
		"thinking_blocks": []any{"block-1"},
		"gemini_thought_signatures": map[string]string{
			"call_0_a": "sig-a",
			"call_2_c": "sig-c",
		},
		// Engine-internal routing hints ride the same response metadata and must
		// not travel with the message.
		"_source":            "nexus.agent.react",
		"_structured_output": true,
	}
	forwarded := ForwardMessageMetadata(published)

	calls := []string{"call_0_a", "call_1_b", "call_2_c"}
	var rebuilt map[string]any
	for _, id := range calls {
		rebuilt = MergeCall(rebuilt, ForCall(forwarded, id))
	}

	sigs, ok := rebuilt["gemini_thought_signatures"].(map[string]any)
	if !ok {
		t.Fatalf("signatures came back as %T", rebuilt["gemini_thought_signatures"])
	}
	want := map[string]any{"call_0_a": "sig-a", "call_2_c": "sig-c"}
	if !reflect.DeepEqual(sigs, want) {
		t.Errorf("signatures = %v, want %v", sigs, want)
	}
	if !reflect.DeepEqual(rebuilt["thinking_blocks"], []any{"block-1"}) {
		t.Errorf("thinking_blocks = %v, want the published value", rebuilt["thinking_blocks"])
	}
	for _, k := range []string{"_source", "_structured_output"} {
		if _, ok := rebuilt[k]; ok {
			t.Errorf("engine-internal %q travelled with the message", k)
		}
	}
}

// An unsigned call carries nothing at all, so a transport serialises it without
// an empty metadata object.
func TestACallWithNoSignatureCarriesNothing(t *testing.T) {
	forwarded := ForwardMessageMetadata(map[string]any{
		"gemini_thought_signatures": map[string]string{"call_0_a": "sig-a"},
	})
	if got := ForCall(forwarded, "call_1_b"); got != nil {
		t.Errorf("ForCall for an unsigned call = %v, want nil", got)
	}
}

// Every value here makes a round trip through JSON on its way to a client and
// back, which reboxes a map[string]string as a map[string]any. A split that
// only understood the live shape would work in process and return nothing on
// exactly the replayed turn the API refuses.
func TestTheSplitSurvivesAJSONRoundTrip(t *testing.T) {
	forwarded := ForwardMessageMetadata(map[string]any{
		"gemini_thought_signatures": map[string]string{"call_0_a": "sig-a"},
	})

	wire, err := json.Marshal(ForCall(forwarded, "call_0_a"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}

	rebuilt := MergeCall(nil, decoded)
	sigs, ok := rebuilt["gemini_thought_signatures"].(map[string]any)
	if !ok || sigs["call_0_a"] != "sig-a" {
		t.Errorf("after a JSON round trip the signature reassembled as %v", rebuilt)
	}
}

// A client authors the inbound map, so anything outside the allowlist is
// dropped rather than merged.
func TestMergeCallDropsWhatIsNotAllowlisted(t *testing.T) {
	rebuilt := MergeCall(nil, map[string]any{
		"_target_plugin":  "nexus.agent.subagent",
		"thinking_blocks": []any{"legit"},
	})
	if _, ok := rebuilt["_target_plugin"]; ok {
		t.Error("a client-authored routing hint was merged into message metadata")
	}
	if _, ok := rebuilt["thinking_blocks"]; !ok {
		t.Error("the allowlisted key was dropped")
	}
}
