package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/blobs"
	"github.com/frankbardon/nexus/pkg/events"
)

// minPNG is the 8-byte PNG signature plus a tiny trailing chunk — enough
// for tests that only care about routing, not pixels.
var minPNG = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x00}

// busCapture wraps a real engine.EventBus and records every tool.result so
// tests can assert what the plugin emitted without booting the full engine.
type busCapture struct {
	engine.EventBus

	mu      sync.Mutex
	results []events.ToolResult
}

func newBusCapture(t *testing.T) *busCapture {
	t.Helper()
	bus := engine.NewEventBus()
	bc := &busCapture{EventBus: bus}
	bus.Subscribe("tool.result", func(e engine.Event[any]) {
		if r, ok := e.Payload.(events.ToolResult); ok {
			bc.mu.Lock()
			bc.results = append(bc.results, r)
			bc.mu.Unlock()
		}
	})
	return bc
}

func (b *busCapture) lastResult(t *testing.T) events.ToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = b.EventBus.Drain(ctx)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.results) == 0 {
		t.Fatal("no tool.result emitted")
	}
	return b.results[len(b.results)-1]
}

// makePlugin builds a plugin scoped to a temp blob dir + given inline cutoff
// + given page-image provider config. The HTTP client is wired up like Init
// does so the provider round-trip works end-to-end.
func makePlugin(t *testing.T, inlineCutoff int64, prov *pageImageProvider) (*Plugin, *busCapture) {
	t.Helper()
	dir := t.TempDir()
	bus := newBusCapture(t)
	store, err := blobs.New(filepath.Join(dir, "blobs"), 0)
	if err != nil {
		t.Fatalf("blobs.New: %v", err)
	}
	p := &Plugin{
		bus:              bus,
		client:           &http.Client{Timeout: 5 * time.Second},
		pageImage:        prov,
		blobStore:        store,
		blobInlineCutoff: inlineCutoff,
		maxSize:          5 * 1024 * 1024,
	}
	return p, bus
}

func TestFetchPageImage_Unconfigured(t *testing.T) {
	p, bus := makePlugin(t, 1<<20, nil)
	p.handleFetchPageImage(events.ToolCall{ID: "fpi-1", Name: "fetch_page_image", Arguments: map[string]any{"url": "https://example.com/"}})

	r := bus.lastResult(t)
	if r.Error == "" {
		t.Fatalf("expected error when screenshot provider unconfigured")
	}
	if !contains(r.Error, "screenshot_provider") {
		t.Errorf("error should mention screenshot_provider config; got %q", r.Error)
	}
	if len(r.OutputParts) != 0 {
		t.Errorf("expected no OutputParts on error; got %d", len(r.OutputParts))
	}
}

func TestFetchPageImage_Configured(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotBody   map[string]any
		gotAuth   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(200)
		_, _ = w.Write(minPNG)
	}))
	defer srv.Close()

	prov := &pageImageProvider{
		url:          srv.URL + "/screenshot",
		method:       http.MethodPost,
		urlParamName: "url",
	}
	p, bus := makePlugin(t, 1<<20, prov)
	p.handleFetchPageImage(events.ToolCall{ID: "fpi-2", Name: "fetch_page_image", Arguments: map[string]any{"url": "https://example.com/page"}})

	r := bus.lastResult(t)
	if r.Error != "" {
		t.Fatalf("unexpected error: %q", r.Error)
	}
	if len(r.OutputParts) != 1 {
		t.Fatalf("OutputParts len: got %d want 1", len(r.OutputParts))
	}
	part := r.OutputParts[0]
	if part.Type != "image" || part.MimeType != "image/png" {
		t.Errorf("unexpected part shape: %+v", part)
	}
	if !bytes.Equal(part.Data, minPNG) {
		t.Errorf("part.Data does not match server fixture")
	}
	if gotMethod != "POST" {
		t.Errorf("provider method: got %q want POST", gotMethod)
	}
	if gotPath != "/screenshot" {
		t.Errorf("provider path: got %q want /screenshot", gotPath)
	}
	if gotBody["url"] != "https://example.com/page" {
		t.Errorf("provider body url: got %v want https://example.com/page", gotBody["url"])
	}
	if gotAuth != "" {
		t.Errorf("expected no Authorization header (no api_key_env set); got %q", gotAuth)
	}
}

func TestFetchPageImage_Blob(t *testing.T) {
	bigData := append(append([]byte{}, minPNG...), bytes.Repeat([]byte{0xAA}, 4096)...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(200)
		_, _ = w.Write(bigData)
	}))
	defer srv.Close()

	prov := &pageImageProvider{
		url:          srv.URL,
		method:       http.MethodPost,
		urlParamName: "url",
	}
	p, bus := makePlugin(t, 1024, prov)
	p.handleFetchPageImage(events.ToolCall{ID: "fpi-3", Name: "fetch_page_image", Arguments: map[string]any{"url": "https://example.com/big"}})

	r := bus.lastResult(t)
	if r.Error != "" {
		t.Fatalf("unexpected error: %q", r.Error)
	}
	if len(r.OutputParts) != 1 {
		t.Fatalf("OutputParts len: got %d want 1", len(r.OutputParts))
	}
	part := r.OutputParts[0]
	if part.URI == "" || blobs.SHAFromURI(part.URI) == "" {
		t.Fatalf("expected nexus-blob URI; got %q", part.URI)
	}
	if len(part.Data) != 0 {
		t.Errorf("expected blob path with no inline Data; got %d inline bytes", len(part.Data))
	}
	got, _, err := p.blobStore.Get(blobs.SHAFromURI(part.URI))
	if err != nil {
		t.Fatalf("blob store Get: %v", err)
	}
	if !bytes.Equal(got, bigData) {
		t.Errorf("blob bytes do not match server fixture")
	}
}

func TestFetchPageImage_GETWithApiKey(t *testing.T) {
	t.Setenv("FAKE_SCREENSHOT_KEY", "k-12345")
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(200)
		_, _ = w.Write(minPNG)
	}))
	defer srv.Close()

	prov := &pageImageProvider{
		url:          srv.URL,
		method:       http.MethodGet,
		apiKeyEnv:    "FAKE_SCREENSHOT_KEY",
		urlParamName: "target",
		requestExtras: map[string]any{
			"format": "png",
		},
	}
	p, bus := makePlugin(t, 1<<20, prov)
	p.handleFetchPageImage(events.ToolCall{ID: "fpi-4", Name: "fetch_page_image", Arguments: map[string]any{"url": "https://example.org/"}})

	r := bus.lastResult(t)
	if r.Error != "" {
		t.Fatalf("unexpected error: %q", r.Error)
	}
	if got := gotQuery.Get("target"); got != "https://example.org/" {
		t.Errorf("query target: got %q want https://example.org/", got)
	}
	if got := gotQuery.Get("api_key"); got != "k-12345" {
		t.Errorf("query api_key: got %q want k-12345", got)
	}
	if got := gotQuery.Get("format"); got != "png" {
		t.Errorf("query format: got %q want png", got)
	}
}

func TestFetchPageImage_ProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte("provider down"))
	}))
	defer srv.Close()

	prov := &pageImageProvider{url: srv.URL, method: http.MethodPost, urlParamName: "url"}
	p, bus := makePlugin(t, 1<<20, prov)
	p.handleFetchPageImage(events.ToolCall{ID: "fpi-5", Name: "fetch_page_image", Arguments: map[string]any{"url": "https://example.com/"}})

	r := bus.lastResult(t)
	if r.Error == "" {
		t.Fatalf("expected error from 503 provider")
	}
	if !contains(r.Error, "503") {
		t.Errorf("expected status 503 in error; got %q", r.Error)
	}
}

func contains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }
