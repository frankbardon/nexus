package openai

// openaiModalityBreakdown extracts a per-modality token map from an apiUsage.
// OpenAI exposes modality detail through prompt_tokens_details.audio_tokens
// and completion_tokens_details.audio_tokens (both Realtime audio paths and
// gpt-4o-audio-preview). Only audio is broken out today; image tokens roll
// into prompt_tokens with no separate field.
//
// Returns nil when no non-zero modality counts are present so callers can
// leave events.Usage.ModalityBreakdown as nil instead of an empty allocated
// map.
func openaiModalityBreakdown(u apiUsage) map[string]int {
	audio := u.PromptTokensDetails.AudioTokens + u.CompletionTokensDetails.AudioTokens
	if audio == 0 {
		return nil
	}
	return map[string]int{"audio": audio}
}
