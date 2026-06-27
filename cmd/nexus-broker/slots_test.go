package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

// waitForQueueLen blocks until the registry's FIFO capacity queue reaches want,
// so tests can deterministically sequence waiters that enqueue from goroutines.
func waitForQueueLen(t *testing.T, reg *Registry, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if reg.QueueLen() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue len did not reach %d (got %d)", want, reg.QueueLen())
}

// TestSlots_QueueDisabledRejectsImmediately proves queue_wait_timeout <= 0
// preserves the pre-queue semantic: an at-capacity claim fails immediately with
// errNoCapacity and is never parked in the queue.
func TestSlots_QueueDisabledRejectsImmediately(t *testing.T) {
	reg := NewRegistry(testLogger(), 1)
	if _, err := reg.NewLease(); err != nil {
		t.Fatalf("occupy slot: %v", err)
	}
	start := time.Now()
	if _, err := reg.NewLeaseQueued(context.Background(), 0); !errors.Is(err, errNoCapacity) {
		t.Fatalf("err = %v, want errNoCapacity", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("non-waiting claim took %v, want immediate", elapsed)
	}
	if got := reg.QueueLen(); got != 0 {
		t.Fatalf("queue len = %d, want 0 (no waiter parked)", got)
	}
}

// TestSlots_QueuedClaimProceedsWhenSlotFrees proves an over-cap claim parks in
// the queue and proceeds automatically — minting its lease — once the holder's
// slot frees, with no drift and no leaked waiter.
func TestSlots_QueuedClaimProceedsWhenSlotFrees(t *testing.T) {
	reg := NewRegistry(testLogger(), 1)
	holder, err := reg.NewLease()
	if err != nil {
		t.Fatalf("occupy slot: %v", err)
	}

	idCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		id, err := reg.NewLeaseQueued(context.Background(), 5*time.Second)
		idCh <- id
		errCh <- err
	}()
	waitForQueueLen(t, reg, 1)

	// Free the holder; the queued claim must be granted the slot and proceed.
	reg.Remove(holder)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("queued claim err: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued claim never proceeded after slot freed")
	}
	id := <-idCh
	if !reg.Has(id) {
		t.Fatal("queued claim's lease is not present")
	}
	if got := reg.SlotsInUse(); got != 1 {
		t.Fatalf("slots in use = %d, want 1 (handed off, no drift)", got)
	}
	if got := reg.QueueLen(); got != 0 {
		t.Fatalf("queue len = %d, want 0", got)
	}
}

// TestSlots_QueueWaitTimeout proves a waiter that never gets a slot returns
// errQueueTimeout after at least queue_wait_timeout, leaks no waiter, and does
// not perturb the holder's slot.
func TestSlots_QueueWaitTimeout(t *testing.T) {
	reg := NewRegistry(testLogger(), 1)
	if _, err := reg.NewLease(); err != nil {
		t.Fatalf("occupy slot: %v", err)
	}
	start := time.Now()
	err := reg.acquireSlot(context.Background(), 80*time.Millisecond)
	if !errors.Is(err, errQueueTimeout) {
		t.Fatalf("err = %v, want errQueueTimeout", err)
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("returned in %v, want >= queue_wait_timeout (80ms)", elapsed)
	}
	waitForQueueLen(t, reg, 0)
	if got := reg.SlotsInUse(); got != 1 {
		t.Fatalf("slots in use = %d, want 1 (no drift on timeout)", got)
	}
}

// TestSlots_QueuedCancelDropsWaiter proves a waiter whose context is cancelled
// while queued is removed from the queue, returns the wrapped ctx error, and is
// never granted the holder's slot (no drift, no leak).
func TestSlots_QueuedCancelDropsWaiter(t *testing.T) {
	reg := NewRegistry(testLogger(), 1)
	holder, err := reg.NewLease()
	if err != nil {
		t.Fatalf("occupy slot: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- reg.acquireSlot(ctx, 5*time.Second) }()
	waitForQueueLen(t, reg, 1)

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled waiter never returned")
	}
	waitForQueueLen(t, reg, 0)
	if got := reg.SlotsInUse(); got != 1 {
		t.Fatalf("slots in use = %d, want 1 (abandoned waiter granted nothing)", got)
	}
	reg.Remove(holder)
	if got := reg.SlotsInUse(); got != 0 {
		t.Fatalf("slots in use after remove = %d, want 0", got)
	}
}

// TestSlots_QueueFIFOOrder proves slots are granted in arrival order: with
// cap=1 and N waiters enqueued in a known order, each freed slot is handed to
// the oldest waiter still queued.
func TestSlots_QueueFIFOOrder(t *testing.T) {
	reg := NewRegistry(testLogger(), 1)
	holder, err := reg.NewLease()
	if err != nil {
		t.Fatalf("occupy slot: %v", err)
	}

	const n = 6
	order := make(chan int, n)
	results := make([]chan error, n)
	for i := 0; i < n; i++ {
		results[i] = make(chan error, 1)
		idx := i
		go func() {
			err := reg.acquireSlot(context.Background(), 5*time.Second)
			order <- idx
			results[idx] <- err
		}()
		// Wait until this waiter is parked before launching the next, so the
		// enqueue order is deterministic (FIFO by arrival).
		waitForQueueLen(t, reg, i+1)
	}

	expectGrant := func(want int) {
		t.Helper()
		select {
		case got := <-order:
			if got != want {
				t.Fatalf("FIFO violation: slot granted to waiter %d, want %d", got, want)
			}
			if err := <-results[want]; err != nil {
				t.Fatalf("waiter %d err: %v", want, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d was never granted a slot", want)
		}
	}

	// Free the holder's slot → waiter 0. Then release each granted slot in turn,
	// which must flow to the next-oldest waiter.
	reg.Remove(holder)
	expectGrant(0)
	for i := 1; i < n; i++ {
		reg.releaseSlot()
		expectGrant(i)
	}
	reg.releaseSlot() // last grant frees the slot for good
	if got := reg.SlotsInUse(); got != 0 {
		t.Fatalf("slots in use = %d, want 0", got)
	}
	if got := reg.QueueLen(); got != 0 {
		t.Fatalf("queue len = %d, want 0", got)
	}
}

// TestSlots_QueueStormNoDrift hammers cap=1 with a storm of queued claims that
// each either time out, get cancelled, or are granted-then-released. Whatever
// the interleaving of grant/timeout/cancel races, no slot may drift and no
// waiter may leak: the count must settle to exactly 0 with an empty queue.
func TestSlots_QueueStormNoDrift(t *testing.T) {
	reg := NewRegistry(testLogger(), 1)
	const n = 300
	var granted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			timeout := time.Duration(1+i%7) * time.Millisecond
			ctx := context.Background()
			if i%3 == 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Duration(1+i%4)*time.Millisecond)
				defer cancel()
			}
			id, err := reg.NewLeaseQueued(ctx, timeout)
			if err == nil {
				granted.Add(1)
				// Hold the slot momentarily, then free it for the next waiter.
				reg.Remove(id)
			}
		}(i)
	}
	wg.Wait()

	if got := reg.QueueLen(); got != 0 {
		t.Fatalf("queue len = %d, want 0 (no leaked waiter); granted=%d", got, granted.Load())
	}
	if got := reg.SlotsInUse(); got != 0 {
		t.Fatalf("slots in use = %d, want 0 (no drift after storm); granted=%d", got, granted.Load())
	}
}

// TestClaim_QueuedClaimProceedsAfterRelease proves the claim handler queues an
// over-cap claim and lets it proceed (spawn + ready + 200) once the holder's
// lease is released — exercising the FIFO wait end to end through HTTP.
func TestClaim_QueuedClaimProceedsAfterRelease(t *testing.T) {
	runner := newQueueRunner()
	cfg := Config{ListenAddr: "127.0.0.1:8080", NexusBinaryPath: "/bin/nexus", QueueWaitTimeout: 5 * time.Second}
	reg := NewRegistry(testLogger(), 1)
	cs := NewClaimServer(testLogger(), reg, cfg, runner)
	cs.sessionReportGrace = 20 * time.Millisecond
	mux := http.NewServeMux()
	cs.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// Claim A takes the only slot and goes live.
	respA := make(chan *http.Response, 1)
	go func() { respA <- postClaim(t, ts.URL, `{"config":"engine: {}\n"}`) }()
	specA := <-runner.started
	reg.MarkReady(specA.leaseID)
	rA := <-respA
	rA.Body.Close()
	if rA.StatusCode != http.StatusOK {
		t.Fatalf("claim A status = %d, want 200", rA.StatusCode)
	}

	// Claim B arrives at capacity: it must park in the queue and NOT spawn.
	respB := make(chan *http.Response, 1)
	go func() { respB <- postClaim(t, ts.URL, `{"config":"engine: {}\n"}`) }()
	waitForQueueLen(t, reg, 1)
	select {
	case <-runner.started:
		t.Fatal("queued claim spawned before a slot freed")
	case <-time.After(100 * time.Millisecond):
	}

	// Release A's slot → B is granted it, spawns, and completes.
	reg.Remove(specA.leaseID)
	var specB spawnSpec
	select {
	case specB = <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("queued claim never spawned after slot freed")
	}
	reg.MarkReady(specB.leaseID)
	rB := <-respB
	rB.Body.Close()
	if rB.StatusCode != http.StatusOK {
		t.Fatalf("claim B status = %d, want 200", rB.StatusCode)
	}
	if specB.leaseID == specA.leaseID {
		t.Fatal("queued claim reused the released lease id")
	}
	if got := reg.SlotsInUse(); got != 1 {
		t.Fatalf("slots in use = %d, want 1 (B holds the only slot)", got)
	}
}

// TestClaim_QueueWaitTimeoutReturns503 proves an over-cap claim that waits past
// queue_wait_timeout returns a distinct 503 "capacity wait timed out", spawns
// nothing, and leaves the slot count and queue clean.
func TestClaim_QueueWaitTimeoutReturns503(t *testing.T) {
	runner := newQueueRunner()
	cfg := Config{ListenAddr: "127.0.0.1:8080", NexusBinaryPath: "/bin/nexus", QueueWaitTimeout: 80 * time.Millisecond}
	reg := NewRegistry(testLogger(), 1)
	cs := NewClaimServer(testLogger(), reg, cfg, runner)
	mux := http.NewServeMux()
	cs.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// Occupy the only slot with a live lease.
	if _, err := reg.NewLease(); err != nil {
		t.Fatalf("occupy slot: %v", err)
	}

	start := time.Now()
	resp := postClaim(t, ts.URL, `{"config":"engine: {}\n"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body["error"] != "capacity wait timed out" {
		t.Fatalf("error = %q, want %q (distinct timeout message)", body["error"], "capacity wait timed out")
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("returned in %v, want >= queue_wait_timeout (80ms)", elapsed)
	}
	select {
	case <-runner.started:
		t.Fatal("timed-out claim spawned an instance")
	default:
	}
	if got := reg.SlotsInUse(); got != 1 {
		t.Fatalf("slots in use = %d, want 1 (no drift)", got)
	}
	waitForQueueLen(t, reg, 0)
}

// TestClaim_ClientCancelWhileQueued proves a client that hangs up while its
// claim is queued is dropped from the queue, holds no slot, and triggers no
// spawn.
func TestClaim_ClientCancelWhileQueued(t *testing.T) {
	runner := newQueueRunner()
	cfg := Config{ListenAddr: "127.0.0.1:8080", NexusBinaryPath: "/bin/nexus", QueueWaitTimeout: 10 * time.Second}
	reg := NewRegistry(testLogger(), 1)
	cs := NewClaimServer(testLogger(), reg, cfg, runner)
	mux := http.NewServeMux()
	cs.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// Occupy the only slot with a live lease.
	if _, err := reg.NewLease(); err != nil {
		t.Fatalf("occupy slot: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/claim",
		strings.NewReader(`{"config":"engine: {}\n"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	doneCh := make(chan struct{})
	go func() {
		resp, _ := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		close(doneCh)
	}()

	// The claim parks in the queue; then the client hangs up.
	waitForQueueLen(t, reg, 1)
	cancel()

	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled request never returned")
	}

	// The waiter is dropped and the holder's slot is untouched; nothing spawned.
	waitForQueueLen(t, reg, 0)
	if got := reg.SlotsInUse(); got != 1 {
		t.Fatalf("slots in use = %d, want 1 (cancelled waiter granted nothing)", got)
	}
	select {
	case <-runner.started:
		t.Fatal("cancelled claim spawned an instance")
	default:
	}
}

// queueRunner is a commandRunner that hands every spawn its own controllable
// fakeProcess (so concurrent claims do not share one handle) and records each
// spawn spec on started.
type queueRunner struct {
	started chan spawnSpec
	pid     atomic.Int64
}

func newQueueRunner() *queueRunner {
	return &queueRunner{started: make(chan spawnSpec, 8)}
}

func (q *queueRunner) start(_ context.Context, spec spawnSpec) (processHandle, error) {
	p := newFakeProcess(int(q.pid.Add(1)))
	q.started <- spec
	return p, nil
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
