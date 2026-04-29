package gemini

import (
	"encoding/base64"
	"fmt"

	"github.com/frankbardon/nexus/pkg/events"
)

// inlineDataLimit is the byte ceiling above which Gemini recommends using the
// Files API instead of inline_data. Set conservatively at 18 MB; the real cap
// is ~20 MB for the entire request.
const inlineDataLimit = 18 * 1024 * 1024

// buildParts converts an events.Message into Gemini "parts" entries. Falls back
// to a single text part when Parts is empty (text-only path used by all
// existing providers). Multimodal parts (image/audio/video/file) are encoded
// inline when small or referenced via URI when the caller has already uploaded
// to the Files API.
func buildParts(msg events.Message) ([]map[string]any, error) {
	if len(msg.Parts) == 0 {
		// Text-only path. An empty message with no parts is unusual but
		// harmless — emit an empty text part to satisfy the schema.
		return []map[string]any{{"text": msg.Content}}, nil
	}

	out := make([]map[string]any, 0, len(msg.Parts)+1)
	if msg.Content != "" {
		out = append(out, map[string]any{"text": msg.Content})
	}

	for _, part := range msg.Parts {
		switch part.Type {
		case "text":
			out = append(out, map[string]any{"text": part.Text})

		case "image", "audio", "video", "file":
			if part.URI != "" {
				if part.MimeType == "" {
					return nil, fmt.Errorf("gemini: part with URI requires mime_type (type=%s)", part.Type)
				}
				out = append(out, map[string]any{
					"fileData": map[string]any{
						"mimeType": part.MimeType,
						"fileUri":  part.URI,
					},
				})
				continue
			}
			if len(part.Data) == 0 {
				return nil, fmt.Errorf("gemini: %s part has neither URI nor Data", part.Type)
			}
			if part.MimeType == "" {
				return nil, fmt.Errorf("gemini: %s part requires mime_type", part.Type)
			}
			if len(part.Data) > inlineDataLimit {
				// Caller should have uploaded via Files API and set URI. Surface
				// a clear error so the issue is obvious.
				return nil, fmt.Errorf("gemini: %s part (%d bytes) exceeds inline limit; upload via Files API and set URI", part.Type, len(part.Data))
			}
			out = append(out, map[string]any{
				"inlineData": map[string]any{
					"mimeType": part.MimeType,
					"data":     base64.StdEncoding.EncodeToString(part.Data),
				},
			})

		default:
			return nil, fmt.Errorf("gemini: unsupported part type %q", part.Type)
		}
	}

	return out, nil
}
