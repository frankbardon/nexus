package gemini

import "strings"

// geminiModalityBreakdown extracts a per-modality token map from Gemini's
// usageMetadata, summing prompt + candidate + cache modality detail entries
// per modality. Modality keys returned are lowercase ("text", "image",
// "audio", "video", "document") to match the cross-provider convention on
// events.Usage.ModalityBreakdown.
//
// Returns nil when the response carries no per-modality detail (e.g. a
// model that doesn't emit promptTokensDetails) so callers leave
// events.Usage.ModalityBreakdown nil instead of an empty allocated map.
func geminiModalityBreakdown(u *apiUsageMetadata) map[string]int {
	if u == nil {
		return nil
	}
	if len(u.PromptTokensDetails) == 0 && len(u.CandidatesTokensDetails) == 0 && len(u.CacheTokensDetails) == 0 {
		return nil
	}
	out := map[string]int{}
	add := func(details []apiModalityDetail) {
		for _, d := range details {
			if d.TokenCount == 0 {
				continue
			}
			key := strings.ToLower(d.Modality)
			if key == "" {
				continue
			}
			out[key] += d.TokenCount
		}
	}
	add(u.PromptTokensDetails)
	add(u.CandidatesTokensDetails)
	add(u.CacheTokensDetails)
	if len(out) == 0 {
		return nil
	}
	return out
}
