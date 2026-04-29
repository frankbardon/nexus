package anthropic

import (
	"encoding/base64"
	"fmt"

	"github.com/frankbardon/nexus/pkg/events"
)

// Anthropic inline payload caps. URI sources sidestep these; oversize inline
// payloads must be uploaded via the Files API (plan 04 will wire that up).
const (
	inlineImageLimit = 5 * 1024 * 1024  // 5 MB; Anthropic's documented inline cap
	inlinePDFLimit   = 32 * 1024 * 1024 // 32 MB
)

// multimodalConfig controls Anthropic-specific multimodal request behavior.
//
//	multimodal:
//	  pdf_beta: false  # default; Sonnet 3.5+ accepts PDFs natively
//
// When pdf_beta is true the request builder appends `pdfs-2024-09-25` to the
// `anthropic-beta` header so legacy models accept document blocks.
type multimodalConfig struct {
	PDFBeta bool
}

// parseMultimodalConfig pulls multimodalConfig out of the plugin's raw config
// map. Absent block returns a zero-value config.
func parseMultimodalConfig(cfg map[string]any) multimodalConfig {
	mc := multimodalConfig{}

	raw, ok := cfg["multimodal"].(map[string]any)
	if !ok {
		return mc
	}

	if v, ok := raw["pdf_beta"].(bool); ok {
		mc.PDFBeta = v
	}

	return mc
}

// buildContentBlocks converts an events.Message with Parts into Anthropic
// content blocks. Returns nil when Parts is empty (caller falls back to the
// string Content path). Returns an error for unsupported part types
// (audio, video) and oversize inline payloads with no URI.
//
// When msg.Content is non-empty, it leads as a text block before any Parts —
// matches the convention already used in Gemini's buildParts.
func buildContentBlocks(msg events.Message) ([]map[string]any, error) {
	if len(msg.Parts) == 0 {
		return nil, nil
	}

	out := make([]map[string]any, 0, len(msg.Parts)+1)
	if msg.Content != "" {
		out = append(out, map[string]any{"type": "text", "text": msg.Content})
	}

	for _, part := range msg.Parts {
		switch part.Type {
		case "text":
			out = append(out, map[string]any{"type": "text", "text": part.Text})

		case "image":
			block, err := buildImageBlock(part)
			if err != nil {
				return nil, err
			}
			out = append(out, block)

		case "file":
			// Treat any non-image binary part as a document. mime_type drives
			// whether Anthropic interprets it as a PDF or another doc format.
			block, err := buildDocumentBlock(part)
			if err != nil {
				return nil, err
			}
			out = append(out, block)

		case "audio", "video":
			return nil, fmt.Errorf("anthropic: %s parts are not supported (use Gemini or OpenAI for these modalities)", part.Type)

		default:
			return nil, fmt.Errorf("anthropic: unsupported part type %q", part.Type)
		}
	}
	return out, nil
}

// buildImageBlock emits an Anthropic `image` content block from a MessagePart.
// Source-selection order: FileID > URI > inline Data. FileID maps to the
// Files API file source (no inline cap, no token cost beyond the reference);
// URI maps to a remote URL source; inline Data must be <=5MB and carry a
// MimeType.
func buildImageBlock(part events.MessagePart) (map[string]any, error) {
	if part.FileID != "" {
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":    "file",
				"file_id": part.FileID,
			},
		}, nil
	}
	if part.URI != "" {
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type": "url",
				"url":  part.URI,
			},
		}, nil
	}
	if len(part.Data) == 0 {
		return nil, fmt.Errorf("anthropic: image part has neither URI nor Data")
	}
	if part.MimeType == "" {
		return nil, fmt.Errorf("anthropic: image part requires mime_type")
	}
	if len(part.Data) > inlineImageLimit {
		return nil, fmt.Errorf("anthropic: image part (%d bytes) exceeds inline limit; upload via Files API and set URI", len(part.Data))
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": part.MimeType,
			"data":       base64.StdEncoding.EncodeToString(part.Data),
		},
	}, nil
}

// buildDocumentBlock emits an Anthropic `document` content block from a
// MessagePart. Same source-selection rules as buildImageBlock (FileID > URI >
// inline Data) but with a 32MB inline cap. Plan 05 will add
// `citations: {enabled: true}` here behind a config flag — not handled in
// this commit.
func buildDocumentBlock(part events.MessagePart) (map[string]any, error) {
	if part.FileID != "" {
		return map[string]any{
			"type": "document",
			"source": map[string]any{
				"type":    "file",
				"file_id": part.FileID,
			},
		}, nil
	}
	if part.URI != "" {
		return map[string]any{
			"type": "document",
			"source": map[string]any{
				"type": "url",
				"url":  part.URI,
			},
		}, nil
	}
	if len(part.Data) == 0 {
		return nil, fmt.Errorf("anthropic: document part has neither URI nor Data")
	}
	if part.MimeType == "" {
		return nil, fmt.Errorf("anthropic: document part requires mime_type")
	}
	if len(part.Data) > inlinePDFLimit {
		return nil, fmt.Errorf("anthropic: document part (%d bytes) exceeds inline limit; upload via Files API and set URI", len(part.Data))
	}
	return map[string]any{
		"type": "document",
		"source": map[string]any{
			"type":       "base64",
			"media_type": part.MimeType,
			"data":       base64.StdEncoding.EncodeToString(part.Data),
		},
	}, nil
}
