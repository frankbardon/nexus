package openai

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/frankbardon/nexus/pkg/events"
)

// inlineImageLimit is the byte ceiling above which OpenAI rejects inline image
// payloads. Per OpenAI's vision docs the cap is roughly 20 MB whether the image
// is referenced by URL or sent as a data URL. We enforce the same limit on
// inline base64 payloads here.
const inlineImageLimit = 20 * 1024 * 1024

// multimodalConfig holds OpenAI multimodal defaults.
//
//	multimodal:
//	  vision: true                  # false suppresses image parts entirely
//	  default_image_detail: auto    # auto | low | high
type multimodalConfig struct {
	// Vision allows image parts onto the wire. Default true. When false,
	// image parts are dropped from the serialized content on BOTH surfaces
	// and never uploaded to the Files API — the turn still goes out, minus
	// the images. It is a deployment-level "this model cannot see" switch,
	// not a validation error, because the messages an agent loop assembles
	// are not the operator's to fix mid-turn.
	Vision bool

	DefaultImageDetail string
}

// parseMultimodalConfig reads the optional "multimodal" map from the plugin
// config. Unknown or invalid values fall back to "auto".
func parseMultimodalConfig(cfg map[string]any) multimodalConfig {
	mc := multimodalConfig{Vision: true, DefaultImageDetail: "auto"}
	raw, ok := cfg["multimodal"].(map[string]any)
	if !ok {
		return mc
	}
	if v, ok := raw["vision"].(bool); ok {
		mc.Vision = v
	}
	if v, ok := raw["default_image_detail"].(string); ok {
		switch v {
		case "auto", "low", "high":
			mc.DefaultImageDetail = v
		}
	}
	return mc
}

// buildContentParts converts an events.Message with Parts into OpenAI's
// content-array form. Returns nil when Parts is empty (caller falls back to
// the string Content path). Errors on unsupported types.
//
// A nil return also happens when every part was suppressed — `vision: false`
// on a message whose only parts are images — so the caller sends the plain
// string Content rather than an empty content array, which the API rejects.
func buildContentParts(msg events.Message, cfg multimodalConfig) ([]map[string]any, error) {
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
			if !cfg.Vision {
				continue
			}
			block, err := buildImagePart(part, cfg.DefaultImageDetail)
			if err != nil {
				return nil, err
			}
			out = append(out, block)

		case "audio":
			block, err := buildAudioPart(part)
			if err != nil {
				return nil, err
			}
			out = append(out, block)

		case "file":
			block, err := buildFilePart(part)
			if err != nil {
				return nil, err
			}
			out = append(out, block)

		case "video":
			return nil, fmt.Errorf("openai: video parts are not supported")

		default:
			return nil, fmt.Errorf("openai: unsupported part type %q", part.Type)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// buildImagePart builds an image_url block. URI passes through; inline Data
// becomes a data URL. OpenAI's chat completions image_url type doesn't accept
// a bare file_id, so a FileID-only image part is rejected with a clear error.
func buildImagePart(part events.MessagePart, defaultDetail string) (map[string]any, error) {
	var url string
	switch {
	case part.URI != "":
		url = part.URI
	case len(part.Data) > 0:
		if part.MimeType == "" {
			return nil, fmt.Errorf("openai: image part requires mime_type when Data is set")
		}
		if len(part.Data) > inlineImageLimit {
			return nil, fmt.Errorf("openai: image part (%d bytes) exceeds %d byte inline limit; upload via Files API and reference via URI", len(part.Data), inlineImageLimit)
		}
		url = "data:" + part.MimeType + ";base64," + base64.StdEncoding.EncodeToString(part.Data)
	case part.FileID != "":
		// OpenAI's chat completions image_url type doesn't accept a bare
		// file_id; images must be either a public URL or a data URL.
		return nil, fmt.Errorf("openai: image parts cannot reference a file_id directly; use URI (public URL) or Data (inline)")
	default:
		return nil, fmt.Errorf("openai: image part has no URI, Data, or FileID")
	}
	return map[string]any{
		"type": "image_url",
		"image_url": map[string]any{
			"url":    url,
			"detail": defaultDetail,
		},
	}, nil
}

// buildAudioPart builds an input_audio block. Audio works only on
// gpt-4o-audio-preview / gpt-4o-realtime-preview models — provider doesn't
// gate; the API will reject if the model doesn't support it.
func buildAudioPart(part events.MessagePart) (map[string]any, error) {
	if len(part.Data) == 0 {
		return nil, fmt.Errorf("openai: audio part requires inline Data")
	}
	if part.MimeType == "" {
		return nil, fmt.Errorf("openai: audio part requires mime_type")
	}
	format, err := audioFormatFromMime(part.MimeType)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"type": "input_audio",
		"input_audio": map[string]any{
			"data":   base64.StdEncoding.EncodeToString(part.Data),
			"format": format,
		},
	}, nil
}

// audioFormatFromMime maps a MIME type to OpenAI's input_audio format string.
// OpenAI accepts "wav" or "mp3" today.
func audioFormatFromMime(mimeType string) (string, error) {
	switch strings.ToLower(mimeType) {
	case "audio/wav", "audio/wave", "audio/x-wav":
		return "wav", nil
	case "audio/mpeg", "audio/mp3":
		return "mp3", nil
	default:
		return "", fmt.Errorf("openai: unsupported audio mime type %q (expected audio/wav or audio/mpeg)", mimeType)
	}
}

// buildFilePart builds a file block. Either FileID (preferred) or inline
// base64 file_data + filename. URI references aren't accepted by OpenAI's
// chat completions file type.
func buildFilePart(part events.MessagePart) (map[string]any, error) {
	inner := map[string]any{}
	switch {
	case part.FileID != "":
		inner["file_id"] = part.FileID
	case len(part.Data) > 0:
		if part.MimeType == "" {
			return nil, fmt.Errorf("openai: file part requires mime_type when Data is set")
		}
		// OpenAI expects filename + file_data (data URL) for inline files.
		// Filename is a placeholder until MessagePart grows a Filename field.
		inner["filename"] = "file"
		inner["file_data"] = "data:" + part.MimeType + ";base64," + base64.StdEncoding.EncodeToString(part.Data)
	case part.URI != "":
		return nil, fmt.Errorf("openai: file parts referenced by URI are not supported; upload via Files API and set FileID")
	default:
		return nil, fmt.Errorf("openai: file part has no FileID, Data, or URI")
	}
	return map[string]any{
		"type": "file",
		"file": inner,
	}, nil
}
