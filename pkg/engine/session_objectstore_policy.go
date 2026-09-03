package engine

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file is what makes core.object_store.failure_policy mean something.
//
// Before it, the policy branched in exactly two places — the shutdown flush
// (E1-S2) and the turn-boundary snapshot (E1-S4) — and in both it only decided
// how loudly to log. A store that went away mid-session produced a warning per
// turn under `degrade` and a warning plus a core.error under `strict`, and in
// neither case did anything retry, recover, or stop. This adds the three
// missing halves: a bounded retry queue with exponential backoff, a degraded
// state published on the bus so an embedder can react, and — under `strict` —
// a refusal to start another turn while the last one is not durably stored.
//
// # What `strict` does and does not guarantee, stated once, plainly
//
// The turn that failed to persist ALREADY HAPPENED. The user saw its output,
// the agent's memory contains it, and the journal recorded it. Nothing here
// un-runs it, and the docs must not imply otherwise.
//
// A literal "the turn fails" would need a pre-commit point — a
// `before:agent.turn.end` veto, or a run-abort path — and neither exists.
// Inventing one would also not help: by the time an agent loop emits
// agent.turn.end, the output has been streamed, the tools have run and the side
// effects are in the world. The only honest place to gate is the *next* turn,
// before the agent has done anything for it, and that is what happens here:
//
//   - The failure is raised immediately: core.error, an error-level log line
//     naming the backend, and session.snapshot.result with ok:false.
//   - session.storage.degraded goes out on the bus with turns_blocked:true.
//   - Every subsequent io.input is VETOED (before:io.input, priority 200, so
//     slash commands and cancellation still get first refusal) until the state
//     is durably stored.
//   - Recovery is automatic. The retry worker re-snapshots with backoff; the
//     first success clears the block, emits session.storage.recovered, and the
//     session carries on. No operator action, no restart.
//
// So `strict` guarantees: **no turn ever runs against state whose predecessor
// was not durably stored, and the divergence is never silent.** It does not
// guarantee that the turn which hit the outage was prevented — it was not.
//
// # What `degrade` costs, stated just as plainly
//
// Turns keep succeeding against the local working copy while the queue backs
// up. During a long outage the durability guarantee is simply not being met,
// even though nothing is failing. That is the trade the operator chose by
// selecting it, and it is documented in
// docs/src/configuration/reference.md rather than only here.
//
// # Why one retry worker rather than a queue per failure site
//
// Every failure site — a snapshot, a blob write-through Put, a write-through
// flush — wants the same thing: try again later, and stop shouting once it
// works. They differ only in what "again" means, and a whole-tree snapshot
// supersedes every individual push (it re-uploads anything the store does not
// already hold at the right size, per listImmutableRemote). So the queue holds
// individual pushes as an optimisation and a single "snapshot pending" flag as
// the backstop, and overflow of the former escalates to the latter rather than
// losing anything.

const (
	// objectStoreRetryQueue bounds the pending-push backlog.
	//
	// Overflow does NOT drop work. A push that does not fit is discarded and
	// the whole-tree snapshot is marked pending instead, and that snapshot
	// re-uploads every object the store is missing — so the escalation is
	// strictly stronger than the item it replaced, at the cost of being
	// coarser. That is what lets this be a fixed bound rather than a knob: the
	// queue is a latency optimisation and the snapshot is the correctness
	// path, so running out of queue costs bandwidth, never durability.
	//
	// 256 matches blobWriteThroughQueue deliberately. The write-through worker
	// is the main producer, so a burst that overflows one and not the other
	// would only be confusing.
	objectStoreRetryQueue = 256

	// objectStoreRetryPutTimeout bounds one retried push. Mirrors
	// blobWriteThroughPutTimeout, because it is retrying exactly that work.
	objectStoreRetryPutTimeout = 2 * time.Minute

	// objectStoreRetryStopTimeout bounds the wait for the worker to finish on
	// shutdown. The stop signal cancels the context the in-flight retry runs
	// under, so this is a backstop against a backend that ignores
	// cancellation, not the expected path.
	objectStoreRetryStopTimeout = 30 * time.Second
)

// Backoff schedule. Vars rather than consts for exactly one reason: the retry
// tests compress the schedule instead of sleeping through the real one.
// Nothing outside a test writes them, and the worker copies them into its own
// fields at construction so a later write cannot race a running worker.
//
// Exponential with no jitter. Jitter exists to de-synchronise many clients
// hammering one endpoint; there is one worker per engine run and it issues one
// request at a time, so jitter would buy nothing and would make the schedule
// untestable without a fake clock.
var (
	objectStoreRetryBaseDelay = 1 * time.Second
	objectStoreRetryMaxDelay  = 60 * time.Second
)

// objectStoreEpisode is one continuous stretch of not-being-able-to-persist,
// from the first failure to the first success after it. Both bus events
// describe an episode, so an operator can count outages rather than failures.
type objectStoreEpisode struct {
	since    time.Time
	failures uint64
	lastErr  string
}

// objectStoreHealth is the run-scoped degraded state. Guarded by a plain mutex
// rather than atomics because the fields have to move together: an episode
// whose failure count advanced but whose start time did not would report a
// nonsense duration on recovery.
type objectStoreHealth struct {
	mu       sync.Mutex
	degraded bool
	// announced records that session.storage.degraded has already gone out for
	// this episode. Kept separate from `degraded` because the emission is
	// deliberately deferred: a failure can be recorded before startJournal has
	// subscribed the journal's wildcard (the blob write-through worker is
	// installed that early), and one event emitted this side of it consumes a
	// dispatch sequence the journal writer then waits for forever. So failures
	// are *recorded* anywhere and *announced* only from the retry worker,
	// which starts after the journal is recording. Same trap, and same fix, as
	// evaluateSessionOwnerMarker.
	announced bool
	// blocked is the strict-policy turn gate. Set when a snapshot fails under
	// FailurePolicyStrict, cleared by the first success after it.
	blocked bool

	episode objectStoreEpisode
}

// fail records a failure and reports whether it opened a new episode.
func (h *objectStoreHealth) fail(err error, strict bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	opened := !h.degraded
	if opened {
		h.degraded = true
		h.announced = false
		h.episode = objectStoreEpisode{since: time.Now()}
	}
	h.episode.failures++
	if err != nil {
		h.episode.lastErr = err.Error()
	}
	if strict {
		h.blocked = true
	}
	return opened
}

// takeAnnouncement claims the right to emit session.storage.degraded for the
// current episode, exactly once.
func (h *objectStoreHealth) takeAnnouncement() (objectStoreEpisode, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.degraded || h.announced {
		return objectStoreEpisode{}, false
	}
	h.announced = true
	return h.episode, true
}

// recover closes the episode and reports whether a recovery event is owed —
// which it is only when the degraded event actually went out. An episode that
// opened and closed before the worker was running was never visible to anyone,
// so announcing its recovery would be the only trace of it.
func (h *objectStoreHealth) recover() (objectStoreEpisode, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.degraded {
		return objectStoreEpisode{}, false
	}
	ep := h.episode
	announced := h.announced
	h.degraded = false
	h.announced = false
	h.blocked = false
	h.episode = objectStoreEpisode{}
	return ep, announced
}

// snapshot reads the current state for an event payload.
func (h *objectStoreHealth) snapshot() (objectStoreEpisode, bool, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.episode, h.degraded, h.blocked
}

// turnBlocked reports whether strict has closed the gate, and why.
func (h *objectStoreHealth) turnBlocked() (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.blocked {
		return "", false
	}
	reason := "object store: the previous turn's state is not durably stored and " +
		"core.object_store.failure_policy is strict; the session will accept input " +
		"again as soon as a snapshot succeeds"
	if h.episode.lastErr != "" {
		reason += " (last error: " + h.episode.lastErr + ")"
	}
	return reason, true
}

// retryPush is one deferred object upload: an object key and the local file
// its bytes come from. A path rather than the bytes, for the same reason
// Backend.Put takes one — the file is the live working copy and re-reading it
// on retry picks up whatever it holds now, which for a content-addressed blob
// is provably the same bytes and for anything else is the fresher answer.
type retryPush struct {
	key string
	src string
}

// objectStoreRetry is the bounded queue and its worker. One per run, created
// only when a backend is configured, so the default path grows no channel and
// no goroutine.
type objectStoreRetry struct {
	pushes chan retryPush
	// wake carries "there is something to do" with capacity 1: the worker only
	// needs to be told once, and a producer must never block on telling it.
	wake chan struct{}
	// snapshotPending is the backstop. Set by any failure the queue cannot
	// express — a failed snapshot, a failed flush, a queue overflow — and
	// cleared only by a snapshot that completed.
	snapshotPending atomic.Bool

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	// started records that the worker goroutine is actually running. The queue
	// is created in openObjectStore but the worker only starts much later in
	// Boot (see installObjectStoreRetry), and every Boot-failure path in
	// between goes through stopObjectStoreRetry — which would otherwise wait
	// the full stop timeout for a `done` nobody is ever going to close.
	started atomic.Bool

	baseDelay time.Duration
	maxDelay  time.Duration

	// Counters for the shutdown summary and for the two bus events.
	dropped  atomic.Uint64
	drained  atomic.Uint64
	attempts atomic.Uint64
}

func newObjectStoreRetry() *objectStoreRetry {
	return &objectStoreRetry{
		pushes:    make(chan retryPush, objectStoreRetryQueue),
		wake:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		baseDelay: objectStoreRetryBaseDelay,
		maxDelay:  objectStoreRetryMaxDelay,
	}
}

// signal nudges the worker without ever blocking the caller, which may be a
// tool goroutine mid-tool-call.
func (r *objectStoreRetry) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// enqueue queues a deferred push, escalating to a whole-tree snapshot when the
// queue is full. See objectStoreRetryQueue for why that is a strengthening
// rather than a loss.
func (r *objectStoreRetry) enqueue(p retryPush) {
	select {
	case r.pushes <- p:
	default:
		r.dropped.Add(1)
		r.snapshotPending.Store(true)
	}
	r.signal()
}

func (r *objectStoreRetry) hasWork() bool {
	return r.snapshotPending.Load() || len(r.pushes) > 0
}

// noteObjectStoreFailure records a persistence failure, schedules the recovery
// work that answers it, and — under strict — closes the turn gate.
//
// It emits nothing. Announcement is the retry worker's job; see
// objectStoreHealth.announced for why.
func (e *Engine) noteObjectStoreFailure(store *sessionObjectStore, err error, needSnapshot bool) {
	if store == nil {
		return
	}
	strict := store.cfg.FailurePolicy == objectstore.FailurePolicyStrict
	store.health.fail(err, strict)
	if store.retry == nil {
		return
	}
	if needSnapshot {
		store.retry.snapshotPending.Store(true)
	}
	store.retry.signal()
}

// noteObjectStorePushFailure defers one object for retry.
//
// Used by the blob write-through path, whose failures are best-effort by
// design: the turn-boundary snapshot is the authority for what the store holds
// and will re-upload anything missing. What changes here is that the push is no
// longer simply dropped — it is queued, retried with backoff, and counted
// towards the degraded state, so an outage that heals between turns drains
// without waiting for a boundary that may never come on an idle session.
//
// It deliberately does NOT close the strict turn gate. A write-through Put is
// an optimisation in front of the snapshot; failing a turn because an
// optimisation stumbled — on an object the very next snapshot repairs — would
// make strict fire on transients it is not there to catch. The snapshot is the
// only thing strict gates on.
func (e *Engine) noteObjectStorePushFailure(store *sessionObjectStore, key, src string, err error) {
	if store == nil || store.retry == nil {
		return
	}
	store.health.fail(err, false)
	store.retry.enqueue(retryPush{key: key, src: src})
}

// maybeRecoverObjectStore closes the episode once nothing is outstanding.
//
// "Nothing outstanding" is deliberately stricter than "the last call
// succeeded": a snapshot that worked while ten pushes are still queued has not
// caught the store up, and reporting recovery there would clear a turn gate the
// store has not earned.
func (e *Engine) maybeRecoverObjectStore(store *sessionObjectStore) {
	if store == nil {
		return
	}
	if r := store.retry; r != nil && r.hasWork() {
		return
	}
	ep, announced := store.health.recover()
	if !announced {
		return
	}

	var attempts, drained, dropped uint64
	if r := store.retry; r != nil {
		attempts, drained, dropped = r.attempts.Load(), r.drained.Load(), r.dropped.Load()
	}
	degradedFor := time.Since(ep.since)
	e.Logger.Info("object store: recovered, the stored session is current again",
		"backend", store.cfg.BackendName,
		"degraded_for", degradedFor.Round(time.Millisecond),
		"failures", ep.failures,
		"retry_attempts", attempts)

	if e.Session == nil {
		return
	}
	_ = e.Bus.Emit("session.storage.recovered", events.SessionStorageRecovered{
		SchemaVersion:      events.SessionStorageRecoveredVersion,
		SessionID:          e.Session.ID,
		Backend:            store.cfg.BackendName,
		FailurePolicy:      string(store.cfg.FailurePolicy),
		DegradedForSeconds: degradedFor.Seconds(),
		Failures:           ep.failures,
		RetryAttempts:      attempts,
		DrainedPushes:      drained,
		DroppedPushes:      dropped,
	})
}

// announceObjectStoreDegraded emits session.storage.degraded at most once per
// episode. Called only from the retry worker, which is started after
// startJournal — see objectStoreHealth.announced.
func (e *Engine) announceObjectStoreDegraded(store *sessionObjectStore) {
	ep, ok := store.health.takeAnnouncement()
	if !ok {
		return
	}
	_, _, blocked := store.health.snapshot()

	var queued int
	var dropped uint64
	pending := false
	if r := store.retry; r != nil {
		queued = len(r.pushes)
		dropped = r.dropped.Load()
		pending = r.snapshotPending.Load()
	}

	e.Logger.Warn("object store: degraded — the session is running against the local working copy",
		"backend", store.cfg.BackendName,
		"failure_policy", string(store.cfg.FailurePolicy),
		"queued_pushes", queued,
		"snapshot_pending", pending,
		"turns_blocked", blocked,
		"error", ep.lastErr)

	if e.Session == nil {
		return
	}
	_ = e.Bus.Emit("session.storage.degraded", events.SessionStorageDegraded{
		SchemaVersion:       events.SessionStorageDegradedVersion,
		SessionID:           e.Session.ID,
		Backend:             store.cfg.BackendName,
		FailurePolicy:       string(store.cfg.FailurePolicy),
		Since:               ep.since.UTC(),
		ConsecutiveFailures: ep.failures,
		QueuedPushes:        queued,
		DroppedPushes:       dropped,
		SnapshotPending:     pending,
		TurnsBlocked:        blocked,
		Error:               ep.lastErr,
	})
}

// installObjectStoreRetry starts the recovery worker and, under strict, the
// turn gate.
//
// Called from installObjectStoreSnapshots, which Boot runs after startJournal
// and after Lifecycle.Boot. That placement is the whole reason the worker is
// allowed to emit: the journal's wildcard is subscribed and plugins have had a
// chance to subscribe to session.storage.degraded. Failures recorded earlier —
// a blob write-through Put during plugin Init — are already in the queue and
// are announced by the worker's first pass.
//
// A no-op when no backend is configured.
func (e *Engine) installObjectStoreRetry() {
	store := e.objectStore
	if store == nil || store.retry == nil {
		return
	}
	// The journal writer is captured once rather than read from e.Journal on
	// every retry: Stop nils that field, and a background goroutine reading it
	// while the shutdown goroutine writes it is a data race whose only symptom
	// is a rare crash under -race. stopObjectStoreRetry runs before the writer
	// is closed, so the captured handle is always live while the worker uses it.
	store.retry.started.Store(true)
	go e.runObjectStoreRetry(store, e.Journal)

	e.installObjectStoreTurnGate(store)
}

// installObjectStoreTurnGate is the teeth of FailurePolicyStrict.
//
// before:io.input is the only honest gate available. agent.turn.end is not
// vetoable and could not help if it were — by then the turn has run. Refusing
// the *next* input is the last point at which nothing has happened yet.
//
// Priority 200 puts it behind every other before:io.input subscriber
// (nexus.control.cancel and nexus.mcp.client both sit at 5), so slash commands
// and cancellation still work while the gate is closed. That matters: an
// operator whose store is down must still be able to type /resume or stop the
// run.
func (e *Engine) installObjectStoreTurnGate(store *sessionObjectStore) {
	if store.cfg.FailurePolicy != objectstore.FailurePolicyStrict {
		return
	}
	e.runUnsubs = append(e.runUnsubs, e.Bus.Subscribe("before:io.input", func(ev Event[any]) {
		vp, ok := ev.Payload.(*VetoablePayload)
		if !ok {
			return
		}
		reason, blocked := store.health.turnBlocked()
		if !blocked {
			return
		}
		vp.Veto = VetoResult{Vetoed: true, Reason: reason}
		e.Logger.Error("object store: refusing input — the last turn is not durably stored",
			"backend", store.cfg.BackendName, "reason", reason)
	}, WithPriority(200), WithSource("nexus.engine.objectstore")))
}

// runObjectStoreRetry is the worker loop: wait for work, announce the outage
// once, back off, try again.
func (e *Engine) runObjectStoreRetry(store *sessionObjectStore, jw *journal.Writer) {
	r := store.retry
	defer close(r.done)

	// A context the stop signal cancels, so a retry in flight when shutdown
	// begins aborts instead of holding Stop for the length of a whole-tree
	// snapshot against a wedged backend.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-r.stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	delay := r.baseDelay
	for {
		if !r.hasWork() {
			select {
			case <-r.wake:
				continue
			case <-r.stop:
				return
			}
		}

		// Announced before the backoff wait, not after it, so an embedder
		// learns the store is down within microseconds of the first failure
		// rather than after the first retry has also failed.
		e.announceObjectStoreDegraded(store)

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-r.stop:
			timer.Stop()
			return
		}

		r.attempts.Add(1)
		if e.retryObjectStoreWork(ctx, store, jw) {
			delay = r.baseDelay
			continue
		}
		delay = nextRetryDelay(delay, r.maxDelay)
	}
}

// nextRetryDelay is the backoff step: double, capped. Pulled out as a function
// so the schedule is testable without waiting for it.
func nextRetryDelay(cur, max time.Duration) time.Duration {
	if cur <= 0 {
		cur = time.Millisecond
	}
	next := cur * 2
	if next > max || next < cur {
		// next < cur catches the overflow a pathological max would allow.
		return max
	}
	return next
}

// retryObjectStoreWork makes one attempt at everything outstanding: the queued
// pushes first, then the pending whole-tree snapshot. Reports whether the
// attempt got through cleanly.
func (e *Engine) retryObjectStoreWork(ctx context.Context, store *sessionObjectStore, jw *journal.Writer) bool {
	r := store.retry

	pushed := 0
drain:
	for {
		select {
		case item := <-r.pushes:
			if !e.retryOnePush(ctx, store, item) {
				return false
			}
			pushed++
		default:
			break drain
		}
	}

	if pushed > 0 {
		flushCtx, cancel := context.WithTimeout(ctx, objectStoreFlushTimeout)
		err := store.backend.Flush(flushCtx)
		cancel()
		if err != nil {
			// The pushes are not durable, so they are not done. Escalate to
			// the snapshot rather than re-queueing each one: Flush is a
			// whole-backend barrier, so its failure says nothing about which
			// individual object survived, and only a full walk can answer that.
			r.snapshotPending.Store(true)
			e.noteObjectStoreFailure(store, err, true)
			e.Logger.Warn("object store: retry flush failed",
				"backend", store.cfg.BackendName, "error", err)
			return false
		}
		r.drained.Add(uint64(pushed))
	}

	if r.snapshotPending.Load() {
		// runSessionSnapshot clears snapshotPending and publishes recovery on
		// success, and calls back into noteObjectStoreFailure on failure — so
		// the flag is the whole answer here.
		e.runSessionSnapshot(snapshotRequest{
			trigger: snapshotTriggerRetry,
			journal: jw,
			parent:  ctx,
		})
		return !r.snapshotPending.Load()
	}

	e.maybeRecoverObjectStore(store)
	return true
}

// retryOnePush re-uploads one deferred object.
//
// A local file that has since vanished is dropped rather than retried forever:
// the commonest cause is the blob store's LRU sweep evicting the file between
// the failed push and this retry, and the object either already reached the
// store or is genuinely gone from this host. Either way there is nothing left
// to upload and the snapshot is the authority on what the store should hold.
func (e *Engine) retryOnePush(ctx context.Context, store *sessionObjectStore, item retryPush) bool {
	r := store.retry

	if _, err := os.Stat(item.src); errors.Is(err, fs.ErrNotExist) {
		r.dropped.Add(1)
		e.Logger.Debug("object store: dropping a retry whose local file is gone",
			"key", item.key, "path", item.src)
		return true
	}

	putCtx, cancel := context.WithTimeout(ctx, objectStoreRetryPutTimeout)
	err := store.backend.Put(putCtx, item.key, item.src)
	cancel()
	if err == nil {
		return true
	}

	// Back on the queue, at the end, so one permanently-bad object cannot
	// starve the rest of the backlog.
	r.enqueue(item)
	e.noteObjectStoreFailure(store, err, false)
	e.Logger.Warn("object store: retrying a deferred push failed",
		"backend", store.cfg.BackendName, "key", item.key, "error", err)
	return false
}

// stopObjectStoreRetry cancels any retry in flight and waits for the worker to
// exit. Idempotent, and a no-op when nothing was installed.
//
// Called at the top of Stop, before the journal writer is closed, because the
// worker snapshots through that writer. The bounded wait is a backstop only —
// the stop signal cancels the retry's context, so a cooperative backend
// unwinds immediately.
func (e *Engine) stopObjectStoreRetry() {
	store := e.objectStore
	if store == nil || store.retry == nil {
		return
	}
	r := store.retry

	r.stopOnce.Do(func() { close(r.stop) })
	if !r.started.Load() {
		// Boot never got as far as starting the worker. Nothing will ever
		// close done, so waiting on it would burn the whole stop timeout on
		// every failed boot and on every run that ends before the snapshot
		// handlers are installed.
		return
	}
	select {
	case <-r.done:
	case <-time.After(objectStoreRetryStopTimeout):
		e.Logger.Warn("object store: retry worker did not stop in time; " +
			"anything still queued is covered by the shutdown snapshot")
	}

	queued, drained, dropped, attempts := len(r.pushes), r.drained.Load(), r.dropped.Load(), r.attempts.Load()
	if attempts == 0 && drained == 0 && dropped == 0 && queued == 0 {
		return
	}
	e.Logger.Info("object store: retry summary",
		"attempts", attempts, "drained", drained, "dropped", dropped, "still_queued", queued)
}
