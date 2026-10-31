package citations

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/memory/vector"
)

// recallRig wires the real nexus.memory.vector plugin onto one bus with
// rag.citations, behind a fake embeddings provider and a vector store that
// always recalls one compaction summary.
func recallRig(t *testing.T) (engine.EventBus, *events.CitedResponse) {
	t.Helper()
	_, bus, cleanup := newTestPlugin(t, "tag", true)
	t.Cleanup(cleanup)

	t.Cleanup(bus.Subscribe("embeddings.request", func(ev engine.Event[any]) {
		if req, ok := ev.Payload.(*events.EmbeddingsRequest); ok {
			req.Vectors = [][]float32{{1, 0}}
		}
	}))
	t.Cleanup(bus.Subscribe("vector.query", func(ev engine.Event[any]) {
		if q, ok := ev.Payload.(*events.VectorQuery); ok {
			q.Matches = []events.VectorMatch{{
				ID:         "mem-1",
				Content:    "The user prefers metric units.",
				Similarity: 0.9,
				Metadata:   map[string]string{"source": "compaction"},
			}}
		}
	}))

	vp := vector.New()
	if err := vp.Init(engine.PluginContext{
		Bus:    bus,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: map[string]any{"namespace": "mem", "auto_store_compaction": false},
	}); err != nil {
		t.Fatalf("vector init: %v", err)
	}
	t.Cleanup(func() { _ = vp.Shutdown(context.Background()) })

	cited, unsub := captureCited(t, bus)
	t.Cleanup(unsub)
	return bus, cited
}

const recallCite = `Use kilometres.<cite source="compaction"/>`

// TestVectorRecall_CitationValidates: memory.vector recalls on io.input,
// before any turn exists, and the citation contract tells the model it may
// cite "memory recall". The recall must therefore be indexed under the turn
// that starts next, so a strict-mode citation of the recalled entry
// survives instead of being stripped.
func TestVectorRecall_CitationValidates(t *testing.T) {
	bus, cited := recallRig(t)

	_ = bus.Emit("io.input", events.UserInput{SchemaVersion: events.UserInputVersion, Content: "how far is it?"})
	_ = bus.Emit("agent.turn.start", events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1"})
	_, _ = engine.PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       recallCite,
		Metadata:      map[string]any{engine.MetaTurnID: "turn-1"},
	})

	if len(cited.Citations) != 1 {
		t.Fatalf("got %d citations, want 1 (recall not indexed for the turn): %+v", len(cited.Citations), cited)
	}
	if c := cited.Citations[0]; c.Source != "compaction" || c.TrustTier != "" {
		t.Fatalf("citation = %+v, want validated compaction recall", c)
	}
	if cited.TurnID != "turn-1" {
		t.Fatalf("cited TurnID = %q, want turn-1", cited.TurnID)
	}
}

// TestVectorRecall_ScopedToItsTurn: a recall announced for one turn does not
// validate a different turn's response.
func TestVectorRecall_ScopedToItsTurn(t *testing.T) {
	bus, cited := recallRig(t)

	_ = bus.Emit("io.input", events.UserInput{SchemaVersion: events.UserInputVersion, Content: "how far is it?"})
	_ = bus.Emit("agent.turn.start", events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1"})
	_ = bus.Emit("agent.turn.end", events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1"})
	_, _ = engine.PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content:       recallCite,
		Metadata:      map[string]any{engine.MetaTurnID: "turn-other"},
	})

	if len(cited.Citations) != 0 {
		t.Fatalf("recall validated a turn it was not announced for: %+v", cited.Citations)
	}
}
