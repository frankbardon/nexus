package hybrid

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// reverseReranker is a stub search.reranker that flips the order of its
// inputs. Lets the hybrid integration test prove the reranker pass actually
// dominates the final ordering.
type reverseReranker struct {
	bus engine.EventBus
}

func (r *reverseReranker) install() func() {
	return r.bus.Subscribe("reranker.rerank", func(ev engine.Event[any]) {
		req := ev.Payload.(*events.RerankRequest)
		req.Provider = "stub.reranker"
		results := make([]events.RerankResult, len(req.Docs))
		for i := range req.Docs {
			results[i] = events.RerankResult{
				ID:    req.Docs[len(req.Docs)-1-i].ID,
				Index: len(req.Docs) - 1 - i,
				Score: float32(len(req.Docs) - i),
			}
		}
		req.Results = results
	}, engine.WithPriority(50))
}

func newOrchestratorWithReranker(t *testing.T, capabilities map[string][]string) (*Plugin, engine.EventBus, func()) {
	t.Helper()
	bus := engine.NewEventBus()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Bus:    bus,
		Logger: logger,
		Config: map[string]any{
			"fusion":     "rrf",
			"retrieve_k": 10,
			"fuse_to":    5,
			"reranker":   map[string]any{"enabled": true},
		},
		Capabilities: capabilities,
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	return p, bus, func() { _ = p.Shutdown(context.Background()) }
}

func TestHybridAppliesReranker(t *testing.T) {
	caps := map[string][]string{"search.reranker": {"stub.reranker"}}
	_, bus, cleanup := newOrchestratorWithReranker(t, caps)
	t.Cleanup(cleanup)

	t.Cleanup((&stubEmbeddings{bus: bus}).install())
	t.Cleanup((&stubVector{bus: bus, matches: map[string][]events.VectorMatch{
		"docs": {
			{ID: "a", Content: "alpha", Similarity: 0.9},
			{ID: "b", Content: "beta", Similarity: 0.8},
			{ID: "c", Content: "gamma", Similarity: 0.7},
		},
	}}).install())
	t.Cleanup((&stubLexical{bus: bus, matches: map[string][]events.LexicalMatch{
		"docs": {{ID: "b", Content: "beta", Score: 5.0}},
	}}).install())
	t.Cleanup((&reverseReranker{bus: bus}).install())

	q := &events.HybridQuery{SchemaVersion: events.HybridQueryVersion, Namespace: "docs", Query: "x", K: 5}
	_ = bus.Emit("hybrid.query", q)
	if q.Error != "" {
		t.Fatalf("error: %s", q.Error)
	}
	if len(q.Matches) == 0 {
		t.Fatalf("got zero matches")
	}
	// Without reranker, b would be top (appears in both backends). With the
	// reverseReranker flipping the fused order, the original last hit moves
	// to the front. So q.Matches[0].ID should NOT be "b".
	if q.Matches[0].ID == "b" {
		t.Fatalf("reranker did not modify ordering: top hit still %q", q.Matches[0].ID)
	}
}

func TestHybridSurvivesRerankerFailure(t *testing.T) {
	caps := map[string][]string{"search.reranker": {"stub.reranker"}}
	_, bus, cleanup := newOrchestratorWithReranker(t, caps)
	t.Cleanup(cleanup)

	t.Cleanup((&stubEmbeddings{bus: bus}).install())
	t.Cleanup((&stubVector{bus: bus, matches: map[string][]events.VectorMatch{
		"docs": {{ID: "a", Content: "x", Similarity: 0.9}, {ID: "b", Content: "y", Similarity: 0.5}},
	}}).install())
	t.Cleanup((&stubLexical{bus: bus, matches: map[string][]events.LexicalMatch{}}).install())

	// Reranker that always errors.
	t.Cleanup(bus.Subscribe("reranker.rerank", func(ev engine.Event[any]) {
		req := ev.Payload.(*events.RerankRequest)
		req.Provider = "stub.fail"
		req.Error = "simulated failure"
	}, engine.WithPriority(50)))

	q := &events.HybridQuery{SchemaVersion: events.HybridQueryVersion, Namespace: "docs", Query: "x", K: 5}
	_ = bus.Emit("hybrid.query", q)
	if q.Error != "" {
		t.Fatalf("hybrid should swallow reranker errors: got %q", q.Error)
	}
	if len(q.Matches) != 2 {
		t.Fatalf("expected 2 matches even with rerank failure, got %d", len(q.Matches))
	}
}
