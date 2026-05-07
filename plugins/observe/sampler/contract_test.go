package sampler

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract_DisabledByDefault(t *testing.T) {
	h := contract.NewContract(t, New)
	// When not enabled in config, sampler advertises no subs/emissions.
	if got := h.Plugin().Subscriptions(); len(got) != 0 {
		t.Errorf("Subscriptions() = %v, want empty when disabled", got)
	}
	if got := h.Plugin().Emissions(); len(got) != 0 {
		t.Errorf("Emissions() = %v, want empty when disabled", got)
	}
}

func TestContract_EnabledAdvertisesSessionEnd(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithSession(),
		contract.WithPluginConfig(map[string]any{"enabled": true}))
	h.AssertSubscribesTo("io.session.end")
	if got := h.Plugin().Emissions(); len(got) == 0 {
		t.Error("Emissions() should advertise EvalCandidateEventType when enabled")
	}
}
