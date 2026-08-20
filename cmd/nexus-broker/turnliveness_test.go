package main

import (
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/frankbardon/nexus/pkg/brokerframe"
)

// This file covers turn liveness: the broker reading the instance's own
// io.status frames so idleness means "nobody is waiting on this" rather than
// "nobody has typed lately", and the max_turn_duration bound that stops a turn
// which never settles from holding a lease forever.
//
// Everything here uses the registry's swappable clock (Registry.now) rather
// than sleeping, so the timings are exact and the tests are instant.

// statusFrame builds an instance -> client io frame carrying one io.status
// payload, the way plugins/io/broker encodes it.
func statusFrame(t *testing.T, leaseID, state string) []byte {
	t.Helper()
	data, err := encodeIOFrame(leaseID, brokerIOMessage{Type: ioTypeStatus, State: state})
	if err != nil {
		t.Fatalf("encodeIOFrame(%q): %v", state, err)
	}
	return data
}

// TestTurnStateIsWork pins the vocabulary the sweeper's exemption keys off. A
// state moving in or out of this set changes which sessions survive an idle
// window, so it is asserted rather than left to the map literal.
func TestTurnStateIsWork(t *testing.T) {
	cases := []struct {
		state string
		want  bool
	}{
		{"thinking", true},
		{"tool_running", true},
		{"streaming", true},
		{"waiting", true},
		{"cancelling", true},
		{ioStateIdle, false},
		{"", false},
		{"a-state-this-broker-has-never-heard-of", false},
	}
	for _, tc := range cases {
		if got := turnStateIsWork(tc.state); got != tc.want {
			t.Errorf("turnStateIsWork(%q) = %v, want %v", tc.state, got, tc.want)
		}
	}
}

// TestIdleLeases_SkipsLeaseWithLiveTurn is the core of the story: a lease whose
// instance is working is not selected for idle reaping however stale its last
// client input is, and becomes selectable again the moment the turn settles.
func TestIdleLeases_SkipsLeaseWithLiveTurn(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(testLogger(), 0)
	reg.now = clk.now

	id, _, _ := seedLiveLease(t, reg, newFakeProcess(420))

	// The instance starts working, then an hour passes with no client input.
	reg.markTurnLive(id)
	clk.advance(time.Hour)

	if got := reg.idleLeases(clk.now().Add(-time.Minute)); len(got) != 0 {
		t.Fatalf("idleLeases = %v for a lease with a live turn, want none", got)
	}
	if !reg.turnLive(id) {
		t.Fatal("turnLive = false after markTurnLive")
	}

	// The turn settles. That restarts the idle clock, so the lease is still not
	// stale immediately — the user has the full window to read the answer.
	reg.markTurnSettled(id)
	if reg.turnLive(id) {
		t.Fatal("turnLive = true after markTurnSettled")
	}
	if got := reg.idleLeases(clk.now().Add(-time.Minute)); len(got) != 0 {
		t.Fatalf("idleLeases = %v immediately after a turn settled, want none: "+
			"settling must restart the idle clock, or a long turn is reaped the "+
			"instant it finishes", got)
	}

	// Past the window measured from the settle: now it is genuinely idle.
	clk.advance(2 * time.Minute)
	got := reg.idleLeases(clk.now().Add(-time.Minute))
	if len(got) != 1 || got[0] != id {
		t.Fatalf("idleLeases = %v after the window elapsed post-turn, want [%s]", got, id)
	}
}

// TestMarkTurnLive_IsIdempotentWithinTurn proves turnStartedAt measures the
// whole turn rather than the gap since its most recent status frame. If a later
// work state re-stamped it, an instance emitting "thinking" in a loop would
// outrun max_turn_duration forever — the exact failure the bound exists to catch.
func TestMarkTurnLive_IsIdempotentWithinTurn(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(testLogger(), 0)
	reg.now = clk.now

	id, _, _ := seedLiveLease(t, reg, newFakeProcess(421))

	reg.markTurnLive(id)
	started := clk.now()

	// Two more work states, ten minutes apart, must not move the start.
	clk.advance(10 * time.Minute)
	reg.markTurnLive(id)
	clk.advance(10 * time.Minute)
	reg.markTurnLive(id)

	reg.mu.Lock()
	got := reg.leases[id].turnStartedAt
	reg.mu.Unlock()
	if !got.Equal(started) {
		t.Fatalf("turnStartedAt = %v after later work states, want %v (the first one)", got, started)
	}
}

// TestOverrunTurnLeases_SelectsOnlyOverrunLiveTurns covers the three ways a
// lease can fail to qualify for the turn bound: no turn in flight, a turn
// younger than the cutoff, and a lease already being torn down.
func TestOverrunTurnLeases_SelectsOnlyOverrunLiveTurns(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(testLogger(), 0)
	reg.now = clk.now

	settled, _, _ := seedLiveLease(t, reg, newFakeProcess(422))
	overrun, _, _ := seedLiveLease(t, reg, newFakeProcess(423))
	releasing, _, _ := seedLiveLease(t, reg, newFakeProcess(424))

	reg.markTurnLive(overrun)
	reg.markTurnLive(releasing)
	reg.mu.Lock()
	reg.leases[releasing].releasing = true
	reg.mu.Unlock()

	// Not yet past the bound.
	if got := reg.overrunTurnLeases(clk.now().Add(-time.Minute)); len(got) != 0 {
		t.Fatalf("overrunTurnLeases = %v for fresh turns, want none", got)
	}

	clk.advance(2 * time.Minute)
	got := reg.overrunTurnLeases(clk.now().Add(-time.Minute))
	if len(got) != 1 || got[0] != overrun {
		t.Fatalf("overrunTurnLeases = %v, want [%s] (a settled lease and a releasing "+
			"lease must both be skipped; settled=%s releasing=%s)",
			got, overrun, settled, releasing)
	}
}

// TestIdleSweeper_LiveTurnSurvivesIdleTimeout is the operational statement of
// the story: a ten-minute autonomous turn outlives a one-minute idle_timeout,
// with no client input at all, and is reaped normally once it settles.
func TestIdleSweeper_LiveTurnSurvivesIdleTimeout(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(testLogger(), 0)
	reg.now = clk.now

	proc := newFakeProcess(425)
	id, l, _ := seedLiveLease(t, reg, proc)
	close(proc.exited)

	// idle_timeout 1m, turn bound 1h: the turn is long but well inside the bound.
	sweeper := newIdleSweeper(testLogger(), reg, time.Minute, time.Hour, time.Second)

	reg.markTurnLive(id)
	for range 10 {
		clk.advance(time.Minute)
		sweeper.sweep()
		if !reg.Has(id) {
			t.Fatal("a lease with a live turn was reaped past idle_timeout")
		}
	}

	// The turn ends. The lease is now an ordinary quiet lease and reaps as idle.
	reg.markTurnSettled(id)
	clk.advance(2 * time.Minute)
	sweeper.sweep()

	waitUntil(t, func() bool { return !reg.Has(id) })
	reg.mu.Lock()
	gotReason := l.reason
	reg.mu.Unlock()
	if gotReason != reasonIdle {
		t.Errorf("settled lease reason = %q, want %q", gotReason, reasonIdle)
	}
}

// TestIdleSweeper_ReapsOverrunTurnWithTurnTimeoutReason proves the bound fires
// and that it is DISTINGUISHABLE from an idle release — an operator reading the
// journal must be able to tell "nobody was here" from "killed mid-work".
func TestIdleSweeper_ReapsOverrunTurnWithTurnTimeoutReason(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(testLogger(), 0)
	reg.now = clk.now

	proc := newFakeProcess(426)
	id, l, client := seedLiveLease(t, reg, proc)
	close(proc.exited)

	// idle_timeout 1m, turn bound 5m: the turn is exempt from the first and
	// caught by the second.
	sweeper := newIdleSweeper(testLogger(), reg, time.Minute, 5*time.Minute, time.Second)

	reg.markTurnLive(id)
	clk.advance(4 * time.Minute)
	sweeper.sweep()
	if !reg.Has(id) {
		t.Fatal("lease reaped before max_turn_duration elapsed")
	}

	clk.advance(2 * time.Minute)
	sweeper.sweep()

	waitUntil(t, func() bool { return !reg.Has(id) })

	reg.mu.Lock()
	gotReason := l.reason
	reg.mu.Unlock()
	if gotReason != reasonTurnTimeout {
		t.Errorf("overrun-turn lease reason = %q, want %q (reusing %q would hide "+
			"a wedged instance in the idle statistics)", gotReason, reasonTurnTimeout, reasonIdle)
	}
	if reasonTurnTimeout == reasonIdle {
		t.Fatal("reasonTurnTimeout must be distinct from reasonIdle")
	}

	// Teardown is the ordinary graceful path, not the crash path.
	select {
	case <-client.closed:
	default:
		t.Fatal("client connection was not closed after a turn-timeout release")
	}
	if client.closeStatus == crashCloseStatus {
		t.Errorf("turn-timeout release closed client with crash status %v", client.closeStatus)
	}
	if got := reg.SlotsInUse(); got != 0 {
		t.Errorf("slots in use after a turn-timeout release = %d, want 0 (the instance leaked)", got)
	}
}

// TestIdleSweeper_TurnBoundDisabledByNonPositiveMaxTurn proves max_turn_duration
// <= 0 restores the unbounded exemption: a live turn then holds its lease
// indefinitely, which is the documented opt-out.
func TestIdleSweeper_TurnBoundDisabledByNonPositiveMaxTurn(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(testLogger(), 0)
	reg.now = clk.now

	id, _, _ := seedLiveLease(t, reg, newFakeProcess(427))
	sweeper := newIdleSweeper(testLogger(), reg, time.Minute, 0, time.Second)

	reg.markTurnLive(id)
	clk.advance(24 * time.Hour)
	sweeper.sweep()

	if !reg.Has(id) {
		t.Fatal("lease reaped despite the turn bound being disabled")
	}
}

// TestIdleSweeper_DisabledTimeoutAlsoDisablesTurnBound pins the documented
// interaction: idle_timeout <= 0 switches the whole sweeper off, so a configured
// max_turn_duration is inert. A broker told never to reap must never reap.
func TestIdleSweeper_DisabledTimeoutAlsoDisablesTurnBound(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(testLogger(), 0)
	reg.now = clk.now

	id, _, _ := seedLiveLease(t, reg, newFakeProcess(428))
	sweeper := newIdleSweeper(testLogger(), reg, 0, time.Minute, time.Second)

	reg.markTurnLive(id)
	clk.advance(time.Hour)
	sweeper.sweep()

	if !reg.Has(id) {
		t.Fatal("lease reaped despite idle_timeout <= 0 disabling reaping entirely")
	}
}

// TestGatewayTurnLiveness_StatusDrivesTurnState runs the real gateway end to
// end: the instance's io.status frames move the lease's turn state AND still
// reach the client untouched. Observation must be purely additive.
func TestGatewayTurnLiveness_StatusDrivesTurnState(t *testing.T) {
	wsURL, registry := newTestGateway(t)

	leaseID, err := registry.NewLease(anonymousOwner())
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}
	registry.SetSpawnSecret(leaseID, testSpawnSecret)

	instance := dial(t, wsURL+instanceWSPath)
	defer instance.Close(websocket.StatusNormalClosure, "")
	writeFrame(t, instance, brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalRegister,
		Secret:  testSpawnSecret,
	})
	waitFor(t, func() bool { return registry.InstanceConn(leaseID) != nil })

	client := dial(t, wsURL+ClientWSPath(leaseID))
	defer client.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return registry.ClientConn(leaseID) != nil })

	if registry.turnLive(leaseID) {
		t.Fatal("turn is live before the instance reported any status")
	}

	// A work state starts the turn, and the frame still reaches the client.
	if err := instance.Write(t.Context(), websocket.MessageText, statusFrame(t, leaseID, "thinking")); err != nil {
		t.Fatalf("write thinking status: %v", err)
	}
	got := readFrame(t, client)
	if got.Signal != brokerframe.SignalIO {
		t.Fatalf("client got %v, want an io frame", got.Signal)
	}
	waitFor(t, func() bool { return registry.turnLive(leaseID) })

	// Idle settles it, and that frame reaches the client too.
	if err := instance.Write(t.Context(), websocket.MessageText, statusFrame(t, leaseID, ioStateIdle)); err != nil {
		t.Fatalf("write idle status: %v", err)
	}
	got = readFrame(t, client)
	if got.Signal != brokerframe.SignalIO {
		t.Fatalf("client got %v, want an io frame", got.Signal)
	}
	waitFor(t, func() bool { return !registry.turnLive(leaseID) })
}

// TestGatewayTurnLiveness_UndecodableAndUnknownPayloadsAreIgnored is the
// leniency guarantee. The gateway now decodes a payload it used to treat as
// opaque, so a payload it cannot read — or a status state it has never heard of
// — must change nothing and must still be forwarded verbatim. Anything else
// would make a newer instance's frames disappear behind an older broker.
func TestGatewayTurnLiveness_UndecodableAndUnknownPayloadsAreIgnored(t *testing.T) {
	wsURL, registry := newTestGateway(t)

	leaseID, err := registry.NewLease(anonymousOwner())
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}
	registry.SetSpawnSecret(leaseID, testSpawnSecret)

	instance := dial(t, wsURL+instanceWSPath)
	defer instance.Close(websocket.StatusNormalClosure, "")
	writeFrame(t, instance, brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalRegister,
		Secret:  testSpawnSecret,
	})
	waitFor(t, func() bool { return registry.InstanceConn(leaseID) != nil })

	client := dial(t, wsURL+ClientWSPath(leaseID))
	defer client.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return registry.ClientConn(leaseID) != nil })

	// Start a turn so there is a state an ignorable payload could wrongly clear.
	if err := instance.Write(t.Context(), websocket.MessageText, statusFrame(t, leaseID, "tool_running")); err != nil {
		t.Fatalf("write tool_running status: %v", err)
	}
	readFrame(t, client)
	waitFor(t, func() bool { return registry.turnLive(leaseID) })

	cases := []struct {
		name    string
		payload string
	}{
		// Not an object at all: decodeIOPayload reports it, the gateway ignores it.
		{"undecodable", `[1,2,3]`},
		// An object, but not a status frame.
		{"non-status", `{"type":"output","content":"hello"}`},
		// A status frame naming a state this broker does not know.
		{"unknown-state", `{"type":"status","state":"defragmenting"}`},
		// A status frame carrying a field this broker does not know.
		{"unknown-field", `{"type":"status","state":"thinking","weather":"fine"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame, err := brokerframe.Encode(brokerframe.Frame{
				LeaseID: leaseID,
				Signal:  brokerframe.SignalIO,
				Payload: []byte(tc.payload),
			})
			if err != nil {
				t.Fatalf("encode frame: %v", err)
			}
			if err := instance.Write(t.Context(), websocket.MessageText, frame); err != nil {
				t.Fatalf("write frame: %v", err)
			}
			got := readFrame(t, client)
			if got.Signal != brokerframe.SignalIO || string(got.Payload) != tc.payload {
				t.Fatalf("client got %+v, want the payload forwarded verbatim (%s)", got, tc.payload)
			}
			if !registry.turnLive(leaseID) {
				t.Fatal("an ignorable payload settled a live turn")
			}
		})
	}
}
