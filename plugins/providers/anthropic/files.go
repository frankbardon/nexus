package anthropic

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
	// filesAPIBaseURL is the production Anthropic Files API endpoint. Tests
	// override Plugin.filesAPIURL with an httptest server URL.
	filesAPIBaseURL = "https://api.anthropic.com/v1/files"
	// filesAPIBetaHeader is the beta gate the Files API requires today.
	// Send it on every request when the files feature is enabled — Anthropic
	// accepts the header even when no file_id is referenced, and the cost of
	// always sending it is one short string in the request header.
	filesAPIBetaHeader = "files-api-2025-04-14"
	// uploadThresholdDefault is 5MB; matches Anthropic's documented inline
	// image cap and is the smallest of the inline limits we care about. Parts
	// over this size get auto-uploaded.
	uploadThresholdDefault = 5 * 1024 * 1024
)

// filesConfig controls Anthropic Files API behavior.
//
//	files:
//	  enabled: true                # default false; when false the plugin never uploads
//	  upload_threshold: 5242880    # bytes; auto-upload Data parts over this size
//	  cache_uploads: true          # in-memory sha256(content) -> file_id reuse within session
//	  delete_on_shutdown: false    # best-effort DELETE for session-uploaded ids on Shutdown
type filesConfig struct {
	Enabled          bool
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
// Note: Anthropic's Files API has 30-day retention. This cache is intentionally
// in-memory only — cross-session caching would have to handle 404s on stale
// ids, and we don't implement that recovery path here. If the user restarts
// the process, files get re-uploaded; the cap on this is the per-session
// workspace storage, which is generous in practice.
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

// uploadFile multipart-POSTs bytes to the Anthropic Files API and returns the
// resulting file_id. Tests inject an alternate base URL via p.filesAPIURL.
func (p *Plugin) uploadFile(ctx context.Context, data []byte, mimeType, filename string) (string, error) {
	endpoint := p.filesAPIURL
	if endpoint == "" {
		endpoint = filesAPIBaseURL
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	if mimeType != "" {
		header.Set("Content-Type", mimeType)
	}
	part, err := mw.CreatePart(header)
	if err != nil {
		return "", fmt.Errorf("anthropic: create multipart part: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return "", fmt.Errorf("anthropic: write multipart payload: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("anthropic: close multipart writer: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, &buf)
	if err != nil {
		return "", fmt.Errorf("anthropic: build files request: %w", err)
	}
	httpReq.Header.Set("x-api-key", p.auth.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("anthropic-beta", filesAPIBetaHeader)
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())
	httpReq.Header.Set("Content-Length", strconv.Itoa(buf.Len()))

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("anthropic: files upload HTTP error: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("anthropic: files upload returned status %d: %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		ID        string `json:"id"`
		Filename  string `json:"filename"`
		MimeType  string `json:"mime_type"`
		SizeBytes int    `json:"size_bytes"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("anthropic: parse files response: %w", err)
	}
	if parsed.ID == "" {
		return "", fmt.Errorf("anthropic: files response missing id: %s", string(body))
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
		return fmt.Errorf("anthropic: build delete request: %w", err)
	}
	httpReq.Header.Set("x-api-key", p.auth.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("anthropic-beta", filesAPIBetaHeader)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("anthropic: files delete HTTP error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("anthropic: files delete returned status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// preuploadParts walks msgs and replaces oversize Data parts with a Files API
// file_id. Parts that already carry a FileID are left alone; URI-only parts
// pass through untouched (the multimodal block builder will use the URI source
// path). Returns a fresh slice so the caller's messages are not mutated.
//
// Cache key is sha256(mime_type + ":" + data) when CacheUploads is true; on
// hit, the existing FileID is reused without an HTTP call. Successful uploads
// clear part.Data to free memory and avoid double-send if the message gets
// re-serialized.
func (p *Plugin) preuploadParts(ctx context.Context, msgs []events.Message) ([]events.Message, error) {
	if !p.files.Enabled || len(msgs) == 0 {
		return msgs, nil
	}

	threshold := p.files.UploadThreshold
	if threshold <= 0 {
		threshold = uploadThresholdDefault
	}

	out := make([]events.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		if len(m.Parts) == 0 {
			continue
		}
		var newParts []events.MessagePart
		for j, part := range m.Parts {
			// Skip if FileID already set, URI already set (provider-hosted
			// reference takes precedence over local Data — no need to upload),
			// or the inline payload is under threshold.
			if part.FileID != "" || part.URI != "" || len(part.Data) <= threshold {
				if newParts != nil {
					newParts[j] = part
				}
				continue
			}
			if part.MimeType == "" {
				return nil, fmt.Errorf("anthropic: oversize %s part requires mime_type for files upload", part.Type)
			}

			// Allocate the per-message copy lazily — saves an alloc when the
			// message has no oversize parts (the common case).
			if newParts == nil {
				newParts = make([]events.MessagePart, len(m.Parts))
				copy(newParts, m.Parts)
			}

			var key string
			if p.files.CacheUploads && p.fileCache != nil {
				key = hashKey(part.MimeType, part.Data)
				if id, ok := p.fileCache.get(key); ok {
					p.logger.Debug("anthropic: files cache hit", "type", part.Type, "bytes", len(part.Data), "file_id", id)
					newParts[j].FileID = id
					newParts[j].Data = nil
					continue
				}
			}

			filename := fmt.Sprintf("nexus-%s-%d-%d", part.Type, i, j)
			p.logger.Info("anthropic: uploading oversize part via Files API", "type", part.Type, "bytes", len(part.Data), "mime", part.MimeType)
			id, err := p.uploadFile(ctx, part.Data, part.MimeType, filename)
			if err != nil {
				return nil, fmt.Errorf("anthropic: files upload: %w", err)
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
