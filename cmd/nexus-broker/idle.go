package main

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const (
	// reasonIdle is the teardown reason recorded on a lease released by the idle
	// sweeper because no real client input arrived within idle_timeout. It is a
	// distinct, non-timing signal so the client WS close (a normal going-away
	// close, per clientCloseForReason) can be told apart from a manual release or
	// a crash by anything inspecting a lease's terminal reason.
	reasonIdle = "idle"

	// reasonTurnTimeout is the teardown reason recorded on a lease released
	// because its IN-FLIGHT TURN outlived max_turn_duration. It is a sibling of
	// reasonIdle, not a reuse of it, and the distinction is the whole point: a
	// lease torn down as reasonIdle was quiet and nobody was waiting on it, while
	// a lease torn down as reasonTurnTimeout was killed mid-work because its
	// instance never reported the turn finished. The first is routine capacity
	// hygiene; the second means an instance wedged, a tool never returned, or the
	// bound is set below how long this workload legitimately takes. An operator
	// reading the journal must be able to tell those apart without guessing.
	reasonTurnTimeout = "turn timeout"

	// defaultMaxTurnDuration bounds an in-flight turn when max_turn_duration is
	// not configured. It is the backstop on the live-turn skip in idleLeases: a
	// turn that never reports idle would otherwise hold its lease, and its
	// capacity slot, for the lifetime of the broker.
	//
	// Thirty minutes is chosen to sit well clear of a genuinely long autonomous
	// turn (deep research, a long tool chain, a slow batch model) while still
	// reclaiming a wedged instance the same working hour it wedged. It is
	// deliberately an order of magnitude above the 5-minute default idle_timeout,
	// because the two answer different questions: idle_timeout asks how long a
	// human may pause, this asks how long a machine may take.
	defaultMaxTurnDuration = 30 * time.Minute

	// idleSweepIntervalCap bounds how often the idle sweeper wakes regardless of
	// how large idle_timeout is, so a long timeout still gets reasonably timely
	// reaping without an unbounded sleep.
	idleSweepIntervalCap = 15 * time.Second

	// idleSweepIntervalFloor bounds the sweep interval from below so a
	// pathologically small idle_timeout cannot spin the sweeper into a hot loop.
	idleSweepIntervalFloor = 50 * time.Millisecond
)

// sweepInterval derives the idle sweeper's tick interval from idle_timeout. It
// ticks roughly four times per timeout window (so a lease is reaped within
// ~1.25×idle_timeout), clamped to [idleSweepIntervalFloor, idleSweepIntervalCap].
// A non-positive timeout returns 0, which disables idle reaping entirely.
func sweepInterval(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 0
	}
	iv := timeout / 4
	if iv > idleSweepIntervalCap {
		iv = idleSweepIntervalCap
	}
	if iv < idleSweepIntervalFloor {
		iv = idleSweepIntervalFloor
	}
	return iv
}

// idleSweeper periodically releases leases nothing is waiting on. It reuses the
// shared releaseLease teardown (shutdown frame → bounded grace → force-kill →
// reap → remove → free slot → close client WS), so it never reimplements
// shutdown logic — it only picks the leases and funnels them through that single
// path with the right reason.
//
// It enforces TWO bounds, which are separate policies with separate reasons:
//
//   - idle_timeout reaps a lease with NO turn in flight whose last activity is
//     older than the window, as reasonIdle. A lease whose instance is thinking
//     or running a tool is exempt however long ago its client last typed, which
//     is what lets idle_timeout be set to the longest expected human pause
//     rather than the longest expected turn.
//
//   - max_turn_duration reaps a lease whose turn HAS been in flight longer than
//     the bound, as reasonTurnTimeout. It is the backstop on that exemption, so
//     a wedged instance cannot hold a lease forever.
//
// A non-positive timeout DISABLES the sweeper entirely: Run returns immediately
// and no lease is released on either bound. That is deliberate — idle_timeout
// <= 0 has always meant "this broker does not reap", and a turn bound that
// started reaping leases on a broker configured never to reap would be a
// surprise, not a fix. A non-positive maxTurn disables only the turn bound,
// restoring the unbounded live-turn exemption.
type idleSweeper struct {
	logger   *slog.Logger
	registry *Registry
	timeout  time.Duration
	maxTurn  time.Duration
	interval time.Duration
	grace    time.Duration
}

// newIdleSweeper builds a sweeper for the given idle timeout, max turn duration
// and release grace. The tick interval is derived from the idle timeout (see
// sweepInterval). A non-positive grace falls back to defaultReleaseGrace,
// matching the manual release path.
func newIdleSweeper(logger *slog.Logger, registry *Registry, timeout, maxTurn, grace time.Duration) *idleSweeper {
	if logger == nil {
		logger = slog.Default()
	}
	if grace <= 0 {
		grace = defaultReleaseGrace
	}
	return &idleSweeper{
		logger:   logger,
		registry: registry,
		timeout:  timeout,
		maxTurn:  maxTurn,
		interval: sweepInterval(timeout),
		grace:    grace,
	}
}

// Run drives the sweep loop until ctx is cancelled. It returns immediately when
// idle reaping is disabled (timeout <= 0). Start it in a goroutine and cancel
// ctx to stop it cleanly on shutdown.
func (s *idleSweeper) Run(ctx context.Context) {
	if s.timeout <= 0 {
		s.logger.Info("idle reaping disabled", "idle_timeout", s.timeout)
		return
	}
	s.logger.Info("idle sweeper started",
		"idle_timeout", s.timeout, "max_turn_duration", s.maxTurn,
		"sweep_interval", s.interval)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep()
		}
	}
}

// sweep finds every lease past one of the two bounds and releases it via the
// shared teardown path. Each release is launched in its own goroutine so a
// stubborn instance (one that overruns the grace period) cannot stall the sweep
// loop or delay reaping its siblings.
//
// The two selections are disjoint by construction — idleLeases skips a lease
// with a turn in flight and overrunTurnLeases selects only those — so no lease
// can be handed to both, and releaseLease latches anyway.
func (s *idleSweeper) sweep() {
	if s.timeout <= 0 {
		// Reaping is disabled; never release on either bound.
		return
	}
	now := s.registry.now()

	for _, id := range s.registry.idleLeases(now.Add(-s.timeout)) {
		s.logger.Info("releasing idle lease",
			"lease_id", id, "idle_timeout", s.timeout)
		s.release(id, reasonIdle)
	}

	if s.maxTurn <= 0 {
		// The turn bound is disabled: a live turn holds its lease indefinitely.
		return
	}
	for _, id := range s.registry.overrunTurnLeases(now.Add(-s.maxTurn)) {
		s.logger.Warn("releasing a lease whose turn outlived max_turn_duration. The instance "+
			"never reported its turn finished — it is wedged, a tool never returned, or this "+
			"workload legitimately takes longer than the configured bound",
			"lease_id", id, "max_turn_duration", s.maxTurn)
		s.release(id, reasonTurnTimeout)
	}
}

// release tears one lease down off the sweep loop's goroutine, so a grace period
// spent waiting on a stubborn instance never delays the rest of the sweep. An
// unknown lease is not an error: a concurrent manual release or crash teardown
// legitimately wins the race with the sweeper.
func (s *idleSweeper) release(id, reason string) {
	go func() {
		if err := s.registry.releaseLease(id, reason, s.grace); err != nil &&
			!errors.Is(err, errUnknownLease) {
			s.logger.Warn("sweeper release failed",
				"lease_id", id, "reason", reason, "error", err)
		}
	}()
}
