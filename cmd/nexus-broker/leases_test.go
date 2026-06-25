package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newLeasesTestServer wires the GET /leases endpoint over an httptest server,
// returning the server and the shared registry so a test can seed leases.
func newLeasesTestServer(t *testing.T, maxConcurrent int) (*httptest.Server, *Registry) {
	t.Helper()
	reg := NewRegistry(testLogger(), maxConcurrent)
	ls := NewLeasesServer(testLogger(), reg)
	mux := http.NewServeMux()
	ls.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, reg
}

// getLeases performs GET /leases and decodes the snapshot, failing on any
// non-200 status.
func getLeases(t *testing.T, base string) RegistrySnapshot {
	t.Helper()
	resp, err := http.Get(base + "/leases")
	if err != nil {
		t.Fatalf("GET /leases: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var snap RegistrySnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	return snap
}

// TestSurfaceState_MapsInternalLifecycle proves the operator-facing state
// projection: a not-yet-registered lease reads spawning, a registered/active
// lease reads active, and a latched teardown reads draining regardless of the
// internal state it was in.
func TestSurfaceState_MapsInternalLifecycle(t *testing.T) {
	cases := []struct {
		name      string
		state     leaseState
		releasing bool
		want      string
	}{
		{"pending", leaseStatePending, false, surfaceStateSpawning},
		{"registered", leaseStateRegistered, false, surfaceStateActive},
		{"active", leaseStateActive, false, surfaceStateActive},
		{"draining overrides pending", leaseStatePending, true, surfaceStateDraining},
		{"draining overrides active", leaseStateActive, true, surfaceStateDraining},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &lease{state: tc.state, releasing: tc.releasing}
			if got := l.surfaceState(); got != tc.want {
				t.Fatalf("surfaceState() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLeasesHTTP_ListsClaimedLeaseThenGoneAfterRelease proves the core surface:
// a seeded (claimed) lease appears with its id, pid, session id, and an active
// state plus the capacity aggregates; after release it disappears and the
// aggregates reflect the freed slot.
func TestLeasesHTTP_ListsClaimedLeaseThenGoneAfterRelease(t *testing.T) {
	ts, reg := newLeasesTestServer(t, 4)

	proc := newFakeProcess(4242)
	id, _ := seedLease(t, reg, proc) // NewLease + AttachInstance + SetProcess
	reg.MarkSessionID(id, "sess-abc")
	close(proc.exited) // let release's graceful path complete without a kill

	snap := getLeases(t, ts.URL)
	if snap.MaxConcurrent != 4 {
		t.Errorf("max_concurrent = %d, want 4", snap.MaxConcurrent)
	}
	if snap.SlotsInUse != 1 {
		t.Errorf("slots_in_use = %d, want 1", snap.SlotsInUse)
	}
	if snap.QueueDepth != 0 {
		t.Errorf("queue_depth = %d, want 0", snap.QueueDepth)
	}
	if len(snap.Leases) != 1 {
		t.Fatalf("leases len = %d, want 1", len(snap.Leases))
	}
	got := snap.Leases[0]
	if got.ID != id {
		t.Errorf("lease_id = %q, want %q", got.ID, id)
	}
	if got.SessionID != "sess-abc" {
		t.Errorf("session_id = %q, want %q", got.SessionID, "sess-abc")
	}
	if got.PID != 4242 {
		t.Errorf("pid = %d, want 4242", got.PID)
	}
	// An instance has registered but no client is attached: surface state is
	// active.
	if got.State != surfaceStateActive {
		t.Errorf("state = %q, want %q", got.State, surfaceStateActive)
	}
	if got.LastActivity.IsZero() || got.CreatedAt.IsZero() {
		t.Errorf("timestamps not populated: %+v", got)
	}

	// Release the lease; it must vanish from the surface and free its slot.
	if err := reg.releaseLease(id, "manual release", time.Second); err != nil {
		t.Fatalf("releaseLease: %v", err)
	}
	after := getLeases(t, ts.URL)
	if len(after.Leases) != 0 {
		t.Fatalf("leases len after release = %d, want 0", len(after.Leases))
	}
	if after.SlotsInUse != 0 {
		t.Errorf("slots_in_use after release = %d, want 0", after.SlotsInUse)
	}
}

// TestLeasesHTTP_QueueDepthReflectsWaiters proves the aggregate queue_depth
// surfaces parked waiters: with cap=1 the only slot is held and a second,
// queued claim parks; GET /leases then reports queue_depth=1 while still
// listing exactly the one live lease.
func TestLeasesHTTP_QueueDepthReflectsWaiters(t *testing.T) {
	ts, reg := newLeasesTestServer(t, 1)

	holder, err := reg.NewLease()
	if err != nil {
		t.Fatalf("occupy slot: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		_, qerr := reg.NewLeaseQueued(context.Background(), 5*time.Second)
		errCh <- qerr
	}()
	waitForQueueLen(t, reg, 1)

	snap := getLeases(t, ts.URL)
	if snap.QueueDepth != 1 {
		t.Errorf("queue_depth = %d, want 1", snap.QueueDepth)
	}
	if snap.SlotsInUse != 1 {
		t.Errorf("slots_in_use = %d, want 1", snap.SlotsInUse)
	}
	if len(snap.Leases) != 1 {
		t.Fatalf("leases len = %d, want 1 (only the holder is live)", len(snap.Leases))
	}
	if snap.Leases[0].ID != holder {
		t.Errorf("listed lease = %q, want holder %q", snap.Leases[0].ID, holder)
	}

	// Free the slot so the queued claim proceeds and the goroutine exits cleanly.
	reg.Remove(holder)
	if err := <-errCh; err != nil {
		t.Fatalf("queued claim err: %v", err)
	}
}

// TestLeasesHTTP_SortedByCreation proves the surface is deterministically
// ordered by creation time (then id), so operators and tests see a stable list.
func TestLeasesHTTP_SortedByCreation(t *testing.T) {
	ts, reg := newLeasesTestServer(t, 0)

	var ids []string
	for i := 0; i < 5; i++ {
		id, err := reg.NewLease()
		if err != nil {
			t.Fatalf("lease %d: %v", i, err)
		}
		ids = append(ids, id)
		time.Sleep(time.Millisecond) // distinct createdAt stamps
	}

	snap := getLeases(t, ts.URL)
	if len(snap.Leases) != len(ids) {
		t.Fatalf("leases len = %d, want %d", len(snap.Leases), len(ids))
	}
	for i, want := range ids {
		if snap.Leases[i].ID != want {
			t.Fatalf("lease[%d] = %q, want %q (creation order)", i, snap.Leases[i].ID, want)
		}
	}
}
