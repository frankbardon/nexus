package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

// TestParseFilesConfig_Defaults asserts that an absent files block yields a
// zero-valued config (Enabled=false, threshold=default, cache_uploads=true).
func TestParseFilesConfig_Defaults(t *testing.T) {
	fc := parseFilesConfig(map[string]any{})
	if fc.Enabled {
		t.Fatalf("expected disabled by default, got enabled")
	}
	if fc.UploadThreshold != uploadThresholdDefault {
		t.Fatalf("expected threshold=%d, got %d", uploadThresholdDefault, fc.UploadThreshold)
	}
	if !fc.CacheUploads {
		t.Fatalf("expected cache_uploads=true by default")
	}
	if fc.DeleteOnShutdown {
		t.Fatalf("expected delete_on_shutdown=false by default")
	}
}

// TestParseFilesConfig_Explicit covers explicit overrides for every field.
func TestParseFilesConfig_Explicit(t *testing.T) {
	cfg := map[string]any{
		"files": map[string]any{
			"enabled":            true,
			"upload_threshold":   1024,
			"cache_uploads":      false,
			"delete_on_shutdown": true,
		},
	}
	fc := parseFilesConfig(cfg)
	if !fc.Enabled {
		t.Fatalf("expected enabled=true")
	}
	if fc.UploadThreshold != 1024 {
		t.Fatalf("expected threshold=1024, got %d", fc.UploadThreshold)
	}
	if fc.CacheUploads {
		t.Fatalf("expected cache_uploads=false")
	}
	if !fc.DeleteOnShutdown {
		t.Fatalf("expected delete_on_shutdown=true")
	}
}

// TestParseFilesConfig_FloatThreshold guards against YAML number-parsing,
// which decodes integers as float64 by default.
func TestParseFilesConfig_FloatThreshold(t *testing.T) {
	cfg := map[string]any{
		"files": map[string]any{
			"enabled":          true,
			"upload_threshold": float64(2048),
		},
	}
	fc := parseFilesConfig(cfg)
	if fc.UploadThreshold != 2048 {
		t.Fatalf("expected threshold=2048, got %d", fc.UploadThreshold)
	}
}

// newTestPlugin returns a Plugin wired to an httptest server with sane
// defaults. Tests pass a custom mux; the server URL becomes filesAPIURL so
// uploadFile and deleteFile route to the fake.
func newTestPlugin(t *testing.T, handler http.Handler) (*Plugin, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p := &Plugin{
		apiKey:      "test-key",
		client:      srv.Client(),
		logger:      silentLogger(),
		filesAPIURL: srv.URL,
		fileCache:   newFileCache(),
	}
	return p, srv
}

// TestUploadFile_Success verifies the multipart body shape, the headers, and
// that the parsed file_id is returned.
func TestUploadFile_Success(t *testing.T) {
	var capturedHeaders http.Header
	var capturedBody []byte
	var capturedContentType string

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		capturedContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		capturedBody = body

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"file_abc","filename":"foo.png","mime_type":"image/png","size_bytes":42,"created_at":"2026-01-01T00:00:00Z"}`))
	})

	p, _ := newTestPlugin(t, mux)
	id, err := p.uploadFile(context.Background(), []byte("payload-bytes"), "image/png", "foo.png")
	if err != nil {
		t.Fatalf("uploadFile failed: %v", err)
	}
	if id != "file_abc" {
		t.Fatalf("expected file_abc, got %q", id)
	}

	// Required Anthropic headers.
	if capturedHeaders.Get("x-api-key") != "test-key" {
		t.Fatalf("missing/wrong x-api-key: %q", capturedHeaders.Get("x-api-key"))
	}
	if capturedHeaders.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("missing anthropic-version header")
	}
	if capturedHeaders.Get("anthropic-beta") != filesAPIBetaHeader {
		t.Fatalf("expected beta header %q, got %q", filesAPIBetaHeader, capturedHeaders.Get("anthropic-beta"))
	}
	if !strings.HasPrefix(capturedContentType, "multipart/form-data") {
		t.Fatalf("expected multipart content-type, got %q", capturedContentType)
	}

	// Multipart body should contain the file field name, filename, mime, and bytes.
	bodyStr := string(capturedBody)
	if !strings.Contains(bodyStr, `name="file"`) {
		t.Fatalf("multipart body missing field name: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `filename="foo.png"`) {
		t.Fatalf("multipart body missing filename: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "image/png") {
		t.Fatalf("multipart body missing content-type: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "payload-bytes") {
		t.Fatalf("multipart body missing payload bytes: %s", bodyStr)
	}
}

// TestUploadFile_Failure asserts that 5xx responses produce a descriptive
// error including status and body.
func TestUploadFile_Failure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	p, _ := newTestPlugin(t, mux)

	_, err := p.uploadFile(context.Background(), []byte("data"), "image/png", "x.png")
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error missing status: %v", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error missing body: %v", err)
	}
}

// TestPreuploadParts_Oversize covers the canonical happy path: an oversize
// Data part is uploaded, FileID is set, Data is cleared.
func TestPreuploadParts_Oversize(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"file_uploaded","filename":"x","mime_type":"image/png","size_bytes":10,"created_at":"now"}`))
	})

	p, _ := newTestPlugin(t, mux)
	p.files = filesConfig{Enabled: true, UploadThreshold: 4, CacheUploads: true}

	original := []byte{0x01, 0x02, 0x03, 0x04, 0x05} // 5 bytes > threshold 4
	msgs := []events.Message{
		{
			Role: "user",
			Parts: []events.MessagePart{
				{Type: "image", MimeType: "image/png", Data: original},
			},
		},
	}
	out, err := p.preuploadParts(context.Background(), msgs)
	if err != nil {
		t.Fatalf("preuploadParts failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 upload, got %d", calls)
	}
	if out[0].Parts[0].FileID != "file_uploaded" {
		t.Fatalf("expected FileID=file_uploaded, got %q", out[0].Parts[0].FileID)
	}
	if out[0].Parts[0].Data != nil {
		t.Fatalf("expected Data cleared after upload, got %v bytes", len(out[0].Parts[0].Data))
	}
	// Caller's slice MUST NOT be mutated.
	if msgs[0].Parts[0].FileID != "" {
		t.Fatal("caller's MessagePart was mutated (FileID set on input)")
	}
	if len(msgs[0].Parts[0].Data) != len(original) {
		t.Fatal("caller's MessagePart was mutated (Data cleared)")
	}
}

// TestPreuploadParts_UnderThreshold ensures small Data parts are left alone.
func TestPreuploadParts_UnderThreshold(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"file_x"}`))
	})
	p, _ := newTestPlugin(t, mux)
	p.files = filesConfig{Enabled: true, UploadThreshold: 1024, CacheUploads: true}

	msgs := []events.Message{
		{Role: "user", Parts: []events.MessagePart{
			{Type: "image", MimeType: "image/png", Data: []byte{0x01, 0x02}},
		}},
	}
	out, err := p.preuploadParts(context.Background(), msgs)
	if err != nil {
		t.Fatalf("preuploadParts failed: %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected 0 uploads for under-threshold data, got %d", calls)
	}
	if out[0].Parts[0].FileID != "" {
		t.Fatal("FileID should not be set for under-threshold data")
	}
	if len(out[0].Parts[0].Data) != 2 {
		t.Fatal("Data should be preserved for under-threshold parts")
	}
}

// TestPreuploadParts_URIPassthrough ensures parts that already have a URI are
// not uploaded (they sidestep the inline cap).
func TestPreuploadParts_URIPassthrough(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	})
	p, _ := newTestPlugin(t, mux)
	p.files = filesConfig{Enabled: true, UploadThreshold: 4, CacheUploads: true}

	msgs := []events.Message{
		{Role: "user", Parts: []events.MessagePart{
			{Type: "image", URI: "https://example.com/big.png", Data: make([]byte, 10)},
		}},
	}
	out, err := p.preuploadParts(context.Background(), msgs)
	if err != nil {
		t.Fatalf("preuploadParts failed: %v", err)
	}
	// URI passthrough: no upload happens, URI is preserved, FileID stays empty.
	if calls != 0 {
		t.Fatalf("expected 0 uploads when URI set, got %d", calls)
	}
	if out[0].Parts[0].URI != "https://example.com/big.png" {
		t.Fatalf("URI mutated: %v", out[0].Parts[0].URI)
	}
	if out[0].Parts[0].FileID != "" {
		t.Fatalf("FileID should not be set when URI passthrough, got %v", out[0].Parts[0].FileID)
	}
}

// TestPreuploadParts_FileIDPassthrough covers the case where a part already
// has FileID set: no upload, no mutation.
func TestPreuploadParts_FileIDPassthrough(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	})
	p, _ := newTestPlugin(t, mux)
	p.files = filesConfig{Enabled: true, UploadThreshold: 4, CacheUploads: true}

	msgs := []events.Message{
		{Role: "user", Parts: []events.MessagePart{
			{Type: "image", FileID: "file_existing", Data: make([]byte, 10)},
		}},
	}
	out, err := p.preuploadParts(context.Background(), msgs)
	if err != nil {
		t.Fatalf("preuploadParts failed: %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected 0 uploads when FileID set, got %d", calls)
	}
	if out[0].Parts[0].FileID != "file_existing" {
		t.Fatalf("FileID should be preserved, got %q", out[0].Parts[0].FileID)
	}
}

// TestPreuploadParts_CacheHit verifies that uploading the same content twice
// hits the in-memory cache and produces only one HTTP call.
func TestPreuploadParts_CacheHit(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"file_cached","filename":"x","mime_type":"image/png","size_bytes":10,"created_at":"now"}`))
	})
	p, _ := newTestPlugin(t, mux)
	p.files = filesConfig{Enabled: true, UploadThreshold: 4, CacheUploads: true}

	payload := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	mk := func() []events.Message {
		return []events.Message{{Role: "user", Parts: []events.MessagePart{
			{Type: "image", MimeType: "image/png", Data: append([]byte(nil), payload...)},
		}}}
	}

	out1, err := p.preuploadParts(context.Background(), mk())
	if err != nil {
		t.Fatalf("first preuploadParts: %v", err)
	}
	out2, err := p.preuploadParts(context.Background(), mk())
	if err != nil {
		t.Fatalf("second preuploadParts: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 upload (cache hit on second), got %d", calls)
	}
	if out1[0].Parts[0].FileID != "file_cached" || out2[0].Parts[0].FileID != "file_cached" {
		t.Fatalf("expected both calls to resolve to file_cached, got %q / %q",
			out1[0].Parts[0].FileID, out2[0].Parts[0].FileID)
	}
}

// TestPreuploadParts_CacheDisabled verifies cache is bypassed when CacheUploads
// is false: identical bytes upload twice.
func TestPreuploadParts_CacheDisabled(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"file_xyz"}`))
	})
	p, _ := newTestPlugin(t, mux)
	p.files = filesConfig{Enabled: true, UploadThreshold: 4, CacheUploads: false}
	p.fileCache = nil // disable cache entirely

	payload := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	mk := func() []events.Message {
		return []events.Message{{Role: "user", Parts: []events.MessagePart{
			{Type: "image", MimeType: "image/png", Data: append([]byte(nil), payload...)},
		}}}
	}
	if _, err := p.preuploadParts(context.Background(), mk()); err != nil {
		t.Fatalf("first preuploadParts: %v", err)
	}
	if _, err := p.preuploadParts(context.Background(), mk()); err != nil {
		t.Fatalf("second preuploadParts: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 uploads with cache disabled, got %d", calls)
	}
}

// TestPreuploadParts_MissingMimeType errors out with a clear message rather
// than uploading garbage.
func TestPreuploadParts_MissingMimeType(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("unexpected upload — should have failed pre-upload")
	})
	p, _ := newTestPlugin(t, mux)
	p.files = filesConfig{Enabled: true, UploadThreshold: 4, CacheUploads: true}

	msgs := []events.Message{{Role: "user", Parts: []events.MessagePart{
		{Type: "image", Data: []byte{0x01, 0x02, 0x03, 0x04, 0x05}}, // no MimeType
	}}}
	_, err := p.preuploadParts(context.Background(), msgs)
	if err == nil {
		t.Fatal("expected error for missing mime_type")
	}
	if !strings.Contains(err.Error(), "mime_type") {
		t.Fatalf("expected mime_type error, got %v", err)
	}
}

// TestPreuploadParts_Disabled is a no-op when files.Enabled is false even if
// parts would otherwise trigger uploads.
func TestPreuploadParts_Disabled(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
	})
	p, _ := newTestPlugin(t, mux)
	p.files = filesConfig{Enabled: false}

	msgs := []events.Message{{Role: "user", Parts: []events.MessagePart{
		{Type: "image", MimeType: "image/png", Data: make([]byte, 10*1024*1024)},
	}}}
	out, err := p.preuploadParts(context.Background(), msgs)
	if err != nil {
		t.Fatalf("preuploadParts failed: %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected 0 uploads when feature disabled, got %d", calls)
	}
	if out[0].Parts[0].FileID != "" {
		t.Fatal("FileID should not be set when feature disabled")
	}
}

// TestDeleteFile_Success exercises the DELETE happy path and header set.
func TestDeleteFile_Success(t *testing.T) {
	var capturedPath string
	var capturedHeaders http.Header
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedHeaders = r.Header.Clone()
		if r.Method != http.MethodDelete {
			t.Fatalf("expected DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	})
	p, _ := newTestPlugin(t, mux)

	if err := p.deleteFile(context.Background(), "file_abc"); err != nil {
		t.Fatalf("deleteFile: %v", err)
	}
	if !strings.HasSuffix(capturedPath, "/file_abc") {
		t.Fatalf("expected path ending in /file_abc, got %s", capturedPath)
	}
	if capturedHeaders.Get("anthropic-beta") != filesAPIBetaHeader {
		t.Fatalf("missing files beta header on delete")
	}
}

// TestDeleteFile_FailureNonFatal asserts that 4xx surfaces a descriptive error
// (callers in Shutdown swallow it).
func TestDeleteFile_FailureNonFatal(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})
	p, _ := newTestPlugin(t, mux)
	err := p.deleteFile(context.Background(), "file_missing")
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error missing status: %v", err)
	}
}

// TestSessionFileIDTracking_DeleteOnShutdown simulates a session where two
// uploads happen, then verifies the snapshot returns both ids and a follow-up
// snapshot is empty (the slice is consumed).
func TestSessionFileIDTracking_DeleteOnShutdown(t *testing.T) {
	var deleted []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			// fake upload; return predictable ids per call order
			id := "file_" + r.URL.Path
			body, _ := json.Marshal(map[string]any{"id": id})
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		case http.MethodDelete:
			parts := strings.Split(r.URL.Path, "/")
			deleted = append(deleted, parts[len(parts)-1])
			w.WriteHeader(http.StatusOK)
		}
	})
	p, _ := newTestPlugin(t, mux)

	p.trackUploadedID("file_one")
	p.trackUploadedID("file_two")
	ids := p.snapshotSessionFileIDs()
	if len(ids) != 2 || ids[0] != "file_one" || ids[1] != "file_two" {
		t.Fatalf("unexpected snapshot: %v", ids)
	}
	if again := p.snapshotSessionFileIDs(); again != nil {
		t.Fatalf("expected second snapshot empty, got %v", again)
	}

	// Round-trip the delete path: deleting both ids should hit the fake.
	for _, id := range ids {
		if err := p.deleteFile(context.Background(), id); err != nil {
			t.Fatalf("deleteFile(%s): %v", id, err)
		}
	}
	if len(deleted) != 2 {
		t.Fatalf("expected 2 deletes, got %d (%v)", len(deleted), deleted)
	}
}

// TestHashKey_Stability verifies the cache key changes when content or
// mime_type changes, and is stable for identical inputs.
func TestHashKey_Stability(t *testing.T) {
	a := hashKey("image/png", []byte("hello"))
	b := hashKey("image/png", []byte("hello"))
	c := hashKey("image/jpeg", []byte("hello"))
	d := hashKey("image/png", []byte("world"))
	if a != b {
		t.Fatal("expected identical hashes for identical inputs")
	}
	if a == c {
		t.Fatal("expected different hash for different mime")
	}
	if a == d {
		t.Fatal("expected different hash for different bytes")
	}
}
