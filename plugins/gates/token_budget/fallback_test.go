package tokenbudget

import (
	"fmt"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/providers/fallback"
)

const sessionBucket = "session|session|_session"

// TestReReserveSameRequestID_ReleasesPriorReservation pins the gate-side
// mechanism on its own: a second before:llm.request for a RequestID that
// still holds a reservation releases the first, so the bucket carries one
// estimate, and the response settles it to exactly the actual usage.
func TestReReserveSameRequestID_ReleasesPriorReservation(t *testing.T) {
	p, bus := newGate(t, map[string]any{
		"ceilings": []any{
			map[string]any{"dimension": "session", "max_total_tokens": 1_000_000},
		},
	})

	mk := func() *events.LLMRequest {
		return &events.LLMRequest{
			SchemaVersion: events.LLMRequestVersion,
			RequestID:     "req-1",
			MaxTokens:     500,
			Messages:      []events.Message{{Role: "user", Content: strings.Repeat("x", 400)}},
		}
	}
	_, _ = bus.EmitVetoable("before:llm.request", mk())
	one := p.totals[sessionBucket].TotalTokens
	if one == 0 {
		t.Fatal("expected first attempt to reserve an estimate")
	}
	_, _ = bus.EmitVetoable("before:llm.request", mk())
	if got := p.totals[sessionBucket].TotalTokens; got != one {
		t.Fatalf("re-reserve under the same RequestID must replace, not stack: got %d, want %d", got, one)
	}

	_ = bus.Emit("llm.response", events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		RequestID:     "req-1",
		Usage:         events.Usage{PromptTokens: 30, CompletionTokens: 12, TotalTokens: 42},
	})
	assertSettled(t, p, 30, 12, 42)
}

// TestFallbackRetry_BudgetEqualsSuccessfulAttemptOnly drives the real
// fallback coordinator and the gate on one bus: the primary attempt reserves,
// the provider fails, fallback vetoes before:core.error (so core.error never
// fires) and re-publishes the same RequestID, the secondary answers. The
// session bucket must hold only the successful attempt's usage.
func TestFallbackRetry_BudgetEqualsSuccessfulAttemptOnly(t *testing.T) {
	p, bus := newGate(t, map[string]any{
		"ceilings": []any{
			map[string]any{"dimension": "session", "max_total_tokens": 1_000_000},
		},
	})
	fb := fallback.New()
	if err := fb.Init(engine.PluginContext{
		Bus:    bus,
		Logger: testLogger,
		Models: engine.NewModelRegistry(map[string]any{
			"default": "balanced",
			"balanced": []any{
				map[string]any{"provider": "nexus.llm.anthropic", "model": "claude-sonnet-4-20250514"},
				map[string]any{"provider": "nexus.llm.openai", "model": "gpt-4o"},
			},
		}),
	}); err != nil {
		t.Fatalf("fallback init: %v", err)
	}

	var published []events.LLMRequest
	bus.Subscribe("llm.request", func(e engine.Event[any]) {
		if r, ok := e.Payload.(events.LLMRequest); ok {
			published = append(published, r)
		}
	})
	coreErrors := 0
	bus.Subscribe("core.error", func(engine.Event[any]) { coreErrors++ })

	res := engine.PublishLLMRequest(bus, events.LLMRequest{
		RequestID: "req-fb",
		Role:      "balanced",
		MaxTokens: 800,
		Messages:  []events.Message{{Role: "user", Content: strings.Repeat("y", 1200)}},
	})
	if !res.Published || len(published) != 1 {
		t.Fatalf("primary attempt not published: %+v", res)
	}

	// Primary provider fails non-retryably; the error path goes through
	// before:core.error exactly as engine.EmitError would.
	errInfo := &events.ErrorInfo{
		SchemaVersion: events.ErrorInfoVersion,
		Source:        "nexus.llm.anthropic",
		Err:           fmt.Errorf("anthropic: status 401"),
		RequestID:     "req-fb",
		RequestMeta:   published[0].Metadata,
	}
	veto, err := bus.EmitVetoable("before:core.error", errInfo)
	if err != nil || !veto.Vetoed {
		t.Fatalf("expected fallback to veto the provider error: veto=%+v err=%v", veto, err)
	}
	if coreErrors != 0 {
		t.Fatalf("core.error must not fire on the fallback path, fired %d times", coreErrors)
	}
	if len(published) != 2 || published[1].RequestID != "req-fb" || published[1].Model != "gpt-4o" {
		t.Fatalf("expected retry of req-fb to gpt-4o, got %+v", published)
	}

	_ = bus.Emit("llm.response", events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		RequestID:     "req-fb",
		Usage:         events.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
	})
	assertSettled(t, p, 100, 20, 120)
}

func assertSettled(t *testing.T, p *Plugin, in, out, total int) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	got := p.totals[sessionBucket]
	if got.InputTokens != in || got.OutputTokens != out || got.TotalTokens != total {
		t.Fatalf("bucket = in %d / out %d / total %d, want %d / %d / %d (leaked reservation?)",
			got.InputTokens, got.OutputTokens, got.TotalTokens, in, out, total)
	}
	if len(p.reservations) != 0 {
		t.Fatalf("reservations left outstanding: %v", p.reservations)
	}
}
