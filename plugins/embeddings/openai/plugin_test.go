package openai

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// newTestPlugin wires a Plugin against an httptest server endpoint so
// embedding calls don't hit api.openai.com.
func newTestPlugin(t *testing.T, baseURL string) (*Plugin, engine.EventBus) {
	t.Helper()
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = slog.Default()
	p.apiKey = "test-key"
	p.baseURL = baseURL
	p.defaultModel = defaultModel
	p.client = &http.Client{Timeout: p.timeout}
	bus.Subscribe("embeddings.request", p.handleEmbeddings,
		engine.WithPriority(50))
	return p, bus
}

// fakeEmbedServer returns a tiny embeddings response so the text-only path
// can complete end-to-end.
func fakeEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
            "data": [
                {"index": 0, "embedding": [0.1, 0.2, 0.3]}
            ],
            "model": "text-embedding-3-small",
            "usage": {"prompt_tokens": 1, "total_tokens": 1}
        }`))
	}))
}

func TestHandleEmbeddings_TextOnly_Texts(t *testing.T) {
	srv := fakeEmbedServer(t)
	defer srv.Close()
	_, bus := newTestPlugin(t, srv.URL)

	req := &events.EmbeddingsRequest{
		SchemaVersion: events.EmbeddingsRequestVersion,
		Texts:         []string{"hello"},
	}
	if err := bus.Emit("embeddings.request", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if req.Error != "" {
		t.Fatalf("unexpected error: %s", req.Error)
	}
	if len(req.Vectors) != 1 || len(req.Vectors[0]) != 3 {
		t.Fatalf("expected 1 vector of length 3; got %v", req.Vectors)
	}
	if req.Provider != pluginID {
		t.Fatalf("expected provider=%s, got %s", pluginID, req.Provider)
	}
}

func TestHandleEmbeddings_TextOnly_Inputs(t *testing.T) {
	srv := fakeEmbedServer(t)
	defer srv.Close()
	_, bus := newTestPlugin(t, srv.URL)

	req := &events.EmbeddingsRequest{
		SchemaVersion: events.EmbeddingsRequestVersion,
		Inputs:        []events.EmbeddingsInput{{Text: "hello"}},
	}
	if err := bus.Emit("embeddings.request", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if req.Error != "" {
		t.Fatalf("unexpected error: %s", req.Error)
	}
	if len(req.Vectors) != 1 {
		t.Fatalf("expected 1 vector; got %d", len(req.Vectors))
	}
}

func TestHandleEmbeddings_RejectsInlineImage(t *testing.T) {
	// No HTTP server: rejection must happen before any HTTP call.
	_, bus := newTestPlugin(t, "http://127.0.0.1:1") // unreachable

	req := &events.EmbeddingsRequest{
		SchemaVersion: events.EmbeddingsRequestVersion,
		Inputs: []events.EmbeddingsInput{
			{Image: []byte{0x89, 0x50, 0x4e, 0x47}, MimeType: "image/png"},
		},
	}
	if err := bus.Emit("embeddings.request", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if req.Error == "" {
		t.Fatal("expected error rejecting image input")
	}
	if !strings.Contains(req.Error, "openai text-embedding-* models accept text only") {
		t.Fatalf("error message missing the expected guidance: %q", req.Error)
	}
	if !strings.Contains(req.Error, "nexus.embeddings.cohere_multimodal") {
		t.Fatalf("error message should point at the multimodal adapter: %q", req.Error)
	}
	if req.Provider != pluginID {
		t.Fatalf("expected provider=%s on rejection, got %s", pluginID, req.Provider)
	}
	if len(req.Vectors) != 0 {
		t.Fatalf("rejection must not produce vectors")
	}
}

func TestHandleEmbeddings_RejectsImageURI(t *testing.T) {
	_, bus := newTestPlugin(t, "http://127.0.0.1:1")

	req := &events.EmbeddingsRequest{
		SchemaVersion: events.EmbeddingsRequestVersion,
		Inputs: []events.EmbeddingsInput{
			{Text: "hello"},
			{ImageURI: "nexus-blob:deadbeef", MimeType: "image/png"},
		},
	}
	if err := bus.Emit("embeddings.request", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if req.Error == "" {
		t.Fatal("expected error rejecting ImageURI input")
	}
	if !strings.Contains(req.Error, "[input 1]") {
		t.Fatalf("error should identify the offending input index: %q", req.Error)
	}
}
