package anthropic

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/pricing"
	"github.com/frankbardon/nexus/pkg/events"
)

// --- parseCitationsConfig ---------------------------------------------------

// TestParseCitationsConfig_Absent verifies an absent block produces a
// zero-value (disabled) config.
func TestParseCitationsConfig_Absent(t *testing.T) {
	cc := parseCitationsConfig(map[string]any{})
	if cc.Enabled {
		t.Fatalf("expected Enabled=false on absent config, got %+v", cc)
	}
}

// TestParseCitationsConfig_Disabled verifies an explicit `enabled: false`
// resolves to disabled (default).
func TestParseCitationsConfig_Disabled(t *testing.T) {
	cc := parseCitationsConfig(map[string]any{
		"citations": map[string]any{"enabled": false},
	})
	if cc.Enabled {
		t.Fatalf("expected Enabled=false, got %+v", cc)
	}
}

// TestParseCitationsConfig_Enabled covers the opt-in path.
func TestParseCitationsConfig_Enabled(t *testing.T) {
	cc := parseCitationsConfig(map[string]any{
		"citations": map[string]any{"enabled": true},
	})
	if !cc.Enabled {
		t.Fatalf("expected Enabled=true, got %+v", cc)
	}
}

// --- buildDocumentBlock with citations -------------------------------------

// TestBuildDocumentBlock_CitationsEnabled verifies the citations marker is
// appended to outgoing document blocks when the flag is on.
func TestBuildDocumentBlock_CitationsEnabled(t *testing.T) {
	part := events.MessagePart{
		Type: "file", MimeType: "text/plain", Data: []byte("policy v3 ..."),
	}
	block, err := buildDocumentBlock(part, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if block["type"] != "document" {
		t.Fatalf("expected document block, got %v", block["type"])
	}
	cit, ok := block["citations"].(map[string]any)
	if !ok {
		t.Fatalf("expected citations map, got %T (%v)", block["citations"], block["citations"])
	}
	if cit["enabled"] != true {
		t.Fatalf("expected citations.enabled=true, got %v", cit["enabled"])
	}
}

// TestBuildDocumentBlock_CitationsDisabled confirms the citations marker is
// absent when the flag is off (default).
func TestBuildDocumentBlock_CitationsDisabled(t *testing.T) {
	part := events.MessagePart{
		Type: "file", MimeType: "text/plain", Data: []byte("policy v3 ..."),
	}
	block, err := buildDocumentBlock(part, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := block["citations"]; ok {
		t.Fatalf("expected no citations field when disabled, got %v", block["citations"])
	}
}

// TestBuildContentBlocks_CitationsThreaded verifies the flag flows through
// buildContentBlocks to per-document marker emission.
func TestBuildContentBlocks_CitationsThreaded(t *testing.T) {
	msg := events.Message{
		Role:    "user",
		Content: "Cite policy v3.",
		Parts: []events.MessagePart{
			{Type: "file", MimeType: "text/plain", Data: []byte("doc bytes")},
		},
	}
	blocks, err := buildContentBlocks(msg, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (text + document), got %d", len(blocks))
	}
	doc := blocks[1]
	if doc["type"] != "document" {
		t.Fatalf("expected document block at [1], got %v", doc["type"])
	}
	cit, ok := doc["citations"].(map[string]any)
	if !ok || cit["enabled"] != true {
		t.Fatalf("expected citations marker on document, got %v", doc["citations"])
	}
}

// --- sync response citation extraction -------------------------------------

// TestConvertAPIResponse_SingleCitation feeds a fixture text block with a
// char_location citation and asserts LLMResponse.Citations carries the entry
// with all populated fields preserved.
func TestConvertAPIResponse_SingleCitation(t *testing.T) {
	p := &Plugin{logger: silentLogger(), pricing: pricing.DefaultsFor(pricing.ProviderAnthropic)}
	api := apiResponse{
		ID:    "msg_cite",
		Model: "claude-sonnet-4-5-20250514",
		Content: []apiContentBlock{
			{
				Type: "text",
				Text: "The policy mandates retention.",
				Citations: []apiCitation{
					{
						Type:           "char_location",
						CitedText:      "retention is required",
						DocumentIndex:  0,
						DocumentTitle:  "Policy v3",
						StartCharIndex: 12,
						EndCharIndex:   48,
					},
				},
			},
		},
		StopReason: "end_turn",
	}
	resp := p.convertAPIResponse(api)
	if len(resp.Citations) != 1 {
		t.Fatalf("expected 1 citation, got %d", len(resp.Citations))
	}
	c := resp.Citations[0]
	if c.Type != "char_location" {
		t.Errorf("Type: got %q, want char_location", c.Type)
	}
	if c.CitedText != "retention is required" {
		t.Errorf("CitedText: got %q", c.CitedText)
	}
	if c.DocumentIndex != 0 {
		t.Errorf("DocumentIndex: got %d", c.DocumentIndex)
	}
	if c.DocumentTitle != "Policy v3" {
		t.Errorf("DocumentTitle: got %q", c.DocumentTitle)
	}
	if c.StartCharIndex != 12 || c.EndCharIndex != 48 {
		t.Errorf("char range mismatch: got [%d, %d]", c.StartCharIndex, c.EndCharIndex)
	}
	if resp.Content != "The policy mandates retention." {
		t.Errorf("Content: got %q", resp.Content)
	}
}

// TestConvertAPIResponse_MultipleCitations verifies citations from multiple
// text blocks accumulate in the order produced by the API.
func TestConvertAPIResponse_MultipleCitations(t *testing.T) {
	p := &Plugin{logger: silentLogger(), pricing: pricing.DefaultsFor(pricing.ProviderAnthropic)}
	api := apiResponse{
		ID:    "msg_multi",
		Model: "claude-sonnet-4-5-20250514",
		Content: []apiContentBlock{
			{
				Type: "text",
				Text: "First sentence. ",
				Citations: []apiCitation{
					{Type: "char_location", DocumentIndex: 0, DocumentTitle: "A", StartCharIndex: 0, EndCharIndex: 5},
				},
			},
			{
				Type: "text",
				Text: "Second sentence.",
				Citations: []apiCitation{
					{Type: "page_location", DocumentIndex: 1, DocumentTitle: "B", StartPageNumber: 7, EndPageNumber: 8},
					{Type: "content_block_location", DocumentIndex: 2, DocumentTitle: "C", StartBlockIndex: 3, EndBlockIndex: 4},
				},
			},
		},
		StopReason: "end_turn",
	}
	resp := p.convertAPIResponse(api)
	if len(resp.Citations) != 3 {
		t.Fatalf("expected 3 citations, got %d", len(resp.Citations))
	}
	if resp.Citations[0].DocumentTitle != "A" || resp.Citations[0].Type != "char_location" {
		t.Errorf("citation[0] mismatch: %+v", resp.Citations[0])
	}
	if resp.Citations[1].Type != "page_location" || resp.Citations[1].StartPageNumber != 7 || resp.Citations[1].EndPageNumber != 8 {
		t.Errorf("citation[1] mismatch: %+v", resp.Citations[1])
	}
	if resp.Citations[2].Type != "content_block_location" || resp.Citations[2].StartBlockIndex != 3 || resp.Citations[2].EndBlockIndex != 4 {
		t.Errorf("citation[2] mismatch: %+v", resp.Citations[2])
	}
}

// TestConvertAPIResponse_NoCitations confirms responses without citations
// produce a nil Citations slice (not an empty non-nil slice).
func TestConvertAPIResponse_NoCitations(t *testing.T) {
	p := &Plugin{logger: silentLogger(), pricing: pricing.DefaultsFor(pricing.ProviderAnthropic)}
	api := apiResponse{
		ID:         "msg_plain",
		Model:      "claude-sonnet-4-5-20250514",
		Content:    []apiContentBlock{{Type: "text", Text: "hello"}},
		StopReason: "end_turn",
	}
	resp := p.convertAPIResponse(api)
	if resp.Citations != nil {
		t.Fatalf("expected nil Citations on plain response, got %+v", resp.Citations)
	}
}

// --- streaming citation extraction -----------------------------------------

// canonicalCitationsStream mirrors Anthropic's SSE format for a text block
// that emits a single char_location citation via citations_delta.
const canonicalCitationsStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_stream_cite","model":"claude-sonnet-4-5-20250514","usage":{"input_tokens":50,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The doc says X."}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"char_location","cited_text":"the doc says X","document_index":0,"document_title":"Source","start_char_index":10,"end_char_index":24}}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

// TestStream_CitationsRoundTrip feeds the canned SSE into the streaming
// state machine and asserts the final llm.response carries the citation.
func TestStream_CitationsRoundTrip(t *testing.T) {
	rec := newBusRecorder()

	p := &Plugin{
		bus:     rec.bus,
		logger:  silentLogger(),
		pricing: pricing.DefaultsFor(pricing.ProviderAnthropic),
	}

	p.handleStreamResponse(strings.NewReader(canonicalCitationsStream))

	resps := rec.byType("llm.response")
	if len(resps) != 1 {
		t.Fatalf("llm.response count: got %d, want 1", len(resps))
	}
	resp := resps[0].Payload.(events.LLMResponse)

	if resp.Content != "The doc says X." {
		t.Errorf("Content: got %q", resp.Content)
	}
	if len(resp.Citations) != 1 {
		t.Fatalf("expected 1 citation, got %d (%+v)", len(resp.Citations), resp.Citations)
	}
	c := resp.Citations[0]
	if c.Type != "char_location" {
		t.Errorf("Type: got %q, want char_location", c.Type)
	}
	if c.CitedText != "the doc says X" {
		t.Errorf("CitedText: got %q", c.CitedText)
	}
	if c.DocumentTitle != "Source" {
		t.Errorf("DocumentTitle: got %q", c.DocumentTitle)
	}
	if c.StartCharIndex != 10 || c.EndCharIndex != 24 {
		t.Errorf("char range mismatch: got [%d, %d]", c.StartCharIndex, c.EndCharIndex)
	}
}
