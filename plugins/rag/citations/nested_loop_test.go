package citations

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/delegate"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/posture"
	knowledgesearch "github.com/frankbardon/nexus/plugins/tools/knowledge_search"
)

const nestedCite = `Tokens last a day.<cite source="/docs/auth.md" chunk="3"/>`

// nestedRig wires the real nexus.tool.knowledge_search plugin and the real
// pkg/delegate loop onto one bus with rag.citations, behind a fake provider
// that echoes request metadata onto its response the way every real
// provider does. The delegated loop's first request answers with a
// knowledge_search call; its second answers with a cited span.
type nestedRig struct {
	bus   engine.EventBus
	plug  *Plugin
	mu    sync.Mutex
	cited []events.CitedResponse
}

func newNestedRig(t *testing.T) *nestedRig {
	t.Helper()
	p, bus, cleanup := newTestPlugin(t, "tag", true)
	t.Cleanup(cleanup)
	r := &nestedRig{bus: bus, plug: p}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ks := knowledgesearch.New()
	if err := ks.Init(engine.PluginContext{
		Bus:    bus,
		Logger: logger,
		Config: map[string]any{"namespaces": []any{"docs"}},
	}); err != nil {
		t.Fatalf("knowledge_search init: %v", err)
	}
	t.Cleanup(func() { _ = ks.Shutdown(context.Background()) })

	// Fake embeddings provider and vector store: one chunk, always.
	t.Cleanup(bus.Subscribe("embeddings.request", func(ev engine.Event[any]) {
		if req, ok := ev.Payload.(*events.EmbeddingsRequest); ok {
			req.Vectors = [][]float32{{1, 0}}
		}
	}))
	t.Cleanup(bus.Subscribe("vector.query", func(ev engine.Event[any]) {
		if q, ok := ev.Payload.(*events.VectorQuery); ok {
			q.Matches = []events.VectorMatch{{
				ID:         "d-1",
				Content:    "Tokens expire after 24 hours.",
				Similarity: 0.9,
				Metadata:   map[string]string{"source": "/docs/auth.md", "chunk_idx": "3"},
			}}
		}
	}))

	t.Cleanup(bus.Subscribe("llm.response.cited", func(ev engine.Event[any]) {
		if c, ok := ev.Payload.(events.CitedResponse); ok {
			r.mu.Lock()
			r.cited = append(r.cited, c)
			r.mu.Unlock()
		}
	}, engine.WithPriority(99)))

	calls := 0
	t.Cleanup(bus.Subscribe("llm.request", func(ev engine.Event[any]) {
		req, ok := ev.Payload.(events.LLMRequest)
		if !ok {
			return
		}
		if kind, _ := req.Metadata[engine.MetaTaskKind].(string); kind != engine.TaskKindDelegate {
			return
		}
		calls++
		resp := events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Metadata:      maps.Clone(req.Metadata),
		}
		if calls == 1 {
			resp.ToolCalls = []events.ToolCallRequest{{
				ID: "call-1", Name: "knowledge_search", Arguments: `{"query":"token expiry"}`,
			}}
		} else {
			resp.Content = nestedCite
		}
		_, _ = engine.PublishLLMResponse(bus, resp)
	}))
	return r
}

func (r *nestedRig) runDelegate(t *testing.T, parentTurn string) {
	t.Helper()
	reg := posture.NewRegistry()
	_ = reg.Register(posture.AgentPosture{
		Name:          "researcher",
		Model:         posture.ModelConfig{Model: "claude-opus-4-7"},
		DefaultBudget: posture.ResourceBudget{Timeout: 5 * time.Second},
	})
	rt := &delegate.Runtime{
		Registry: reg,
		Bus:      r.bus,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxDepth: 3,
	}
	out, err := rt.Run(context.Background(), delegate.Input{
		Posture:    "researcher",
		Task:       "How long do tokens last?",
		ParentTurn: parentTurn,
	})
	if err != nil {
		t.Fatalf("delegate run: %v", err)
	}
	if out.Status != delegate.StatusSuccess {
		t.Fatalf("delegate status = %v (%s), want success", out.Status, out.Error)
	}
}

func (r *nestedRig) last(t *testing.T) events.CitedResponse {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cited) == 0 {
		t.Fatal("no llm.response.cited emitted")
	}
	return r.cited[len(r.cited)-1]
}

// TestNestedLoop_KnowledgeSearchCitationValidates drives knowledge_search
// from inside a delegated loop. Its tool call carries the loop's private
// "delegate_<id>" turn, while the loop's llm.response carries the PARENT
// turn under engine.MetaTurnID; the citation must still validate against
// the chunk that call retrieved, and the cited event must name the parent
// turn a transport saw start.
func TestNestedLoop_KnowledgeSearchCitationValidates(t *testing.T) {
	r := newNestedRig(t)
	r.runDelegate(t, "turn-parent")

	got := r.last(t)
	if len(got.Citations) != 1 {
		t.Fatalf("got %d citations, want 1 (nested retrieval not matched): %+v", len(got.Citations), got)
	}
	if c := got.Citations[0]; c.Source != "/docs/auth.md" || c.ChunkIdx != "3" || c.TrustTier != "" {
		t.Fatalf("citation = %+v, want validated /docs/auth.md#3", c)
	}
	if got.TurnID != "turn-parent" {
		t.Fatalf("cited TurnID = %q, want turn-parent", got.TurnID)
	}
}

// TestNestedLoop_RetrievalDoesNotLeakIntoParentTurn pins the scoping choice:
// a nested loop's retrievals validate that loop's responses only. The
// parent's own llm.response cites with the parent's own retrievals, never
// with sources only a child loop saw.
func TestNestedLoop_RetrievalDoesNotLeakIntoParentTurn(t *testing.T) {
	r := newNestedRig(t)
	r.runDelegate(t, "turn-parent")

	_, _ = engine.PublishLLMResponse(r.bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       nestedCite,
		Metadata:      map[string]any{engine.MetaTurnID: "turn-parent"},
	})
	got := r.last(t)
	if len(got.Citations) != 0 {
		t.Fatalf("parent response validated a child-only source: %+v", got.Citations)
	}
}

// TestNestedLoop_IndexDroppedAtParentTurnEnd: no agent.turn.end ever names
// a nested loop's private turn, so its retrieval index is released when the
// parent turn it served ends.
func TestNestedLoop_IndexDroppedAtParentTurnEnd(t *testing.T) {
	r := newNestedRig(t)
	r.runDelegate(t, "turn-parent")

	r.plug.mu.Lock()
	n := len(r.plug.turns)
	r.plug.mu.Unlock()
	if n == 0 {
		t.Fatal("nested retrieval was not indexed")
	}

	_ = r.bus.Emit("agent.turn.end", events.TurnInfo{TurnID: "turn-parent"})

	r.plug.mu.Lock()
	defer r.plug.mu.Unlock()
	if len(r.plug.turns) != 0 {
		t.Fatalf("retrieval index not released at parent turn end: %v", r.plug.turns)
	}
}
