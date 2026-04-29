package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/frankbardon/nexus/pkg/events"
)

// preuploadParts walks msgs and replaces any oversize multimodal Data with a
// Files API URI. Inputs already carrying a URI are left alone. Returns a fresh
// slice so the caller's messages are not mutated.
func (p *Plugin) preuploadParts(ctx context.Context, msgs []events.Message) ([]events.Message, error) {
	if p.auth.filesAPIBaseURL() == "" {
		// Vertex mode — Files API unavailable; surface an error if any part is
		// oversize so the user gets a clear message rather than a 4xx.
		for _, m := range msgs {
			for _, part := range m.Parts {
				if len(part.Data) > inlineDataLimit && part.URI == "" {
					return nil, fmt.Errorf("gemini: %s part (%d bytes) exceeds inline limit and Files API is unavailable in Vertex mode; upload to Cloud Storage and set URI", part.Type, len(part.Data))
				}
			}
		}
		return msgs, nil
	}

	out := make([]events.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		if len(m.Parts) == 0 {
			continue
		}
		newParts := make([]events.MessagePart, len(m.Parts))
		for j, part := range m.Parts {
			newParts[j] = part
			if len(part.Data) > inlineDataLimit && part.URI == "" {
				if part.MimeType == "" {
					return nil, fmt.Errorf("gemini: oversize %s part requires mime_type", part.Type)
				}
				p.logger.Info("uploading oversize part via Files API", "type", part.Type, "bytes", len(part.Data), "mime", part.MimeType)
				uri, err := p.uploadFile(ctx, part.Data, part.MimeType, fmt.Sprintf("nexus-%s-%d", part.Type, i))
				if err != nil {
					return nil, fmt.Errorf("gemini: Files API upload: %w", err)
				}
				newParts[j].URI = uri
				newParts[j].Data = nil
			}
		}
		out[i].Parts = newParts
	}
	return out, nil
}

// uploadFile uploads bytes to the Gemini Files API and returns the resulting
// file resource. Used for multimodal payloads above the inline limit.
//
// Files API is only available on the public api-key endpoint; in Vertex mode
// callers should use Cloud Storage URIs instead (not implemented here — see
// authState.filesAPIBaseURL).
func (p *Plugin) uploadFile(ctx context.Context, data []byte, mimeType, displayName string) (uri string, err error) {
	base := p.auth.filesAPIBaseURL()
	if base == "" {
		return "", fmt.Errorf("gemini: files API unavailable in Vertex mode")
	}
	if p.auth.mode != authModeAPIKey {
		return "", fmt.Errorf("gemini: files API requires api_key auth")
	}
	return p.uploadFileTo(ctx, base, data, mimeType, displayName)
}

// uploadFileTo is the test-friendly variant: it uploads to an explicit base
// URL rather than the one resolved from authState. Production code uses
// uploadFile; tests inject a fake base URL.
func (p *Plugin) uploadFileTo(ctx context.Context, base string, data []byte, mimeType, displayName string) (string, error) {
	startURL := base + "/upload/v1beta/files"

	meta := map[string]any{
		"file": map[string]any{
			"display_name": displayName,
		},
	}
	metaJSON, _ := json.Marshal(meta)

	startReq, err := http.NewRequestWithContext(ctx, "POST", startURL, bytes.NewReader(metaJSON))
	if err != nil {
		return "", err
	}
	startReq.Header.Set("X-Goog-Upload-Protocol", "resumable")
	startReq.Header.Set("X-Goog-Upload-Command", "start")
	startReq.Header.Set("X-Goog-Upload-Header-Content-Length", strconv.Itoa(len(data)))
	startReq.Header.Set("X-Goog-Upload-Header-Content-Type", mimeType)
	startReq.Header.Set("Content-Type", "application/json")
	startReq.Header.Set("x-goog-api-key", p.auth.apiKey)

	startResp, err := p.client.Do(startReq)
	if err != nil {
		return "", err
	}
	defer startResp.Body.Close()
	if startResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(startResp.Body)
		return "", fmt.Errorf("files API start failed (%d): %s", startResp.StatusCode, string(body))
	}

	uploadURL := startResp.Header.Get("X-Goog-Upload-URL")
	if uploadURL == "" {
		return "", fmt.Errorf("files API: missing X-Goog-Upload-URL header")
	}

	// Phase 2: upload bytes + finalize.
	upReq, err := http.NewRequestWithContext(ctx, "POST", uploadURL, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	upReq.Header.Set("Content-Length", strconv.Itoa(len(data)))
	upReq.Header.Set("X-Goog-Upload-Offset", "0")
	upReq.Header.Set("X-Goog-Upload-Command", "upload, finalize")

	upResp, err := p.client.Do(upReq)
	if err != nil {
		return "", err
	}
	defer upResp.Body.Close()
	body, _ := io.ReadAll(upResp.Body)
	if upResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("files API upload failed (%d): %s", upResp.StatusCode, string(body))
	}

	var fileResp struct {
		File struct {
			URI string `json:"uri"`
		} `json:"file"`
	}
	if err := json.Unmarshal(body, &fileResp); err != nil {
		return "", fmt.Errorf("parse files API response: %w", err)
	}
	if fileResp.File.URI == "" {
		return "", fmt.Errorf("files API: empty URI in response: %s", string(body))
	}
	return fileResp.File.URI, nil
}
