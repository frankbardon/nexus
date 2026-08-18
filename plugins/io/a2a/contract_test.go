package a2a

import (
	"net/http"
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// TestContract_NoBusSurface asserts the plugin's declared event contract is
// honest for this story: it neither subscribes to nor emits anything.
//
// This is the assertion that matters right now. The transport is stood up but no
// request reaches the bus, so declaring the events a later story will need would
// make the harness pass against an intention. When the bus mapping lands, this
// test flips into the usual Inject/AssertEmitted shape.
func TestContract_NoBusSurface(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(testConfig(t, nil)))

	if subs := h.Plugin().Subscriptions(); len(subs) != 0 {
		t.Errorf("Subscriptions() = %v; the plugin wires no bus handlers in Init", subs)
	}
	if emits := h.Plugin().Emissions(); len(emits) != 0 {
		t.Errorf("Emissions() = %v; the plugin publishes nothing", emits)
	}

	// Drive the transport end to end through a real listener and confirm the bus
	// stays quiet: the declaration and the runtime agree because there is no
	// runtime bus behaviour, not because nothing was exercised.
	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}
	rec := do(t, p.server, http.MethodGet, a2a.AgentCardPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("agent card status = %d: %s", rec.Code, rec.Body)
	}
	rec = do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
		jsonrpcBody(t, a2a.MethodGetTask, map[string]any{"id": "task-1"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("jsonrpc status = %d: %s", rec.Code, rec.Body)
	}

	h.AssertNoUndeclaredEmissions()
	for _, ev := range h.PluginEmissions() {
		if ev.Source == pluginID {
			t.Errorf("plugin emitted %q while declaring no emissions", ev.Type)
		}
	}
}

// TestContract_BootsThroughTheHarness asserts Init and Ready succeed under the
// harness's PluginContext, which is what the engine hands every plugin.
func TestContract_BootsThroughTheHarness(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(testConfig(t, map[string]any{
		"bearer_token": "s3cret",
	})))

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}
	card := p.Card()
	if err := a2a.ValidateAgentCard(&card); err != nil {
		t.Fatalf("the plugin booted with an unservable card: %v", err)
	}
	if len(card.SecuritySchemes) == 0 {
		t.Error("a guarded listener published no securitySchemes for a client to discover")
	}
}
