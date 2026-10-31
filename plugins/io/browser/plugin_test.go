package browser

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// The harness calls Ready, which binds the HTTP server. Port 0 lets the OS
// pick a free port so the test never collides with whatever holds the default
// :8080; open_browser is off so a run never launches a browser.
func TestContract_StaticContract(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"host":         "127.0.0.1",
		"port":         0,
		"open_browser": false,
	}))
	subs := h.Plugin().Subscriptions()
	if len(subs) == 0 {
		t.Error("Subscriptions() empty")
	}
	if emits := h.Plugin().Emissions(); len(emits) == 0 {
		t.Error("Emissions() empty")
	}
}
