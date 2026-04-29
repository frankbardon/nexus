package gemini

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

func TestUploadFile_HappyPath(t *testing.T) {
	var seenBody []byte

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload/v1beta/files":
			// Phase 1: respond with an upload URL pointing back to this server.
			w.Header().Set("X-Goog-Upload-URL", srv.URL+"/_upload-bytes")
			w.WriteHeader(http.StatusOK)
		case "/_upload-bytes":
			seenBody, _ = io.ReadAll(r.Body)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"file": map[string]any{
					"uri": "https://generativelanguage.googleapis.com/v1beta/files/abc",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := &Plugin{
		client: srv.Client(),
		logger: slog.Default(),
		auth:   &authState{mode: authModeAPIKey, apiKey: "test"},
	}

	uri, err := p.uploadFileTo(context.Background(), srv.URL, []byte("hello"), "text/plain", "name")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(uri, "/files/abc") {
		t.Fatalf("unexpected URI: %s", uri)
	}
	if string(seenBody) != "hello" {
		t.Fatalf("server saw body %q want %q", string(seenBody), "hello")
	}
}

func TestUploadFile_VertexBlocked(t *testing.T) {
	p := &Plugin{
		logger: slog.Default(),
		auth:   &authState{mode: authModeVertex, projectID: "p", location: "us-central1"},
	}
	if _, err := p.uploadFile(context.Background(), []byte("x"), "text/plain", "n"); err == nil {
		t.Fatal("expected error in Vertex mode")
	}
}

func TestPreuploadParts_VertexBlocked(t *testing.T) {
	p := &Plugin{
		logger: slog.Default(),
		auth:   &authState{mode: authModeVertex, projectID: "p", location: "us-central1"},
	}
	big := make([]byte, inlineDataLimit+1)
	_, err := p.preuploadParts(context.Background(), []events.Message{{
		Role: "user",
		Parts: []events.MessagePart{{Type: "image", MimeType: "image/png", Data: big}},
	}})
	if err == nil {
		t.Fatal("expected vertex-mode oversize error")
	}
	if !strings.Contains(err.Error(), "Vertex") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPreuploadParts_NoChangeForSmall(t *testing.T) {
	p := &Plugin{
		logger: slog.Default(),
		auth:   &authState{mode: authModeAPIKey, apiKey: "k"},
	}
	small := []byte("tiny")
	out, err := p.preuploadParts(context.Background(), []events.Message{{
		Role: "user",
		Parts: []events.MessagePart{{Type: "image", MimeType: "image/png", Data: small}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out[0].Parts[0].Data) == 0 || out[0].Parts[0].URI != "" {
		t.Fatalf("small part should be unchanged, got %+v", out[0].Parts[0])
	}
}
