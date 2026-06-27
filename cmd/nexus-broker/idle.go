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

// idleSweeper periodically releases leases that have seen no real client input
// for longer than idle_timeout. It reuses the shared releaseLease teardown
// (shutdown frame → bounded grace → force-kill → reap → remove → free slot →
// close client WS), so it never reimplements shutdown logic — it only picks the
// idle leases and funnels them through that single path with reasonIdle.
//
// A non-positive timeout DISABLES idle reaping: Run returns immediately and no
// lease is ever released on idle.
type idleSweeper struct {
	logger   *slog.Logger
	registry *Registry
	timeout  time.Duration
	interval time.Duration
	grace    time.Duration
}

// newIdleSweeper builds a sweeper for the given idle timeout and release grace.
// The tick interval is derived from the timeout (see sweepInterval). A
// non-positive grace falls back to defaultReleaseGrace, matching the manual
// release path.
func newIdleSweeper(logger *slog.Logger, registry *Registry, timeout, grace time.Duration) *idleSweeper {
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
		"idle_timeout", s.timeout, "sweep_interval", s.interval)
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

// sweep finds every lease idle past idle_timeout and releases it via the shared
// teardown path. Each release is launched in its own goroutine so a stubborn
// instance (one that overruns the grace period) cannot stall the sweep loop or
// delay reaping its siblings.
func (s *idleSweeper) sweep() {
	if s.timeout <= 0 {
		// Idle reaping is disabled; never release on idle.
		return
	}
	cutoff := s.registry.now().Add(-s.timeout)
	for _, id := range s.registry.idleLeases(cutoff) {
		s.logger.Info("releasing idle lease",
			"lease_id", id, "idle_timeout", s.timeout)
		go func(id string) {
			if err := s.registry.releaseLease(id, reasonIdle, s.grace); err != nil &&
				!errors.Is(err, errUnknownLease) {
				s.logger.Warn("idle release failed", "lease_id", id, "error", err)
			}
		}(id)
	}
}
