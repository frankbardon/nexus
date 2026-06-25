package main

import "errors"

// errNoCapacity is returned by NewLease when the registry is already at its
// max_concurrent ceiling and no slot can be acquired. The claim handler maps it
// to HTTP 503 so an over-capacity claim is clearly distinguishable from other
// failures (which map to 4xx/5xx). E3-S2 replaces the immediate rejection with
// a bounded FIFO wait that layers on top of this SAME slot counter — see the
// seam note on releaseSlotLocked.
var errNoCapacity = errors.New("no capacity")

// Slot accounting is the registry's single source of truth for capacity. A
// slot is acquired exactly once per live lease (inside NewLease, BEFORE the
// claim spawns a process) and freed exactly once when that lease is dropped
// (inside Remove, the one teardown sink every path — manual release, idle,
// crash, and the claim error/cleanup paths — funnels through). Binding a slot
// to a lease this way makes the count impossible to drift: it is freed iff a
// lease is removed.

// tryAcquireSlotLocked reserves one capacity slot if the registry is below its
// max_concurrent ceiling, returning true on success and false at capacity. A
// non-positive maxConcurrent means UNLIMITED capacity, so it always succeeds —
// the least-surprising meaning for an unconfigured cap.
//
// Caller MUST hold r.mu. This is the single acquire primitive: the
// blocking/awaitable acquire E3-S2's FIFO queue needs layers on top of this
// counter (a queued claim waits for releaseSlotLocked to free a slot, then
// retries NewLease) rather than introducing a second counter.
func (r *Registry) tryAcquireSlotLocked() bool {
	if r.maxConcurrent > 0 && r.slotsInUse >= r.maxConcurrent {
		return false
	}
	r.slotsInUse++
	return true
}

// releaseSlotLocked frees one previously acquired capacity slot. It is driven
// exclusively by Remove, so a slot is freed exactly once per acquire and the
// count never drifts. The guard keeps the counter from underflowing if it is
// ever called without a matching acquire.
//
// Caller MUST hold r.mu. E3-S2 signals queued waiters from here once a slot
// frees, so a blocking acquire can wake and claim the slot.
func (r *Registry) releaseSlotLocked() {
	if r.slotsInUse > 0 {
		r.slotsInUse--
	}
}

// SlotsInUse returns the number of capacity slots currently held — one per live
// lease. Exposed for observability and for tests asserting no slot drift across
// release, idle, crash, and failed-spawn paths.
func (r *Registry) SlotsInUse() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.slotsInUse
}
