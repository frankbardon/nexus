//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestMultiSubagent_Boot validates that three named subagent instances boot
// with distinct plugin IDs.
func TestMultiSubagent_Boot(t *testing.T) {
	h := testharness.New(t, "configs/test-multi-subagent.yaml", testharness.WithTimeout(20*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.agent.react",
		"nexus.agent.subagent/researcher",
		"nexus.agent.subagent/coder",
		"nexus.agent.subagent/reviewer",
	)
}

// TestMultiSubagent_RegistersPerInstanceTools validates that each subagent
// instance registers a tool named "spawn_<suffix>" where suffix is the
// instance ID (e.g. nexus.agent.subagent/researcher → spawn_researcher).
func TestMultiSubagent_RegistersPerInstanceTools(t *testing.T) {
	h := testharness.New(t, "configs/test-multi-subagent.yaml", testharness.WithTimeout(20*time.Second))
	h.Run()

	wantTools := map[string]bool{
		"spawn_researcher": false,
		"spawn_coder":      false,
		"spawn_reviewer":   false,
	}
	for _, e := range h.Events() {
		if e.Type != "tool.register" {
			continue
		}
		td, ok := e.Payload.(events.ToolDef)
		if !ok {
			continue
		}
		if _, want := wantTools[td.Name]; want {
			wantTools[td.Name] = true
		}
	}
	for name, registered := range wantTools {
		if !registered {
			t.Errorf("expected tool %q to be registered, but it was not", name)
		}
	}
}
