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
//  2. Waits a BOUNDED grace period for the process to exit on its own; if the
//     grace elapses it force-kills the process so nothing is orphaned. Either
//     way the process is reaped by the registry's single reaper goroutine.
//  3. Removes the lease from the registry, freeing its slot and closing both
//     connections.
//
// It returns errUnknownLease for an unknown lease and is safe to call
// concurrently: only the first caller performs the teardown; a second
// concurrent call returns nil immediately (the teardown is already underway).
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

	// 2. Wait out a bounded grace period for the process to exit, force-killing
	//    it if it overruns. The reaper closes exited after wait() returns, so we
	//    always reap — graceful exit or forced kill alike.
	if process != nil {
		select {
		case <-exited:
			r.logger.Info("instance exited gracefully", "lease_id", id, "reason", reason)
		case <-time.After(grace):
			r.logger.Warn("instance did not exit within grace; force-killing",
				"lease_id", id, "grace", grace, "reason", reason)
			if err := process.kill(); err != nil {
				r.logger.Warn("force-kill failed", "lease_id", id, "error", err)
			}
			<-exited // reap the killed process so nothing leaks
		}
	}

	// 3. Drop the lease and free its slot.
	r.Remove(id)
	r.logger.Info("lease released", "lease_id", id, "reason", reason)
	return nil
}

// defaultReleaseGrace bounds how long releaseLease waits for an instance to
// exit gracefully before force-killing it. It is used when the broker config
// does not specify release_grace.
const defaultReleaseGrace = 10 * time.Second

// ReleaseServer handles POST /release/{lease_id}: it tears the lease down
// through the shared releaseLease path and reports the outcome.
type ReleaseServer struct {
	logger   *slog.Logger
	registry *Registry
	grace    time.Duration
}

// NewReleaseServer constructs a release handler. A non-positive grace falls
// back to defaultReleaseGrace.
func NewReleaseServer(logger *slog.Logger, registry *Registry, grace time.Duration) *ReleaseServer {
	if logger == nil {
		logger = slog.Default()
	}
	if grace <= 0 {
		grace = defaultReleaseGrace
	}
	return &ReleaseServer{logger: logger, registry: registry, grace: grace}
}

// Register wires the release endpoint onto a mux.
func (s *ReleaseServer) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /release/{lease_id}", s.handleRelease)
}

// handleRelease tears down the lease named in the path. Unknown leases return
// 404; the teardown itself is bounded by the configured grace period.
func (s *ReleaseServer) handleRelease(w http.ResponseWriter, r *http.Request) {
	leaseID := r.PathValue("lease_id")
	if leaseID == "" {
		s.fail(w, http.StatusBadRequest, "release requires a lease id")
		return
	}

	err := s.registry.releaseLease(leaseID, "manual release", s.grace)
	if errors.Is(err, errUnknownLease) {
		s.fail(w, http.StatusNotFound, "unknown lease")
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
