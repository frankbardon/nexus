package broker

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// TestContract asserts the declared Subscriptions/Emissions match the wired
// runtime surface. With no broker_addr in config the plugin stays dormant, so
// Init/Ready/Shutdown run cleanly inside the harness.
func TestContract(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo(
		"io.output",
		"llm.stream.chunk",
		"llm.stream.end",
		"io.status",
		"io.approval.request",
		"hitl.requested",
		"cancel.complete",
	)
	declared := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		declared[e] = true
	}
	for _, want := range []string{
		"io.input",
		"before:io.input",
		"io.approval.response",
		"hitl.responded",
		"cancel.request",
	} {
		if !declared[want] {
			t.Errorf("Emissions() missing %q", want)
		}
	}
}
