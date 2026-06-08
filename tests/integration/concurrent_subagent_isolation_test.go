//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestConcurrentSubagent_CausationProvenance locks in the P1-B fix from the
// 2026-06 audit: events emitted by a subagent goroutine must carry a non-empty
// Causation.AgentID prefixed with the subagent plugin's instance ID, and
// Depth = ParentDepth + 1.
//
// Before the fix, subagent's `go func() { p.runSubagent(...) }()` ran on a
// fresh goroutine with no pushed CausationContext, so emitted events fell
// back to the bus-wide default (SessionID only) and AgentID/Depth were empty.
//
// Mock mode — the orchestrator runs three workers in parallel against a
// scripted mock response sequence; no API key required.
func TestConcurrentSubagent_CausationProvenance(t *testing.T) {
	h := testharness.New(t, "configs/test-orchestrator-mock.yaml", testharness.WithTimeout(30*time.Second))
	h.Run()

	// Sanity: the orchestrator did fan workers out.
	h.AssertEventEmitted("subagent.spawn")
	h.AssertEventEmitted("subagent.started")
	h.AssertEventEmitted("subagent.complete")

	// Collect subagent.* events. Each one originates from inside a worker
	// goroutine, so every one must carry the pushed CausationContext.
	const subagentInstanceID = "nexus.agent.subagent"
	var (
		subagentEventCount    int
		emptyAgentIDCount     int
		wrongPrefixCount      int
		wrongDepthCount       int
		seenSpawnIDsInAgentID = make(map[string]struct{})
	)

	for _, e := range h.Events() {
		if !strings.HasPrefix(e.Type, "subagent.") {
			continue
		}
		// subagent.spawn is emitted by the orchestrator, not by a worker
		// goroutine — skip it; the assertion is about events emitted from
		// inside runSubagent.
		if e.Type == "subagent.spawn" {
			continue
		}
		subagentEventCount++

		if e.Causation.AgentID == "" {
			emptyAgentIDCount++
			t.Logf("event %q missing Causation.AgentID (payload=%T)", e.Type, e.Payload)
			continue
		}
		if !strings.HasPrefix(e.Causation.AgentID, subagentInstanceID+"/") {
			wrongPrefixCount++
			t.Logf("event %q AgentID=%q lacks expected prefix %q/",
				e.Type, e.Causation.AgentID, subagentInstanceID)
			continue
		}
		if e.Causation.Depth != 1 {
			wrongDepthCount++
			t.Logf("event %q Depth=%d, want 1 (orchestrator at depth 0 → worker at depth 1)",
				e.Type, e.Causation.Depth)
		}
		// AgentID after the prefix should be the SpawnID — record so we
		// can assert worker isolation below.
		suffix := strings.TrimPrefix(e.Causation.AgentID, subagentInstanceID+"/")
		if suffix != "" {
			seenSpawnIDsInAgentID[suffix] = struct{}{}
		}
	}

	if subagentEventCount == 0 {
		t.Fatal("no subagent.* events captured beyond spawn — mock setup did not produce worker emissions")
	}
	if emptyAgentIDCount > 0 {
		t.Errorf("%d subagent events had empty Causation.AgentID (audit P1-B regression)", emptyAgentIDCount)
	}
	if wrongPrefixCount > 0 {
		t.Errorf("%d subagent events had unexpected AgentID prefix", wrongPrefixCount)
	}
	if wrongDepthCount > 0 {
		t.Errorf("%d subagent events had wrong Depth (want 1)", wrongDepthCount)
	}

	// With max_workers=3 + 3 subtasks, three distinct SpawnIDs should appear
	// in the AgentID stream — proves multiple workers each pushed their own
	// causation context, not a single shared one.
	if len(seenSpawnIDsInAgentID) < 2 {
		t.Errorf("expected at least 2 distinct spawn IDs in subagent AgentIDs, got %d (%v)",
			len(seenSpawnIDsInAgentID), seenSpawnIDsInAgentID)
	}
}

// TestConcurrentSubagent_LLMResponseRequestIDPropagation locks in the P1-A
// fix: every llm.response generated for a request with a RequestID must
// carry the same RequestID back, so the token-budget gate's reserve/commit
// accounting and the provider's cancel registry can correlate by ID instead
// of relying on a single-slot race-prone field.
//
// Mock-mode caveat: when the io.test mock plugin vetoes before:llm.request,
// the bus suppresses the bare llm.request event. We therefore can't pair
// responses against captured llm.request events; instead we check that
// every non-empty RequestID on a response is preserved (non-empty) and
// distinct across the concurrent worker fan-out — proving the synthetic
// response did not share a slot or overwrite per-request state.
func TestConcurrentSubagent_LLMResponseRequestIDPropagation(t *testing.T) {
	h := testharness.New(t, "configs/test-orchestrator-mock.yaml", testharness.WithTimeout(30*time.Second))
	h.Run()

	seen := make(map[string]int)
	for _, e := range h.Events() {
		if e.Type != "llm.response" {
			continue
		}
		resp, ok := e.Payload.(events.LLMResponse)
		if !ok || resp.RequestID == "" {
			continue
		}
		seen[resp.RequestID]++
	}

	// At least three distinct RequestIDs (orchestrator decompose + 3 workers
	// at minimum; synthesis adds another). Duplicates would indicate that
	// the SyncLLM correlation key wasn't unique per request.
	if len(seen) < 3 {
		t.Errorf("expected at least 3 distinct response RequestIDs from concurrent fan-out, got %d (%v)",
			len(seen), seen)
	}
	for id, count := range seen {
		if count > 1 {
			t.Errorf("response RequestID %q appeared %d times — concurrent requests likely collided", id, count)
		}
	}
}
