package opener

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract_DeclaresExpectedContract(t *testing.T) {
	// Use an explicit open_cmd so Init doesn't try to read ctx.System
	// (the contract harness leaves it nil for plugin isolation).
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"open_cmd": "/usr/bin/true",
	}))
	h.AssertSubscribesTo("tool.invoke")

	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{"before:tool.result", "tool.result", "tool.register"} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q (got %v)", want, h.Plugin().Emissions())
		}
	}
}

func TestContract_RegistersOpenPathToolOnReady(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"open_cmd": "/usr/bin/true",
	}))
	// Ready emits tool.register with the open_path tool def.
	h.AssertEmitted("tool.register")
}
