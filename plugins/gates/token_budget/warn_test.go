package tokenbudget

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// collectWarnings subscribes to io.output and returns an accessor for the
// 80%-utilisation warnings seen so far.
func collectWarnings(t *testing.T, bus engine.EventBus) func() []string {
	t.Helper()
	var mu sync.Mutex
	var got []string
	bus.Subscribe("io.output", func(e engine.Event[any]) {
		out, ok := e.Payload.(events.AgentOutput)
		if !ok || !strings.HasPrefix(out.Content, "Warning:") {
			return
		}
		mu.Lock()
		got = append(got, out.Content)
		mu.Unlock()
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// emitWithin fires before:llm.request and fails the test instead of hanging
// the suite if the handler never returns.
func emitWithin(t *testing.T, bus engine.EventBus, req *events.LLMRequest) engine.VetoResult {
	t.Helper()
	type result struct {
		veto engine.VetoResult
		err  error
	}
	done := make(chan result, 1)
	go func() {
		veto, err := bus.EmitVetoable("before:llm.request", req)
		done <- result{veto, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("emit %s: %v", req.RequestID, r.err)
		}
		return r.veto
	case <-time.After(2 * time.Second):
		t.Fatalf("before:llm.request %s did not return: handler deadlocked", req.RequestID)
		return engine.VetoResult{}
	}
}

// Regression for #272: crossing 80% on the session reservation path used to
// re-lock p.mu inside maybeWarn and hang the turn forever.
func TestSessionCeiling_WarnAt80PercentDoesNotDeadlock(t *testing.T) {
	_, bus := newGate(t, map[string]any{
		"ceilings": []any{
			map[string]any{"dimension": "session", "max_total_tokens": 100},
		},
	})
	warnings := collectWarnings(t, bus)

	// 50% reserved: below the warning threshold.
	if v := emitWithin(t, bus, &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, RequestID: "r1", MaxTokens: 50}); v.Vetoed {
		t.Fatalf("r1 vetoed: %s", v.Reason)
	}
	if n := len(warnings()); n != 0 {
		t.Fatalf("warnings after r1 = %d, want 0", n)
	}

	// 90% reserved: crosses 80% without exceeding the ceiling.
	if v := emitWithin(t, bus, &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, RequestID: "r2", MaxTokens: 40}); v.Vetoed {
		t.Fatalf("r2 vetoed: %s", v.Reason)
	}
	// 95% reserved: still over 80%, warning must not repeat.
	if v := emitWithin(t, bus, &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, RequestID: "r3", MaxTokens: 5}); v.Vetoed {
		t.Fatalf("r3 vetoed: %s", v.Reason)
	}

	got := warnings()
	if len(got) != 1 {
		t.Fatalf("warnings = %q, want exactly one", got)
	}
	if !strings.Contains(got[0], "session budget at 90%") {
		t.Fatalf("warning = %q, want session budget at 90%%", got[0])
	}
}

// A request vetoed by a later block ceiling rolls back its reservation, so
// it must not consume the one-shot 80% warning of an earlier ceiling.
func TestSessionCeiling_BlockedRequestDoesNotConsumeWarning(t *testing.T) {
	_, bus := newGate(t, map[string]any{
		"ceilings": []any{
			map[string]any{"dimension": "session", "max_total_tokens": 100},
			map[string]any{"dimension": "source_plugin", "match": "planner", "max_total_tokens": 10},
		},
	})
	warnings := collectWarnings(t, bus)

	blocked := &events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		RequestID:     "r1",
		MaxTokens:     90,
		Tags:          map[string]string{"source_plugin": "planner"},
	}
	if v := emitWithin(t, bus, blocked); !v.Vetoed {
		t.Fatal("r1 should be vetoed by the source_plugin ceiling")
	}
	if n := len(warnings()); n != 0 {
		t.Fatalf("warnings after vetoed r1 = %q, want none", warnings())
	}

	if v := emitWithin(t, bus, &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, RequestID: "r2", MaxTokens: 85}); v.Vetoed {
		t.Fatalf("r2 vetoed: %s", v.Reason)
	}
	if got := warnings(); len(got) != 1 {
		t.Fatalf("warnings after r2 = %q, want exactly one", got)
	}
}

// The tenant pass runs without p.mu held and goes through maybeWarn.
func TestTenantCeiling_WarnAt80PercentEmitsOnce(t *testing.T) {
	_, bus := newGate(t, map[string]any{
		"ceilings": []any{
			map[string]any{"dimension": "tenant", "max_total_tokens": 100},
		},
	})
	warnings := collectWarnings(t, bus)
	tags := map[string]string{"tenant": "acme"}

	_ = bus.Emit("llm.response", events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Tags: tags, Usage: events.Usage{TotalTokens: 85}})
	for _, id := range []string{"r1", "r2"} {
		if v := emitWithin(t, bus, &events.LLMRequest{SchemaVersion: events.LLMRequestVersion, RequestID: id, Tags: tags}); v.Vetoed {
			t.Fatalf("%s vetoed: %s", id, v.Reason)
		}
	}
	if got := warnings(); len(got) != 1 || !strings.Contains(got[0], "tenant budget at 85%") {
		t.Fatalf("warnings = %q, want one tenant 85%% warning", got)
	}
}
