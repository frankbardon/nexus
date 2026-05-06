package citations

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

func newTestPlugin(t *testing.T, mode string, strict bool) (*Plugin, engine.EventBus, func()) {
	t.Helper()
	bus := engine.NewEventBus()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	prompts := engine.NewPromptRegistry()

	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Bus:     bus,
		Logger:  logger,
		Prompts: prompts,
		Config: map[string]any{
			"mode":   mode,
			"strict": strict,
		},
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	return p, bus, func() { _ = p.Shutdown(context.Background()) }
}

// captureCited returns a thunk pointing at the latest llm.response.cited
// emitted on the bus, plus the unsubscribe.
func captureCited(t *testing.T, bus engine.EventBus) (*events.CitedResponse, func()) {
	t.Helper()
	captured := &events.CitedResponse{SchemaVersion: events.CitedResponseVersion}
	unsub := bus.Subscribe("llm.response.cited", func(ev engine.Event[any]) {
		c, _ := ev.Payload.(events.CitedResponse)
		*captured = c
	}, engine.WithPriority(99))
	return captured, unsub
}

func TestParsesAndStripsTagInStrictMode(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t, "tag", true)
	t.Cleanup(cleanup)

	_ = bus.Emit("rag.retrieved", events.RetrievalContext{SchemaVersion: events.RetrievalContextVersion, TurnID: "t1",
		Chunks: []events.RetrievedChunk{
			{Source: "/docs/auth.md", DocID: "d-1", ChunkIdx: "3"},
		},
	})

	cited, unsub := captureCited(t, bus)
	t.Cleanup(unsub)

	resp := events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: `The token expires in 24 hours.<cite source="/docs/auth.md" chunk="3"/> Renewal is automatic.`,
		Metadata: map[string]any{"turn_id": "t1"},
	}
	_ = bus.Emit("llm.response", resp)

	if cited.Mode != "tag" {
		t.Fatalf("mode = %q, want tag", cited.Mode)
	}
	if strings.Contains(cited.Text, "<cite") {
		t.Fatalf("text retains <cite tag: %q", cited.Text)
	}
	if cited.Text != `The token expires in 24 hours. Renewal is automatic.` {
		t.Fatalf("text = %q", cited.Text)
	}
	if len(cited.Citations) != 1 {
		t.Fatalf("got %d citations, want 1", len(cited.Citations))
	}
	if cited.Citations[0].Source != "/docs/auth.md" || cited.Citations[0].ChunkIdx != "3" {
		t.Fatalf("citation fields = %+v", cited.Citations[0])
	}
}

func TestStrictDropsUnknownCitation(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t, "tag", true)
	t.Cleanup(cleanup)

	_ = bus.Emit("rag.retrieved", events.RetrievalContext{SchemaVersion: events.RetrievalContextVersion, TurnID: "t2",
		Chunks: []events.RetrievedChunk{
			{Source: "/docs/known.md", ChunkIdx: "1"},
		},
	})

	cited, unsub := captureCited(t, bus)
	t.Cleanup(unsub)

	_ = bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: `Fact A.<cite source="/docs/unknown.md" chunk="9"/> Fact B.<cite source="/docs/known.md" chunk="1"/>`,
		Metadata: map[string]any{"turn_id": "t2"},
	})

	if len(cited.Citations) != 1 {
		t.Fatalf("strict mode kept %d citations, want 1 (the known one)", len(cited.Citations))
	}
	if cited.Citations[0].Source != "/docs/known.md" {
		t.Fatalf("kept wrong citation: %+v", cited.Citations[0])
	}
}

func TestNonStrictMarksUnknownAsUnverified(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t, "tag", false)
	t.Cleanup(cleanup)

	_ = bus.Emit("rag.retrieved", events.RetrievalContext{SchemaVersion: events.RetrievalContextVersion, TurnID: "t3",
		Chunks: []events.RetrievedChunk{{Source: "/docs/known.md", ChunkIdx: "1"}},
	})

	cited, unsub := captureCited(t, bus)
	t.Cleanup(unsub)

	_ = bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: `Fact.<cite source="/docs/unknown.md" chunk="9"/>`,
		Metadata: map[string]any{"turn_id": "t3"},
	})

	if len(cited.Citations) != 1 {
		t.Fatalf("non-strict dropped instead of marking unverified")
	}
	if cited.Citations[0].TrustTier != "unverified" {
		t.Fatalf("TrustTier = %q, want unverified", cited.Citations[0].TrustTier)
	}
}

func TestNoTagsNoEmit(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t, "tag", true)
	t.Cleanup(cleanup)

	cited, unsub := captureCited(t, bus)
	t.Cleanup(unsub)

	_ = bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: "no citations here",
		Metadata: map[string]any{"turn_id": "t4"},
	})

	if cited.Text != "" || len(cited.Citations) != 0 {
		t.Fatalf("expected no emit, captured %+v", cited)
	}
}

func TestAnthropicNativePath(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t, "anthropic_native", false)
	t.Cleanup(cleanup)

	cited, unsub := captureCited(t, bus)
	t.Cleanup(unsub)

	_ = bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: "answer with native citations",
		Citations: []events.Citation{
			{DocumentTitle: "doc-A.md", CitedText: "snippet", StartCharIndex: 0, EndCharIndex: 7},
		},
		Metadata: map[string]any{"turn_id": "t5"},
	})

	if cited.Mode != "anthropic_native" {
		t.Fatalf("mode = %q, want anthropic_native", cited.Mode)
	}
	if len(cited.Citations) != 1 || cited.Citations[0].Source != "doc-A.md" {
		t.Fatalf("citations = %+v", cited.Citations)
	}
}

func TestAutoModeDetectsNativeWhenPresent(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t, "auto", true)
	t.Cleanup(cleanup)

	cited, unsub := captureCited(t, bus)
	t.Cleanup(unsub)

	_ = bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: "x",
		Citations: []events.Citation{{DocumentTitle: "doc"}},
		Metadata:  map[string]any{"turn_id": "t6"},
	})

	if cited.Mode != "anthropic_native" {
		t.Fatalf("auto-mode with native citations did not pick native: %q", cited.Mode)
	}
}

func TestTurnEndDiscardsContext(t *testing.T) {
	p, bus, cleanup := newTestPlugin(t, "tag", true)
	t.Cleanup(cleanup)

	_ = bus.Emit("rag.retrieved", events.RetrievalContext{SchemaVersion: events.RetrievalContextVersion, TurnID: "t7",
		Chunks: []events.RetrievedChunk{{Source: "x", ChunkIdx: "0"}},
	})
	_ = bus.Emit("agent.turn.end", events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "t7"})

	p.mu.Lock()
	_, present := p.turns["t7"]
	p.mu.Unlock()
	if present {
		t.Fatalf("turn-end did not discard retrieval context")
	}
}

func TestPromptSectionRegistered(t *testing.T) {
	_, bus, cleanup := newTestPlugin(t, "tag", true)
	t.Cleanup(cleanup)
	_ = bus

	// The section function is on Plugin; calling it directly verifies the
	// registered text mentions the contract. We test the function rather
	// than the registry so this stays isolated from PromptRegistry behavior.
	p := New().(*Plugin)
	out := p.buildPromptSection()
	if !strings.Contains(out, "<cite") {
		t.Fatalf("prompt section missing <cite tag instruction: %s", out)
	}
}
