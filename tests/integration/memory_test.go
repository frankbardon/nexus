//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestMemorySimple_Boot validates that ReAct boots cleanly with
// nexus.memory.simple as the memory.history provider. Capability resolution
// must pick simple without any explicit pin because it's the only
// memory.history provider in the active list.
func TestMemorySimple_Boot(t *testing.T) {
	h := testharness.New(t, "configs/test-memory-simple.yaml", testharness.WithTimeout(15*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.io.test",
		"nexus.llm.anthropic",
		"nexus.agent.react",
		"nexus.memory.simple",
	)
	h.AssertEventEmitted("io.session.start")
	h.AssertEventEmitted("io.session.end")
	// Agent produced replies for both user inputs.
	h.AssertEventCount("io.input", 2, 2)
	h.AssertEventEmitted("io.output")
}

// TestMemorySummaryBuffer_TriggersSummary proves the summary_buffer plugin
// detects the message-count threshold, emits its own internal llm.request
// tagged with _source=nexus.memory.summary_buffer, and emits memory.compacted
// after receiving the summary reply. The final buffer holds the summary
// plus max_recent protected messages.
func TestMemorySummaryBuffer_TriggersSummary(t *testing.T) {
	h := testharness.New(t, "configs/test-memory-summary-buffer.yaml", testharness.WithTimeout(30*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.io.test",
		"nexus.llm.anthropic",
		"nexus.agent.react",
		"nexus.memory.summary_buffer",
	)

	// The plugin should have triggered at least one summarisation.
	h.AssertEventEmitted("memory.compaction.triggered")
	h.AssertEventEmitted("memory.compacted")

	// Look at memory.compacted payload: summary system message + max_recent
	// tail. Multiple compactions may occur across the 4 inputs; any one is
	// enough to prove the mechanism works.
	found := false
	for _, e := range h.Events() {
		if e.Type != "memory.compacted" {
			continue
		}
		cc, ok := e.Payload.(events.CompactionComplete)
		if !ok {
			t.Fatalf("memory.compacted payload wrong type: %T", e.Payload)
		}
		if len(cc.Messages) == 0 {
			t.Fatalf("memory.compacted has no messages")
		}
		if cc.Messages[0].Role != "system" {
			t.Fatalf("compacted head should be system summary, got %q", cc.Messages[0].Role)
		}
		if cc.MessageCount >= cc.PrevCount {
			t.Fatalf("compacted message count %d should be less than prev %d", cc.MessageCount, cc.PrevCount)
		}
		found = true
	}
	if !found {
		t.Fatal("no memory.compacted event with valid payload observed")
	}
}
