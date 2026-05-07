package gemini

import (
	"encoding/json"
	"testing"
)

// TestGeminiModalityBreakdown_PromptDetails verifies that promptTokensDetails
// entries are summed under lowercase modality keys.
func TestGeminiModalityBreakdown_PromptDetails(t *testing.T) {
	u := &apiUsageMetadata{
		PromptTokenCount: 1000,
		PromptTokensDetails: []apiModalityDetail{
			{Modality: "TEXT", TokenCount: 800},
			{Modality: "IMAGE", TokenCount: 200},
		},
	}

	mb := geminiModalityBreakdown(u)
	if got := mb["text"]; got != 800 {
		t.Errorf("text modality tokens: got %d want 800", got)
	}
	if got := mb["image"]; got != 200 {
		t.Errorf("image modality tokens: got %d want 200", got)
	}
}

// TestGeminiModalityBreakdown_PromptAndCandidates verifies that prompt and
// candidate modality details accumulate per-modality across both sides.
func TestGeminiModalityBreakdown_PromptAndCandidates(t *testing.T) {
	u := &apiUsageMetadata{
		PromptTokensDetails: []apiModalityDetail{
			{Modality: "AUDIO", TokenCount: 300},
			{Modality: "TEXT", TokenCount: 100},
		},
		CandidatesTokensDetails: []apiModalityDetail{
			{Modality: "AUDIO", TokenCount: 50},
			{Modality: "TEXT", TokenCount: 200},
		},
	}

	mb := geminiModalityBreakdown(u)
	if got, want := mb["audio"], 350; got != want {
		t.Errorf("audio: got %d want %d", got, want)
	}
	if got, want := mb["text"], 300; got != want {
		t.Errorf("text: got %d want %d", got, want)
	}
}

// TestGeminiModalityBreakdown_NoDetails verifies that responses without any
// modality detail return nil so callers leave events.Usage.ModalityBreakdown
// unset.
func TestGeminiModalityBreakdown_NoDetails(t *testing.T) {
	u := &apiUsageMetadata{
		PromptTokenCount:     500,
		CandidatesTokenCount: 200,
	}
	if mb := geminiModalityBreakdown(u); mb != nil {
		t.Errorf("expected nil ModalityBreakdown when no details present; got %v", mb)
	}
}

// TestGeminiModalityBreakdown_NilSafe guards the nil-input path.
func TestGeminiModalityBreakdown_NilSafe(t *testing.T) {
	if mb := geminiModalityBreakdown(nil); mb != nil {
		t.Errorf("expected nil for nil usage; got %v", mb)
	}
}

// TestApiUsageMetadata_ParsesModalityDetails verifies the JSON tags match
// Gemini's wire shape — a producer test against the documented payload.
func TestApiUsageMetadata_ParsesModalityDetails(t *testing.T) {
	raw := []byte(`{
		"promptTokenCount": 1000,
		"candidatesTokenCount": 200,
		"totalTokenCount": 1200,
		"promptTokensDetails": [
			{"modality": "TEXT", "tokenCount": 800},
			{"modality": "IMAGE", "tokenCount": 200}
		],
		"candidatesTokensDetails": [
			{"modality": "TEXT", "tokenCount": 200}
		]
	}`)

	var u apiUsageMetadata
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatalf("unmarshal apiUsageMetadata: %v", err)
	}

	if got := len(u.PromptTokensDetails); got != 2 {
		t.Fatalf("PromptTokensDetails len: got %d want 2", got)
	}
	if u.PromptTokensDetails[0].Modality != "TEXT" || u.PromptTokensDetails[0].TokenCount != 800 {
		t.Errorf("PromptTokensDetails[0]: got %+v", u.PromptTokensDetails[0])
	}
	if u.PromptTokensDetails[1].Modality != "IMAGE" || u.PromptTokensDetails[1].TokenCount != 200 {
		t.Errorf("PromptTokensDetails[1]: got %+v", u.PromptTokensDetails[1])
	}

	mb := geminiModalityBreakdown(&u)
	if got := mb["text"]; got != 1000 {
		t.Errorf("text combined: got %d want 1000", got)
	}
	if got := mb["image"]; got != 200 {
		t.Errorf("image combined: got %d want 200", got)
	}
}
