package openai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"sync"

	"github.com/frankbardon/nexus/pkg/events"
)

const (
	// filesAPIBaseURL is the production OpenAI Files API endpoint. Tests
	// override Plugin.filesAPIURL with an httptest server URL.
	filesAPIBaseURL = "https://api.openai.com/v1/files"
	// defaultFilesPurpose is the default `purpose` field used when uploading.
	// "user_data" is the value OpenAI documents for files referenced from
	// chat completions.
	defaultFilesPurpose = "user_data"
	// uploadThresholdDefault is 5MB; matches the smallest of the inline image
	// limits we care about. Image parts over this size are not auto-uploaded
	// by OpenAI (the chat completions image_url type doesn't accept file_ids),
	// but the threshold is kept in config for forward-compatibility and for
	// any future part type that does benefit from it.
	uploadThresholdDefault = 5 * 1024 * 1024
)

// filesConfig controls OpenAI Files API behavior.
//
//	files:
//	  enabled: true                # default false; when false the plugin never uploads
//	  purpose: user_data           # default "user_data" — sent as multipart `purpose` field
//	  upload_threshold: 5242880    # bytes; reserved for future use, doesn't affect file parts
//	  cache_uploads: true          # in-memory sha256(content) -> file_id reuse within session
//	  delete_on_shutdown: false    # best-effort DELETE for session-uploaded ids on Shutdown
//
// Note: file-type parts (PDFs, etc.) are ALWAYS uploaded when Enabled is true,
// because OpenAI's chat completions API doesn't accept inline file payloads
// well — file_id is the canonical reference. Image parts are never
// auto-uploaded since the chat completions image_url type doesn't accept a
// bare file_id; oversize images must be passed by public URL or rejected.
type filesConfig struct {
	Enabled          bool
	Purpose          string
	UploadThreshold  int
	CacheUploads     bool
	DeleteOnShutdown bool
}

// parseFilesConfig pulls filesConfig out of the plugin's raw config map.
// Absent block returns Enabled=false; an explicit empty map returns
// Enabled=false but with sensible defaults for the other fields. When Enabled
// is true and UploadThreshold is unset/zero, fall back to uploadThresholdDefault.
func parseFilesConfig(cfg map[string]any) filesConfig {
	fc := filesConfig{
		Purpose:         defaultFilesPurpose,
		UploadThreshold: uploadThresholdDefault,
		CacheUploads:    true,
	}

	raw, ok := cfg["files"].(map[string]any)
	if !ok {
		return fc
	}

	if v, ok := raw["enabled"].(bool); ok {
		fc.Enabled = v
	}
	if v, ok := raw["purpose"].(string); ok && v != "" {
		fc.Purpose = v
	}
	if v, ok := raw["upload_threshold"].(int); ok && v > 0 {
		fc.UploadThreshold = v
	} else if v, ok := raw["upload_threshold"].(float64); ok && v > 0 {
		fc.UploadThreshold = int(v)
	}
	if v, ok := raw["cache_uploads"].(bool); ok {
		fc.CacheUploads = v
	}
	if v, ok := raw["delete_on_shutdown"].(bool); ok {
		fc.DeleteOnShutdown = v
	}

	return fc
}

// fileCache maps content hash -> file_id for in-session deduplication. Backed
// by a plain mutex; Files API uploads are infrequent enough that contention
// isn't a concern.
//
// In-memory only — cross-session caching would have to handle 404s on stale
// ids (OpenAI doesn't auto-expire files, but they can be deleted out of
// band), and we don't implement that recovery path here.
type fileCache struct {
	mu      sync.Mutex
	entries map[string]string
}

func newFileCache() *fileCache {
	return &fileCache{entries: make(map[string]string)}
}

func (c *fileCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.entries[key]
	return id, ok
}

func (c *fileCache) put(key, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = id
}

// hashKey computes the cache key for an inline payload. MimeType is folded in
// so identical bytes uploaded under different mime types don't collide.
func hashKey(mimeType string, data []byte) string {
	h := sha256.New()
	h.Write([]byte(mimeType))
	h.Write([]byte{':'})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// uploadFile multipart-POSTs bytes to the OpenAI Files API and returns the
// resulting file_id. Tests inject an alternate base URL via p.filesAPIURL.
//
// purpose is sent as a separate multipart field (OpenAI requires it). When
// empty, defaultFilesPurpose ("user_data") is used.
func (p *Plugin) uploadFile(ctx context.Context, data []byte, mimeType, filename, purpose string) (string, error) {
	endpoint := p.filesAPIURL
	if endpoint == "" {
		endpoint = filesAPIBaseURL
	}
	if purpose == "" {
		purpose = defaultFilesPurpose
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	// purpose field first — it's the cheaper write and easier to spot in
	// captured request bodies during debugging.
	if err := mw.WriteField("purpose", purpose); err != nil {
		return "", fmt.Errorf("openai: write purpose field: %w", err)
	}

	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	if mimeType != "" {
		header.Set("Content-Type", mimeType)
	}
	part, err := mw.CreatePart(header)
	if err != nil {
		return "", fmt.Errorf("openai: create multipart part: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return "", fmt.Errorf("openai: write multipart payload: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("openai: close multipart writer: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, &buf)
	if err != nil {
		return "", fmt.Errorf("openai: build files request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())
	httpReq.Header.Set("Content-Length", strconv.Itoa(buf.Len()))

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("openai: files upload HTTP error: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openai: files upload returned status %d: %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		ID        string `json:"id"`
		Object    string `json:"object"`
		Filename  string `json:"filename"`
		Bytes     int    `json:"bytes"`
		Purpose   string `json:"purpose"`
		CreatedAt int64  `json:"created_at"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("openai: parse files response: %w", err)
	}
	if parsed.ID == "" {
		return "", fmt.Errorf("openai: files response missing id: %s", string(body))
	}
	return parsed.ID, nil
}

// deleteFile issues a DELETE against /v1/files/{id}. Best-effort — used by
// Shutdown when delete_on_shutdown is enabled.
func (p *Plugin) deleteFile(ctx context.Context, id string) error {
	base := p.filesAPIURL
	if base == "" {
		base = filesAPIBaseURL
	}
	endpoint := base + "/" + id

	httpReq, err := http.NewRequestWithContext(ctx, "DELETE", endpoint, nil)
	if err != nil {
		return fmt.Errorf("openai: build delete request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("openai: files delete HTTP error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("openai: files delete returned status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// preuploadParts walks msgs and uploads file-type Data parts to the Files API,
// swapping in the returned file_id (preferred over inline data).
//
// Rules:
//   - file parts: ALWAYS upload when Data is present (no inline path on
//     OpenAI's chat completions API matches well — file_id is the canonical
//     reference). If the part already carries FileID, leave it alone. URI on a
//     file part is rejected at request-build time by buildFilePart.
//   - image parts: SKIPPED entirely. OpenAI's chat completions image_url type
//     doesn't accept a bare file_id, so auto-upload would just lose data.
//     Oversize images must be referenced by public URL.
//   - other types (text, audio, video): skipped — they don't go through
//     the Files API.
//
// Returns a fresh slice (caller's messages are not mutated). Successful
// uploads clear part.Data to free memory and avoid double-send if the
// message is re-serialized.
func (p *Plugin) preuploadParts(ctx context.Context, msgs []events.Message) ([]events.Message, error) {
	if !p.files.Enabled || len(msgs) == 0 {
		return msgs, nil
	}

	out := make([]events.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		if len(m.Parts) == 0 {
			continue
		}
		var newParts []events.MessagePart
		for j, part := range m.Parts {
			// Only file-type parts are eligible for auto-upload. Image,
			// audio, video, and text are passed through unchanged.
			if part.Type != "file" {
				if newParts != nil {
					newParts[j] = part
				}
				continue
			}
			// Already has a FileID — nothing to do.
			if part.FileID != "" {
				if newParts != nil {
					newParts[j] = part
				}
				continue
			}
			// Need bytes to upload. URI alone won't work because OpenAI's
			// Files API doesn't fetch from URLs; the user has to provide
			// the bytes. buildFilePart will surface a clearer error if the
			// caller meant to reference an already-uploaded file.
			if len(part.Data) == 0 {
				return nil, fmt.Errorf("openai: file part has no Data to upload (set FileID or Data)")
			}
			if part.MimeType == "" {
				return nil, fmt.Errorf("openai: file part requires mime_type for files upload")
			}

			// Allocate the per-message copy lazily — saves an alloc when
			// the message has no eligible parts (the common case).
			if newParts == nil {
				newParts = make([]events.MessagePart, len(m.Parts))
				copy(newParts, m.Parts)
			}

			var key string
			if p.files.CacheUploads && p.fileCache != nil {
				key = hashKey(part.MimeType, part.Data)
				if id, ok := p.fileCache.get(key); ok {
					p.logger.Debug("openai: files cache hit", "type", part.Type, "bytes", len(part.Data), "file_id", id)
					newParts[j].FileID = id
					newParts[j].Data = nil
					continue
				}
			}

			filename := fmt.Sprintf("nexus-%s-%d-%d", part.Type, i, j)
			p.logger.Info("openai: uploading file part via Files API", "type", part.Type, "bytes", len(part.Data), "mime", part.MimeType)
			id, err := p.uploadFile(ctx, part.Data, part.MimeType, filename, p.files.Purpose)
			if err != nil {
				return nil, fmt.Errorf("openai: files upload: %w", err)
			}
			newParts[j].FileID = id
			newParts[j].Data = nil

			if p.files.CacheUploads && p.fileCache != nil && key != "" {
				p.fileCache.put(key, id)
			}
			p.trackUploadedID(id)
		}
		if newParts != nil {
			out[i].Parts = newParts
		}
	}
	return out, nil
}

// trackUploadedID records a file_id this session uploaded so Shutdown can
// optionally delete it. Safe to call from concurrent request goroutines.
func (p *Plugin) trackUploadedID(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessionFileIDs = append(p.sessionFileIDs, id)
}

// snapshotSessionFileIDs returns a copy of the session-tracked file IDs and
// clears the slice. Used by Shutdown.
func (p *Plugin) snapshotSessionFileIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sessionFileIDs) == 0 {
		return nil
	}
	ids := make([]string, len(p.sessionFileIDs))
	copy(ids, p.sessionFileIDs)
	p.sessionFileIDs = nil
	return ids
}
