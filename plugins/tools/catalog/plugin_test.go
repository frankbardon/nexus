package catalog

import (
	"fmt"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract_DeclaresExpectedSubscriptions(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("tool.register", "tool.catalog.query", "tool.invoke", "tool.result")
}

func TestContract_AdvertisesToolCatalogCapability(t *testing.T) {
	h := contract.NewContract(t, New)
	caps := h.Plugin().Capabilities()
	if len(caps) != 1 || caps[0].Name != "tool.catalog" {
		t.Errorf("Capabilities() = %v, want [tool.catalog]", caps)
	}
}

func TestContract_RegisterAndQueryRoundTrip(t *testing.T) {
	h := contract.NewContract(t, New)

	h.Inject("tool.register", events.ToolDef{Name: "alpha", Description: "alpha tool"})
	h.Inject("tool.register", events.ToolDef{Name: "beta", Description: "beta tool"})

	q := &events.ToolCatalogQuery{}
	h.Inject("tool.catalog.query", q)

	if len(q.Tools) != 2 {
		t.Fatalf("expected 2 tools registered, got %d (%v)", len(q.Tools), q.Tools)
	}
}

func TestContract_ReRegisterReplacesByName(t *testing.T) {
	h := contract.NewContract(t, New)

	h.Inject("tool.register", events.ToolDef{Name: "shell", Description: "v1"})
	h.Inject("tool.register", events.ToolDef{Name: "shell", Description: "v2"})

	q := &events.ToolCatalogQuery{}
	h.Inject("tool.catalog.query", q)

	if len(q.Tools) != 1 {
		t.Fatalf("expected 1 entry after re-register, got %d", len(q.Tools))
	}
	if q.Tools[0].Description != "v2" {
		t.Errorf("expected v2 to win, got %q", q.Tools[0].Description)
	}
}

// toolResults returns every tool.result the plugin (or a downstream handler)
// emitted, i.e. excluding test injects.
func toolResults(h *contract.ContractHarness) []events.ToolResult {
	var out []events.ToolResult
	for _, e := range h.PluginEmissions() {
		if e.Type != "tool.result" {
			continue
		}
		if r, ok := e.Payload.(events.ToolResult); ok {
			out = append(out, r)
		}
	}
	return out
}

func TestUnknownTool_SynthesizesErrorResult(t *testing.T) {
	h := contract.NewContract(t, New)

	h.Inject("tool.invoke", events.ToolCall{ID: "c1", Name: "get_weather", TurnID: "t1"})

	got := toolResults(h)
	if len(got) != 1 {
		t.Fatalf("want exactly one synthesized tool.result, got %d (%v)", len(got), got)
	}
	r := got[0]
	if r.ID != "c1" || r.Name != "get_weather" || r.TurnID != "t1" {
		t.Errorf("result correlation = {ID:%q Name:%q TurnID:%q}", r.ID, r.Name, r.TurnID)
	}
	if r.Error != "unknown tool: get_weather" {
		t.Errorf("Error = %q, want %q", r.Error, "unknown tool: get_weather")
	}
	h.AssertEmitted("before:tool.result")
	h.AssertNoUndeclaredEmissions()
}

func TestUnknownTool_RegisteredToolLeftToItsHandler(t *testing.T) {
	h := contract.NewContract(t, New)
	h.Inject("tool.register", events.ToolDef{Name: "shell"})

	// No handler answers here (an async tool, say) — the catalog must still
	// stay out of the way because the name is registered.
	h.Inject("tool.invoke", events.ToolCall{ID: "c1", Name: "shell"})

	if got := toolResults(h); len(got) != 0 {
		t.Fatalf("registered tool got a synthesized result: %v", got)
	}
}

func TestUnknownTool_QueryContributedToolLeftAlone(t *testing.T) {
	h := contract.NewContract(t, New)
	// Stand-in for nexus.io.agui: contributes the active run's client tools to
	// tool.catalog.query without registering them.
	unsub := h.Bus().Subscribe("tool.catalog.query", func(e engine.Event[any]) {
		if q, ok := e.Payload.(*events.ToolCatalogQuery); ok {
			q.Tools = append(q.Tools, events.ToolDef{Name: "get_weather"})
		}
	}, engine.WithPriority(60))
	defer unsub()

	h.Inject("tool.invoke", events.ToolCall{ID: "c1", Name: "get_weather"})

	if got := toolResults(h); len(got) != 0 {
		t.Fatalf("client tool got a synthesized result: %v", got)
	}
}

func TestUnknownTool_SynchronousUnregisteredHandlerNotDuplicated(t *testing.T) {
	h := contract.NewContract(t, New)
	// Stand-in for progressive discovery's "discover" meta-tool: handled
	// inline, never registered.
	bus := h.Bus()
	unsub := bus.Subscribe("tool.invoke", func(e engine.Event[any]) {
		tc, _ := e.Payload.(events.ToolCall)
		if tc.Name != "discover" {
			return
		}
		_ = bus.Emit("tool.result", events.ToolResult{ID: tc.ID, Name: tc.Name, Output: "ok"})
	}, engine.WithPriority(40))
	defer unsub()

	h.Inject("tool.invoke", events.ToolCall{ID: "c1", Name: "discover"})

	got := toolResults(h)
	if len(got) != 1 || got[0].Output != "ok" {
		t.Fatalf("want only the handler's own result, got %v", got)
	}
}

func TestUnknownTool_NoIDSkipped(t *testing.T) {
	h := contract.NewContract(t, New)
	h.Inject("tool.invoke", events.ToolCall{Name: "scene_create"})
	if got := toolResults(h); len(got) != 0 {
		t.Fatalf("uncorrelatable invoke got a result: %v", got)
	}
}

func TestUnknownTool_ConcurrentInvokes(t *testing.T) {
	h := contract.NewContract(t, New)
	h.Inject("tool.register", events.ToolDef{Name: "known"})

	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "known"
			if i%2 == 0 {
				name = "ghost"
			}
			_ = h.Bus().Emit("tool.invoke", events.ToolCall{ID: fmt.Sprintf("c%d", i), Name: name})
		}(i)
	}
	wg.Wait()

	seen := map[string]int{}
	for _, r := range toolResults(h) {
		seen[r.ID]++
		if r.Name != "ghost" {
			t.Errorf("synthesized result for registered tool %q", r.Name)
		}
	}
	if len(seen) != n/2 {
		t.Fatalf("want %d synthesized results, got %d", n/2, len(seen))
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("call %s got %d results", id, c)
		}
	}
}
