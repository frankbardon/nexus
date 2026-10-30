package engine_test

// The tag seeder must run before every other before:llm.request handler
// (#148). These tests boot the real plugins that used to outrank it — the
// fanout coordinator (2), token_budget (7) and the test IO mock (20) — and
// assert each one now sees, or passes on, the seeded session dimensions.
// External package so it can import the plugins, which import pkg/engine.

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

func bootSeederOrderEngine(t *testing.T, defaultRole, pluginsYAML string) *engine.Engine {
	t.Helper()
	root := t.TempDir()
	cfg := fmt.Sprintf(`
core:
  log_level: error
  sessions:
    root: %s
  storage:
    root: %s
  models:
    default: %s
    plain:
      provider: nexus.llm.anthropic
      model: plain-model
    compare:
      fanout: true
      providers:
        - provider: nexus.llm.anthropic
          model: model-a
        - provider: nexus.llm.openai
          model: model-b
%s
`, filepath.Join(root, "sessions"), root, defaultRole, pluginsYAML)
	eng := bootYAML(t, cfg)
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	if err := eng.Session.SetLabel("tenant", "acme"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	return eng
}

// A fanout-role request is intercepted at priority 2. Both the parent and
// every leg — which skip the hop — carry the session dimensions, and each leg
// owns its tag map.
func TestTagSeeder_FanoutParentAndLegsSeeded(t *testing.T) {
	eng := bootSeederOrderEngine(t, "compare", `
plugins:
  active: [nexus.io.test, nexus.provider.fanout]
  nexus.io.test:
    timeout: 30s
    mock_responses:
      - content: "a"
      - content: "b"
  nexus.provider.fanout:
    strategy: all
    deadline_ms: 10000
`)

	var mu sync.Mutex
	var legs []events.LLMRequest
	got2 := make(chan struct{})
	unsub := eng.Bus.Subscribe("llm.request", func(e engine.Event[any]) {
		req, ok := e.Payload.(events.LLMRequest)
		if !ok {
			return
		}
		if _, ok := req.Metadata["_fanout_id"]; !ok {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		legs = append(legs, req)
		if len(legs) == 2 {
			close(got2)
		}
	}, engine.WithPriority(-1))
	defer unsub()

	res := engine.PublishLLMRequest(eng.Bus, events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Role:          "compare",
		Messages:      []events.Message{{Role: "user", Content: "hi"}},
		Tags:          map[string]string{"source_plugin": "test.producer"},
	})
	if !res.Intercepted() {
		t.Fatalf("fanout request not intercepted: %+v", res.Veto)
	}
	want := eng.Session.ID
	if res.Request.Tags["session_id"] != want || res.Request.Tags["tenant"] != "acme" {
		t.Fatalf("fanout parent not seeded: %v", res.Request.Tags)
	}

	select {
	case <-got2:
	case <-time.After(5 * time.Second):
		t.Fatal("fanout legs not dispatched")
	}
	mu.Lock()
	defer mu.Unlock()
	for i, leg := range legs {
		if leg.Tags["session_id"] != want || leg.Tags["tenant"] != "acme" || leg.Tags["source_plugin"] != "test.producer" {
			t.Fatalf("leg %d tags = %v, want parent's seeded tags", i, leg.Tags)
		}
	}
	legs[0].Tags["probe"] = "x"
	if _, leaked := legs[1].Tags["probe"]; leaked {
		t.Fatal("fanout legs share one tag map")
	}
	if _, leaked := res.Request.Tags["probe"]; leaked {
		t.Fatal("fanout leg aliases the parent's tag map")
	}
}

// token_budget (priority 7) reads Tags["tenant"] for a tenant ceiling. With
// the seeder at 100 the tag arrived after the gate ran, so a tenant over its
// ceiling was never blocked.
func TestTagSeeder_TokenBudgetSeesSeededTenant(t *testing.T) {
	eng := bootSeederOrderEngine(t, "plain", `
plugins:
  active: [nexus.gate.token_budget]
  nexus.gate.token_budget:
    ceilings:
      - dimension: tenant
        match: acme
        max_total_tokens: 100
        on_exceed: block
`)

	// Spend the tenant's whole ceiling.
	engine.PublishLLMResponse(eng.Bus, events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Usage:         events.Usage{TotalTokens: 100},
		Tags:          map[string]string{"tenant": "acme"},
	})

	res := engine.PublishLLMRequest(eng.Bus, events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Messages:      []events.Message{{Role: "user", Content: "hi"}},
	})
	if !res.Blocked() {
		t.Fatalf("token_budget did not block the seeded tenant over its ceiling: published=%v veto=%+v tags=%v",
			res.Published, res.Veto, res.Request.Tags)
	}
}

// The test IO mock intercepts at priority 20; the intercepted request — and
// the scripted response it publishes — carry the session dimensions.
func TestTagSeeder_MockInterceptedRequestSeeded(t *testing.T) {
	eng := bootSeederOrderEngine(t, "plain", `
plugins:
  active: [nexus.io.test]
  nexus.io.test:
    timeout: 30s
    mock_responses:
      - content: "mocked"
`)

	gotResp := make(chan events.LLMResponse, 1)
	unsub := eng.Bus.Subscribe("llm.response", func(e engine.Event[any]) {
		if resp, ok := e.Payload.(events.LLMResponse); ok && resp.Model == "mock" {
			select {
			case gotResp <- resp:
			default:
			}
		}
	})
	defer unsub()

	res := engine.PublishLLMRequest(eng.Bus, events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Messages:      []events.Message{{Role: "user", Content: "hi"}},
	})
	if !res.Intercepted() {
		t.Fatalf("mock did not intercept: %+v", res.Veto)
	}
	want := eng.Session.ID
	if res.Request.Tags["session_id"] != want || res.Request.Tags["tenant"] != "acme" {
		t.Fatalf("mock-intercepted request not seeded: %v", res.Request.Tags)
	}
	select {
	case resp := <-gotResp:
		if resp.Tags["session_id"] != want || resp.Tags["tenant"] != "acme" {
			t.Fatalf("mock response tags = %v, want request's seeded tags", resp.Tags)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mock response not published")
	}
}
