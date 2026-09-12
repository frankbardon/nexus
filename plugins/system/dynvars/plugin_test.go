package dynvars

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// dynvars reads one event and writes none: it decorates the outbound request
// in place rather than announcing anything of its own.
func TestContract_SubscribesToTheRequestAndEmitsNothing(t *testing.T) {
	h := contract.NewContract(t, New)
	subs := h.Plugin().Subscriptions()
	if len(subs) != 1 || subs[0].EventType != "before:llm.request" {
		t.Errorf("Subscriptions() = %v, want exactly before:llm.request", subs)
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

// dynvars no longer touches the PromptRegistry at all — it applies its block to
// the outbound request — so a context without one must still initialize
// cleanly. The harness stubs ctx.Prompts to nil, which is exactly that case.
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
