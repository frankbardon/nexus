package jina

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

func newPluginWithServer(t *testing.T, srv *httptest.Server) (*Plugin, engine.EventBus, func()) {
	t.Helper()
	bus := engine.NewEventBus()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New().(*Plugin)
	cfg := map[string]any{
		"api_key":  "test-key",
		"api_base": srv.URL,
		"model":    "jina-test",
	}
	if err := p.Init(engine.PluginContext{Bus: bus, Logger: logger, Config: cfg}); err != nil {
		t.Fatalf("init: %v", err)
	}
	return p, bus, func() { _ = p.Shutdown(context.Background()) }
}

func TestRerankSuccessfulResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "missing bearer", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		docs := body["documents"].([]any)
		results := make([]map[string]any, len(docs))
		for i := range docs {
			results[i] = map[string]any{
				"index":           i,
				"relevance_score": 1.0 - float64(i)*0.1,
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	t.Cleanup(srv.Close)
	_, bus, cleanup := newPluginWithServer(t, srv)
	t.Cleanup(cleanup)

	docs := []events.RerankDoc{
		{ID: "a", Content: "alpha"},
		{ID: "b", Content: "beta"},
	}
	req := &events.RerankRequest{SchemaVersion: events.RerankRequestVersion, Query: "test", Docs: docs, TopN: 2}
	_ = bus.Emit("reranker.rerank", req)
	if req.Error != "" {
		t.Fatalf("error: %s", req.Error)
	}
	if len(req.Results) != 2 || req.Results[0].ID != "a" {
		t.Fatalf("got %v", req.Results)
	}
}

func TestRerankHTTPErrorSurfacedAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	_, bus, cleanup := newPluginWithServer(t, srv)
	t.Cleanup(cleanup)

	req := &events.RerankRequest{SchemaVersion: events.RerankRequestVersion, Query: "test",
		Docs: []events.RerankDoc{{ID: "a", Content: "x"}},
	}
	_ = bus.Emit("reranker.rerank", req)
	if req.Error == "" {
		t.Fatalf("expected error on HTTP 500")
	}
}
