package dynvars

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract_NoSubscriptionsOrEmissions(t *testing.T) {
	h := contract.NewContract(t, New)
	if got := h.Plugin().Subscriptions(); len(got) != 0 {
		t.Errorf("Subscriptions() = %v, want empty", got)
	}
	if got := h.Plugin().Emissions(); len(got) != 0 {
		t.Errorf("Emissions() = %v, want empty", got)
	}
}

func TestContract_AdvertisesNoCapability(t *testing.T) {
	h := contract.NewContract(t, New)
	if got := h.Plugin().Capabilities(); len(got) != 0 {
		t.Errorf("Capabilities() = %v, want empty", got)
	}
}

// dynvars contributes via the engine's PromptRegistry; that wiring lives in
// engine.LifecycleManager and is not exercisable from the contract harness
// (the harness deliberately stubs ctx.Prompts to nil for plugin isolation).
// The plugin tolerates a nil registry — verify Init returns no error when
// ctx.Prompts is absent.
func TestContract_InitWithoutPromptRegistry(t *testing.T) {
	// NewContract calls Init internally and t.Fatal on error; reaching this
	// line means Init succeeded with a nil PromptRegistry.
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"date": true,
		"time": true,
		"os":   true,
	}))
	if id := h.Plugin().ID(); !strings.Contains(id, "dynvars") {
		t.Errorf("plugin ID = %q, want to contain dynvars", id)
	}
}

func TestContract_NoBusEmissionsOnInit(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"date": true,
	}))
	if got := h.PluginEmissions(); len(got) != 0 {
		t.Errorf("dynvars must emit nothing on init; got %v", got)
	}
}
