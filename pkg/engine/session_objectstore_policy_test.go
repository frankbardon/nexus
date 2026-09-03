package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/events"
)

// compressRetrySchedule shrinks the backoff so a recovery test finishes in
// milliseconds instead of minutes. The worker copies these into its own fields
// at construction, so a test that has already booted is unaffected — which is
// why every caller sets them before Boot.
func compressRetrySchedule(t *testing.T) {
	t.Helper()
	base, max := objectStoreRetryBaseDelay, objectStoreRetryMaxDelay
	objectStoreRetryBaseDelay = 2 * time.Millisecond
	objectStoreRetryMaxDelay = 20 * time.Millisecond
	t.Cleanup(func() {
		objectStoreRetryBaseDelay, objectStoreRetryMaxDelay = base, max
	})
}

// storageEvents collects the two health events in order, so a test can assert
// on the episode rather than on a single sighting.
type storageEvents struct {
	degraded  chan events.SessionStorageDegraded
	recovered chan events.SessionStorageRecovered
}

func watchStorageHealth(t *testing.T, eng *Engine) *storageEvents {
	t.Helper()
	w := &storageEvents{
		degraded:  make(chan events.SessionStorageDegraded, 16),
		recovered: make(chan events.SessionStorageRecovered, 16),
	}
	unsubD := eng.Bus.Subscribe("session.storage.degraded", func(ev Event[any]) {
		if p, ok := ev.Payload.(events.SessionStorageDegraded); ok {
			w.degraded <- p
		}
	})
	unsubR := eng.Bus.Subscribe("session.storage.recovered", func(ev Event[any]) {
		if p, ok := ev.Payload.(events.SessionStorageRecovered); ok {
			w.recovered <- p
		}
	})
	t.Cleanup(func() { unsubD(); unsubR() })
	return w
}

func (w *storageEvents) awaitDegraded(t *testing.T) events.SessionStorageDegraded {
	t.Helper()
	select {
	case ev := <-w.degraded:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no session.storage.degraded was published; an outage the embedder cannot see is the failure this event exists to remove")
		return events.SessionStorageDegraded{}
	}
}

func (w *storageEvents) awaitRecovered(t *testing.T) events.SessionStorageRecovered {
	t.Helper()
	select {
	case ev := <-w.recovered:
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("no session.storage.recovered was published; the backlog never drained")
		return events.SessionStorageRecovered{}
	}
}

// offerInput drives the vetoable gate the way every IO transport does.
func offerInput(t *testing.T, eng *Engine, content string) VetoResult {
	t.Helper()
	input := events.UserInput{SchemaVersion: events.UserInputVersion, Content: content}
	veto, err := eng.Bus.EmitVetoable("before:io.input", &input)
	if err != nil {
		t.Fatalf("EmitVetoable(before:io.input): %v", err)
	}
	return veto
}

// ---------------------------------------------------------------------------
// degrade
// ---------------------------------------------------------------------------

// The degrade contract in one test: the turn still happens, the session keeps
// taking input, and the outage is visible on the bus.
func TestDegradeKeepsRunningAndPublishesTheOutage(t *testing.T) {
	compressRetrySchedule(t)
	eng, backend := newMemoryObjectStoreEngine(t, objectstore.FailurePolicyDegrade)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	watch := watchStorageHealth(t, eng)

	if err := eng.Session.WriteFile("files/report.md", []byte("hello")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Unbounded: the store stays down for the rest of this test, so the
	// assertion is about what happens *while* degraded.
	backend.SetFlushError(errors.New("bucket unreachable"))
	endTurn(t, eng, "turn-1")

	ev := watch.awaitDegraded(t)
	if ev.SessionID != eng.Session.ID {
		t.Errorf("degraded event session = %q, want %q", ev.SessionID, eng.Session.ID)
	}
	if ev.FailurePolicy != string(objectstore.FailurePolicyDegrade) {
		t.Errorf("degraded event policy = %q, want degrade", ev.FailurePolicy)
	}
	if ev.TurnsBlocked {
		t.Error("degrade must never block turns — that is the entire difference from strict")
	}
	if !ev.SnapshotPending {
		t.Error("a failed snapshot must leave the snapshot backstop pending")
	}
	if ev.Error == "" {
		t.Error("degraded event carries no error message; an embedder has nothing to act on")
	}

	// The local working copy is still the live one and still readable, which is
	// what "keeps running" means concretely.
	if body, err := eng.Session.ReadFile("files/report.md"); err != nil || string(body) != "hello" {
		t.Fatalf("local working copy unusable while degraded: %q, %v", body, err)
	}
	if veto := offerInput(t, eng, "next question"); veto.Vetoed {
		t.Errorf("degrade refused input: %q", veto.Reason)
	}
}

// Exactly one degraded event per outage, however many turns fail inside it. An
// event per failure would make an embedder's "page someone" handler fire once a
// turn for the length of the outage.
func TestDegradedIsAnnouncedOncePerEpisode(t *testing.T) {
	compressRetrySchedule(t)
	eng, backend := newMemoryObjectStoreEngine(t, objectstore.FailurePolicyDegrade)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	watch := watchStorageHealth(t, eng)

	backend.SetFlushError(errors.New("bucket unreachable"))
	endTurn(t, eng, "turn-1")
	watch.awaitDegraded(t)
	endTurn(t, eng, "turn-2")
	endTurn(t, eng, "turn-3")

	// Let the retry worker fail a few more times for good measure.
	time.Sleep(50 * time.Millisecond)
	if extra := len(watch.degraded); extra != 0 {
		t.Errorf("%d further degraded events inside one episode, want 0", extra)
	}
}

// ---------------------------------------------------------------------------
// recovery — the acceptance criterion this story turns on
// ---------------------------------------------------------------------------

// An outage that heals mid-session drains the backlog and returns to normal
// with no operator action and, crucially, no further turn: the retry worker is
// the only thing that runs between the failure and the recovery.
func TestHealedOutageDrainsWithoutOperatorAction(t *testing.T) {
	compressRetrySchedule(t)
	eng, backend := newMemoryObjectStoreEngine(t, objectstore.FailurePolicyDegrade)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	watch := watchStorageHealth(t, eng)
	sessionID := eng.Session.ID

	if err := eng.Session.WriteFile("files/report.md", []byte("work the user watched happen")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// A bounded outage: three flushes fail, then the bucket is back. The turn
	// snapshot spends the first, so the worker has to retry at least twice.
	backend.SetFlushErrorTimes(errors.New("bucket unreachable"), 3)
	endTurn(t, eng, "turn-1")

	degraded := watch.awaitDegraded(t)
	if degraded.ConsecutiveFailures == 0 {
		t.Error("degraded event reports no failures")
	}

	// Nothing else is emitted, no further turn ends, and no test code touches
	// the engine from here: recovery has to be the worker's doing.
	recovered := watch.awaitRecovered(t)
	if recovered.SessionID != sessionID {
		t.Errorf("recovered event session = %q, want %q", recovered.SessionID, sessionID)
	}
	if recovered.RetryAttempts == 0 {
		t.Error("recovery reports no retry attempts, so it cannot have come from the retry worker")
	}
	if recovered.DegradedForSeconds <= 0 {
		t.Error("recovered event reports a non-positive degraded duration")
	}

	// The proof the backlog actually drained rather than merely being forgotten:
	// the session tree and its commit marker are in the store.
	marker := readMarker(t, backend, sessionID)
	if marker.Trigger != snapshotTriggerRetry {
		t.Errorf("commit marker trigger = %q, want %q — the marker must have been "+
			"published by the recovery snapshot", marker.Trigger, snapshotTriggerRetry)
	}
	keys := sessionKeys(backend, sessionID)
	found := false
	for _, k := range keys {
		if k == "files/report.md" {
			found = true
		}
	}
	if !found {
		t.Errorf("the file written before the outage is not in the store after recovery: %v", keys)
	}

	// And the state machine is closed: not degraded, nothing pending.
	if _, degradedNow, blocked := eng.objectStore.health.snapshot(); degradedNow || blocked {
		t.Errorf("still degraded=%v blocked=%v after recovery", degradedNow, blocked)
	}
}

// The backlog is not only "a snapshot we owe": individual write-through pushes
// that failed are queued, retried and land — again with no turn boundary
// anywhere, which is the case an idle session depends on.
func TestHealedOutageDrainsQueuedBlobPushes(t *testing.T) {
	compressRetrySchedule(t)
	eng, backend := newMemoryObjectStoreEngine(t, objectstore.FailurePolicyDegrade)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	watch := watchStorageHealth(t, eng)

	// Both halves of the blob pair fail, then the bucket is back.
	backend.SetPutErrorTimes(errors.New("bucket unreachable"), 2)

	blobStore, err := eng.Session.BlobStore(0)
	if err != nil {
		t.Fatalf("BlobStore: %v", err)
	}
	data := []byte("bytes that only exist locally for now")
	h, err := blobStore.Put(data, "image/png")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	watch.awaitDegraded(t)
	recovered := watch.awaitRecovered(t)
	if recovered.DrainedPushes < 2 {
		t.Errorf("drained %d pushes, want both halves of the blob pair", recovered.DrainedPushes)
	}

	prefix := "sessions/" + eng.Session.ID + "/blobs/" + h.SHA256[:2] + "/" + h.SHA256
	body := waitForKey(t, backend, prefix+".bin")
	if string(body) != string(data) {
		t.Errorf("stored bin = %q, want %q", body, data)
	}
	waitForKey(t, backend, prefix+".meta")

	// No turn boundary ever happened, so the snapshot cannot be what saved this.
	if eng.objectStore.snapshotSeq != 0 {
		t.Errorf("snapshotSeq = %d, want 0 — the retry queue, not a snapshot, "+
			"must be what drained the backlog", eng.objectStore.snapshotSeq)
	}
}

// The cheapest recovery path, and the common one: the store heals before the
// next turn ends, so that turn's ordinary snapshot closes the episode and the
// retry worker never has to do anything.
func TestNextTurnSnapshotClosesTheEpisode(t *testing.T) {
	compressRetrySchedule(t)
	eng, backend := newMemoryObjectStoreEngine(t, objectstore.FailurePolicyDegrade)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	watch := watchStorageHealth(t, eng)

	backend.SetFlushError(errors.New("bucket unreachable"))
	endTurn(t, eng, "turn-1")
	watch.awaitDegraded(t)

	backend.SetFlushError(nil)
	endTurn(t, eng, "turn-2")

	recovered := watch.awaitRecovered(t)
	if recovered.Failures == 0 {
		t.Error("recovered event reports no failures for an episode that had at least one")
	}
	if _, degradedNow, _ := eng.objectStore.health.snapshot(); degradedNow {
		t.Error("still degraded after a successful turn-boundary snapshot")
	}
}

// ---------------------------------------------------------------------------
// strict
// ---------------------------------------------------------------------------

// Strict's teeth. The turn that failed already ran and is NOT un-run — that is
// asserted here explicitly so the honest limit stays pinned — but no further
// turn starts until the state is durably stored, and the block lifts on its own.
func TestStrictRefusesFurtherInputUntilTheStateIsStored(t *testing.T) {
	compressRetrySchedule(t)
	eng, backend := newMemoryObjectStoreEngine(t, objectstore.FailurePolicyStrict)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	watch := watchStorageHealth(t, eng)

	if veto := offerInput(t, eng, "first question"); veto.Vetoed {
		t.Fatalf("strict refused input before anything failed: %q", veto.Reason)
	}

	var results []events.SessionSnapshotResult
	unsub := eng.Bus.Subscribe("session.snapshot.result", func(ev Event[any]) {
		if r, ok := ev.Payload.(events.SessionSnapshotResult); ok {
			results = append(results, r)
		}
	})
	defer unsub()

	backend.SetFlushErrorTimes(errors.New("bucket unreachable"), 2)
	endTurn(t, eng, "turn-1")

	// The honest limit, pinned: agent.turn.end was dispatched and the turn is
	// over. Strict did not and cannot prevent it.
	if len(results) == 0 || results[0].OK {
		t.Fatalf("expected a failed snapshot result for the completed turn, got %+v", results)
	}

	degraded := watch.awaitDegraded(t)
	if !degraded.TurnsBlocked {
		t.Error("strict published a degraded event that does not say turns are blocked")
	}

	// Everything after it is refused.
	veto := offerInput(t, eng, "second question")
	if !veto.Vetoed {
		t.Fatal("strict accepted a turn while the previous turn was not durably stored")
	}
	if veto.Reason == "" {
		t.Error("strict vetoed with no reason; the user is told nothing")
	}

	// And it lifts by itself once the store comes back.
	watch.awaitRecovered(t)
	if veto := offerInput(t, eng, "third question"); veto.Vetoed {
		t.Errorf("strict still refusing input after recovery: %q", veto.Reason)
	}
}

// Degrade must never install the gate at all — not merely never trigger it.
func TestDegradeInstallsNoTurnGate(t *testing.T) {
	compressRetrySchedule(t)
	eng, backend := newMemoryObjectStoreEngine(t, objectstore.FailurePolicyDegrade)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	watch := watchStorageHealth(t, eng)

	backend.SetFlushError(errors.New("bucket unreachable"))
	endTurn(t, eng, "turn-1")
	watch.awaitDegraded(t)

	if veto := offerInput(t, eng, "next"); veto.Vetoed {
		t.Errorf("degrade blocked a turn: %q", veto.Reason)
	}
	if _, _, blocked := eng.objectStore.health.snapshot(); blocked {
		t.Error("degrade set the strict turn gate")
	}
}

// A write-through failure is an optimisation stumbling, not a durability
// failure: the snapshot repairs it at the boundary. Strict must not fail a turn
// over it, or it fires on transients it is not there to catch.
func TestStrictDoesNotBlockOnAWriteThroughFailure(t *testing.T) {
	compressRetrySchedule(t)
	eng, backend := newMemoryObjectStoreEngine(t, objectstore.FailurePolicyStrict)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	watch := watchStorageHealth(t, eng)

	backend.SetPutErrorTimes(errors.New("transient"), 2)
	blobStore, err := eng.Session.BlobStore(0)
	if err != nil {
		t.Fatalf("BlobStore: %v", err)
	}
	if _, err := blobStore.Put([]byte("a screenshot"), "image/png"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	degraded := watch.awaitDegraded(t)
	if degraded.TurnsBlocked {
		t.Error("a failed blob write-through closed the strict turn gate")
	}
	if veto := offerInput(t, eng, "carry on"); veto.Vetoed {
		t.Errorf("strict refused input over a write-through failure: %q", veto.Reason)
	}
	watch.awaitRecovered(t)
}

// ---------------------------------------------------------------------------
// the bound, and the default path
// ---------------------------------------------------------------------------

// Overflow does not lose work: the item is dropped and the whole-tree snapshot
// — which re-uploads everything the store does not already hold — is marked
// pending in its place.
func TestRetryQueueOverflowEscalatesToASnapshot(t *testing.T) {
	r := newObjectStoreRetry()

	for i := 0; i < objectStoreRetryQueue; i++ {
		r.enqueue(retryPush{key: "sessions/s/files/a", src: "/tmp/a"})
	}
	if r.dropped.Load() != 0 {
		t.Fatalf("dropped %d pushes before the bound was reached", r.dropped.Load())
	}
	if r.snapshotPending.Load() {
		t.Fatal("escalated to a snapshot before the queue was full")
	}

	r.enqueue(retryPush{key: "sessions/s/files/b", src: "/tmp/b"})
	if r.dropped.Load() != 1 {
		t.Errorf("dropped = %d, want 1", r.dropped.Load())
	}
	if !r.snapshotPending.Load() {
		t.Error("an overflowed push must escalate to a whole-tree snapshot, which supersedes it")
	}
	if got := len(r.pushes); got != objectStoreRetryQueue {
		t.Errorf("queue depth = %d, want the bound %d — the queue must not grow", got, objectStoreRetryQueue)
	}
}

func TestNextRetryDelayDoublesAndCaps(t *testing.T) {
	max := 8 * time.Second
	for _, tc := range []struct {
		in, want time.Duration
	}{
		{1 * time.Second, 2 * time.Second},
		{4 * time.Second, 8 * time.Second},
		{8 * time.Second, 8 * time.Second},
		{time.Duration(1) << 62, max},
	} {
		if got := nextRetryDelay(tc.in, max); got != tc.want {
			t.Errorf("nextRetryDelay(%v, %v) = %v, want %v", tc.in, max, got, tc.want)
		}
	}
}

// Zero-impact default: with no backend named, none of this exists — no queue,
// no goroutine, no before:io.input handler.
func TestNoBackendInstallsNoRetryWorkerOrTurnGate(t *testing.T) {
	cfg := DefaultConfig()
	root := t.TempDir()
	cfg.Core.Sessions.Root = root + "/sessions"
	cfg.Core.Storage.Root = root
	eng := newFromConfig(cfg)

	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	if eng.objectStore != nil {
		t.Fatal("objectStore handle installed with no backend configured")
	}
	if veto := offerInput(t, eng, "hello"); veto.Vetoed {
		t.Errorf("input vetoed with no object store configured: %q", veto.Reason)
	}
}

// The worker is stopped before the journal writer it snapshots through is
// closed, so a clean Stop can never leave one running.
func TestStopHaltsTheRetryWorker(t *testing.T) {
	compressRetrySchedule(t)
	eng, backend := newMemoryObjectStoreEngine(t, objectstore.FailurePolicyDegrade)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	watch := watchStorageHealth(t, eng)

	backend.SetFlushError(errors.New("bucket unreachable"))
	endTurn(t, eng, "turn-1")
	watch.awaitDegraded(t)

	retry := eng.objectStore.retry
	backend.SetFlushError(nil)
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-retry.done:
	default:
		t.Error("Stop returned with the retry worker still running")
	}
}

// The health events must reach the journal. They are emitted only from the
// retry worker, which Boot starts after startJournal, for exactly this reason:
// an event dispatched before the journal's wildcard subscribes consumes a
// sequence the writer never sees, and the writer only flushes contiguous
// sequences — so one early emit empties the journal for the whole run.
//
// The blob write-through path can fail during plugin Init, well before the
// journal exists, which is why that failure is *recorded* there and *announced*
// here. This test is what stops that split being undone.
func TestStorageHealthEventsReachTheJournal(t *testing.T) {
	compressRetrySchedule(t)
	eng, backend := newMemoryObjectStoreEngine(t, objectstore.FailurePolicyDegrade)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	watch := watchStorageHealth(t, eng)
	sessionDir := eng.Session.RootDir

	backend.SetFlushErrorTimes(errors.New("bucket unreachable"), 2)
	endTurn(t, eng, "turn-1")
	watch.awaitDegraded(t)
	watch.awaitRecovered(t)

	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(sessionDir, "journal", "events.jsonl"))
	if err != nil {
		t.Fatalf("reading journal: %v", err)
	}
	// Empty is the specific symptom of emitting too early: the drain stalls on
	// a sequence the writer never received.
	if len(body) == 0 {
		t.Fatal("the journal is empty — an event was emitted before startJournal subscribed")
	}
	for _, want := range []string{"session.storage.degraded", "session.storage.recovered"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the journal does not contain %s; it holds %d bytes", want, len(body))
		}
	}
}
