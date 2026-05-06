package local

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

func newTestPlugin(t *testing.T) (*Plugin, engine.EventBus, func()) {
	t.Helper()
	bus := engine.NewEventBus()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{Bus: bus, Logger: logger, Config: map[string]any{}}); err != nil {
		t.Fatalf("init: %v", err)
	}
	return p, bus, func() { _ = p.Shutdown(context.Background()) }
}

func TestRerankRanksTermOverlap(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t)
	t.Cleanup(cleanup)

	docs := []events.RerankDoc{
		{ID: "irrelevant", Content: "the lazy dog sleeps under the apple tree"},
		{ID: "match", Content: "ENOENT means the file does not exist on disk"},
		{ID: "partial", Content: "errors include EAGAIN and ENFILE conditions"},
	}
	req := &events.RerankRequest{SchemaVersion: events.RerankRequestVersion, Query: "ENOENT file missing", Docs: docs, TopN: 3}
	if err := bus.Emit("reranker.rerank", req); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if req.Error != "" {
		t.Fatalf("rerank: %s", req.Error)
	}
	if len(req.Results) != 3 {
		t.Fatalf("got %d results, want 3", len(req.Results))
	}
	if req.Results[0].ID != "match" {
		t.Fatalf("top hit = %q, want match", req.Results[0].ID)
	}
}

func TestRerankRespectsTopN(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t)
	t.Cleanup(cleanup)

	docs := []events.RerankDoc{
		{ID: "1", Content: "alpha beta"},
		{ID: "2", Content: "alpha gamma"},
		{ID: "3", Content: "delta epsilon"},
	}
	req := &events.RerankRequest{SchemaVersion: events.RerankRequestVersion, Query: "alpha", Docs: docs, TopN: 1}
	_ = bus.Emit("reranker.rerank", req)
	if len(req.Results) != 1 {
		t.Fatalf("TopN=1 returned %d results", len(req.Results))
	}
}

func TestRerankCarriesIndex(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t)
	t.Cleanup(cleanup)

	docs := []events.RerankDoc{
		{ID: "a", Content: "irrelevant"},
		{ID: "b", Content: "the answer is here"},
		{ID: "c", Content: "irrelevant"},
	}
	req := &events.RerankRequest{SchemaVersion: events.RerankRequestVersion, Query: "answer here", Docs: docs}
	_ = bus.Emit("reranker.rerank", req)
	if req.Results[0].Index != 1 {
		t.Fatalf("top hit Index = %d, want 1 (b)", req.Results[0].Index)
	}
}

func TestRerankEmptyDocs(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t)
	t.Cleanup(cleanup)

	req := &events.RerankRequest{SchemaVersion: events.RerankRequestVersion, Query: "x", Docs: nil}
	_ = bus.Emit("reranker.rerank", req)
	if req.Error != "" {
		t.Fatalf("empty docs should not error, got %q", req.Error)
	}
	if len(req.Results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(req.Results))
	}
}

func TestRerankIgnoresAlreadyClaimed(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t)
	t.Cleanup(cleanup)

	req := &events.RerankRequest{SchemaVersion: events.RerankRequestVersion, Query: "x",
		Docs:     []events.RerankDoc{{ID: "a", Content: "x"}},
		Provider: "someone.else",
	}
	_ = bus.Emit("reranker.rerank", req)
	if req.Provider != "someone.else" {
		t.Fatalf("Provider was overwritten: %q", req.Provider)
	}
	if req.Results != nil {
		t.Fatalf("Results was populated despite pre-claimed Provider")
	}
}
