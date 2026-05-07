package coheremultimodal

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/blobs"
	"github.com/frankbardon/nexus/pkg/events"
)

// stubEmbedServer returns a minimal Cohere-compatible /v2/embed handler
// that records the last received request body for assertions.
type stubEmbedServer struct {
	*httptest.Server
	lastRequest cohereEmbedRequest
	requests    int
	// vectorsPerInput controls how many vectors of length 4 the server
	// returns. Defaults to "match the request inputs" via len(req.Inputs).
	overrideVectors [][]float32
}

func newStubServer(t *testing.T) *stubEmbedServer {
	t.Helper()
	s := &stubEmbedServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/embed" {
			http.Error(w, "wrong path: "+r.URL.Path, http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var parsed cohereEmbedRequest
		if err := json.Unmarshal(body, &parsed); err != nil {
			http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
			return
		}
		s.lastRequest = parsed
		s.requests++

		// Default vector set: one 4-dim vector per input.
		vectors := s.overrideVectors
		if vectors == nil {
			vectors = make([][]float32, len(parsed.Inputs))
			for i := range vectors {
				vectors[i] = []float32{0.1, 0.2, 0.3, 0.4}
			}
		}
		resp := cohereEmbedResponse{}
		resp.Embeddings.Float = vectors
		resp.Meta.BilledUnits.InputTokens = 7
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	return s
}

func newTestPlugin(t *testing.T, baseURL string, sess *engine.SessionWorkspace) (*Plugin, engine.EventBus) {
	t.Helper()
	bus := engine.NewEventBus()
	p := New().(*Plugin)
	p.bus = bus
	p.logger = slog.Default()
	p.apiKey = "test-key"
	p.baseURL = baseURL
	p.defaultModel = defaultModel
	p.inputType = defaultInputType
	p.client = &http.Client{Timeout: p.timeout}
	p.session = sess
	bus.Subscribe("embeddings.request", p.handleEmbeddings,
		engine.WithPriority(50))
	return p, bus
}

func TestEmbed_TextOnly_ViaTexts(t *testing.T) {
	srv := newStubServer(t)
	defer srv.Close()
	_, bus := newTestPlugin(t, srv.URL, nil)

	req := &events.EmbeddingsRequest{
		SchemaVersion: events.EmbeddingsRequestVersion,
		Texts:         []string{"hello", "world"},
	}
	if err := bus.Emit("embeddings.request", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if req.Error != "" {
		t.Fatalf("unexpected error: %s", req.Error)
	}
	if len(req.Vectors) != 2 {
		t.Fatalf("expected 2 vectors, got %d", len(req.Vectors))
	}
	if req.Provider != pluginID {
		t.Fatalf("provider mismatch: %s", req.Provider)
	}
	if req.Model != defaultModel {
		t.Fatalf("model mismatch: %s", req.Model)
	}
	// Verify wire shape.
	if srv.lastRequest.Model != defaultModel {
		t.Errorf("wire: model = %q", srv.lastRequest.Model)
	}
	if srv.lastRequest.InputType != defaultInputType {
		t.Errorf("wire: input_type = %q", srv.lastRequest.InputType)
	}
	if got := srv.lastRequest.EmbeddingTypes; len(got) != 1 || got[0] != "float" {
		t.Errorf("wire: embedding_types = %v", got)
	}
	if len(srv.lastRequest.Inputs) != 2 {
		t.Errorf("wire: expected 2 inputs, got %d", len(srv.lastRequest.Inputs))
	}
	for i, in := range srv.lastRequest.Inputs {
		if len(in.Content) != 1 || in.Content[0].Type != "text" {
			t.Errorf("wire: input %d not text-shaped: %+v", i, in)
		}
	}
}

func TestEmbed_TextOnly_ViaInputs(t *testing.T) {
	srv := newStubServer(t)
	defer srv.Close()
	_, bus := newTestPlugin(t, srv.URL, nil)

	req := &events.EmbeddingsRequest{
		SchemaVersion: events.EmbeddingsRequestVersion,
		Inputs:        []events.EmbeddingsInput{{Text: "alpha"}, {Text: "beta"}},
	}
	if err := bus.Emit("embeddings.request", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if req.Error != "" {
		t.Fatalf("unexpected error: %s", req.Error)
	}
	if len(req.Vectors) != 2 {
		t.Fatalf("expected 2 vectors, got %d", len(req.Vectors))
	}
}

func TestEmbed_InlineImage(t *testing.T) {
	srv := newStubServer(t)
	defer srv.Close()
	_, bus := newTestPlugin(t, srv.URL, nil)

	pngBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	req := &events.EmbeddingsRequest{
		SchemaVersion: events.EmbeddingsRequestVersion,
		Inputs: []events.EmbeddingsInput{
			{Image: pngBytes, MimeType: "image/png"},
		},
	}
	if err := bus.Emit("embeddings.request", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if req.Error != "" {
		t.Fatalf("unexpected error: %s", req.Error)
	}
	if len(req.Vectors) != 1 {
		t.Fatalf("expected 1 vector, got %d", len(req.Vectors))
	}
	if len(srv.lastRequest.Inputs) != 1 {
		t.Fatalf("wire: expected 1 input, got %d", len(srv.lastRequest.Inputs))
	}
	c := srv.lastRequest.Inputs[0].Content
	if len(c) != 1 || c[0].Type != "image_url" || c[0].ImageURL == nil {
		t.Fatalf("wire: input not image-shaped: %+v", c)
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
	if c[0].ImageURL.URL != want {
		t.Fatalf("wire: image_url mismatch:\n got %q\nwant %q", c[0].ImageURL.URL, want)
	}
}

func TestEmbed_MixedTextAndImage(t *testing.T) {
	srv := newStubServer(t)
	defer srv.Close()
	_, bus := newTestPlugin(t, srv.URL, nil)

	req := &events.EmbeddingsRequest{
		SchemaVersion: events.EmbeddingsRequestVersion,
		Inputs: []events.EmbeddingsInput{
			{Text: "caption"},
			{Image: []byte{0x01, 0x02}, MimeType: "image/jpeg"},
			{Text: "more text"},
		},
	}
	if err := bus.Emit("embeddings.request", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if req.Error != "" {
		t.Fatalf("unexpected error: %s", req.Error)
	}
	if len(req.Vectors) != 3 {
		t.Fatalf("expected 3 vectors, got %d", len(req.Vectors))
	}
	if len(srv.lastRequest.Inputs) != 3 {
		t.Fatalf("wire: expected 3 inputs, got %d", len(srv.lastRequest.Inputs))
	}
	types := []string{
		srv.lastRequest.Inputs[0].Content[0].Type,
		srv.lastRequest.Inputs[1].Content[0].Type,
		srv.lastRequest.Inputs[2].Content[0].Type,
	}
	want := []string{"text", "image_url", "text"}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("wire: input[%d].type = %q, want %q", i, types[i], want[i])
		}
	}
}

func TestEmbed_ImageURI_Blob(t *testing.T) {
	srv := newStubServer(t)
	defer srv.Close()

	tmp := t.TempDir()
	store, err := blobs.New(filepath.Join(tmp, "blobs"), 0)
	if err != nil {
		t.Fatalf("blobs.New: %v", err)
	}
	imgBytes := []byte("\x89PNG-fake")
	h, err := store.Put(imgBytes, "image/png")
	if err != nil {
		t.Fatalf("blob put: %v", err)
	}

	// Inject a session workspace pointing at the same root.
	// SessionWorkspace.BlobsDir() returns RootDir/blobs — set RootDir = tmp.
	sess := &engine.SessionWorkspace{RootDir: tmp}
	_, bus := newTestPlugin(t, srv.URL, sess)

	req := &events.EmbeddingsRequest{
		SchemaVersion: events.EmbeddingsRequestVersion,
		Inputs: []events.EmbeddingsInput{
			{ImageURI: h.URI()},
		},
	}
	if err := bus.Emit("embeddings.request", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if req.Error != "" {
		t.Fatalf("unexpected error: %s", req.Error)
	}
	if len(req.Vectors) != 1 {
		t.Fatalf("expected 1 vector, got %d", len(req.Vectors))
	}
	c := srv.lastRequest.Inputs[0].Content
	if c[0].Type != "image_url" || c[0].ImageURL == nil {
		t.Fatalf("wire: input not image-shaped: %+v", c)
	}
	wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imgBytes)
	if c[0].ImageURL.URL != wantURL {
		t.Fatalf("wire: image_url mismatch:\n got %q\nwant %q", c[0].ImageURL.URL, wantURL)
	}
}

func TestEmbed_ImageURI_External(t *testing.T) {
	srv := newStubServer(t)
	defer srv.Close()
	_, bus := newTestPlugin(t, srv.URL, nil)

	req := &events.EmbeddingsRequest{
		SchemaVersion: events.EmbeddingsRequestVersion,
		Inputs: []events.EmbeddingsInput{
			{ImageURI: "https://example.com/img.png", MimeType: "image/png"},
		},
	}
	if err := bus.Emit("embeddings.request", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if req.Error != "" {
		t.Fatalf("unexpected error: %s", req.Error)
	}
	c := srv.lastRequest.Inputs[0].Content
	if c[0].Type != "image_url" || c[0].ImageURL == nil {
		t.Fatalf("wire: input not image-shaped: %+v", c)
	}
	if c[0].ImageURL.URL != "https://example.com/img.png" {
		t.Fatalf("wire: external URL not passed through: %q", c[0].ImageURL.URL)
	}
}

func TestEmbed_BlobURI_NoSession(t *testing.T) {
	srv := newStubServer(t)
	defer srv.Close()
	// session==nil → blob URIs cannot be resolved.
	_, bus := newTestPlugin(t, srv.URL, nil)

	req := &events.EmbeddingsRequest{
		SchemaVersion: events.EmbeddingsRequestVersion,
		Inputs: []events.EmbeddingsInput{
			{ImageURI: "nexus-blob:deadbeef"},
		},
	}
	if err := bus.Emit("embeddings.request", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !strings.Contains(req.Error, "blob URI cannot be resolved") {
		t.Fatalf("expected clear blob-resolution error, got: %q", req.Error)
	}
}

func TestEmbed_ImageWithoutMimeType(t *testing.T) {
	_, bus := newTestPlugin(t, "http://127.0.0.1:1", nil)

	req := &events.EmbeddingsRequest{
		SchemaVersion: events.EmbeddingsRequestVersion,
		Inputs: []events.EmbeddingsInput{
			{Image: []byte{0x01}},
		},
	}
	if err := bus.Emit("embeddings.request", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !strings.Contains(req.Error, "MimeType") {
		t.Fatalf("expected MimeType validation error, got: %q", req.Error)
	}
}

func TestEmbed_RequestShape_FloatEmbeddings(t *testing.T) {
	srv := newStubServer(t)
	defer srv.Close()
	_, bus := newTestPlugin(t, srv.URL, nil)

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

	// embedding_types must be exactly ["float"] per the documented contract.
	if got := srv.lastRequest.EmbeddingTypes; len(got) != 1 || got[0] != "float" {
		t.Errorf("embedding_types = %v, want [\"float\"]", got)
	}
}

func TestEmbed_UsageReported(t *testing.T) {
	srv := newStubServer(t)
	defer srv.Close()
	_, bus := newTestPlugin(t, srv.URL, nil)

	req := &events.EmbeddingsRequest{
		SchemaVersion: events.EmbeddingsRequestVersion,
		Texts:         []string{"hello"},
	}
	if err := bus.Emit("embeddings.request", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if req.Usage.PromptTokens != 7 || req.Usage.TotalTokens != 7 {
		t.Errorf("usage not surfaced: got %+v, want PromptTokens/TotalTokens=7", req.Usage)
	}
}
