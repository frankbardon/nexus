package main

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// errNoCapacity is returned by NewLease (and by NewLeaseQueued when
// queue_wait_timeout <= 0) when the registry is already at its max_concurrent
// ceiling and no slot can be acquired without waiting. The claim handler maps
// it to HTTP 503 so an over-capacity claim is clearly distinguishable from
// other failures (which map to 4xx/5xx).
var errNoCapacity = errors.New("no capacity")

// errQueueTimeout is returned by the queued acquire path when a claim waited in
// the FIFO capacity queue for longer than queue_wait_timeout without a slot
// freeing. The claim handler maps it to HTTP 503 with a distinct "capacity wait
// timed out" body so a timed-out waiter is told apart from an immediate
// over-capacity rejection (which carries "no capacity") and from other errors.
var errQueueTimeout = errors.New("capacity wait timed out")

// errQueueFull is returned when a claim arrives at capacity and the FIFO wait
// queue is ALREADY holding max_queue_depth parked waiters. Such a claim is
// refused immediately rather than parked, so it costs no goroutine, no timer and
// no held-open connection.
//
// It is the THIRD capacity refusal and deliberately carries its own message, so
// the three are distinguishable in a claim-failure log without correlating
// timings:
//
//   - "no capacity"           — the cap is full and waiting is switched off.
//   - "capacity wait timed out" — this claim waited and gave up.
//   - "capacity queue full"   — this claim was never allowed to wait, because
//     the queue itself is at its bound.
var errQueueFull = errors.New("capacity queue full")

// errPrincipalLeaseLimit is returned when the calling principal already holds
// max_leases_per_principal live leases. It is a per-caller QUOTA refusal, not a
// statement about the broker's capacity — the broker may be entirely idle — so
// the claim handler answers it with 429 rather than one of the 503s above.
var errPrincipalLeaseLimit = errors.New("lease limit reached for this principal")

// errPrincipalQueueLimit is returned when the calling principal already has
// max_queued_per_principal claims parked in the capacity queue. It is what stops
// one caller looping on /claim from occupying the whole FIFO queue and timing
// every other tenant's single claim out behind it.
var errPrincipalQueueLimit = errors.New("queued claim limit reached for this principal")

// Slot accounting is the registry's single source of truth for capacity. A
// slot is acquired exactly once per live lease (inside NewLease / NewLeaseQueued,
// BEFORE the claim spawns a process) and freed exactly once when that lease is
// dropped (inside Remove, the one teardown sink every path — manual release,
// idle, crash, and the claim error/cleanup paths — funnels through). Binding a
// slot to a lease this way makes the count impossible to drift: it is freed iff
// a lease is removed.
//
// E3-S2 layers a FIFO wait queue on top of this SAME counter. When the cap is
// full, a queued acquire parks on a waiter; a freed slot is handed DIRECTLY to
// the oldest waiter (the count is not decremented — ownership transfers) so no
// fresh claim can barge ahead of the queue. Only one counter exists: the queue
// never introduces a second accounting path.
//
// E4-S4 layers the ADMISSION checks (max_queue_depth and the two per-principal
// caps) on top of that, and they too introduce no counter. Each one is answered
// by reading state that already exists — r.waiters.Len(), and a walk of
// r.leases / r.waiters filtered by owner id — and each is evaluated BEFORE a
// slot is taken, so a refused claim has nothing to give back. The invariant
// above is therefore unchanged: a slot is held iff a lease exists (or a granted
// waiter is in flight to become one).

// waiter is one parked claim in the FIFO capacity queue. It owns a channel the
// release path closes to grant it the freed slot. granted records (under
// Registry.mu) whether the slot has been handed to this waiter, so a waiter
// that wakes on timeout/cancel can tell a real grant from a giving-up and
// release the slot back to the next waiter if it was granted at the last moment.
// elem is this waiter's node in Registry.waiters, kept so it can be removed in
// O(1) when the waiter gives up before being granted.
//
// ownerID is the principal id of the claim that parked here, kept so the
// per-principal queue cap can be answered by walking the queue itself rather
// than by maintaining a second tally beside it. It is the empty string for an
// anonymous claim, which the caps skip entirely (see principalCapsApply).
type waiter struct {
	ch      chan struct{}
	granted bool
	elem    *list.Element
	ownerID string
}

// tryAcquireSlotLocked reserves one capacity slot if the registry is below its
// max_concurrent ceiling, returning true on success and false at capacity. A
// non-positive maxConcurrent means UNLIMITED capacity, so it always succeeds —
// the least-surprising meaning for an unconfigured cap.
//
// Caller MUST hold r.mu. This is the single acquire primitive: the queued
// acquire (acquireSlot) calls it for the fast path and otherwise parks a waiter
// that releaseSlotLocked later grants — both layered on this one counter.
func (r *Registry) tryAcquireSlotLocked() bool {
	if r.maxConcurrent > 0 && r.slotsInUse >= r.maxConcurrent {
		return false
	}
	r.slotsInUse++
	return true
}

// releaseSlotLocked frees one previously acquired capacity slot, OR hands it
// directly to the oldest queued waiter if any are parked. Direct handoff keeps
// the slot count constant across a wait-queue grant (the slot's owner changes
// from the released lease to the granted waiter) so no fresh claim can win the
// slot ahead of a waiter that has been queued longer — FIFO fairness. Only when
// the queue is empty does the count actually decrement.
//
// It is driven by Remove (a lease teardown) and by releaseSlot (a granted
// waiter that gave up), so a slot is freed exactly once per acquire and the
// count never drifts. The decrement guard keeps the counter from underflowing
// if it is ever called without a matching acquire.
//
// Caller MUST hold r.mu.
func (r *Registry) releaseSlotLocked() {
	if front := r.waiters.Front(); front != nil {
		w := front.Value.(*waiter)
		r.waiters.Remove(front)
		w.elem = nil
		w.granted = true
		close(w.ch)
		return
	}
	if r.slotsInUse > 0 {
		r.slotsInUse--
	}
}

// releaseSlot is the lock-taking wrapper around releaseSlotLocked, used by the
// queued acquire path when a granted-but-abandoned waiter must hand its slot
// back to the next waiter (or decrement the count).
func (r *Registry) releaseSlot() {
	r.mu.Lock()
	r.releaseSlotLocked()
	r.mu.Unlock()
}

// acquireSlot reserves one capacity slot, waiting in FIFO order if the registry
// is at capacity. It returns:
//
//   - nil once a slot is held (either the fast path was below the cap, or this
//     waiter was granted a freed slot);
//   - errNoCapacity immediately if the cap is full AND timeout <= 0 (no waiting
//     is configured — preserves the pre-queue 503 behaviour);
//   - errQueueFull immediately if the cap is full and the wait queue already
//     holds max_queue_depth waiters;
//   - errPrincipalLeaseLimit / errPrincipalQueueLimit immediately if owner is
//     over one of its per-principal caps (only when those are in force — see
//     principalCapsApply);
//   - errQueueTimeout if the cap stayed full for longer than timeout;
//   - the wrapped ctx error if the caller's request context is cancelled while
//     queued (client hung up) — the waiter is dropped from the queue and frees
//     nothing it never held.
//
// On every non-nil return the caller holds no slot. On a nil return the caller
// owns exactly one slot and must bind it to a lease (insertLeaseLocked) or hand
// it back via releaseSlot.
func (r *Registry) acquireSlot(ctx context.Context, timeout time.Duration, owner nexusauth.Principal) error {
	r.mu.Lock()
	// Per-principal lease quota FIRST, ahead of the slot and ahead of the queue.
	// A principal already holding its maximum must be told so immediately: parking
	// it would burn a queue slot another tenant could use, only to refuse it again
	// when the wait finally produced a capacity slot.
	if err := r.admitPrincipalLeaseLocked(owner); err != nil {
		r.mu.Unlock()
		return err
	}
	if r.tryAcquireSlotLocked() {
		r.mu.Unlock()
		return nil
	}
	if timeout <= 0 {
		// No waiting configured: at capacity is an immediate rejection.
		r.mu.Unlock()
		return errNoCapacity
	}
	// The queue is bounded before anything is allocated for this waiter, so a
	// refused claim costs no goroutine, no timer and no held-open connection —
	// which is the whole point of bounding it.
	if r.maxQueueDepth > 0 && r.waiters.Len() >= r.maxQueueDepth {
		r.mu.Unlock()
		return errQueueFull
	}
	if err := r.admitPrincipalQueueLocked(owner); err != nil {
		r.mu.Unlock()
		return err
	}
	w := &waiter{ch: make(chan struct{}), ownerID: owner.ID}
	w.elem = r.waiters.PushBack(w)
	r.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-w.ch:
		// Granted a freed slot via direct handoff; slotsInUse already accounts
		// for it.
		return nil
	case <-timer.C:
		return r.abandonWaiter(w, errQueueTimeout)
	case <-ctx.Done():
		return r.abandonWaiter(w, fmt.Errorf("claim cancelled while queued: %w", ctx.Err()))
	}
}

// abandonWaiter removes a waiter that gave up (timed out or had its context
// cancelled) and returns cause. It resolves the grant race under r.mu: if the
// waiter was granted a slot between waking and acquiring the lock, it holds a
// slot it will not use, so it releases it back (which hands off to the next
// waiter or decrements the count) — no slot is granted to an abandoned waiter
// and the count never drifts.
func (r *Registry) abandonWaiter(w *waiter, cause error) error {
	r.mu.Lock()
	if w.granted {
		r.mu.Unlock()
		// Slot was handed to us at the last moment; give it back to the queue.
		r.releaseSlot()
		return cause
	}
	if w.elem != nil {
		r.waiters.Remove(w.elem)
		w.elem = nil
	}
	r.mu.Unlock()
	return cause
}

// principalCapsApply reports whether the per-principal admission caps are in
// force for owner.
//
// TWO conditions, and both are required. The caps engage only when the broker
// has an `auth:` block (r.principalCaps), and never for the anonymous principal
// (an empty id).
//
// Either test alone would be wrong. With authentication off every lease is
// owned by anonymousOwner() — a zero nexusauth.Principal whose ID is "" — so a
// per-principal cap applied there would count EVERY lease in the broker against
// one identity and silently become a second, lower global cap, breaking every
// unauthenticated deployment. The empty-id test covers the mixed case as well: a
// broker that does configure auth can still serve a route registered outside the
// guard, and a claim arriving that way is anonymous too, so it must be treated
// exactly as an auth-off claim is.
//
// Caller MUST hold r.mu (it reads r.principalCaps, which is only ever written at
// wiring time, and is read here under the lock for consistency with its
// neighbours).
func (r *Registry) principalCapsApply(owner nexusauth.Principal) bool {
	return r.principalCaps && owner.ID != ""
}

// admitPrincipalLeaseLocked refuses owner a new lease once it already holds
// max_leases_per_principal live ones.
//
// It is an ADMISSION CHECK, not a counter. The answer is derived by walking the
// leases the registry already holds, so there is no per-principal tally to keep
// in sync with insertLeaseLocked and Remove and therefore nothing that can
// drift. Slot accounting keeps its single counter untouched (see the file
// header). The walk is bounded by max_concurrent, and only runs at all when the
// caps are in force and a limit is configured.
//
// A non-positive limit means UNLIMITED, matching max_concurrent's reading of the
// same shape.
//
// Caller MUST hold r.mu.
func (r *Registry) admitPrincipalLeaseLocked(owner nexusauth.Principal) error {
	if r.maxLeasesPerPrincipal <= 0 || !r.principalCapsApply(owner) {
		return nil
	}
	live := 0
	for _, l := range r.leases {
		if l.owner.ID != owner.ID {
			continue
		}
		live++
		if live >= r.maxLeasesPerPrincipal {
			return errPrincipalLeaseLimit
		}
	}
	return nil
}

// admitPrincipalQueueLocked refuses owner a place in the FIFO capacity queue
// once it already has max_queued_per_principal claims parked there.
//
// Like the lease cap it derives its answer from existing state — a walk of the
// waiter list, bounded by max_queue_depth — rather than from a tally of its own.
// The check and the PushBack that follows it happen under one uninterrupted hold
// of r.mu, so the cap is exact even under a storm of concurrent claims from one
// caller.
//
// This is the cap that closes the actual finding: FIFO ordering across the queue
// is deliberately left alone (per-principal fair queueing is out of scope), so
// what stops one caller occupying the whole queue is a bound on how much of it
// that caller may hold at once.
//
// Caller MUST hold r.mu.
func (r *Registry) admitPrincipalQueueLocked(owner nexusauth.Principal) error {
	if r.maxQueuedPerPrincipal <= 0 || !r.principalCapsApply(owner) {
		return nil
	}
	queued := 0
	for e := r.waiters.Front(); e != nil; e = e.Next() {
		if e.Value.(*waiter).ownerID != owner.ID {
			continue
		}
		queued++
		if queued >= r.maxQueuedPerPrincipal {
			return errPrincipalQueueLimit
		}
	}
	return nil
}

// SlotsInUse returns the number of capacity slots currently held — one per live
// lease (plus any granted-but-not-yet-bound waiter in flight). Exposed for
// observability and for tests asserting no slot drift across release, idle,
// crash, failed-spawn, queue-timeout, and cancel paths.
func (r *Registry) SlotsInUse() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.slotsInUse
}

// QueueLen returns the number of claims currently parked in the FIFO capacity
// queue. Exposed for observability and for tests asserting no leaked waiters.
func (r *Registry) QueueLen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.waiters.Len()
}
