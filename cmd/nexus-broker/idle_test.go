package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeClock is a controllable clock the registry uses for lease timestamps and
// the idle sweeper's cutoff, so idle reaping is exercised deterministically by
// advancing time rather than sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// waitUntil polls cond until it is true or a bounded deadline elapses. It backs
// the one place an idle release runs asynchronously (the sweeper spawns a
// goroutine per release) without an unbounded sleep.
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func TestSweepInterval(t *testing.T) {
	cases := []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{"disabled-zero", 0, 0},
		{"disabled-negative", -time.Second, 0},
		{"quarter", 40 * time.Second, 10 * time.Second},
		{"capped", time.Hour, idleSweepIntervalCap},
		{"floored", 10 * time.Millisecond, idleSweepIntervalFloor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sweepInterval(tc.timeout); got != tc.want {
				t.Errorf("sweepInterval(%v) = %v, want %v", tc.timeout, got, tc.want)
			}
		})
	}
}

func TestIdleLeases_SelectsOnlyStaleLeases(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(testLogger())
	reg.now = clk.now

	id, _, _ := seedLiveLease(t, reg, newFakeProcess(400))

	// Not yet idle: cutoff is before the lease's lastActivity.
	if got := reg.idleLeases(clk.now().Add(-time.Minute)); len(got) != 0 {
		t.Fatalf("idleLeases returned %v for a fresh lease, want none", got)
	}

	// Advance past the window: the lease is now stale.
	clk.advance(2 * time.Minute)
	got := reg.idleLeases(clk.now().Add(-time.Minute))
	if len(got) != 1 || got[0] != id {
		t.Fatalf("idleLeases = %v, want [%s]", got, id)
	}
}

func TestMarkActivity_ResetsIdleTimer(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(testLogger())
	reg.now = clk.now

	id, _, _ := seedLiveLease(t, reg, newFakeProcess(401))

	// Almost idle.
	clk.advance(55 * time.Second)
	// Real client input resets the timer to "now".
	reg.markActivity(id)

	// Another 55s: still within the 1m window measured from the reset, so the
	// lease is NOT selected for reaping.
	clk.advance(55 * time.Second)
	if got := reg.idleLeases(clk.now().Add(-time.Minute)); len(got) != 0 {
		t.Fatalf("idleLeases = %v after activity reset, want none", got)
	}

	// Past the window from the reset: now it is stale.
	clk.advance(10 * time.Second)
	if got := reg.idleLeases(clk.now().Add(-time.Minute)); len(got) != 1 {
		t.Fatalf("idleLeases = %v after window elapsed, want 1", got)
	}
}

func TestMarkActivity_IgnoresReleasingLease(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(testLogger())
	reg.now = clk.now

	id, _, _ := seedLiveLease(t, reg, newFakeProcess(402))
	reg.mu.Lock()
	reg.leases[id].releasing = true
	reg.mu.Unlock()

	clk.advance(2 * time.Minute)
	if got := reg.idleLeases(clk.now().Add(-time.Minute)); len(got) != 0 {
		t.Fatalf("idleLeases = %v, want none (a releasing lease is skipped)", got)
	}
}

// TestIdleSweeper_ReleasesIdleLease drives a full sweep: an idle lease is torn
// down through the shared release path with reasonIdle, and its client WS is
// closed with the graceful (non-crash) status so idle is distinguishable from a
// crash.
func TestIdleSweeper_ReleasesIdleLease(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(testLogger())
	reg.now = clk.now

	proc := newFakeProcess(403)
	id, l, client := seedLiveLease(t, reg, proc)
	// The instance exits cleanly on the shutdown frame (graceful release path).
	close(proc.exited)

	sweeper := newIdleSweeper(testLogger(), reg, time.Minute, time.Second)
	clk.advance(2 * time.Minute)
	sweeper.sweep()

	waitUntil(t, func() bool { return !reg.Has(id) })

	reg.mu.Lock()
	gotReason := l.reason
	reg.mu.Unlock()
	if gotReason != reasonIdle {
		t.Errorf("idle-released lease reason = %q, want %q", gotReason, reasonIdle)
	}

	select {
	case <-client.closed:
	default:
		t.Fatal("client connection was not closed after idle release")
	}
	if client.closeStatus == crashCloseStatus {
		t.Errorf("idle release closed client with crash status %v", client.closeStatus)
	}
}

// TestIdleSweeper_ActivityKeepsLeaseAlive proves a client io frame within the
// window keeps the lease alive across a sweep.
func TestIdleSweeper_ActivityKeepsLeaseAlive(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(testLogger())
	reg.now = clk.now

	id, _, _ := seedLiveLease(t, reg, newFakeProcess(404))

	sweeper := newIdleSweeper(testLogger(), reg, time.Minute, time.Second)

	clk.advance(40 * time.Second)
	reg.markActivity(id) // real client input resets the timer
	clk.advance(40 * time.Second)
	sweeper.sweep()

	if !reg.Has(id) {
		t.Fatal("active lease was reaped despite recent client input")
	}
}

// TestIdleSweeper_DisabledByNonPositiveTimeout proves idle_timeout <= 0 disables
// reaping entirely: Run returns immediately and no lease is released.
func TestIdleSweeper_DisabledByNonPositiveTimeout(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(testLogger())
	reg.now = clk.now

	id, _, _ := seedLiveLease(t, reg, newFakeProcess(405))

	sweeper := newIdleSweeper(testLogger(), reg, 0, time.Second)
	if sweeper.interval != 0 {
		t.Fatalf("disabled sweeper interval = %v, want 0", sweeper.interval)
	}

	// Run must return immediately (no ticker); a long-stale lease stays put.
	done := make(chan struct{})
	go func() { sweeper.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disabled sweeper Run did not return immediately")
	}

	clk.advance(time.Hour)
	sweeper.sweep() // even a manual sweep is a no-op when disabled
	if !reg.Has(id) {
		t.Error("lease released despite idle reaping being disabled")
	}
}
