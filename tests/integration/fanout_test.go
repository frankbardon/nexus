//go:build integration

package integration

import (
	"testing"
	"time"

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
		"nexus.memory.conversation",
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

	// Agent should still produce output (from combined response).
	h.AssertEventEmitted("io.output")

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
