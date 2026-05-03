package cohere

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

func newServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func newPluginWithServer(t *testing.T, srv *httptest.Server) (*Plugin, engine.EventBus, func()) {
	t.Helper()
	bus := engine.NewEventBus()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New().(*Plugin)
	cfg := map[string]any{
		"api_key":  "test-key",
		"api_base": srv.URL,
		"model":    "rerank-test",
	}
	if err := p.Init(engine.PluginContext{Bus: bus, Logger: logger, Config: cfg}); err != nil {
		t.Fatalf("init: %v", err)
	}
	return p, bus, func() { _ = p.Shutdown(context.Background()) }
}

func TestRerankSuccessfulResponse(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "missing bearer", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		// Echo the docs ranked in reverse order so we can assert the
		// reranker plugin honors the server's ordering, not the input order.
		docs := body["documents"].([]any)
		results := make([]map[string]any, len(docs))
		for i := range docs {
			results[i] = map[string]any{
				"index":           len(docs) - 1 - i,
				"relevance_score": 1.0 - float64(i)*0.1,
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	})
	_, bus, cleanup := newPluginWithServer(t, srv)
	t.Cleanup(cleanup)

	docs := []events.RerankDoc{
		{ID: "first", Content: "alpha"},
		{ID: "second", Content: "beta"},
		{ID: "third", Content: "gamma"},
	}
	req := &events.RerankRequest{Query: "test", Docs: docs, TopN: 3}
	if err := bus.Emit("reranker.rerank", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if req.Error != "" {
		t.Fatalf("error: %s", req.Error)
	}
	if len(req.Results) != 3 {
		t.Fatalf("got %d results, want 3", len(req.Results))
	}
	if req.Results[0].ID != "third" {
		t.Fatalf("top hit ID = %q, want third (server reversed)", req.Results[0].ID)
	}
	if req.Results[0].Index != 2 {
		t.Fatalf("top hit Index = %d, want 2", req.Results[0].Index)
	}
}

func TestRerankHTTPErrorSurfacedAsError(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"rate limited"}`, http.StatusTooManyRequests)
	})
	_, bus, cleanup := newPluginWithServer(t, srv)
	t.Cleanup(cleanup)

	req := &events.RerankRequest{
		Query: "test",
		Docs:  []events.RerankDoc{{ID: "a", Content: "x"}},
	}
	_ = bus.Emit("reranker.rerank", req)
	if req.Error == "" {
		t.Fatalf("expected error on HTTP 429")
	}
}

func TestRerankRespectsAlreadyClaimedProvider(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("server should not be hit when Provider is pre-claimed")
		w.WriteHeader(http.StatusOK)
	})
	_, bus, cleanup := newPluginWithServer(t, srv)
	t.Cleanup(cleanup)

	req := &events.RerankRequest{
		Query:    "test",
		Docs:     []events.RerankDoc{{ID: "a", Content: "x"}},
		Provider: "someone.else",
	}
	_ = bus.Emit("reranker.rerank", req)
}
