package openai

import (
	"encoding/json"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/pricing"
)

// TestOpenaiModalityBreakdown_AudioOnly verifies that prompt audio tokens are
// surfaced under the "audio" key, matching the cross-provider lowercase
// convention on events.Usage.ModalityBreakdown.
func TestOpenaiModalityBreakdown_AudioOnly(t *testing.T) {
	u := apiUsage{
		PromptTokens:     1000,
		CompletionTokens: 200,
	}
	u.PromptTokensDetails.AudioTokens = 320
	u.CompletionTokensDetails.AudioTokens = 80

	mb := openaiModalityBreakdown(u)
	if got, want := mb["audio"], 400; got != want {
		t.Errorf("audio modality tokens: got %d want %d", got, want)
	}
	if got := len(mb); got != 1 {
		t.Errorf("expected single modality entry; got %d (%v)", got, mb)
	}
}

// TestOpenaiModalityBreakdown_NoAudio verifies that text-only requests yield
// nil so callers can leave events.Usage.ModalityBreakdown unset rather than
// emitting an empty map.
func TestOpenaiModalityBreakdown_NoAudio(t *testing.T) {
	u := apiUsage{
		PromptTokens:     500,
		CompletionTokens: 200,
	}
	if mb := openaiModalityBreakdown(u); mb != nil {
		t.Errorf("expected nil ModalityBreakdown for text-only request; got %v", mb)
	}
}

// TestConvertAPIResponse_PopulatesModalityBreakdown verifies the wire path:
// when the OpenAI usage payload reports audio tokens, the resulting
// events.Usage carries a ModalityBreakdown map ready for cost-attribution
// consumers.
func TestConvertAPIResponse_PopulatesModalityBreakdown(t *testing.T) {
	tbl := pricing.DefaultsFor(pricing.ProviderOpenAI)
	p := &Plugin{pricing: tbl}

	raw := []byte(`{
		"prompt_tokens": 1500,
		"completion_tokens": 300,
		"total_tokens": 1800,
		"prompt_tokens_details": {
			"cached_tokens": 0,
			"audio_tokens": 250
		},
		"completion_tokens_details": {
			"reasoning_tokens": 0,
			"audio_tokens": 100
		}
	}`)
	var u apiUsage
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatalf("unmarshal apiUsage: %v", err)
	}

	apiResp := apiResponse{ID: "chatcmpl-test", Model: "gpt-4o-audio-preview", Usage: u}
	resp := p.convertAPIResponse(apiResp)

	if got, want := resp.Usage.ModalityBreakdown["audio"], 350; got != want {
		t.Errorf("Usage.ModalityBreakdown[audio]: got %d want %d", got, want)
	}
}
