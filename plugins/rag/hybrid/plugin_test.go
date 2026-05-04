package hybrid

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// stubEmbeddings stands in for embeddings.provider during orchestration tests.
type stubEmbeddings struct {
	bus engine.EventBus
}

func (s *stubEmbeddings) install() func() {
	return s.bus.Subscribe("embeddings.request", func(ev engine.Event[any]) {
		req := ev.Payload.(*events.EmbeddingsRequest)
		req.Vectors = make([][]float32, len(req.Texts))
		for i := range req.Texts {
			req.Vectors[i] = []float32{1, 0, 0}
		}
	}, engine.WithPriority(50))
}

// stubVector returns canned vector matches for a namespace.
type stubVector struct {
	bus     engine.EventBus
	matches map[string][]events.VectorMatch
}

func (s *stubVector) install() func() {
	return s.bus.Subscribe("vector.query", func(ev engine.Event[any]) {
		q := ev.Payload.(*events.VectorQuery)
		q.Provider = "stub.vector"
		q.Matches = s.matches[q.Namespace]
	}, engine.WithPriority(50))
}

// stubLexical returns canned lexical matches for a namespace.
type stubLexical struct {
	bus     engine.EventBus
	matches map[string][]events.LexicalMatch
}

func (s *stubLexical) install() func() {
	return s.bus.Subscribe("lexical.query", func(ev engine.Event[any]) {
		q := ev.Payload.(*events.LexicalQuery)
		q.Provider = "stub.lexical"
		q.Matches = s.matches[q.Namespace]
	}, engine.WithPriority(50))
}

func newOrchestrator(t *testing.T) (*Plugin, engine.EventBus, func()) {
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
		},
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	return p, bus, func() { _ = p.Shutdown(context.Background()) }
}

func TestHybridFusesVectorAndLexical(t *testing.T) {
	_, bus, cleanup := newOrchestrator(t)
	t.Cleanup(cleanup)

	emb := &stubEmbeddings{bus: bus}
	vec := &stubVector{bus: bus, matches: map[string][]events.VectorMatch{
		"docs": {
			{ID: "a", Content: "alpha", Similarity: 0.9, Metadata: map[string]string{"source": "/a"}},
			{ID: "b", Content: "beta", Similarity: 0.8, Metadata: map[string]string{"source": "/b"}},
		},
	}}
	lex := &stubLexical{bus: bus, matches: map[string][]events.LexicalMatch{
		"docs": {
			{ID: "b", Content: "beta", Score: 5.0, Metadata: map[string]string{"source": "/b"}},
			{ID: "c", Content: "gamma", Score: 3.0, Metadata: map[string]string{"source": "/c"}},
		},
	}}
	t.Cleanup(emb.install())
	t.Cleanup(vec.install())
	t.Cleanup(lex.install())

	q := &events.HybridQuery{Namespace: "docs", Query: "beta", K: 5}
	if err := bus.Emit("hybrid.query", q); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if q.Error != "" {
		t.Fatalf("query error: %s", q.Error)
	}
	if q.Provider != pluginID {
		t.Fatalf("Provider = %q, want %q", q.Provider, pluginID)
	}
	if len(q.Matches) != 3 {
		t.Fatalf("got %d fused matches, want 3 (a, b, c)", len(q.Matches))
	}
	// Doc b appears in both backends → top.
	if q.Matches[0].ID != "b" {
		t.Fatalf("top hit = %q, want b", q.Matches[0].ID)
	}
	// Doc b should carry both source tags.
	if len(q.Matches[0].Sources) != 2 {
		t.Fatalf("sources for b = %v, want 2 entries", q.Matches[0].Sources)
	}
}

func TestHybridUsesPreEmbeddedVector(t *testing.T) {
	_, bus, cleanup := newOrchestrator(t)
	t.Cleanup(cleanup)

	embCalls := 0
	t.Cleanup(bus.Subscribe("embeddings.request", func(ev engine.Event[any]) {
		embCalls++
	}, engine.WithPriority(50)))

	vec := &stubVector{bus: bus, matches: map[string][]events.VectorMatch{
		"docs": {{ID: "a", Content: "x", Similarity: 0.9}},
	}}
	lex := &stubLexical{bus: bus, matches: map[string][]events.LexicalMatch{}}
	t.Cleanup(vec.install())
	t.Cleanup(lex.install())

	q := &events.HybridQuery{
		Namespace: "docs",
		Query:     "ignored",
		Vector:    []float32{1, 2, 3},
		K:         5,
	}
	_ = bus.Emit("hybrid.query", q)
	if embCalls != 0 {
		t.Fatalf("expected zero embedding requests when Vector is supplied, got %d", embCalls)
	}
}

func TestHybridSurvivesLexicalMissing(t *testing.T) {
	_, bus, cleanup := newOrchestrator(t)
	t.Cleanup(cleanup)

	emb := &stubEmbeddings{bus: bus}
	vec := &stubVector{bus: bus, matches: map[string][]events.VectorMatch{
		"docs": {{ID: "a", Content: "x", Similarity: 0.9}},
	}}
	t.Cleanup(emb.install())
	t.Cleanup(vec.install())
	// Intentionally no lexical handler installed — orchestrator should
	// proceed with vector-only results.

	q := &events.HybridQuery{Namespace: "docs", Query: "x", K: 5}
	_ = bus.Emit("hybrid.query", q)
	if q.Error != "" {
		t.Fatalf("hybrid should tolerate missing lexical: %s", q.Error)
	}
	if len(q.Matches) != 1 || q.Matches[0].ID != "a" {
		t.Fatalf("got %v, want single match for a", q.Matches)
	}
}

func TestHybridRespectsK(t *testing.T) {
	_, bus, cleanup := newOrchestrator(t)
	t.Cleanup(cleanup)

	emb := &stubEmbeddings{bus: bus}
	vec := &stubVector{bus: bus, matches: map[string][]events.VectorMatch{
		"docs": {
			{ID: "a", Similarity: 0.9}, {ID: "b", Similarity: 0.8},
			{ID: "c", Similarity: 0.7}, {ID: "d", Similarity: 0.6},
		},
	}}
	lex := &stubLexical{bus: bus, matches: map[string][]events.LexicalMatch{}}
	t.Cleanup(emb.install())
	t.Cleanup(vec.install())
	t.Cleanup(lex.install())

	q := &events.HybridQuery{Namespace: "docs", Query: "x", K: 2}
	_ = bus.Emit("hybrid.query", q)
	if len(q.Matches) != 2 {
		t.Fatalf("K=2 returned %d matches, want 2", len(q.Matches))
	}
}
