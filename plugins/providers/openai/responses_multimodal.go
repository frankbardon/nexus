package openai

import (
	"encoding/base64"
	"fmt"

	"github.com/frankbardon/nexus/pkg/events"
)

// This file is the multimodal half of the Responses request serializer — the
// growth point responses.go reserved. It is a second implementation rather than
// a flag on buildContentParts because the Responses API renames every content
// part and changes what two of them accept:
//
//	Chat Completions                       Responses
//	-------------------------------------  -------------------------------------
//	{"type":"text","text":…}               {"type":"input_text","text":…} on an
//	                                       input role; {"type":"output_text",…}
//	                                       on an assistant one
//	{"type":"image_url","image_url":{      {"type":"input_image",
//	   "url":…,"detail":…}}                 "image_url":…,"detail":…} — flat, and
//	                                       it also accepts "file_id"
//	{"type":"file","file":{"file_id":…}}   {"type":"input_file","file_id":…} —
//	                                       flat
//
// The `file_id` on `input_image` is the one that changes behaviour rather than
// just spelling: the chat surface has no way to reference an uploaded image, so
// preuploadParts skips images there and an oversize inline image is an error.
// Here it is uploadable, which is what makes `files.upload_threshold` mean
// something on this surface — see preuploadParts.

// responsesTextType is the content-part type name for text in an Item of the
// given role.
//
// Assistant Items carry produced text, which the API types `output_text`;
// everything else carries supplied text, typed `input_text`. Sending
// `input_text` inside an assistant Item is rejected, so this is not cosmetic.
func responsesTextType(role string) string {
	if role == "assistant" {
		return "output_text"
	}
	return "input_text"
}

// buildResponsesContentParts converts an events.Message with Parts into the
// Responses content-array form for an Item of that message's role.
//
// Returns nil when the message has no Parts, and also when every part it had
// was suppressed (`vision: false` over an image-only message) — in both cases
// the caller falls back to the plain string content, which is what responses.go
// sends for the ordinary text-only turn. An empty content array is not a thing
// the API accepts.
//
// Assistant Items are text-only by construction: the only content types an
// assistant Item accepts are `output_text` and `refusal`, so a non-text part on
// an assistant message is an error here and the caller degrades to the string.
// That is not a regression against the chat surface — an image an assistant
// "said" was never something OpenAI accepted on replay either.
func buildResponsesContentParts(msg events.Message, cfg multimodalConfig) ([]map[string]any, error) {
	if len(msg.Parts) == 0 {
		return nil, nil
	}
	textType := responsesTextType(msg.Role)
	assistant := msg.Role == "assistant"

	out := make([]map[string]any, 0, len(msg.Parts)+1)
	if msg.Content != "" {
		out = append(out, map[string]any{"type": textType, "text": msg.Content})
	}
	for _, part := range msg.Parts {
		switch part.Type {
		case "text":
			out = append(out, map[string]any{"type": textType, "text": part.Text})

		case "image":
			if !cfg.Vision {
				continue
			}
			if assistant {
				return nil, fmt.Errorf("openai: assistant Items accept only text content on the Responses API; image part cannot be replayed")
			}
			block, err := buildResponsesImagePart(part, cfg.DefaultImageDetail)
			if err != nil {
				return nil, err
			}
			out = append(out, block)

		case "file":
			if assistant {
				return nil, fmt.Errorf("openai: assistant Items accept only text content on the Responses API; file part cannot be replayed")
			}
			block, err := buildResponsesFilePart(part)
			if err != nil {
				return nil, err
			}
			out = append(out, block)

		case "audio":
			if assistant {
				return nil, fmt.Errorf("openai: assistant Items accept only text content on the Responses API; audio part cannot be replayed")
			}
			// Same nested shape and the same non-gate as the chat path: only
			// the audio-capable models accept this, and the provider holds no
			// model-capability table, so the API rejects it if the model
			// cannot take it.
			block, err := buildAudioPart(part)
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

// buildResponsesImagePart builds an `input_image` content part.
//
// Three sources, in the precedence events.MessagePart documents: a provider
// file id wins, then inline bytes as a data URL, then a URI passed through.
// Unlike the chat path a bare FileID is legal here — `input_image` carries one
// natively — which is why preuploadParts can upload an oversize inline image on
// this surface instead of refusing it.
func buildResponsesImagePart(part events.MessagePart, defaultDetail string) (map[string]any, error) {
	block := map[string]any{
		"type":   "input_image",
		"detail": defaultDetail,
	}
	switch {
	case part.FileID != "":
		block["file_id"] = part.FileID
	case len(part.Data) > 0:
		if part.MimeType == "" {
			return nil, fmt.Errorf("openai: image part requires mime_type when Data is set")
		}
		if len(part.Data) > inlineImageLimit {
			return nil, fmt.Errorf("openai: image part (%d bytes) exceeds %d byte inline limit; enable files: to upload it or reference it by URI", len(part.Data), inlineImageLimit)
		}
		block["image_url"] = "data:" + part.MimeType + ";base64," + base64.StdEncoding.EncodeToString(part.Data)
	case part.URI != "":
		block["image_url"] = part.URI
	default:
		return nil, fmt.Errorf("openai: image part has no URI, Data, or FileID")
	}
	return block, nil
}

// buildResponsesFilePart builds an `input_file` content part.
//
// FileID is preferred and is what the Files preflight leaves behind; inline
// bytes become `filename` + a `file_data` data URL, exactly as on the chat
// path. A URI is refused on both surfaces for the same reason — nothing fetches
// it, so accepting it would send a file the model never sees.
func buildResponsesFilePart(part events.MessagePart) (map[string]any, error) {
	block := map[string]any{"type": "input_file"}
	switch {
	case part.FileID != "":
		block["file_id"] = part.FileID
	case len(part.Data) > 0:
		if part.MimeType == "" {
			return nil, fmt.Errorf("openai: file part requires mime_type when Data is set")
		}
		// Filename is a placeholder until MessagePart grows a Filename field;
		// the API requires one alongside inline file_data.
		block["filename"] = "file"
		block["file_data"] = "data:" + part.MimeType + ";base64," + base64.StdEncoding.EncodeToString(part.Data)
	case part.URI != "":
		return nil, fmt.Errorf("openai: file parts referenced by URI are not supported; upload via Files API and set FileID")
	default:
		return nil, fmt.Errorf("openai: file part has no FileID, Data, or URI")
	}
	return block, nil
}
