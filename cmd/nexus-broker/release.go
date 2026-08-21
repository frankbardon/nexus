package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/frankbardon/nexus/pkg/brokerframe"
)

// errUnknownLease is returned by releaseLease for a lease id that is not (or no
// longer) in the registry. The HTTP layer maps it to 404 so a manual release of
// an already-gone lease is a clean, idempotent no-op rather than a server error.
var errUnknownLease = errors.New("unknown lease")

// releaseLease is the single, shared teardown path for a lease. Manual release
// (E2-S2), idle timeout (E2-S3), crash handling (E2-S4), and slot accounting
// (E3-S1) all funnel through here so shutdown/reap logic lives in exactly one
// place. It:
//
//  1. Sends a shutdown frame to the instance so it shuts its engine down
//     cleanly, flushing and persisting the session (the session directory under
//     ~/.nexus/sessions/<id>/ is left intact and resumable).
//  2. Waits a BOUNDED grace period for the process to exit on its own, then
//     ESCALATES: SIGTERM to the instance's process group, and SIGKILL to the same
//     group after a second, shorter window. Either way the process is reaped by
//     the registry's single reaper goroutine.
//  3. Removes the lease from the registry, freeing its slot and closing both
//     connections.
//
// The SIGTERM step exists because step 1 is delivered over the dial-back socket
// and step 3's accounting has to be correct even when that socket is not there.
// An instance that is wedged, or mid reconnect-backoff, receives no shutdown
// frame at all: without the signal the grace period is spent waiting for
// something that was never asked, and the engine's first notice of teardown is
// SIGKILL — no flush, no session persisted. SIGTERM is what the engine handles
// as a clean shutdown, and it needs nothing from the instance to arrive.
//
// It returns errUnknownLease for an unknown lease and is safe to call
// concurrently: only the first caller performs the teardown; a second
// concurrent call returns nil immediately (the teardown is already underway).
//
// It performs NO authorization. Lease ownership is checked by handleRelease
// before it calls through, because the idle sweeper and the crash watcher reach
// this function as the broker acting on itself, with no principal at all. Do not
// move an ownership check in here: those two paths would then either have to
// forge a caller or start failing, and a failing teardown leaks instances
// silently.
func (r *Registry) releaseLease(id, reason string, grace time.Duration) error {
	r.mu.Lock()
	l, ok := r.leases[id]
	if !ok {
		r.mu.Unlock()
		return errUnknownLease
	}
	if l.releasing {
		// Another teardown is already underway; treat as an idempotent no-op.
		r.mu.Unlock()
		return nil
	}
	l.releasing = true
	l.reason = reason
	instance := l.instance
	process := l.process
	exited := l.exited
	r.mu.Unlock()

	// 1. Ask the instance to shut its engine down cleanly. A missing/dead
	//    instance connection just means we skip straight to grace + kill.
	if instance != nil {
		if data, err := brokerframe.Encode(brokerframe.Frame{
			LeaseID: id,
			Signal:  brokerframe.SignalShutdown,
		}); err == nil {
			if !instance.queue(data) {
				r.logger.Warn("could not deliver shutdown frame to instance",
					"lease_id", id, "reason", reason)
			}
		}
	}

	// 2. Wait out a bounded grace period for the process to exit, then escalate
	//    SIGTERM → SIGKILL, both to the instance's whole process group so its
	//    subprocesses go with it. The reaper closes exited after wait() returns,
	//    so we always reap — graceful exit, signalled exit or forced kill alike.
	if process != nil {
		select {
		case <-exited:
			r.logger.Info("instance exited gracefully", "lease_id", id, "reason", reason)
		case <-time.After(grace):
			r.logger.Warn("instance did not exit within grace; sending SIGTERM to its process group",
				"lease_id", id, "grace", grace, "reason", reason)
			if err := process.terminate(); err != nil {
				r.logger.Warn("terminating the instance failed", "lease_id", id, "error", err)
			}
			select {
			case <-exited:
				r.logger.Info("instance exited after SIGTERM", "lease_id", id, "reason", reason)
			case <-time.After(r.termGrace):
				r.logger.Warn("instance did not exit after SIGTERM; force-killing its process group",
					"lease_id", id, "term_grace", r.termGrace, "reason", reason)
				if err := process.kill(); err != nil {
					r.logger.Warn("force-kill failed", "lease_id", id, "error", err)
				}
				<-exited // reap the killed process so nothing leaks
			}
		}
	}

	// 3. Drop the lease and free its slot.
	r.Remove(id)
	r.logger.Info("lease released", "lease_id", id, "reason", reason)
	return nil
}

// defaultReleaseGrace bounds how long releaseLease waits for an instance to
// exit gracefully — on its own, after the shutdown frame — before it escalates
// to signals. It is used when the broker config does not specify release_grace.
const defaultReleaseGrace = 10 * time.Second

// termGrace bounds the SECOND teardown window: how long a process is given after
// SIGTERM before SIGKILL, on both the spawned path (releaseLease) and the adopted
// one (adoptedProcess.kill).
//
// It is ONE constant for both deliberately. The two paths were written apart —
// the adopted one had a SIGTERM escalation from the start, the spawned one went
// straight to SIGKILL — and the cost of that divergence was that a spawned
// instance whose socket had gone got no graceful signal at all. Sharing the value
// is the cheap half of keeping them from drifting again.
//
// It is not configurable, and is short on purpose. It is not the operator's
// shutdown budget — `release_grace` is, and it has already elapsed by the time
// this window opens. This is only the interval between "we have now actually
// asked the OS" and "we stop asking", and an engine that has not exited two
// seconds after SIGTERM is not going to.
const termGrace = 2 * time.Second

// ReleaseServer handles POST /release/{lease_id}: it tears the lease down
// through the shared releaseLease path and reports the outcome.
type ReleaseServer struct {
	logger   *slog.Logger
	registry *Registry

	// live is the configuration snapshot the teardown bound is read from, at the
	// moment a release is handled rather than at construction, so a SIGHUP that
	// changes release_grace changes what the next release waits.
	live *configHolder
}

// NewReleaseServer constructs a release handler over a private configuration
// snapshot carrying just this bound. A non-positive grace falls back to
// defaultReleaseGrace (see settleConfig).
//
// run() calls useConfigHolder afterwards to point it at the process-wide
// snapshot instead.
func NewReleaseServer(logger *slog.Logger, registry *Registry, grace time.Duration) *ReleaseServer {
	if logger == nil {
		logger = slog.Default()
	}
	return &ReleaseServer{logger: logger, registry: registry, live: newLocalConfigHolder(Config{ReleaseGrace: grace})}
}

// useConfigHolder points the handler at the process-wide configuration snapshot.
// A nil holder is ignored, leaving the private one the constructor built.
func (s *ReleaseServer) useConfigHolder(h *configHolder) {
	if h == nil {
		return
	}
	s.live = h
}

// grace is the bounded wait a release gives an instance to exit on its own
// before signals escalate, from `release_grace`.
func (s *ReleaseServer) grace() time.Duration { return s.live.config().ReleaseGrace }

// Register wires the release endpoint onto a mux. It takes a routeMux so main
// can register it behind the auth guard.
func (s *ReleaseServer) Register(mux routeMux) {
	mux.HandleFunc("POST /release/{lease_id}", s.handleRelease)
}

// handleRelease tears down the lease named in the path, provided the
// authenticated caller owns it. Unknown AND unowned leases return the same 404;
// the teardown itself is bounded by the configured grace period.
func (s *ReleaseServer) handleRelease(w http.ResponseWriter, r *http.Request) {
	leaseID := r.PathValue("lease_id")
	if leaseID == "" {
		s.fail(w, http.StatusBadRequest, "release requires a lease id")
		return
	}

	// Ownership is enforced HERE, in the HTTP handler, and deliberately NOT inside
	// releaseLease. That function is the single shared teardown path for the idle
	// sweeper and the crash watcher as well, and those two are the broker acting on
	// itself, not callers: they have no principal to check. A check pushed down into
	// releaseLease could only be satisfied by handing them a pretend caller, and
	// getting that wrong would silently leak instances — a worse failure than the
	// one being fixed. Keeping the check in the handler makes the internal paths
	// structurally incapable of enforcing it.
	//
	// The check runs BEFORE releaseLease, so a refused caller never causes a
	// shutdown frame, a grace wait, a kill, or a freed slot. A 404 that had already
	// asked the instance to stop would pass a status-code-only test while still
	// killing the owner's session.
	caller := callerPrincipal(r)
	if !ownsLease(s.registry, leaseID, caller) {
		logLeaseDenied(s.logger, r, caller, leaseID)
		writeUnknownLease(w)
		return
	}

	// Still mapped, and still reachable: the lease can be torn down by the idle
	// sweeper or a crash between the ownership check and here. Both answers are the
	// same 404, so the race is invisible to the caller.
	err := s.registry.releaseLease(leaseID, "manual release", s.grace())
	if errors.Is(err, errUnknownLease) {
		writeUnknownLease(w)
		return
	}
	if err != nil {
		s.logger.Warn("release failed", "lease_id", leaseID, "error", err)
		s.fail(w, http.StatusInternalServerError, "release failed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":   "released",
		"lease_id": leaseID,
	})
}

// fail writes a JSON error response.
func (s *ReleaseServer) fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
