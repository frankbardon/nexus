package openai

import (
	"bytes"
	"encoding/base64"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

func newTestPlugin(cfg multimodalConfig) *Plugin {
	return &Plugin{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		multimodal: cfg,
	}
}

func TestParseMultimodalConfig_Defaults(t *testing.T) {
	mc := parseMultimodalConfig(nil)
	if mc.DefaultImageDetail != "auto" {
		t.Errorf("expected default detail 'auto', got %q", mc.DefaultImageDetail)
	}
	mc = parseMultimodalConfig(map[string]any{})
	if mc.DefaultImageDetail != "auto" {
		t.Errorf("expected default detail 'auto', got %q", mc.DefaultImageDetail)
	}
}

func TestParseMultimodalConfig_Explicit(t *testing.T) {
	for _, level := range []string{"auto", "low", "high"} {
		cfg := map[string]any{
			"multimodal": map[string]any{
				"default_image_detail": level,
			},
		}
		mc := parseMultimodalConfig(cfg)
		if mc.DefaultImageDetail != level {
			t.Errorf("expected detail %q, got %q", level, mc.DefaultImageDetail)
		}
	}
}

func TestParseMultimodalConfig_InvalidFallback(t *testing.T) {
	cfg := map[string]any{
		"multimodal": map[string]any{
			"default_image_detail": "ultra",
		},
	}
	mc := parseMultimodalConfig(cfg)
	if mc.DefaultImageDetail != "auto" {
		t.Errorf("expected fallback to 'auto' on invalid input, got %q", mc.DefaultImageDetail)
	}
}

func TestBuildContentParts_EmptyParts(t *testing.T) {
	parts, err := buildContentParts(events.Message{Content: "hello"}, multimodalConfig{DefaultImageDetail: "auto"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parts != nil {
		t.Errorf("expected nil parts when Parts is empty, got %v", parts)
	}
}

func TestBuildContentParts_TextPart(t *testing.T) {
	msg := events.Message{
		Parts: []events.MessagePart{{Type: "text", Text: "first"}},
	}
	parts, err := buildContentParts(msg, multimodalConfig{DefaultImageDetail: "auto"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	if parts[0]["type"] != "text" || parts[0]["text"] != "first" {
		t.Errorf("unexpected text block: %+v", parts[0])
	}
}

func TestBuildContentParts_TextPart_WithContentPrefix(t *testing.T) {
	msg := events.Message{
		Content: "leading",
		Parts:   []events.MessagePart{{Type: "text", Text: "follow-up"}},
	}
	parts, err := buildContentParts(msg, multimodalConfig{DefaultImageDetail: "auto"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	if parts[0]["text"] != "leading" || parts[1]["text"] != "follow-up" {
		t.Errorf("unexpected order: %+v", parts)
	}
}

func TestBuildContentParts_ImageURI(t *testing.T) {
	msg := events.Message{
		Parts: []events.MessagePart{{Type: "image", URI: "https://example.com/cat.png"}},
	}
	parts, err := buildContentParts(msg, multimodalConfig{DefaultImageDetail: "high"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	block := parts[0]
	if block["type"] != "image_url" {
		t.Errorf("expected type=image_url, got %v", block["type"])
	}
	inner, ok := block["image_url"].(map[string]any)
	if !ok {
		t.Fatalf("expected image_url object, got %T", block["image_url"])
	}
	if inner["url"] != "https://example.com/cat.png" {
		t.Errorf("expected URI passthrough, got %v", inner["url"])
	}
	if inner["detail"] != "high" {
		t.Errorf("expected detail=high, got %v", inner["detail"])
	}
}

func TestBuildContentParts_ImageDataInline(t *testing.T) {
	data := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d}
	msg := events.Message{
		Parts: []events.MessagePart{{Type: "image", Data: data, MimeType: "image/png"}},
	}
	parts, err := buildContentParts(msg, multimodalConfig{DefaultImageDetail: "auto"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	inner := parts[0]["image_url"].(map[string]any)
	url := inner["url"].(string)
	prefix := "data:image/png;base64,"
	if !strings.HasPrefix(url, prefix) {
		t.Fatalf("expected data URL prefix %q, got %q", prefix, url)
	}
	enc := strings.TrimPrefix(url, prefix)
	decoded, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("failed to decode base64: %v", err)
	}
	if !bytes.Equal(decoded, data) {
		t.Errorf("round-trip mismatch")
	}
}

func TestBuildContentParts_ImageDataTooLarge(t *testing.T) {
	data := bytes.Repeat([]byte{0x01}, inlineImageLimit+1)
	msg := events.Message{
		Parts: []events.MessagePart{{Type: "image", Data: data, MimeType: "image/png"}},
	}
	_, err := buildContentParts(msg, multimodalConfig{DefaultImageDetail: "auto"})
	if err == nil {
		t.Fatal("expected error for oversized image, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestBuildContentParts_ImageDataMissingMime(t *testing.T) {
	msg := events.Message{
		Parts: []events.MessagePart{{Type: "image", Data: []byte{1, 2, 3}}},
	}
	_, err := buildContentParts(msg, multimodalConfig{DefaultImageDetail: "auto"})
	if err == nil {
		t.Fatal("expected error for missing mime_type, got nil")
	}
	if !strings.Contains(err.Error(), "mime_type") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestBuildContentParts_ImageFileIDRejected(t *testing.T) {
	msg := events.Message{
		Parts: []events.MessagePart{{Type: "image", FileID: "file-abc"}},
	}
	_, err := buildContentParts(msg, multimodalConfig{DefaultImageDetail: "auto"})
	if err == nil {
		t.Fatal("expected error for image with FileID, got nil")
	}
	if !strings.Contains(err.Error(), "file_id") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestBuildContentParts_AudioWAV(t *testing.T) {
	data := []byte("RIFFwave-bytes")
	msg := events.Message{
		Parts: []events.MessagePart{{Type: "audio", Data: data, MimeType: "audio/wav"}},
	}
	parts, err := buildContentParts(msg, multimodalConfig{DefaultImageDetail: "auto"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	block := parts[0]
	if block["type"] != "input_audio" {
		t.Errorf("expected type=input_audio, got %v", block["type"])
	}
	inner := block["input_audio"].(map[string]any)
	if inner["format"] != "wav" {
		t.Errorf("expected format=wav, got %v", inner["format"])
	}
	enc := inner["data"].(string)
	decoded, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("failed to decode base64: %v", err)
	}
	if !bytes.Equal(decoded, data) {
		t.Errorf("audio data round-trip mismatch")
	}
}

func TestBuildContentParts_AudioMP3(t *testing.T) {
	msg := events.Message{
		Parts: []events.MessagePart{{Type: "audio", Data: []byte{0xFF, 0xFB}, MimeType: "audio/mpeg"}},
	}
	parts, err := buildContentParts(msg, multimodalConfig{DefaultImageDetail: "auto"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	inner := parts[0]["input_audio"].(map[string]any)
	if inner["format"] != "mp3" {
		t.Errorf("expected format=mp3, got %v", inner["format"])
	}
}

func TestBuildContentParts_AudioUnsupportedMime(t *testing.T) {
	msg := events.Message{
		Parts: []events.MessagePart{{Type: "audio", Data: []byte{0}, MimeType: "audio/ogg"}},
	}
	_, err := buildContentParts(msg, multimodalConfig{DefaultImageDetail: "auto"})
	if err == nil {
		t.Fatal("expected error for unsupported audio mime, got nil")
	}
}

func TestBuildContentParts_AudioMissingData(t *testing.T) {
	msg := events.Message{
		Parts: []events.MessagePart{{Type: "audio", MimeType: "audio/wav"}},
	}
	_, err := buildContentParts(msg, multimodalConfig{DefaultImageDetail: "auto"})
	if err == nil {
		t.Fatal("expected error for missing audio data, got nil")
	}
}

func TestBuildContentParts_AudioMissingMime(t *testing.T) {
	msg := events.Message{
		Parts: []events.MessagePart{{Type: "audio", Data: []byte{1}}},
	}
	_, err := buildContentParts(msg, multimodalConfig{DefaultImageDetail: "auto"})
	if err == nil {
		t.Fatal("expected error for missing audio mime, got nil")
	}
}

func TestBuildContentParts_FileWithFileID(t *testing.T) {
	msg := events.Message{
		Parts: []events.MessagePart{{Type: "file", FileID: "file-abc123"}},
	}
	parts, err := buildContentParts(msg, multimodalConfig{DefaultImageDetail: "auto"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	block := parts[0]
	if block["type"] != "file" {
		t.Errorf("expected type=file, got %v", block["type"])
	}
	inner := block["file"].(map[string]any)
	if inner["file_id"] != "file-abc123" {
		t.Errorf("expected file_id passthrough, got %v", inner["file_id"])
	}
	if _, ok := inner["file_data"]; ok {
		t.Errorf("file_data should be absent when FileID is set")
	}
}

func TestBuildContentParts_FileWithDataInline(t *testing.T) {
	data := []byte("PDF-bytes")
	msg := events.Message{
		Parts: []events.MessagePart{{Type: "file", Data: data, MimeType: "application/pdf"}},
	}
	parts, err := buildContentParts(msg, multimodalConfig{DefaultImageDetail: "auto"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	inner := parts[0]["file"].(map[string]any)
	if inner["filename"] != "file" {
		t.Errorf("expected filename placeholder 'file', got %v", inner["filename"])
	}
	fd, ok := inner["file_data"].(string)
	if !ok {
		t.Fatalf("expected file_data string, got %T", inner["file_data"])
	}
	prefix := "data:application/pdf;base64,"
	if !strings.HasPrefix(fd, prefix) {
		t.Errorf("expected data URL prefix %q, got %q", prefix, fd)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(fd, prefix))
	if err != nil {
		t.Fatalf("failed to decode base64: %v", err)
	}
	if !bytes.Equal(decoded, data) {
		t.Errorf("file data round-trip mismatch")
	}
}

func TestBuildContentParts_FileWithDataMissingMime(t *testing.T) {
	msg := events.Message{
		Parts: []events.MessagePart{{Type: "file", Data: []byte{1, 2}}},
	}
	_, err := buildContentParts(msg, multimodalConfig{DefaultImageDetail: "auto"})
	if err == nil {
		t.Fatal("expected error for missing mime_type on inline file, got nil")
	}
}

func TestBuildContentParts_FileWithURIRejected(t *testing.T) {
	msg := events.Message{
		Parts: []events.MessagePart{{Type: "file", URI: "https://example.com/x.pdf"}},
	}
	_, err := buildContentParts(msg, multimodalConfig{DefaultImageDetail: "auto"})
	if err == nil {
		t.Fatal("expected error for file URI, got nil")
	}
}

func TestBuildContentParts_FileEmpty(t *testing.T) {
	msg := events.Message{
		Parts: []events.MessagePart{{Type: "file"}},
	}
	_, err := buildContentParts(msg, multimodalConfig{DefaultImageDetail: "auto"})
	if err == nil {
		t.Fatal("expected error for empty file part, got nil")
	}
}

func TestBuildContentParts_VideoRejected(t *testing.T) {
	msg := events.Message{
		Parts: []events.MessagePart{{Type: "video", URI: "https://example.com/v.mp4", MimeType: "video/mp4"}},
	}
	_, err := buildContentParts(msg, multimodalConfig{DefaultImageDetail: "auto"})
	if err == nil {
		t.Fatal("expected error for video part, got nil")
	}
}

func TestBuildContentParts_UnknownTypeRejected(t *testing.T) {
	msg := events.Message{
		Parts: []events.MessagePart{{Type: "hologram"}},
	}
	_, err := buildContentParts(msg, multimodalConfig{DefaultImageDetail: "auto"})
	if err == nil {
		t.Fatal("expected error for unknown type, got nil")
	}
}

func TestConvertMessage_UserWithImagePart(t *testing.T) {
	p := newTestPlugin(multimodalConfig{DefaultImageDetail: "auto"})
	msg := events.Message{
		Role:    "user",
		Content: "what's in this picture?",
		Parts: []events.MessagePart{
			{Type: "image", URI: "https://example.com/cat.png"},
		},
	}
	apiMsg := p.convertMessage(msg)
	if apiMsg["role"] != "user" {
		t.Errorf("expected role=user, got %v", apiMsg["role"])
	}
	content, ok := apiMsg["content"].([]map[string]any)
	if !ok {
		t.Fatalf("expected content to be []map[string]any, got %T", apiMsg["content"])
	}
	if len(content) != 2 {
		t.Fatalf("expected 2 content blocks (text + image), got %d", len(content))
	}
	if content[0]["type"] != "text" || content[0]["text"] != "what's in this picture?" {
		t.Errorf("expected leading text block, got %+v", content[0])
	}
	if content[1]["type"] != "image_url" {
		t.Errorf("expected image_url block, got %+v", content[1])
	}
}

func TestConvertMessage_UserNoParts_FallbackString(t *testing.T) {
	p := newTestPlugin(multimodalConfig{DefaultImageDetail: "auto"})
	msg := events.Message{Role: "user", Content: "hi"}
	apiMsg := p.convertMessage(msg)
	if apiMsg["content"] != "hi" {
		t.Errorf("expected bare string content, got %v", apiMsg["content"])
	}
}

func TestConvertMessage_ToolWithImagePart(t *testing.T) {
	p := newTestPlugin(multimodalConfig{DefaultImageDetail: "auto"})
	msg := events.Message{
		Role:       "tool",
		ToolCallID: "call_123",
		Content:    "screenshot captured",
		Parts: []events.MessagePart{
			{Type: "image", Data: []byte{0x89, 0x50}, MimeType: "image/png"},
		},
	}
	apiMsg := p.convertMessage(msg)
	if apiMsg["role"] != "tool" {
		t.Errorf("expected role=tool, got %v", apiMsg["role"])
	}
	if apiMsg["tool_call_id"] != "call_123" {
		t.Errorf("expected tool_call_id passthrough, got %v", apiMsg["tool_call_id"])
	}
	content, ok := apiMsg["content"].([]map[string]any)
	if !ok {
		t.Fatalf("expected content array, got %T", apiMsg["content"])
	}
	if len(content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(content))
	}
	if content[1]["type"] != "image_url" {
		t.Errorf("expected image_url in second block, got %+v", content[1])
	}
}

func TestConvertMessage_ToolNoParts_FallbackString(t *testing.T) {
	p := newTestPlugin(multimodalConfig{DefaultImageDetail: "auto"})
	msg := events.Message{Role: "tool", ToolCallID: "call_x", Content: "ok"}
	apiMsg := p.convertMessage(msg)
	if apiMsg["content"] != "ok" {
		t.Errorf("expected bare string content, got %v", apiMsg["content"])
	}
}

func TestConvertMessage_AssistantWithParts(t *testing.T) {
	p := newTestPlugin(multimodalConfig{DefaultImageDetail: "auto"})
	msg := events.Message{
		Role:    "assistant",
		Content: "here you go",
		Parts: []events.MessagePart{
			{Type: "text", Text: "extra context"},
		},
	}
	apiMsg := p.convertMessage(msg)
	content, ok := apiMsg["content"].([]map[string]any)
	if !ok {
		t.Fatalf("expected array content, got %T", apiMsg["content"])
	}
	if len(content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(content))
	}
}

func TestConvertMessage_UserBadPart_FallbackToText(t *testing.T) {
	p := newTestPlugin(multimodalConfig{DefaultImageDetail: "auto"})
	msg := events.Message{
		Role:    "user",
		Content: "fallback text",
		Parts: []events.MessagePart{
			{Type: "video", URI: "https://example.com/x.mp4", MimeType: "video/mp4"},
		},
	}
	apiMsg := p.convertMessage(msg)
	if apiMsg["content"] != "fallback text" {
		t.Errorf("expected fallback bare-string content on conversion error, got %v", apiMsg["content"])
	}
}
