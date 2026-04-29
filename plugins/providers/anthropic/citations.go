package anthropic

import "github.com/frankbardon/nexus/pkg/events"

// citationsConfig controls Anthropic's native citations feature. When Enabled,
// outgoing document content blocks gain `citations: {enabled: true}` and the
// API returns per-text-block citation arrays that the provider surfaces on
// LLMResponse.Citations.
//
//	citations:
//	  enabled: false  # opt-in; only affects requests that include document blocks
type citationsConfig struct {
	Enabled bool
}

// parseCitationsConfig pulls citationsConfig out of the plugin's raw config map.
// Absent block returns a zero-value config (disabled).
func parseCitationsConfig(cfg map[string]any) citationsConfig {
	raw, ok := cfg["citations"].(map[string]any)
	if !ok {
		return citationsConfig{}
	}
	cc := citationsConfig{}
	if v, ok := raw["enabled"].(bool); ok {
		cc.Enabled = v
	}
	return cc
}

// apiCitation matches the Anthropic Messages API citation response shape.
// Used inside text content blocks (sync) and inside `citations_delta` deltas
// (streaming). All location-specific fields are optional; only the subset
// matching `type` is populated.
type apiCitation struct {
	Type            string `json:"type"`
	CitedText       string `json:"cited_text,omitempty"`
	DocumentIndex   int    `json:"document_index"`
	DocumentTitle   string `json:"document_title,omitempty"`
	StartCharIndex  int    `json:"start_char_index,omitempty"`
	EndCharIndex    int    `json:"end_char_index,omitempty"`
	StartPageNumber int    `json:"start_page_number,omitempty"`
	EndPageNumber   int    `json:"end_page_number,omitempty"`
	StartBlockIndex int    `json:"start_block_index,omitempty"`
	EndBlockIndex   int    `json:"end_block_index,omitempty"`
}

// toEvent converts the API-shape citation into the public events.Citation
// surfaced on LLMResponse.
func (c apiCitation) toEvent() events.Citation {
	return events.Citation{
		Type:            c.Type,
		CitedText:       c.CitedText,
		DocumentIndex:   c.DocumentIndex,
		DocumentTitle:   c.DocumentTitle,
		StartCharIndex:  c.StartCharIndex,
		EndCharIndex:    c.EndCharIndex,
		StartPageNumber: c.StartPageNumber,
		EndPageNumber:   c.EndPageNumber,
		StartBlockIndex: c.StartBlockIndex,
		EndBlockIndex:   c.EndBlockIndex,
	}
}
