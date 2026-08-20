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

	// live is the configuration snapshot all three bounds are read from, per
	// sweep rather than at construction, so a SIGHUP that changes idle_timeout,
	// max_turn_duration or release_grace changes what the next sweep does.
	live *configHolder

	// reloadable records that live is the PROCESS-WIDE snapshot rather than the
	// private one the constructor built, which is the only case in which the
	// bounds can change while the sweeper runs.
	//
	// It decides one thing: whether Run may return early. A sweeper built with
	// idle_timeout <= 0 and no way to be reconfigured has nothing to do for the
	// life of the process, and returning says so. A sweeper wired to the process
	// snapshot has: a reload can switch reaping on, and a goroutine that had
	// already exited could not notice.
	reloadable bool
}

// newIdleSweeper builds a sweeper for the given idle timeout, max turn duration
// and release grace, over a private configuration snapshot carrying just those
// three. A non-positive grace falls back to defaultReleaseGrace (see
// settleConfig), matching the manual release path.
//
// run() calls useConfigHolder afterwards to point it at the process-wide
// snapshot instead.
func newIdleSweeper(logger *slog.Logger, registry *Registry, timeout, maxTurn, grace time.Duration) *idleSweeper {
	if logger == nil {
		logger = slog.Default()
	}
	return &idleSweeper{
		logger:   logger,
		registry: registry,
		live: newLocalConfigHolder(Config{
			IdleTimeout:     timeout,
			MaxTurnDuration: maxTurn,
			ReleaseGrace:    grace,
		}),
	}
}

// useConfigHolder points the sweeper at the process-wide configuration snapshot
// and marks it reloadable. A nil holder is ignored, leaving the private snapshot
// the constructor built and the sweeper fixed for the life of the process.
func (s *idleSweeper) useConfigHolder(h *configHolder) {
	if h == nil {
		return
	}
	s.live = h
	s.reloadable = true
}

// timeout is the idle bound in force. Non-positive disables reaping entirely.
func (s *idleSweeper) timeout() time.Duration { return s.live.config().IdleTimeout }

// maxTurn is the in-flight-turn backstop in force. Non-positive disables it.
func (s *idleSweeper) maxTurn() time.Duration { return s.live.config().MaxTurnDuration }

// interval is the tick interval derived from the idle bound in force.
func (s *idleSweeper) interval() time.Duration { return sweepInterval(s.timeout()) }

// wakeInterval is how long Run sleeps before looking again: the derived sweep
// interval while reaping is on, and a fixed poll while it is off, so a reload
// that switches reaping on is noticed within one poll rather than never.
func (s *idleSweeper) wakeInterval() time.Duration {
	if iv := s.interval(); iv > 0 {
		return iv
	}
	return idleSweepIntervalCap
}

// Run drives the sweep loop until ctx is cancelled. It returns immediately when
// idle reaping is disabled (timeout <= 0). Start it in a goroutine and cancel
// ctx to stop it cleanly on shutdown.
func (s *idleSweeper) Run(ctx context.Context) {
	if s.timeout() <= 0 {
		if !s.reloadable {
			s.logger.Info("idle reaping disabled", "idle_timeout", s.timeout())
			return
		}
		s.logger.Info("idle reaping disabled; the sweeper stays parked so a config reload can "+
			"switch it on without a restart", "idle_timeout", s.timeout())
	} else {
		s.logger.Info("idle sweeper started",
			"idle_timeout", s.timeout(), "max_turn_duration", s.maxTurn(),
			"sweep_interval", s.interval())
	}
	// A timer rather than a ticker, re-armed from the bound in force each time
	// round, because idle_timeout is reloadable: a ticker's period is fixed at
	// construction, so a broker whose operator raised or lowered idle_timeout
	// would keep sweeping on the old cadence for the life of the process.
	t := time.NewTimer(s.wakeInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep()
			t.Reset(s.wakeInterval())
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
	// Both bounds are read ONCE, from one snapshot, so a reload landing mid-sweep
	// cannot have this pass reap on a mixture of old and new values.
	cfg := s.live.config()
	if cfg.IdleTimeout <= 0 {
		// Reaping is disabled; never release on either bound.
		return
	}
	now := s.registry.now()

	for _, id := range s.registry.idleLeases(now.Add(-cfg.IdleTimeout)) {
		s.logger.Info("releasing idle lease",
			"lease_id", id, "idle_timeout", cfg.IdleTimeout)
		s.release(id, reasonIdle, cfg.ReleaseGrace)
	}

	if cfg.MaxTurnDuration <= 0 {
		// The turn bound is disabled: a live turn holds its lease indefinitely.
		return
	}
	for _, id := range s.registry.overrunTurnLeases(now.Add(-cfg.MaxTurnDuration)) {
		s.logger.Warn("releasing a lease whose turn outlived max_turn_duration. The instance "+
			"never reported its turn finished — it is wedged, a tool never returned, or this "+
			"workload legitimately takes longer than the configured bound",
			"lease_id", id, "max_turn_duration", cfg.MaxTurnDuration)
		s.release(id, reasonTurnTimeout, cfg.ReleaseGrace)
	}
}

// release tears one lease down off the sweep loop's goroutine, so a grace period
// spent waiting on a stubborn instance never delays the rest of the sweep. The
// grace is passed in rather than read here, so every lease one sweep reaps is
// torn down on the same bound the sweep selected it under. An
// unknown lease is not an error: a concurrent manual release or crash teardown
// legitimately wins the race with the sweeper.
func (s *idleSweeper) release(id, reason string, grace time.Duration) {
	go func() {
		if err := s.registry.releaseLease(id, reason, grace); err != nil &&
			!errors.Is(err, errUnknownLease) {
			s.logger.Warn("sweeper release failed",
				"lease_id", id, "reason", reason, "error", err)
		}
	}()
}
