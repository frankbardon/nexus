package citations

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
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
	bus, cited, _ := recallRigWith(t, []events.VectorMatch{{
		ID:         "mem-1",
		Content:    "The user prefers metric units.",
		Similarity: 0.9,
		Metadata:   map[string]string{"source": "compaction"},
	}})
	return bus, cited
}

// recallRigWith is recallRig with the recalled matches supplied, plus the
// prompt registry memory.vector renders its <recalled_memory> block into.
func recallRigWith(t *testing.T, matches []events.VectorMatch) (engine.EventBus, *events.CitedResponse, *engine.PromptRegistry) {
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
			q.Matches = matches
		}
	}))

	prompts := engine.NewPromptRegistry()
	vp := vector.New()
	if err := vp.Init(engine.PluginContext{
		Bus:     bus,
		Prompts: prompts,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config:  map[string]any{"namespace": "mem", "auto_store_compaction": false},
	}); err != nil {
		t.Fatalf("vector init: %v", err)
	}
	t.Cleanup(func() { _ = vp.Shutdown(context.Background()) })

	cited, unsub := captureCited(t, bus)
	t.Cleanup(unsub)
	return bus, cited, prompts
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

// TestVectorRecall_DocIDNamesOneEntry: two recalled entries share the source
// "compaction", so a source-only cite cannot tell them apart. Each rendered
// <recalled_memory> item carries doc="<id>", and a cite naming one entry's
// doc validates as that entry only — a doc no recalled entry has is stripped
// even though its source matches.
func TestVectorRecall_DocIDNamesOneEntry(t *testing.T) {
	bus, cited, prompts := recallRigWith(t, []events.VectorMatch{
		{ID: "mem-units", Content: "The user prefers metric units.", Similarity: 0.9,
			Metadata: map[string]string{"source": "compaction"}},
		{ID: "mem-tz", Content: "The user lives in Lisbon.", Similarity: 0.8,
			Metadata: map[string]string{"source": "compaction"}},
	})

	_ = bus.Emit("io.input", events.UserInput{SchemaVersion: events.UserInputVersion, Content: "how far is it?"})
	_ = bus.Emit("agent.turn.start", events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1"})

	// Read the doc id the way the model would: off the rendered item.
	rendered := prompts.Apply("")
	m := regexp.MustCompile(`<item [^>]*doc="([^"]*)"[^>]*>\s*The user lives in Lisbon\.`).FindStringSubmatch(rendered)
	if m == nil {
		t.Fatalf("recalled item renders no doc attribute:\n%s", rendered)
	}
	doc := m[1]
	if strings.Count(rendered, `doc="`) != 2 {
		t.Fatalf("want a doc attribute on both items:\n%s", rendered)
	}

	_, _ = engine.PublishLLMResponse(bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Content: `Lisbon.<cite source="compaction" doc="` + doc + `"/>` +
			` Made up.<cite source="compaction" doc="mem-absent"/>`,
		Metadata: map[string]any{engine.MetaTurnID: "turn-1"},
	})

	if len(cited.Citations) != 1 {
		t.Fatalf("got %d citations, want 1 (the doc-named entry only): %+v", len(cited.Citations), cited.Citations)
	}
	if c := cited.Citations[0]; c.DocID != "mem-tz" || c.Source != "compaction" || c.TrustTier != "" {
		t.Fatalf("citation = %+v, want validated mem-tz", c)
	}
}
