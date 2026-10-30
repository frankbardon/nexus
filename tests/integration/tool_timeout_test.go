//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestToolTimeout_DefaultOnUnblocksTurn pins issue #211: a tool call whose
// result never arrives ends with an error tool.result from the auto-activated
// nexus.gate.tool_timeout, and the turn completes instead of hanging. The gate
// is not in plugins.active — nexus.tool.catalog's Requires() brings it in.
// Mock mode; no API key.
func TestToolTimeout_DefaultOnUnblocksTurn(t *testing.T) {
	h := testharness.New(t, "configs/test-tool-timeout.yaml", testharness.WithTimeout(30*time.Second))
	h.Run()

	h.AssertBooted("nexus.gate.tool_timeout")
	// tool.invoke itself is not asserted: its dispatch is still parked inside
	// the never-answering ask_user handler, which is exactly the hang the gate
	// routes around (Go cancellation is cooperative; the handler is released at
	// shutdown, bounded by core.shutdown.drain_timeout in the profile).
	h.AssertEventCount("tool.timeout", 1, 1)

	var result events.ToolResult
	var found bool
	for _, e := range h.Events() {
		if e.Type != "tool.result" {
			continue
		}
		if r, ok := e.Payload.(events.ToolResult); ok && r.Name == "ask_user" {
			result, found = r, true
		}
	}
	if !found {
		t.Fatal("no ask_user tool.result collected")
	}
	if !strings.Contains(result.Error, "exceeded timeout 300ms") {
		t.Fatalf("tool.result.Error = %q, want the gate's timeout error", result.Error)
	}

	h.AssertOutputContains("No answer arrived in time")
}

// TestToolTimeout_AutoActivatedWithoutConfig: with no gate config anywhere, a
// profile that dispatches tools through the catalog boots the gate.
func TestToolTimeout_AutoActivatedWithoutConfig(t *testing.T) {
	h := testharness.New(t, "configs/test-unknown-tool.yaml", testharness.WithTimeout(30*time.Second))
	h.Run()
	h.AssertBooted("nexus.gate.tool_timeout")
	h.AssertEventNotEmitted("tool.timeout")
}
