package agui

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// TestContract_Subscriptions asserts the plugin's declared Subscriptions()
// cover every bus event the outbound translator wires in Init. The list is the
// canonical set plus the non-canonical events bridged as AG-UI Custom.
func TestContract_Subscriptions(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}))

	h.AssertSubscribesTo(
		"agent.turn.start",
		"agent.turn.end",
		"llm.stream.chunk",
		"llm.stream.end",
		"io.output",
		// Real agent tool calls ride tool.invoke (not tool.call); client-executed
		// tools ride the same event and additionally suspend the run.
		"tool.invoke",
		"tool.result",
		"thinking.step",
		// Virtual-run interrupt/cancel at the transport boundary.
		"hitl.requested",
		"hitl.cancel",
		// Per-run client tools are appended to this synchronous catalog snapshot.
		"tool.catalog.query",
	)
	// Non-canonical bus events ride the AG-UI Custom event and must be declared.
	h.AssertSubscribesTo(customBridgedEvents...)
}

// TestContract_EmitsInputOnRun asserts the runtime emission contract: starting a
// run publishes before:io.input then io.input, and no undeclared event types
// escape. startRun is the inbound seam the HTTP handler uses; driving it here
// exercises the same bus path without a socket round-trip.
func TestContract_EmitsInputOnRun(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}))

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	// Observe the emitted UserInput to confirm the inbound mapping is real.
	seen := make(chan events.UserInput, 1)
	h.Bus().Subscribe("io.input", func(e engine.Event[any]) {
		if in, ok := e.Payload.(events.UserInput); ok {
			select {
			case seen <- in:
			default:
			}
		}
	})

	run, started := p.startRun(runInput{
		threadID: "thread-contract",
		runID:    "run-contract",
		messages: []agui.Message{{ID: "m1", Role: "user", Content: "ping"}},
	})
	if !started {
		t.Fatal("startRun rejected on a fresh plugin")
	}
	t.Cleanup(func() {
		run.finish()
		p.endRun(run)
	})

	select {
	case in := <-seen:
		if in.Content != "ping" {
			t.Errorf("io.input content = %q, want ping", in.Content)
		}
		if in.SessionID != "thread-contract" {
			t.Errorf("io.input session = %q, want thread-contract", in.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for io.input emission")
	}

	// Declared emissions actually fired (before:io.input rides the veto path;
	// io.input is a plain emit captured by the harness).
	//
	// WaitForEmitted, not AssertEmitted: the select above woke this goroutine
	// from the test's OWN typed subscriber, and pkg/engine dispatches every
	// typed subscriber before any wildcard one — which is how the harness
	// captures. The recorder therefore has not necessarily run yet at this
	// point, and a bare AssertEmitted was reading state that had provably not
	// been written. It passed only because the emitting goroutine usually
	// finished its wildcard loop first; under -race that ordering shifted and
	// CI failed here.
	h.WaitForEmitted("io.input", 2*time.Second)
	h.AssertNoUndeclaredEmissions()
}

// TestContract_StartRunBindsPrincipalAndContextBeforeInput is the core E2-S1
// acceptance test: startRun binds _principal_id and every RunAgentInput.
// Context item into the session's Labels BEFORE the run's io.input is
// emitted, so a subscriber reacting to io.input already observes the bind.
func TestContract_StartRunBindsPrincipalAndContextBeforeInput(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}), contract.WithSession())

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	seenLabels := make(chan map[string]string, 1)
	h.Bus().Subscribe("io.input", func(engine.Event[any]) {
		meta, err := p.session.SessionMetadata()
		if err != nil {
			t.Errorf("session metadata at io.input: %v", err)
			return
		}
		select {
		case seenLabels <- meta.Labels:
		default:
		}
	})

	run, started := p.startRun(runInput{
		threadID:    "thread-bind",
		runID:       "run-bind",
		messages:    []agui.Message{{ID: "m1", Role: "user", Content: "hi"}},
		principalID: "principal-abc",
		contextItems: []agui.ContextItem{
			{Description: "dataset", Value: "prod"},
		},
	})
	if !started {
		t.Fatal("startRun rejected on a fresh plugin")
	}
	t.Cleanup(func() {
		run.finish()
		p.endRun(run)
	})

	select {
	case labels := <-seenLabels:
		if labels["_principal_id"] != "principal-abc" {
			t.Errorf("_principal_id = %q, want principal-abc", labels["_principal_id"])
		}
		if labels["dataset"] != "prod" {
			t.Errorf("dataset = %q, want prod", labels["dataset"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for io.input")
	}
}

// emitTurn is the agent stand-in these contract tests use for a turn
// boundary: the real emitters (react, planexec, orchestrator) always announce
// agent.turn.start before the matching agent.turn.end, and the identity
// lifetime correlates on exactly that pair.
func emitTurn(t *testing.T, h *contract.ContractHarness, eventType, turnID string) {
	t.Helper()
	if err := h.Bus().Emit(eventType, events.TurnInfo{
		SchemaVersion: events.TurnInfoVersion,
		TurnID:        turnID,
	}); err != nil {
		t.Fatalf("emit %s: %v", eventType, err)
	}
}

// TestContract_TurnEndClearsPrincipalIDLabel asserts the identity clear
// follows the TURN, not the run: endRun leaves _principal_id bound (the work
// may still be in flight — a HITL park frees the slot with the agent still
// blocked), and the turn's own agent.turn.end is what clears it. E1-S2 owns
// the full lifetime matrix.
func TestContract_TurnEndClearsPrincipalIDLabel(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}), contract.WithSession())

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	run, started := p.startRun(runInput{
		threadID:    "thread-end",
		runID:       "run-end",
		principalID: "principal-end",
	})
	if !started {
		t.Fatal("startRun rejected on a fresh plugin")
	}
	emitTurn(t, h, "agent.turn.start", "turn-end")
	run.finish()
	p.endRun(run)

	meta, err := p.session.SessionMetadata()
	if err != nil {
		t.Fatalf("session metadata: %v", err)
	}
	if meta.Labels["_principal_id"] != "principal-end" {
		t.Errorf("_principal_id = %q after endRun, want it still bound: %v",
			meta.Labels["_principal_id"], meta.Labels)
	}

	// The turn the identity belongs to ends; the identity goes with it.
	emitTurn(t, h, "agent.turn.end", "turn-end")

	meta, err = p.session.SessionMetadata()
	if err != nil {
		t.Fatalf("session metadata: %v", err)
	}
	if _, ok := meta.Labels["_principal_id"]; ok {
		t.Errorf("_principal_id still present after agent.turn.end: %v", meta.Labels)
	}
}

// TestContract_TurnEndClearsPrincipalIDWhileRunStillActive is the happy path:
// the turn ends while its own run is still draining SSE, which is when a
// completed AG-UI turn normally ends. The identity must go there too — a
// guard that skipped whenever a run held the slot would never fire at all.
func TestContract_TurnEndClearsPrincipalIDWhileRunStillActive(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}), contract.WithSession())

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	if _, started := p.startRun(runInput{
		threadID:    "thread-live",
		runID:       "run-live",
		principalID: "principal-live",
	}); !started {
		t.Fatal("startRun rejected on a fresh plugin")
	}
	emitTurn(t, h, "agent.turn.start", "turn-live")

	if p.currentRun() == nil {
		t.Fatal("run released before the turn ended; this test needs it live")
	}
	emitTurn(t, h, "agent.turn.end", "turn-live")

	meta, err := p.session.SessionMetadata()
	if err != nil {
		t.Fatalf("session metadata: %v", err)
	}
	if _, ok := meta.Labels["_principal_id"]; ok {
		t.Errorf("_principal_id still present after the turn ended on a live run: %v", meta.Labels)
	}
}

// TestContract_TurnEndOfAbandonedRunKeepsNewerRunsIdentity is the late-turn
// race: run A binds and its handler returns early (disconnect), run B POSTs
// and binds its own principal, and only then does A's turn end. B's identity
// must survive — the ending turn is not the one B's bind was made for.
func TestContract_TurnEndOfAbandonedRunKeepsNewerRunsIdentity(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}), contract.WithSession())

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	runA, started := p.startRun(runInput{
		threadID:    "thread-race",
		runID:       "run-a",
		principalID: "principal-a",
	})
	if !started {
		t.Fatal("startRun rejected on a fresh plugin")
	}
	emitTurn(t, h, "agent.turn.start", "turn-a")
	// Run A's handler returns without its turn ending: the slot frees, the
	// agent keeps working.
	p.endRun(runA)

	if _, started := p.startRun(runInput{
		threadID:    "thread-race",
		runID:       "run-b",
		principalID: "principal-b",
	}); !started {
		t.Fatal("startRun rejected after run A was released")
	}
	emitTurn(t, h, "agent.turn.start", "turn-b")

	// Turn A finally ends, long after B took over.
	emitTurn(t, h, "agent.turn.end", "turn-a")

	meta, err := p.session.SessionMetadata()
	if err != nil {
		t.Fatalf("session metadata: %v", err)
	}
	if meta.Labels["_principal_id"] != "principal-b" {
		t.Errorf("_principal_id = %q after an abandoned turn ended, want principal-b: %v",
			meta.Labels["_principal_id"], meta.Labels)
	}
}

// TestContract_ResumeRunRebindsPrincipalIndependentOfOriginal asserts
// resumeRun re-binds _principal_id from the resume request's own resolved
// principal, independent of (and replacing) whatever was bound at the
// original interrupted run — no "unchanged principal, skip the write" special
// case.
func TestContract_ResumeRunRebindsPrincipalIndependentOfOriginal(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}), contract.WithSession())

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	// Simulate the state left by an interrupted first run under one principal.
	if err := p.session.SetReservedLabel(reservedPrincipalIDKey, "principal-original"); err != nil {
		t.Fatalf("seed original principal: %v", err)
	}
	p.pendingMu.Lock()
	p.pending["interrupt-1"] = pendingInterrupt{
		Kind:        interruptHITL,
		InterruptID: "interrupt-1",
		RequestID:   "req-1",
		ThreadID:    "thread-resume",
		Mode:        events.HITLModeChoices,
	}
	p.pendingMu.Unlock()

	run, err := p.resumeRun(runInput{
		threadID:    "thread-resume",
		runID:       "run-resume",
		principalID: "principal-new",
		resume: []agui.ResumeItem{
			{InterruptID: "interrupt-1", Status: agui.ResumeResolved},
		},
		contextItems: []agui.ContextItem{
			{Description: "region", Value: "us-east"},
		},
	})
	if err != nil {
		t.Fatalf("resumeRun: %v", err)
	}
	t.Cleanup(func() {
		run.finish()
		p.endRun(run)
	})

	meta, err := p.session.SessionMetadata()
	if err != nil {
		t.Fatalf("session metadata: %v", err)
	}
	if meta.Labels["_principal_id"] != "principal-new" {
		t.Errorf("_principal_id = %q, want principal-new (independent of original)", meta.Labels["_principal_id"])
	}
	if meta.Labels["region"] != "us-east" {
		t.Errorf("region = %q, want us-east", meta.Labels["region"])
	}
}

// TestContract_ContextItemCannotSpoofReservedKey asserts the structural
// separation the interview called for: a client cannot smuggle a "_"-prefixed
// key through RunAgentInput.Context and have it land in the reserved
// namespace, even though both ride the same general SetLabel call.
func TestContract_ContextItemCannotSpoofReservedKey(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}), contract.WithSession())

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	run, started := p.startRun(runInput{
		threadID: "thread-spoof",
		runID:    "run-spoof",
		contextItems: []agui.ContextItem{
			{Description: reservedPrincipalIDKey, Value: "spoofed"},
		},
	})
	if !started {
		t.Fatal("startRun rejected on a fresh plugin")
	}
	t.Cleanup(func() {
		run.finish()
		p.endRun(run)
	})

	meta, err := p.session.SessionMetadata()
	if err != nil {
		t.Fatalf("session metadata: %v", err)
	}
	if meta.Labels[reservedPrincipalIDKey] == "spoofed" {
		t.Error("a RunAgentInput.Context item was able to write the reserved _principal_id key")
	}
}

// TestContract_BindAndClearAnnounceTagEvents asserts session.tag.set/
// session.tag.deleted fire for the identity bind, the identity clear (at the
// turn's end, not the run's) and each context item — the announce channel
// Arc's own registry is expected to subscribe to.
func TestContract_BindAndClearAnnounceTagEvents(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"bind": freeAddr(t),
	}), contract.WithSession())

	p, ok := h.Plugin().(*Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *Plugin", h.Plugin())
	}

	var mu sync.Mutex
	var setKeys, deletedKeys []string
	h.Bus().Subscribe("session.tag.set", func(e engine.Event[any]) {
		if s, ok := e.Payload.(events.SessionTagSet); ok {
			mu.Lock()
			setKeys = append(setKeys, s.Key)
			mu.Unlock()
		}
	})
	h.Bus().Subscribe("session.tag.deleted", func(e engine.Event[any]) {
		if d, ok := e.Payload.(events.SessionTagDeleted); ok {
			mu.Lock()
			deletedKeys = append(deletedKeys, d.Key)
			mu.Unlock()
		}
	})

	run, started := p.startRun(runInput{
		threadID:    "thread-announce",
		runID:       "run-announce",
		principalID: "principal-announce",
		contextItems: []agui.ContextItem{
			{Description: "dataset", Value: "prod"},
		},
	})
	if !started {
		t.Fatal("startRun rejected on a fresh plugin")
	}
	emitTurn(t, h, "agent.turn.start", "turn-announce")
	run.finish()
	p.endRun(run)
	// The clear rides agent.turn.end now, so the announce does too.
	emitTurn(t, h, "agent.turn.end", "turn-announce")

	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(setKeys, reservedPrincipalIDKey) {
		t.Errorf("session.tag.set never fired for %q; saw %v", reservedPrincipalIDKey, setKeys)
	}
	if !slices.Contains(setKeys, "dataset") {
		t.Errorf("session.tag.set never fired for dataset; saw %v", setKeys)
	}
	if !slices.Contains(deletedKeys, reservedPrincipalIDKey) {
		t.Errorf("session.tag.deleted never fired for %q; saw %v", reservedPrincipalIDKey, deletedKeys)
	}
}
