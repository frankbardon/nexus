package testio

import (
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// io/test is the test IO plugin used by integration tests in
// pkg/testharness/. The contract test here just verifies the plugin
// advertises a stable static contract.
func TestContract_StaticContract(t *testing.T) {
	h := contract.NewContract(t, New)
	subs := h.Plugin().Subscriptions()
	if len(subs) == 0 {
		t.Error("Subscriptions() empty")
	}
	emits := h.Plugin().Emissions()
	if len(emits) == 0 {
		t.Error("Emissions() empty")
	}
}

// TestContract_DeclarationsMatchRuntime pins Subscriptions() to exactly the
// typed subscriptions Init registers (no stale declarations such as the
// retired io.output@50 / io.ask@10), and Emissions() to what the plugin
// actually emits: hitl.responded is answered by handleHITL, while the
// retired io.ask.response is never produced.
func TestContract_DeclarationsMatchRuntime(t *testing.T) {
	h := contract.NewContract(t, New)

	declared := map[string]bool{}
	for _, s := range h.Plugin().Subscriptions() {
		declared[s.EventType] = true
	}
	registered := map[string]bool{}
	for _, r := range h.RegisteredSubscriptions() {
		registered[r.EventType] = true
	}
	for typ := range declared {
		if !registered[typ] {
			t.Errorf("Subscriptions() declares %q but Init never subscribes to it", typ)
		}
	}
	for typ := range registered {
		if !declared[typ] {
			t.Errorf("Init subscribes to %q but Subscriptions() does not declare it", typ)
		}
	}

	emits := map[string]bool{}
	for _, e := range h.Plugin().Emissions() {
		emits[e] = true
	}
	if !emits["hitl.responded"] {
		t.Error(`Emissions() missing "hitl.responded" (emitted by handleHITL)`)
	}
	for _, retired := range []string{"io.ask.response"} {
		if emits[retired] {
			t.Errorf("Emissions() still declares retired event %q", retired)
		}
	}
}

// TestInputsWaitForCoreReady pins the first scripted input to core.ready rather
// than to this plugin's own Ready. The engine runs every Ready concurrently and
// emits core.ready only after all of them return; feeding from Ready raced the
// tool plugins' tool.register, so under load the first turn could call a tool
// the catalog had not seen yet and get "unknown tool" back while the real
// handler was still running (TestA2ALoopback_ChainedHITLInputTimeout's flake).
func TestInputsWaitForCoreReady(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"inputs": []any{"hello"},
	}))

	// The harness has already run Init and Ready. Give a goroutine started
	// from Ready ample time to have fed the input if it was going to.
	time.Sleep(200 * time.Millisecond)
	h.AssertNotEmitted("io.input")

	h.Inject("core.ready", nil)
	h.WaitForEmitted("io.input", 2*time.Second)

	// A second core.ready (a re-emission, a hot reload) must not replay the
	// script.
	h.Inject("core.ready", nil)
	time.Sleep(100 * time.Millisecond)
	n := 0
	for _, e := range h.PluginEmissions() {
		if e.Type == "io.input" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("io.input emitted %d times, want exactly 1", n)
	}
}
