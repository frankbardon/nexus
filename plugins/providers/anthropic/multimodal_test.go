package anthropic

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

// TestBuildContentBlocks_EmptyParts confirms that messages without Parts fall
// through to the legacy string-content path (caller checks for nil to decide).
func TestBuildContentBlocks_EmptyParts(t *testing.T) {
	blocks, err := buildContentBlocks(events.Message{Role: "user", Content: "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blocks != nil {
		t.Fatalf("expected nil blocks for empty Parts, got %v", blocks)
	}
}

// TestBuildContentBlocks_TextPart covers a single text Part with no Content
// field, ensuring it serializes as a `text` block.
func TestBuildContentBlocks_TextPart(t *testing.T) {
	msg := events.Message{
		Role:  "user",
		Parts: []events.MessagePart{{Type: "text", Text: "describe this"}},
	}
	blocks, err := buildContentBlocks(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0]["type"] != "text" || blocks[0]["text"] != "describe this" {
		t.Fatalf("unexpected block: %v", blocks[0])
	}
}

// TestBuildContentBlocks_ImageInline encodes a small image inline and verifies
// the base64 source shape (data + media_type).
func TestBuildContentBlocks_ImageInline(t *testing.T) {
	raw := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A} // PNG header
	msg := events.Message{
		Role: "user",
		Parts: []events.MessagePart{
			{Type: "image", MimeType: "image/png", Data: raw},
		},
	}
	blocks, err := buildContentBlocks(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0]["type"] != "image" {
		t.Fatalf("expected image block, got %v", blocks[0]["type"])
	}
	source, ok := blocks[0]["source"].(map[string]any)
	if !ok {
		t.Fatalf("expected source map, got %T", blocks[0]["source"])
	}
	if source["type"] != "base64" {
		t.Fatalf("expected base64 source type, got %v", source["type"])
	}
	if source["media_type"] != "image/png" {
		t.Fatalf("expected image/png media_type, got %v", source["media_type"])
	}
	expected := base64.StdEncoding.EncodeToString(raw)
	if source["data"] != expected {
		t.Fatalf("data mismatch: want %q, got %v", expected, source["data"])
	}
}

// TestBuildContentBlocks_ImageURI verifies URI sources are passed through
// unchanged (no inline cap applies, MimeType not strictly required by Anthropic).
func TestBuildContentBlocks_ImageURI(t *testing.T) {
	msg := events.Message{
		Role: "user",
		Parts: []events.MessagePart{
			{Type: "image", URI: "https://example.com/cat.png"},
		},
	}
	blocks, err := buildContentBlocks(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	source := blocks[0]["source"].(map[string]any)
	if source["type"] != "url" {
		t.Fatalf("expected url source type, got %v", source["type"])
	}
	if source["url"] != "https://example.com/cat.png" {
		t.Fatalf("url mismatch: %v", source["url"])
	}
}

// TestBuildContentBlocks_ImageOversize confirms that an image >5MB without a
// URI errors out (caller is expected to upload via Files API).
func TestBuildContentBlocks_ImageOversize(t *testing.T) {
	huge := make([]byte, inlineImageLimit+1)
	msg := events.Message{
		Role: "user",
		Parts: []events.MessagePart{
			{Type: "image", MimeType: "image/png", Data: huge},
		},
	}
	_, err := buildContentBlocks(msg)
	if err == nil {
		t.Fatal("expected error for oversize image")
	}
	if !strings.Contains(err.Error(), "exceeds inline limit") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// TestBuildContentBlocks_ImageMissingMimeType ensures inline data without
// MimeType is rejected (Anthropic's base64 source requires media_type).
func TestBuildContentBlocks_ImageMissingMimeType(t *testing.T) {
	msg := events.Message{
		Role: "user",
		Parts: []events.MessagePart{
			{Type: "image", Data: []byte{0x01, 0x02}},
		},
	}
	_, err := buildContentBlocks(msg)
	if err == nil {
		t.Fatal("expected error for missing mime_type")
	}
	if !strings.Contains(err.Error(), "mime_type") {
		t.Fatalf("expected mime_type error, got: %v", err)
	}
}

// TestBuildContentBlocks_PDFInline covers small PDF data → document base64
// block with application/pdf media_type.
func TestBuildContentBlocks_PDFInline(t *testing.T) {
	raw := []byte("%PDF-1.4 fake pdf bytes")
	msg := events.Message{
		Role: "user",
		Parts: []events.MessagePart{
			{Type: "file", MimeType: "application/pdf", Data: raw},
		},
	}
	blocks, err := buildContentBlocks(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blocks[0]["type"] != "document" {
		t.Fatalf("expected document block, got %v", blocks[0]["type"])
	}
	source := blocks[0]["source"].(map[string]any)
	if source["type"] != "base64" {
		t.Fatalf("expected base64 source, got %v", source["type"])
	}
	if source["media_type"] != "application/pdf" {
		t.Fatalf("expected application/pdf, got %v", source["media_type"])
	}
	if source["data"] != base64.StdEncoding.EncodeToString(raw) {
		t.Fatal("data mismatch")
	}
}

// TestBuildContentBlocks_PDFURI verifies URI document blocks.
func TestBuildContentBlocks_PDFURI(t *testing.T) {
	msg := events.Message{
		Role: "user",
		Parts: []events.MessagePart{
			{Type: "file", URI: "https://example.com/report.pdf"},
		},
	}
	blocks, err := buildContentBlocks(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	source := blocks[0]["source"].(map[string]any)
	if source["type"] != "url" || source["url"] != "https://example.com/report.pdf" {
		t.Fatalf("unexpected source: %v", source)
	}
}

// TestBuildContentBlocks_PDFOversize confirms the 32MB cap is enforced for
// documents.
func TestBuildContentBlocks_PDFOversize(t *testing.T) {
	huge := make([]byte, inlinePDFLimit+1)
	msg := events.Message{
		Role: "user",
		Parts: []events.MessagePart{
			{Type: "file", MimeType: "application/pdf", Data: huge},
		},
	}
	_, err := buildContentBlocks(msg)
	if err == nil {
		t.Fatal("expected error for oversize PDF")
	}
	if !strings.Contains(err.Error(), "exceeds inline limit") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// TestBuildContentBlocks_AudioRejected ensures audio parts produce a clear
// error (Anthropic's API does not accept audio).
func TestBuildContentBlocks_AudioRejected(t *testing.T) {
	msg := events.Message{
		Role: "user",
		Parts: []events.MessagePart{
			{Type: "audio", MimeType: "audio/wav", Data: []byte{0x00}},
		},
	}
	_, err := buildContentBlocks(msg)
	if err == nil {
		t.Fatal("expected error for audio part")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected 'not supported' error, got: %v", err)
	}
}

// TestBuildContentBlocks_VideoRejected mirrors the audio test for video.
func TestBuildContentBlocks_VideoRejected(t *testing.T) {
	msg := events.Message{
		Role: "user",
		Parts: []events.MessagePart{
			{Type: "video", MimeType: "video/mp4", Data: []byte{0x00}},
		},
	}
	_, err := buildContentBlocks(msg)
	if err == nil {
		t.Fatal("expected error for video part")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected 'not supported' error, got: %v", err)
	}
}

// TestBuildContentBlocks_UnknownTypeRejected covers the default switch arm.
func TestBuildContentBlocks_UnknownTypeRejected(t *testing.T) {
	msg := events.Message{
		Role: "user",
		Parts: []events.MessagePart{
			{Type: "hologram"},
		},
	}
	_, err := buildContentBlocks(msg)
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
	if !strings.Contains(err.Error(), "unsupported part type") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// TestConvertMessage_UserWithImagePart round-trips a user message carrying
// both a text Content and an image Part through convertMessage. The output
// must be a blocks-array with text first, image second.
func TestConvertMessage_UserWithImagePart(t *testing.T) {
	p := &Plugin{logger: silentLogger()}
	msg := events.Message{
		Role:    "user",
		Content: "what is in this image?",
		Parts: []events.MessagePart{
			{Type: "image", MimeType: "image/png", Data: []byte{0x89, 0x50, 0x4E, 0x47}},
		},
	}
	apiMsg := p.convertMessage(msg)
	if apiMsg["role"] != "user" {
		t.Fatalf("expected role=user, got %v", apiMsg["role"])
	}
	blocks, ok := apiMsg["content"].([]map[string]any)
	if !ok {
		t.Fatalf("expected []map[string]any content, got %T", apiMsg["content"])
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0]["type"] != "text" || blocks[0]["text"] != "what is in this image?" {
		t.Fatalf("expected leading text block, got %v", blocks[0])
	}
	if blocks[1]["type"] != "image" {
		t.Fatalf("expected image block at index 1, got %v", blocks[1])
	}
}

// TestConvertMessage_ToolResultWithImage covers a tool result message that
// carries a screenshot Part — it should land inside the tool_result.content
// array as a content-blocks slice.
func TestConvertMessage_ToolResultWithImage(t *testing.T) {
	p := &Plugin{logger: silentLogger()}
	msg := events.Message{
		Role:       "tool",
		ToolCallID: "toolu_123",
		Content:    "screenshot attached",
		Parts: []events.MessagePart{
			{Type: "image", MimeType: "image/png", Data: []byte{0x01, 0x02, 0x03}},
		},
	}
	apiMsg := p.convertMessage(msg)
	if apiMsg["role"] != "user" {
		t.Fatalf("expected role=user (Anthropic packs tool results as user), got %v", apiMsg["role"])
	}
	outer, ok := apiMsg["content"].([]map[string]any)
	if !ok || len(outer) != 1 {
		t.Fatalf("expected single tool_result envelope, got %v", apiMsg["content"])
	}
	tr := outer[0]
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "toolu_123" {
		t.Fatalf("unexpected tool_result envelope: %v", tr)
	}
	inner, ok := tr["content"].([]map[string]any)
	if !ok {
		t.Fatalf("expected tool_result.content to be blocks array, got %T", tr["content"])
	}
	if len(inner) != 2 {
		t.Fatalf("expected 2 inner blocks (text + image), got %d", len(inner))
	}
	if inner[0]["type"] != "text" {
		t.Fatalf("expected leading text block, got %v", inner[0])
	}
	if inner[1]["type"] != "image" {
		t.Fatalf("expected trailing image block, got %v", inner[1])
	}
}
