package main

import (
	"testing"
	"time"
)

// seedLiveLease mints a lease, attaches nil-conn instance + client connections
// (mirroring an active, claimed lease), records the process, and returns the
// lease id plus the lease pointer and its client conn. The pointer lets a test
// inspect the lease's reason field after it has been removed from the map (the
// object outlives its map entry).
func seedLiveLease(t *testing.T, reg *Registry, proc processHandle) (string, *lease, *wsConn) {
	t.Helper()
	id, err := reg.NewLease()
	if err != nil {
		t.Fatalf("NewLease: %v", err)
	}
	inst := newWSConn(nil)
	client := newWSConn(nil)
	if err := reg.AttachInstance(id, inst); err != nil {
		t.Fatalf("AttachInstance: %v", err)
	}
	if err := reg.AttachClient(id, client); err != nil {
		t.Fatalf("AttachClient: %v", err)
	}
	reg.SetProcess(id, proc)
	reg.mu.Lock()
	l := reg.leases[id]
	reg.mu.Unlock()
	return id, l, client
}

// TestWatchExit_UnexpectedExitIsCrash proves the crash path: when an instance
// exits with no deliberate teardown latched, watchExit frees the slot, records
// a crash reason, and closes the client with the distinguishable crash status.
func TestWatchExit_UnexpectedExitIsCrash(t *testing.T) {
	reg := NewRegistry(testLogger(), 0)
	proc := newFakeProcess(300)
	id, l, client := seedLiveLease(t, reg, proc)

	// The instance dies unexpectedly (nobody set releasing).
	close(proc.exited)
	// watchExit returns once the crash has been handled, so we can assert
	// synchronously without sleeps.
	reg.watchExit(id)

	// Slot freed / lease removed; no orphaned registry entry.
	if reg.Has(id) {
		t.Error("crash did not free the slot: lease still present")
	}
	// Lease reason records the crash (non-timing signal for /leases later).
	reg.mu.Lock()
	gotReason := l.reason
	reg.mu.Unlock()
	if gotReason != reasonCrash {
		t.Errorf("lease reason = %q, want %q", gotReason, reasonCrash)
	}
	// Client WS closed with the distinguishable crash status + reason.
	select {
	case <-client.closed:
	default:
		t.Fatal("client connection was not closed after a crash")
	}
	if client.closeStatus != crashCloseStatus {
		t.Errorf("client close status = %v, want %v", client.closeStatus, crashCloseStatus)
	}
	if client.closeReason != crashCloseReason {
		t.Errorf("client close reason = %q, want %q", client.closeReason, crashCloseReason)
	}
}

// TestWatchExit_GracefulReleaseNotCrash is the regression guard: when a
// deliberate teardown has already latched the lease (the releasing flag),
// watchExit must NOT classify the exit as a crash, must not remove the lease,
// and must not close the client. The owning release path keeps that
// responsibility.
func TestWatchExit_GracefulReleaseNotCrash(t *testing.T) {
	reg := NewRegistry(testLogger(), 0)
	proc := newFakeProcess(301)
	id, l, client := seedLiveLease(t, reg, proc)

	// A deliberate release latches the teardown FIRST (as releaseLease does at
	// its very start), then the instance exits in response.
	reg.mu.Lock()
	l.releasing = true
	l.reason = "manual release"
	reg.mu.Unlock()

	close(proc.exited)
	reg.watchExit(id) // synchronous: observes the latch and bails

	// watchExit must not have torn anything down — that is the release path's job.
	if !reg.Has(id) {
		t.Error("watchExit removed a lease that was under deliberate release")
	}
	select {
	case <-client.closed:
		t.Error("watchExit closed the client during a deliberate release")
	default:
	}
	reg.mu.Lock()
	gotReason := l.reason
	reg.mu.Unlock()
	if gotReason == reasonCrash {
		t.Error("a graceful release was misclassified as a crash")
	}
}

// TestReleaseLease_ClosesClientWithGracefulStatus proves the full release path
// closes the client with the going-away (non-crash) status, so a crash and a
// graceful release remain distinguishable on the wire.
func TestReleaseLease_ClosesClientWithGracefulStatus(t *testing.T) {
	reg := NewRegistry(testLogger(), 0)
	proc := newFakeProcess(302)
	id, _, client := seedLiveLease(t, reg, proc)
	close(proc.exited)

	if err := reg.releaseLease(id, "manual release", time.Second); err != nil {
		t.Fatalf("releaseLease: %v", err)
	}

	select {
	case <-client.closed:
	default:
		t.Fatal("client connection was not closed after release")
	}
	if client.closeStatus == crashCloseStatus {
		t.Errorf("graceful release closed client with crash status %v", client.closeStatus)
	}
}

// TestClientCloseForReason maps reasons to close statuses.
func TestClientCloseForReason(t *testing.T) {
	if s, _ := clientCloseForReason(reasonCrash); s != crashCloseStatus {
		t.Errorf("crash status = %v, want %v", s, crashCloseStatus)
	}
	if s, _ := clientCloseForReason("manual release"); s == crashCloseStatus {
		t.Errorf("graceful reason mapped to crash status %v", s)
	}
}
