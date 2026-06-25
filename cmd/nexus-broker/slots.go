package main

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"time"
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

// waiter is one parked claim in the FIFO capacity queue. It owns a channel the
// release path closes to grant it the freed slot. granted records (under
// Registry.mu) whether the slot has been handed to this waiter, so a waiter
// that wakes on timeout/cancel can tell a real grant from a giving-up and
// release the slot back to the next waiter if it was granted at the last moment.
// elem is this waiter's node in Registry.waiters, kept so it can be removed in
// O(1) when the waiter gives up before being granted.
type waiter struct {
	ch      chan struct{}
	granted bool
	elem    *list.Element
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
//   - errQueueTimeout if the cap stayed full for longer than timeout;
//   - the wrapped ctx error if the caller's request context is cancelled while
//     queued (client hung up) — the waiter is dropped from the queue and frees
//     nothing it never held.
//
// On every non-nil return the caller holds no slot. On a nil return the caller
// owns exactly one slot and must bind it to a lease (insertLeaseLocked) or hand
// it back via releaseSlot.
func (r *Registry) acquireSlot(ctx context.Context, timeout time.Duration) error {
	r.mu.Lock()
	if r.tryAcquireSlotLocked() {
		r.mu.Unlock()
		return nil
	}
	if timeout <= 0 {
		// No waiting configured: at capacity is an immediate rejection.
		r.mu.Unlock()
		return errNoCapacity
	}
	w := &waiter{ch: make(chan struct{})}
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
