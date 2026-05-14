package client

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// TestContract_DeclaresExpectedSubscriptions pins the plugin's subscription
// surface so future refactors can't quietly drop one of the channels we
// document in the plugin reference.
func TestContract_DeclaresExpectedSubscriptions(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo(
		"tool.invoke",
		"before:io.input",
		"io.session.start",
		"io.session.end",
		"mcp.prompts.list",
	)
}

func TestContract_AdvertisesMCPClientCapability(t *testing.T) {
	h := contract.NewContract(t, New)
	caps := h.Plugin().Capabilities()
	if len(caps) != 1 || caps[0].Name != "mcp.client" {
		t.Fatalf("Capabilities() = %v, want [mcp.client]", caps)
	}
}

// TestContract_NoOpWithoutServers ensures the plugin boots cleanly with an
// empty server list and emits nothing it didn't declare.
func TestContract_NoOpWithoutServers(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertNoUndeclaredEmissions()
}
