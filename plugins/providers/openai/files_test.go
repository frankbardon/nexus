package openai

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
// zero-valued config (Enabled=false, threshold=default, cache_uploads=true,
// purpose=user_data).
func TestParseFilesConfig_Defaults(t *testing.T) {
	fc := parseFilesConfig(map[string]any{})
	if fc.Enabled {
		t.Fatalf("expected disabled by default, got enabled")
	}
	if fc.Purpose != defaultFilesPurpose {
		t.Fatalf("expected purpose=%q, got %q", defaultFilesPurpose, fc.Purpose)
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
			"purpose":            "assistants",
			"upload_threshold":   1024,
			"cache_uploads":      false,
			"delete_on_shutdown": true,
		},
	}
	fc := parseFilesConfig(cfg)
	if !fc.Enabled {
		t.Fatalf("expected enabled=true")
	}
	if fc.Purpose != "assistants" {
		t.Fatalf("expected purpose=assistants, got %q", fc.Purpose)
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

// newFilesTestPlugin returns a Plugin wired to an httptest server with sane
// defaults. The server URL becomes filesAPIURL so uploadFile and deleteFile
// route to the fake.
func newFilesTestPlugin(t *testing.T, handler http.Handler) (*Plugin, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p := &Plugin{
		auth:        &authState{mode: authModeOpenAI, apiKey: "test-key"},
		client:      srv.Client(),
		logger:      silentLogger(),
		filesAPIURL: srv.URL,
		fileCache:   newFileCache(),
	}
	return p, srv
}

// TestUploadFile_Success verifies the multipart body shape, the Bearer auth
// header, and that the parsed file_id is returned.
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
		_, _ = w.Write([]byte(`{"id":"file-abc","object":"file","filename":"foo.pdf","bytes":42,"purpose":"user_data","created_at":1700000000}`))
	})

	p, _ := newFilesTestPlugin(t, mux)
	id, err := p.uploadFile(context.Background(), []byte("payload-bytes"), "application/pdf", "foo.pdf", "user_data")
	if err != nil {
		t.Fatalf("uploadFile failed: %v", err)
	}
	if id != "file-abc" {
		t.Fatalf("expected file-abc, got %q", id)
	}

	// Bearer auth — same as the rest of the OpenAI plugin.
	if capturedHeaders.Get("Authorization") != "Bearer test-key" {
		t.Fatalf("missing/wrong Authorization: %q", capturedHeaders.Get("Authorization"))
	}
	if !strings.HasPrefix(capturedContentType, "multipart/form-data") {
		t.Fatalf("expected multipart content-type, got %q", capturedContentType)
	}
	// No Anthropic-style beta header should leak in.
	if capturedHeaders.Get("anthropic-beta") != "" {
		t.Fatalf("unexpected anthropic-beta header on OpenAI request")
	}

	bodyStr := string(capturedBody)
	if !strings.Contains(bodyStr, `name="purpose"`) {
		t.Fatalf("multipart body missing purpose field: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "user_data") {
		t.Fatalf("multipart body missing purpose value: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `name="file"`) {
		t.Fatalf("multipart body missing file field name: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `filename="foo.pdf"`) {
		t.Fatalf("multipart body missing filename: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "application/pdf") {
		t.Fatalf("multipart body missing content-type: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "payload-bytes") {
		t.Fatalf("multipart body missing payload bytes: %s", bodyStr)
	}
}

// TestUploadFile_DefaultPurpose asserts that an empty purpose argument
// defaults to "user_data" in the multipart body.
func TestUploadFile_DefaultPurpose(t *testing.T) {
	var capturedBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"file-def","object":"file"}`))
	})
	p, _ := newFilesTestPlugin(t, mux)

	if _, err := p.uploadFile(context.Background(), []byte("x"), "application/pdf", "x.pdf", ""); err != nil {
		t.Fatalf("uploadFile: %v", err)
	}
	if !strings.Contains(string(capturedBody), "user_data") {
		t.Fatalf("expected default purpose user_data in body, got: %s", string(capturedBody))
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
	p, _ := newFilesTestPlugin(t, mux)

	_, err := p.uploadFile(context.Background(), []byte("data"), "application/pdf", "x.pdf", "user_data")
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

// TestPreuploadParts_FilePart_Uploaded covers the canonical happy path: a
// file-type Data part is uploaded, FileID is set, Data is cleared. Caller's
// slice is not mutated.
func TestPreuploadParts_FilePart_Uploaded(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"file-uploaded","object":"file","filename":"x","bytes":10,"purpose":"user_data","created_at":1}`))
	})

	p, _ := newFilesTestPlugin(t, mux)
	p.files = filesConfig{Enabled: true, Purpose: "user_data", CacheUploads: true}

	original := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	msgs := []events.Message{
		{Role: "user", Parts: []events.MessagePart{
			{Type: "file", MimeType: "application/pdf", Data: original},
		}},
	}
	out, err := p.preuploadParts(context.Background(), msgs)
	if err != nil {
		t.Fatalf("preuploadParts failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 upload, got %d", calls)
	}
	if out[0].Parts[0].FileID != "file-uploaded" {
		t.Fatalf("expected FileID=file-uploaded, got %q", out[0].Parts[0].FileID)
	}
	if out[0].Parts[0].Data != nil {
		t.Fatalf("expected Data cleared, got %d bytes", len(out[0].Parts[0].Data))
	}
	// Caller's slice MUST NOT be mutated.
	if msgs[0].Parts[0].FileID != "" {
		t.Fatal("caller's MessagePart was mutated (FileID set on input)")
	}
	if len(msgs[0].Parts[0].Data) != len(original) {
		t.Fatal("caller's MessagePart was mutated (Data cleared)")
	}
}

// TestPreuploadParts_FilePart_URIOnly errors out: OpenAI's Files API needs
// bytes, and the chat completions file type doesn't accept URI references.
func TestPreuploadParts_FilePart_URIOnly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("unexpected upload — should have failed pre-upload")
	})
	p, _ := newFilesTestPlugin(t, mux)
	p.files = filesConfig{Enabled: true, Purpose: "user_data", CacheUploads: true}

	msgs := []events.Message{{Role: "user", Parts: []events.MessagePart{
		{Type: "file", MimeType: "application/pdf", URI: "https://example.com/x.pdf"},
	}}}
	_, err := p.preuploadParts(context.Background(), msgs)
	if err == nil {
		t.Fatal("expected error for URI-only file part")
	}
	if !strings.Contains(err.Error(), "Data") {
		t.Fatalf("expected error to mention Data: %v", err)
	}
}

// TestPreuploadParts_ImagePart_NotUploaded asserts that even oversize image
// parts are skipped — chat completions image_url doesn't accept file_ids.
func TestPreuploadParts_ImagePart_NotUploaded(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	})
	p, _ := newFilesTestPlugin(t, mux)
	p.files = filesConfig{Enabled: true, Purpose: "user_data", UploadThreshold: 4, CacheUploads: true}

	// 10 bytes — well over UploadThreshold=4. Should still be skipped.
	msgs := []events.Message{{Role: "user", Parts: []events.MessagePart{
		{Type: "image", MimeType: "image/png", Data: make([]byte, 10)},
	}}}
	out, err := p.preuploadParts(context.Background(), msgs)
	if err != nil {
		t.Fatalf("preuploadParts failed: %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected 0 uploads for image parts, got %d", calls)
	}
	if out[0].Parts[0].FileID != "" {
		t.Fatal("FileID should not be set for image parts (chat completions doesn't accept image file_ids)")
	}
	if len(out[0].Parts[0].Data) != 10 {
		t.Fatal("Data should be preserved for image parts")
	}
}

// TestPreuploadParts_FileIDPassthrough covers parts that already have a
// FileID: no upload, no mutation.
func TestPreuploadParts_FileIDPassthrough(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
	})
	p, _ := newFilesTestPlugin(t, mux)
	p.files = filesConfig{Enabled: true, Purpose: "user_data", CacheUploads: true}

	msgs := []events.Message{{Role: "user", Parts: []events.MessagePart{
		{Type: "file", FileID: "file-existing", Data: make([]byte, 10)},
	}}}
	out, err := p.preuploadParts(context.Background(), msgs)
	if err != nil {
		t.Fatalf("preuploadParts failed: %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected 0 uploads when FileID set, got %d", calls)
	}
	if out[0].Parts[0].FileID != "file-existing" {
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
		_, _ = w.Write([]byte(`{"id":"file-cached","object":"file","filename":"x","bytes":10,"purpose":"user_data","created_at":1}`))
	})
	p, _ := newFilesTestPlugin(t, mux)
	p.files = filesConfig{Enabled: true, Purpose: "user_data", CacheUploads: true}

	payload := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	mk := func() []events.Message {
		return []events.Message{{Role: "user", Parts: []events.MessagePart{
			{Type: "file", MimeType: "application/pdf", Data: append([]byte(nil), payload...)},
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
	if out1[0].Parts[0].FileID != "file-cached" || out2[0].Parts[0].FileID != "file-cached" {
		t.Fatalf("expected both calls to resolve to file-cached, got %q / %q",
			out1[0].Parts[0].FileID, out2[0].Parts[0].FileID)
	}
}

// TestPreuploadParts_Disabled is a no-op when files.Enabled is false.
func TestPreuploadParts_Disabled(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
	})
	p, _ := newFilesTestPlugin(t, mux)
	p.files = filesConfig{Enabled: false}

	msgs := []events.Message{{Role: "user", Parts: []events.MessagePart{
		{Type: "file", MimeType: "application/pdf", Data: make([]byte, 10)},
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

// TestDeleteFile_Success exercises the DELETE happy path.
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
		_, _ = w.Write([]byte(`{"id":"file-abc","object":"file","deleted":true}`))
	})
	p, _ := newFilesTestPlugin(t, mux)

	if err := p.deleteFile(context.Background(), "file-abc"); err != nil {
		t.Fatalf("deleteFile: %v", err)
	}
	if !strings.HasSuffix(capturedPath, "/file-abc") {
		t.Fatalf("expected path ending in /file-abc, got %s", capturedPath)
	}
	if capturedHeaders.Get("Authorization") != "Bearer test-key" {
		t.Fatalf("expected Bearer auth on delete, got %q", capturedHeaders.Get("Authorization"))
	}
}

// TestDeleteFile_FailureNonFatal asserts that 4xx surfaces a descriptive error.
func TestDeleteFile_FailureNonFatal(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})
	p, _ := newFilesTestPlugin(t, mux)
	err := p.deleteFile(context.Background(), "file-missing")
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
			id := "file-" + r.URL.Path
			body, _ := json.Marshal(map[string]any{"id": id, "object": "file"})
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		case http.MethodDelete:
			parts := strings.Split(r.URL.Path, "/")
			deleted = append(deleted, parts[len(parts)-1])
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"x","object":"file","deleted":true}`))
		}
	})
	p, _ := newFilesTestPlugin(t, mux)

	p.trackUploadedID("file-one")
	p.trackUploadedID("file-two")
	ids := p.snapshotSessionFileIDs()
	if len(ids) != 2 || ids[0] != "file-one" || ids[1] != "file-two" {
		t.Fatalf("unexpected snapshot: %v", ids)
	}
	if again := p.snapshotSessionFileIDs(); again != nil {
		t.Fatalf("expected second snapshot empty, got %v", again)
	}

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
	a := hashKey("application/pdf", []byte("hello"))
	b := hashKey("application/pdf", []byte("hello"))
	c := hashKey("text/plain", []byte("hello"))
	d := hashKey("application/pdf", []byte("world"))
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
