package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSlots_CapRejectsThenReleaseFrees exercises the slot primitive directly:
// up to max_concurrent leases acquire a slot, the next is rejected with
// errNoCapacity (no slot consumed), and removing a lease frees its slot so a
// fresh lease can be minted again. This is the single source of truth every
// teardown path (release/idle/crash) and the claim error paths rely on.
func TestSlots_CapRejectsThenReleaseFrees(t *testing.T) {
	reg := NewRegistry(testLogger(), 2)

	id1, err := reg.NewLease()
	if err != nil {
		t.Fatalf("lease 1: %v", err)
	}
	if _, err := reg.NewLease(); err != nil {
		t.Fatalf("lease 2: %v", err)
	}
	if got := reg.SlotsInUse(); got != 2 {
		t.Fatalf("slots in use = %d, want 2", got)
	}

	// Third lease is over capacity.
	if _, err := reg.NewLease(); !errors.Is(err, errNoCapacity) {
		t.Fatalf("lease 3 err = %v, want errNoCapacity", err)
	}
	// The rejected acquire consumed no slot.
	if got := reg.SlotsInUse(); got != 2 {
		t.Fatalf("slots in use after rejection = %d, want 2 (no drift)", got)
	}

	// Free a slot; a new lease now fits.
	reg.Remove(id1)
	if got := reg.SlotsInUse(); got != 1 {
		t.Fatalf("slots in use after remove = %d, want 1", got)
	}
	if _, err := reg.NewLease(); err != nil {
		t.Fatalf("lease after release: %v", err)
	}
	if got := reg.SlotsInUse(); got != 2 {
		t.Fatalf("slots in use after refill = %d, want 2", got)
	}
}

// TestSlots_NonPositiveCapIsUnlimited proves the documented semantic: a
// non-positive max_concurrent means no cap, so every claim acquires a slot.
func TestSlots_NonPositiveCapIsUnlimited(t *testing.T) {
	for _, limit := range []int{0, -1} {
		reg := NewRegistry(testLogger(), limit)
		const n = 64
		for i := 0; i < n; i++ {
			if _, err := reg.NewLease(); err != nil {
				t.Fatalf("cap=%d lease %d: %v", limit, i, err)
			}
		}
		if got := reg.SlotsInUse(); got != n {
			t.Fatalf("cap=%d slots in use = %d, want %d (unlimited)", limit, got, n)
		}
	}
}

// TestSlots_ConcurrentAcquireRespectsCap fires N>cap concurrent acquires and
// asserts exactly cap succeed and the rest get errNoCapacity, with the slot
// count landing exactly at cap (no drift under contention).
func TestSlots_ConcurrentAcquireRespectsCap(t *testing.T) {
	const limit = 5
	const n = 50
	reg := NewRegistry(testLogger(), limit)

	var ok, rejected atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // line everyone up to maximise contention
			if _, err := reg.NewLease(); err != nil {
				if errors.Is(err, errNoCapacity) {
					rejected.Add(1)
				}
				return
			}
			ok.Add(1)
		}()
	}
	close(start)
	wg.Wait()

	if ok.Load() != limit {
		t.Errorf("successful acquires = %d, want %d", ok.Load(), limit)
	}
	if rejected.Load() != n-limit {
		t.Errorf("rejected acquires = %d, want %d", rejected.Load(), n-limit)
	}
	if got := reg.SlotsInUse(); got != limit {
		t.Errorf("slots in use = %d, want %d", got, limit)
	}
}

// TestSlots_ReleasePathFreesSlot proves the shared releaseLease teardown frees
// the slot (release/idle both funnel through it). A nil process short-circuits
// the grace wait, so this is deterministic.
func TestSlots_ReleasePathFreesSlot(t *testing.T) {
	reg := NewRegistry(testLogger(), 1)
	id, err := reg.NewLease()
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := reg.releaseLease(id, "manual release", time.Second); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := reg.SlotsInUse(); got != 0 {
		t.Fatalf("slots in use after release = %d, want 0", got)
	}
	// Double remove must not drive the count negative or double-free.
	reg.Remove(id)
	if got := reg.SlotsInUse(); got != 0 {
		t.Fatalf("slots in use after double remove = %d, want 0", got)
	}
}

// TestClaim_OverCapacityReturns503 proves the claim handler maps a full
// registry to a distinct 503 "no capacity" and spawns nothing.
func TestClaim_OverCapacityReturns503(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1), handle: newFakeProcess(1)}
	cfg := Config{ListenAddr: "127.0.0.1:8080", NexusBinaryPath: "/bin/nexus"}
	reg := NewRegistry(testLogger(), 1)
	cs := NewClaimServer(testLogger(), reg, cfg, runner)
	mux := http.NewServeMux()
	cs.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// Occupy the only slot with a live lease.
	if _, err := reg.NewLease(); err != nil {
		t.Fatalf("pre-occupy slot: %v", err)
	}

	resp := postClaim(t, ts.URL, `{"config":"engine: {}\n"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	// No spawn happened and the count did not drift.
	select {
	case <-runner.started:
		t.Fatal("over-cap claim spawned an instance")
	default:
	}
	if got := reg.SlotsInUse(); got != 1 {
		t.Fatalf("slots in use = %d, want 1 (no drift on rejection)", got)
	}
}

// TestClaim_FailedSpawnReturnsSlot proves a slot acquired by a claim is returned
// when the spawn itself fails (the error path runs through Remove), so a failed
// claim never leaks capacity.
func TestClaim_FailedSpawnReturnsSlot(t *testing.T) {
	runner := &fakeRunner{started: make(chan spawnSpec, 1), err: errors.New("exec failed")}
	cfg := Config{ListenAddr: "127.0.0.1:8080", NexusBinaryPath: "/bin/nexus"}
	reg := NewRegistry(testLogger(), 2)
	cs := NewClaimServer(testLogger(), reg, cfg, runner)
	mux := http.NewServeMux()
	cs.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp := postClaim(t, ts.URL, `{"config":"engine: {}\n"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if got := reg.SlotsInUse(); got != 0 {
		t.Fatalf("slots in use after failed spawn = %d, want 0 (slot returned)", got)
	}
}
