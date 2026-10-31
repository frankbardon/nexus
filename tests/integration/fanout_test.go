//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestFanout_Boot validates that the engine boots cleanly with a fanout
// role config and the fanout coordinator plugin active.
func TestFanout_Boot(t *testing.T) {
	h := testharness.New(t, "configs/test-fanout.yaml", testharness.WithTimeout(30*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.io.test",
		"nexus.llm.anthropic",
		"nexus.llm.openai",
		"nexus.provider.fanout",
		"nexus.agent.react",
		"nexus.memory.capped",
	)
	h.AssertEventEmitted("io.session.start")
	h.AssertEventEmitted("io.session.end")
}

// TestFanout_DispatchesAndCollects validates that the fanout plugin dispatches
// to multiple providers and collects responses into a single combined response.
func TestFanout_DispatchesAndCollects(t *testing.T) {
	h := testharness.New(t, "configs/test-fanout.yaml", testharness.WithTimeout(30*time.Second))
	h.Run()

	// Fanout lifecycle events.
	h.AssertEventEmitted("provider.fanout.start")
	h.AssertEventEmitted("provider.fanout.complete")

	// Two per-provider response events (one per fanout leg).
	h.AssertEventCount("provider.fanout.response", 2, 2)

	// Agent should still produce output (from combined response). The merged
	// response must carry the ReAct request's own _source, not the fanout's,
	// or the agent drops it and the turn never completes with the answer.
	h.AssertEventEmitted("io.output")
	h.AssertOutputContains("Hello from provider")
	h.AssertEventEmitted("agent.turn.end")

	// The response the agent consumed (the merged one: it has no leg marker)
	// carries the original request's _source and the fanout markers.
	var merged *events.LLMResponse
	for _, e := range h.Events() {
		if e.Type != "llm.response" {
			continue
		}
		resp, ok := e.Payload.(events.LLMResponse)
		if !ok || engine.IsFanoutLegResponse(resp.Metadata) {
			continue
		}
		merged = &resp
	}
	if merged == nil {
		t.Fatal("no merged llm.response observed")
	}
	if src, _ := merged.Metadata["_source"].(string); src != "nexus.agent.react" {
		t.Fatalf("merged response _source = %q, want nexus.agent.react", src)
	}
	if merged.Metadata["_fanout"] != true {
		t.Fatalf("merged response missing _fanout marker: %v", merged.Metadata)
	}

	// Verify fanout.start payload has correct targets.
	for _, e := range h.Events() {
		if e.Type == "provider.fanout.start" {
			start, ok := e.Payload.(events.ProviderFanoutStart)
			if !ok {
				t.Fatal("provider.fanout.start payload is not ProviderFanoutStart")
			}
			if len(start.Targets) != 2 {
				t.Fatalf("expected 2 fanout targets, got %d", len(start.Targets))
			}
			if start.Strategy != "all" {
				t.Fatalf("expected strategy 'all', got %q", start.Strategy)
			}
		}
	}

	// Verify fanout.complete shows 2 successes.
	for _, e := range h.Events() {
		if e.Type == "provider.fanout.complete" {
			complete, ok := e.Payload.(events.ProviderFanoutComplete)
			if !ok {
				t.Fatal("provider.fanout.complete payload is not ProviderFanoutComplete")
			}
			if complete.Succeeded != 2 {
				t.Fatalf("expected 2 succeeded, got %d", complete.Succeeded)
			}
			if complete.Failed != 0 {
				t.Fatalf("expected 0 failed, got %d", complete.Failed)
			}
		}
	}
}
