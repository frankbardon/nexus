package main

import "github.com/coder/websocket"

const (
	// reasonCrash is the teardown reason recorded on a lease whose instance
	// exited UNEXPECTEDLY — i.e. not through a deliberate release. It is the
	// non-timing signal that distinguishes a crash from a graceful release and
	// selects the client's close status via clientCloseForReason.
	reasonCrash = "crash"

	// crashCloseStatus is the WebSocket close code the gateway uses when an
	// instance crashes, so a connected client can tell a crash apart from a
	// normal release (which uses websocket.StatusGoingAway). It sits in the
	// application-defined range (4000-4999).
	crashCloseStatus = websocket.StatusCode(4500)

	// crashCloseReason is the human-readable close reason paired with
	// crashCloseStatus.
	crashCloseReason = "instance crashed"
)

// clientCloseForReason maps a lease's teardown reason to the WebSocket close
// status and message used for its client connection. A crash gets a
// distinguishable code so the client learns the session died unexpectedly;
// every deliberate teardown (manual release, idle, …) gets a normal going-away
// close.
func clientCloseForReason(reason string) (websocket.StatusCode, string) {
	if reason == reasonCrash {
		return crashCloseStatus, crashCloseReason
	}
	return websocket.StatusGoingAway, "lease closed"
}

// watchExit watches a single lease's process-exit signal and classifies the
// exit. POST /claim starts it once an instance is live (ready), so from then on
// an exit means one of two things:
//
//   - A deliberate teardown already latched the lease's releasing flag (manual
//     release, idle, …). releaseLease owns that teardown; watchExit observes the
//     latch and does nothing, so the two paths never race or double-fire.
//   - Nobody asked the instance to stop: it died unexpectedly. watchExit records
//     a crash reason, then frees the slot via the shared Remove path, which
//     closes the client WS with a distinguishable crash status.
//
// The process is already reaped by the registry's single reaper goroutine (it is
// what closes the exited channel), so watchExit never calls wait()/kill() — it
// cannot double-reap. It returns once the exit has been handled, which keeps it
// deterministic to drive from tests.
func (r *Registry) watchExit(id string) {
	exited := r.ExitedChan(id)
	if exited == nil {
		return
	}
	<-exited

	r.mu.Lock()
	l, ok := r.leases[id]
	if !ok || l.releasing {
		// Either the lease is already gone, or a deliberate teardown is
		// underway/done. Not a crash; leave it to the owning path.
		r.mu.Unlock()
		return
	}
	l.releasing = true
	l.reason = reasonCrash
	exitErr := l.exitErr
	r.mu.Unlock()

	r.logger.Warn("instance exited unexpectedly; treating as crash",
		"lease_id", id, "error", exitErr)

	// Free the slot and close both connections. The process is already reaped,
	// so this only drops the lease and closes the client (with the crash status,
	// via clientCloseForReason) and any lingering instance connection.
	r.Remove(id)
	r.logger.Info("lease removed after crash", "lease_id", id, "reason", reasonCrash)
}
