package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// ---- fixtures ----

// openTestTaskStore opens a durable store under dir. A dir of "" gives the
// memory-only store a broker with no state_dir runs.
func openTestTaskStore(t *testing.T, dir string, policy a2aTaskRetention) *a2aTaskStore {
	t.Helper()
	store, err := openA2ATaskStore(testLogger(), Config{StateDir: dir, A2ATaskRetention: policy})
	if err != nil {
		t.Fatalf("openA2ATaskStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// noA2ARetention keeps everything, so a test about durability is not also a test
// about eviction.
var noA2ARetention = a2aTaskRetention{}

func a2aPrincipal(id string) nexusauth.Principal { return nexusauth.Principal{ID: id} }

// seedA2ATask creates a task in one state, with an inbound message, in one call.
func seedA2ATask(t *testing.T, view *a2aPrincipalTasks, taskID, contextID string, state a2a.TaskState) {
	t.Helper()
	view.Create(taskID, contextID, a2a.NewTaskStatus(a2a.TaskStateSubmitted),
		a2aStoredMessage{MessageID: "m-" + taskID, Role: a2a.RoleUser, Text: "ask " + taskID})
	if state != a2a.TaskStateSubmitted {
		view.RecordStatus(taskID, a2a.NewTaskStatus(state))
	}
}

// ---- the structural half of principal scoping ----

// TestEveryTaskReadIsOwnerScoped is the structural half of the security
// constraint, checked against the SOURCE rather than against behaviour.
//
// The store's promise is that an unscoped read is not an expression this file
// can form. Behavioural tests can only show that the reads which exist today are
// scoped; this one shows that a read which is NOT scoped cannot be added without
// the test noticing, which is the property the story actually asks for.
//
// It is the JSONL analogue of nexus.io.a2a's TestEverySelectIsPrincipalScoped.
// There is no SQL predicate to look for here, so the invariant is expressed on
// the fold instead, and it is stricter: the map is keyed by OWNER FIRST, so a
// caller holding only a task id cannot form a lookup at all. What the test
// enforces is that the map is reachable through exactly one accessor, and that
// the accessor indexes it with the owner key.
func TestEveryTaskReadIsOwnerScoped(t *testing.T) {
	const (
		file        = "a2ataskstore.go"
		fold        = "byOwner"
		accessor    = "ownedLocked"
		ownerKeyRef = "ownerKey"
	)

	// The functions allowed to touch the fold without an owner key. Every one of
	// them is process-wide MAINTENANCE — folding a file on open, evicting under
	// retention, rewriting the file, counting — and none of them returns a record
	// to a caller. A request-serving read may never be added to this list.
	housekeeping := map[string]bool{
		"newA2ATaskStore": true,
		"foldLocked":      true,
		"countLocked":     true,
		"evictLocked":     true,
		"recordsLocked":   true,
		"settleOrphans":   true,
	}

	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var (
		inspected   int
		sawAccessor bool
	)
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		inspected++
		body := string(src[fn.Body.Pos()-1 : fn.Body.End()-1])
		if !strings.Contains(body, fold) {
			continue
		}
		if fn.Name.Name == accessor {
			sawAccessor = true
			// The accessor is the whole of the scoping: it must index the fold
			// with the owner key and with nothing else.
			if !strings.Contains(body, fold+"[t."+ownerKeyRef+"]") {
				t.Errorf("%s does not index %s by t.%s; the store's scoping is only as good as this one line:\n%s",
					accessor, fold, ownerKeyRef, body)
			}
			continue
		}
		if !housekeeping[fn.Name.Name] {
			t.Errorf("%s reaches into %s without an owner key, and is not declared housekeeping; "+
				"every request-serving read must go through %s", fn.Name.Name, fold, accessor)
		}
	}

	if !sawAccessor {
		// Guards the guard: a refactor that renamed or inlined the accessor would
		// silently make this test vacuous.
		t.Fatalf("no %s function was found in %s; the scan no longer sees the store's scoping", accessor, file)
	}
	if inspected < 15 {
		t.Fatalf("only %d functions were inspected in %s; the scan no longer sees the store", inspected, file)
	}
}

// TestA2ATaskSinkIsWriteOnly pins the other half: the surface a LIVE task holds
// cannot read at all, so the read pump translating a turn has no lookup within
// reach — let alone an unscoped one. Asserted against the INTERFACE's method
// set, since that is what a task is handed; the concrete type behind it has
// reads, and the point is that they are not reachable through this seam.
func TestA2ATaskSinkIsWriteOnly(t *testing.T) {
	sink := reflect.TypeOf((*a2aTaskSink)(nil)).Elem()
	if sink.NumMethod() == 0 {
		t.Fatal("a2aTaskSink has no methods; the scan below would be vacuous")
	}
	for i := range sink.NumMethod() {
		name := sink.Method(i).Name
		if !strings.HasPrefix(name, "Record") {
			t.Errorf("a2aTaskSink.%s is not a write; a live task must not be able to read tasks", name)
		}
	}
}

// TestAForeignTaskIsAbsentFromTheStore is the behavioural half, at the store's
// own surface: one principal's view reports another's task exactly as it reports
// one nobody ever created, and a listing never crosses the partition.
func TestAForeignTaskIsAbsentFromTheStore(t *testing.T) {
	store := openTestTaskStore(t, t.TempDir(), noA2ARetention)
	a, b := store.For(a2aPrincipal("partner-a"), "support"), store.For(a2aPrincipal("partner-b"), "support")

	seedA2ATask(t, b, "task-b", "ctx-b", a2a.TaskStateCompleted)

	if _, found := a.Get("task-b"); found {
		t.Fatal("partner-a can read partner-b's task")
	}
	if _, found := a.Get("task-nobody-minted"); found {
		t.Fatal("an id nobody minted was found")
	}
	if _, found := b.Get("task-b"); !found {
		t.Fatal("partner-b cannot read its own task")
	}

	recs, _, total := a.Query(a2aTaskQuery{})
	if len(recs) != 0 || total != 0 {
		t.Fatalf("partner-a listed %d task(s) (total %d), want none", len(recs), total)
	}

	// A write is scoped by the same lookup, so a foreign task cannot be moved
	// either — there is no separate ownership check to forget.
	a.RecordStatus("task-b", a2a.NewTaskStatus(a2a.TaskStateCanceled))
	rec, found := b.Get("task-b")
	if !found {
		t.Fatal("partner-b's task vanished")
	}
	if rec.State != string(a2a.TaskStateCompleted) {
		t.Errorf("partner-a moved partner-b's task to %s", rec.State)
	}
}

// ---- durability ----

// TestTasksSurviveABrokerRestart is the acceptance criterion taken literally:
// the store is closed, reopened from the file on disk, and asked for a task it
// recorded before the close.
func TestTasksSurviveABrokerRestart(t *testing.T) {
	dir := t.TempDir()
	owner := a2aPrincipal("partner-a")

	store := openTestTaskStore(t, dir, noA2ARetention)
	view := store.For(owner, "support")
	seedA2ATask(t, view, "task-1", "ctx-1", a2a.TaskStateSubmitted)
	view.RecordStatus("task-1", a2a.NewTaskStatus(a2a.TaskStateWorking))
	view.RecordArtifact("task-1", a2a.NewTextArtifact("task-1-response", "response", "the answer is 42"))
	view.RecordStatus("task-1", a2a.NewTaskStatus(a2a.TaskStateCompleted))
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openTestTaskStore(t, dir, noA2ARetention)
	rec, found := reopened.For(owner, "support").Get("task-1")
	if !found {
		t.Fatal("the task did not survive the restart")
	}
	if rec.State != string(a2a.TaskStateCompleted) {
		t.Errorf("state = %s, want COMPLETED", rec.State)
	}
	if len(rec.Artifacts) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(rec.Artifacts))
	}
	if text, _ := rec.Artifacts[0].Parts[0].TextValue(); text != "the answer is 42" {
		t.Errorf("artifact text = %q, want the recorded answer", text)
	}
	if len(rec.History) != 1 || rec.History[0].Text != "ask task-1" {
		t.Errorf("history = %+v, want the inbound message", rec.History)
	}
	// And the partition survives with it.
	if _, found := reopened.For(a2aPrincipal("partner-b"), "support").Get("task-1"); found {
		t.Error("a restart widened the owner partition")
	}
}

// TestATaskLeftInFlightIsFailedOnOpen covers the crash case: a broker that
// stopped mid-turn leaves a WORKING record nothing will ever move, so the store
// settles it when it opens.
//
// Leaving it as it stood would be worse in three separate ways — a client
// polling GetTask would see WORKING for ever, SubscribeToTask could only snapshot
// and hang up, and retention could never evict it because only terminal tasks
// are evictable.
func TestATaskLeftInFlightIsFailedOnOpen(t *testing.T) {
	dir := t.TempDir()
	owner := a2aPrincipal("partner-a")

	store := openTestTaskStore(t, dir, noA2ARetention)
	view := store.For(owner, "support")
	seedA2ATask(t, view, "task-live", "ctx-1", a2a.TaskStateWorking)
	seedA2ATask(t, view, "task-done", "ctx-1", a2a.TaskStateCompleted)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openTestTaskStore(t, dir, noA2ARetention)
	rec, found := reopened.For(owner, "support").Get("task-live")
	if !found {
		t.Fatal("the in-flight task was dropped rather than settled")
	}
	if rec.State != string(a2a.TaskStateFailed) {
		t.Errorf("state = %s, want FAILED", rec.State)
	}
	if rec.StatusMessage == nil {
		t.Fatal("the settled task carries no status message saying what happened")
	}
	if text, _ := rec.StatusMessage.Parts[0].TextValue(); !strings.Contains(text, "broker stopped") {
		t.Errorf("status message = %q, want it to say the broker stopped", text)
	}
	// A task that had already finished is untouched.
	done, _ := reopened.For(owner, "support").Get("task-done")
	if done.State != string(a2a.TaskStateCompleted) {
		t.Errorf("a completed task was rewritten to %s", done.State)
	}
}

// TestATornTrailingRecordIsSkipped: a broker killed mid-write leaves a final
// line with no newline. Every earlier record must still load, because a full
// stop over one torn line would cost every task in the file.
func TestATornTrailingRecordIsSkipped(t *testing.T) {
	dir := t.TempDir()
	owner := a2aPrincipal("partner-a")

	store := openTestTaskStore(t, dir, noA2ARetention)
	seedA2ATask(t, store.For(owner, "support"), "task-1", "ctx-1", a2a.TaskStateCompleted)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := filepath.Join(dir, a2aTaskStoreName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	// A valid record, cut off mid-way, with no terminating newline.
	if err := os.WriteFile(path, append(data, []byte(`{"task_id":"task-2","state":"TASK_ST`)...), 0o600); err != nil {
		t.Fatalf("write torn store: %v", err)
	}

	reopened := openTestTaskStore(t, dir, noA2ARetention)
	if _, found := reopened.For(owner, "support").Get("task-1"); !found {
		t.Error("a torn trailing record cost the records before it")
	}
	if _, found := reopened.For(owner, "support").Get("task-2"); found {
		t.Error("a torn record was loaded")
	}
}

// TestAMemoryOnlyStoreStillAnswers pins the state_dir-less mode. A broker with
// no state_dir must still answer every read for the life of the process — the
// operations refusing would be a far worse degradation than losing them across a
// restart, which is what that broker has already chosen for leases.
func TestAMemoryOnlyStoreStillAnswers(t *testing.T) {
	store := openTestTaskStore(t, "", noA2ARetention)
	if store.path != "" {
		t.Fatalf("a store with no state_dir opened a file at %q", store.path)
	}
	view := store.For(a2aPrincipal("partner-a"), "support")
	seedA2ATask(t, view, "task-1", "ctx-1", a2a.TaskStateCompleted)
	if _, found := view.Get("task-1"); !found {
		t.Fatal("a memory-only store does not answer a read")
	}
}

// TestTheStoreCompactsInPlace: the file is append-only between rewrites, so a
// task written many times leaves many lines — and a rewrite must reduce them to
// one per task without changing a single answer.
func TestTheStoreCompactsInPlace(t *testing.T) {
	dir := t.TempDir()
	owner := a2aPrincipal("partner-a")
	store := openTestTaskStore(t, dir, noA2ARetention)
	view := store.For(owner, "support")

	seedA2ATask(t, view, "task-1", "ctx-1", a2a.TaskStateSubmitted)
	for range a2aTaskCompactEvery + 10 {
		view.RecordStatus("task-1", a2a.NewTaskStatus(a2a.TaskStateWorking))
	}
	view.RecordStatus("task-1", a2a.NewTaskStatus(a2a.TaskStateCompleted))

	data, err := os.ReadFile(filepath.Join(dir, a2aTaskStoreName))
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	lines := strings.Count(string(data), "\n")
	if lines > 32 {
		t.Errorf("the store holds %d lines after %d writes; compaction is not running",
			lines, a2aTaskCompactEvery+12)
	}
	rec, found := view.Get("task-1")
	if !found || rec.State != string(a2a.TaskStateCompleted) {
		t.Fatalf("compaction lost the task's final state: %+v (found=%v)", rec, found)
	}
}

// ---- retention ----

// TestRetentionEvictsByTTL uses an injected clock, so the test needs no sleeps
// and the assertion is about the policy rather than about timing.
func TestRetentionEvictsByTTL(t *testing.T) {
	store := openTestTaskStore(t, t.TempDir(), a2aTaskRetention{ttl: time.Hour})
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	view := store.For(a2aPrincipal("partner-a"), "support")

	seedA2ATask(t, view, "task-old", "ctx-1", a2a.TaskStateCompleted)

	// Two hours later, the old task is past its TTL and a new task's creation is
	// what triggers the sweep — the store is bounded by the event that grows it.
	now = now.Add(2 * time.Hour)
	seedA2ATask(t, view, "task-new", "ctx-1", a2a.TaskStateCompleted)

	if _, found := view.Get("task-old"); found {
		t.Error("a task past its TTL was kept")
	}
	if _, found := view.Get("task-new"); !found {
		t.Error("the task that triggered the sweep was evicted by it")
	}
}

// TestRetentionKeepsALiveTaskPastTheCap: only TERMINAL tasks are evictable. A
// live task is the one thing a client is certain to ask about — it is the task it
// is streaming right now — so dropping it to satisfy a cap would turn a retention
// policy into a correctness bug. It still COUNTS against the cap, so a wedged
// broker shows up as pressure on retention rather than exempting itself.
func TestRetentionKeepsALiveTaskPastTheCap(t *testing.T) {
	store := openTestTaskStore(t, t.TempDir(), a2aTaskRetention{maxPerContext: 2})
	view := store.For(a2aPrincipal("partner-a"), "support")

	seedA2ATask(t, view, "task-1", "ctx-1", a2a.TaskStateWorking)
	seedA2ATask(t, view, "task-2", "ctx-1", a2a.TaskStateCompleted)
	seedA2ATask(t, view, "task-3", "ctx-1", a2a.TaskStateCompleted)
	seedA2ATask(t, view, "task-4", "ctx-1", a2a.TaskStateCompleted)

	if _, found := view.Get("task-1"); !found {
		t.Error("a non-terminal task was evicted by the per-context cap")
	}
	if _, found := view.Get("task-2"); found {
		t.Error("the oldest terminal task over the cap was kept")
	}
	if _, found := view.Get("task-4"); !found {
		t.Error("the newest task was evicted")
	}
}

// TestRetentionCapIsPerContextAndPerOwner: one context filling its quota must
// not push another context's tasks out, and one principal's traffic must not
// evict another's — an eviction channel is still a channel.
func TestRetentionCapIsPerContextAndPerOwner(t *testing.T) {
	store := openTestTaskStore(t, t.TempDir(), a2aTaskRetention{maxPerContext: 1})
	a := store.For(a2aPrincipal("partner-a"), "support")
	b := store.For(a2aPrincipal("partner-b"), "support")

	seedA2ATask(t, a, "a-ctx1-1", "ctx-1", a2a.TaskStateCompleted)
	seedA2ATask(t, a, "a-ctx2-1", "ctx-2", a2a.TaskStateCompleted)
	seedA2ATask(t, b, "b-ctx1-1", "ctx-1", a2a.TaskStateCompleted)
	// Overflow ctx-1 for partner-a only.
	seedA2ATask(t, a, "a-ctx1-2", "ctx-1", a2a.TaskStateCompleted)

	for _, c := range []struct {
		view *a2aPrincipalTasks
		id   string
		want bool
	}{
		{a, "a-ctx1-1", false},
		{a, "a-ctx1-2", true},
		{a, "a-ctx2-1", true},
		{b, "b-ctx1-1", true},
	} {
		if _, found := c.view.Get(c.id); found != c.want {
			t.Errorf("%s present = %v, want %v", c.id, found, c.want)
		}
	}
}

// TestRetentionSurvivesARestart: eviction runs on open too, so a broker that was
// down while tasks aged out does not carry them back in — and a record that was
// evicted from the fold but not yet rewritten out of the file cannot come back
// from the dead.
func TestRetentionSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	owner := a2aPrincipal("partner-a")
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	store := openTestTaskStore(t, dir, noA2ARetention)
	store.now = func() time.Time { return now }
	seedA2ATask(t, store.For(owner, "support"), "task-old", "ctx-1", a2a.TaskStateCompleted)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopened with a TTL, long after the record was written.
	reopened, err := openA2ATaskStore(testLogger(), Config{
		StateDir:         dir,
		A2ATaskRetention: a2aTaskRetention{ttl: time.Nanosecond},
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	if _, found := reopened.For(owner, "support").Get("task-old"); found {
		t.Error("a task past its TTL was carried back in across a restart")
	}
}

// ---- bounded growth ----

// TestStoredTextIsTruncated pins the store's real growth term. A turn's answer
// is unbounded and the record is rewritten on each transition, so an uncapped
// answer would be written several times at whatever size it happened to be.
//
// The cut is marked rather than silent, so a client reading a task back can tell
// it is holding an excerpt — and the STREAM is never truncated, only the stored
// copy, so a client that was attached when the turn ran got the whole thing.
func TestStoredTextIsTruncated(t *testing.T) {
	store := openTestTaskStore(t, t.TempDir(), noA2ARetention)
	view := store.For(a2aPrincipal("partner-a"), "support")
	huge := strings.Repeat("x", 4*maxA2AStoredTextBytes)

	seedA2ATask(t, view, "task-1", "ctx-1", a2a.TaskStateSubmitted)
	view.RecordArtifact("task-1", a2a.NewTextArtifact("task-1-response", "response", huge))
	view.RecordMessage("task-1", a2aStoredMessage{Role: a2a.RoleUser, Text: huge})

	rec, found := view.Get("task-1")
	if !found {
		t.Fatal("the task vanished")
	}
	text, _ := rec.Artifacts[0].Parts[0].TextValue()
	if len(text) > maxA2AStoredTextBytes+len(a2aTruncationNotice) {
		t.Errorf("stored artifact text is %d bytes, want at most %d",
			len(text), maxA2AStoredTextBytes+len(a2aTruncationNotice))
	}
	if !strings.HasSuffix(text, a2aTruncationNotice) {
		t.Error("the truncated artifact does not say it was truncated")
	}
	for _, msg := range rec.History {
		if len(msg.Text) > maxA2AStoredTextBytes+len(a2aTruncationNotice) {
			t.Errorf("stored message text is %d bytes, want at most %d",
				len(msg.Text), maxA2AStoredTextBytes+len(a2aTruncationNotice))
		}
	}
}

// TestStoreGrowthIsBoundedOnAnArtifactHeavyTurn is the acceptance criterion
// taken literally, and it is asserted end to end rather than at the store's own
// surface: a real turn is driven through the ingress with a real durable store
// behind it, and the FILE ON DISK is measured.
//
// The turn is deliberately the worst shape this mapping can produce — thousands
// of streamed deltas adding up to megabytes, then an answer larger than the
// store's text cap. The two mechanisms that bound it are both exercised: a delta
// emits no frame at all, so nothing is written for any of them, and the answer
// that finally becomes an artifact is truncated to the cap.
func TestStoreGrowthIsBoundedOnAnArtifactHeavyTurn(t *testing.T) {
	const (
		deltas     = 2000
		deltaBytes = 1024
	)
	dir := t.TempDir()
	cfg := a2aTestConfig(t, "")
	cfg.StateDir = dir

	server, err := NewA2AServer(testLogger(), cfg)
	if err != nil {
		t.Fatalf("NewA2AServer: %v", err)
	}
	store := openTestTaskStore(t, dir, cfg.A2ATaskRetention)
	server.useTaskStore(store)

	instance := &conformInstance{}
	server.useLeaseProvider(&conformLeaseProvider{instance: instance})

	task, sub := startConformTask(t, server, instance, "summarize the repository")
	defer task.detach(sub)

	chunk := strings.Repeat("y", deltaBytes)
	for range deltas {
		instance.deliver(brokerIOMessage{Type: ioTypeStreamDelta, Content: chunk, TurnID: "t1"})
	}
	instance.deliver(brokerIOMessage{Type: ioTypeStreamEnd, TurnID: "t1"})
	instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: ioStateIdle})

	if state := task.snapshotTask().Status.State; state != a2a.TaskStateCompleted {
		t.Fatalf("the turn ended at %s, want COMPLETED", state)
	}
	// The client got the WHOLE answer: only the stored copy is capped.
	if got := len(task.answerText()); got != deltas*deltaBytes {
		t.Errorf("the streamed answer is %d bytes, want the full %d; the stream must not be truncated",
			got, deltas*deltaBytes)
	}

	info, err := os.Stat(filepath.Join(dir, a2aTaskStoreName))
	if err != nil {
		t.Fatalf("stat store: %v", err)
	}
	// Generous, and still two orders of magnitude below the ~2 MiB the turn
	// produced: the point is that the file scales with the SHAPE of a turn (a
	// handful of transitions) and not with its VOLUME.
	const bound = 128 << 10
	if info.Size() > bound {
		t.Errorf("the task store grew to %d bytes on one turn, want at most %d", info.Size(), bound)
	}
	t.Logf("one %d-byte turn produced a %d-byte store file", deltas*deltaBytes, info.Size())
}

// TestTheGlobalTaskCeilingHolds: the per-context cap alone bounds nothing a
// broker cares about, because a broker fronts an unbounded number of contexts.
// The global ceiling is what makes the store's footprint statable.
func TestTheGlobalTaskCeilingHolds(t *testing.T) {
	store := openTestTaskStore(t, "", noA2ARetention)
	view := store.For(a2aPrincipal("partner-a"), "support")
	// One context each, so the per-context cap cannot be what evicts.
	for i := range maxA2ATaskRecords + 50 {
		id := fmt.Sprintf("task-%d", i)
		seedA2ATask(t, view, id, fmt.Sprintf("ctx-%d", i), a2a.TaskStateCompleted)
	}

	store.mu.Lock()
	held := store.countLocked()
	store.mu.Unlock()
	if held > maxA2ATaskRecords {
		t.Errorf("the store holds %d records, want at most %d", held, maxA2ATaskRecords)
	}
	// The newest survive; the oldest are the ones least likely to be read back.
	if _, found := view.Get(fmt.Sprintf("task-%d", maxA2ATaskRecords+49)); !found {
		t.Error("the newest task was evicted")
	}
	if _, found := view.Get("task-0"); found {
		t.Error("the oldest task was kept past the ceiling")
	}
}

// ---- listing ----

// TestListTasksPaginatesWithAKeysetCursor walks every page and checks the walk
// is complete, ordered and free of repeats — the three ways a cursor goes wrong.
func TestListTasksPaginatesWithAKeysetCursor(t *testing.T) {
	store := openTestTaskStore(t, t.TempDir(), noA2ARetention)
	view := store.For(a2aPrincipal("partner-a"), "support")

	var want []string
	for i := range 5 {
		id := fmt.Sprintf("task-%d", i)
		seedA2ATask(t, view, id, "ctx-1", a2a.TaskStateCompleted)
		// Newest first, so the expectation is the reverse of creation order.
		want = append([]string{id}, want...)
	}

	var (
		got    []string
		cursor a2aListCursor
		pages  int
	)
	for {
		pages++
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		recs, next, total := view.Query(a2aTaskQuery{limit: 2, after: cursor})
		if total != 5 {
			t.Errorf("totalSize = %d, want 5 on every page", total)
		}
		for _, rec := range recs {
			got = append(got, rec.TaskID)
		}
		if !next.set {
			break
		}
		cursor = next
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the walk produced %v, want %v", got, want)
	}
}

// TestListTasksFilters covers the three narrowing filters the specification
// defines, each of which may only ever REDUCE what a principal can already see.
func TestListTasksFilters(t *testing.T) {
	store := openTestTaskStore(t, t.TempDir(), noA2ARetention)
	base := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return base }
	view := store.For(a2aPrincipal("partner-a"), "support")

	// The statuses carry their OWN timestamps, because state_at records when a
	// status was observed rather than when the store happened to write it — which
	// is exactly what the specification's statusTimestampAfter filter selects on.
	// Creation order (and so the listing order) still comes from the store clock,
	// tie-broken by sequence.
	inbound := func(id string) a2aStoredMessage {
		return a2aStoredMessage{MessageID: "m-" + id, Role: a2a.RoleUser, Text: "ask " + id}
	}
	view.Create("task-a", "ctx-1", a2a.NewTaskStatusAt(a2a.TaskStateCompleted, base), inbound("task-a"))
	view.Create("task-b", "ctx-2", a2a.NewTaskStatusAt(a2a.TaskStateCompleted, base), inbound("task-b"))
	later := base.Add(time.Hour)
	view.Create("task-c", "ctx-1", a2a.NewTaskStatusAt(a2a.TaskStateFailed, later), inbound("task-c"))

	for _, c := range []struct {
		name string
		q    a2aTaskQuery
		want []string
	}{
		{"by context", a2aTaskQuery{contextID: "ctx-1"}, []string{"task-c", "task-a"}},
		{"by state", a2aTaskQuery{state: a2a.TaskStateFailed}, []string{"task-c"}},
		{"by status timestamp", a2aTaskQuery{changedAfter: later}, []string{"task-c"}},
		{"combined", a2aTaskQuery{contextID: "ctx-1", state: a2a.TaskStateCompleted}, []string{"task-a"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			recs, _, total := view.Query(c.q)
			var got []string
			for _, rec := range recs {
				got = append(got, rec.TaskID)
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("listed %v, want %v", got, c.want)
			}
			if total != len(c.want) {
				t.Errorf("totalSize = %d, want %d: the count must use the same filters", total, len(c.want))
			}
		})
	}
}

// TestHistoryLengthKeepsTheMostRecentMessages: "historyLength" means the LAST n
// exchanges everywhere the specification uses it, and zero means omit history
// rather than "no limit" — which is why the field is a pointer.
func TestHistoryLengthKeepsTheMostRecentMessages(t *testing.T) {
	store := openTestTaskStore(t, "", noA2ARetention)
	view := store.For(a2aPrincipal("partner-a"), "support")
	view.Create("task-1", "ctx-1", a2a.NewTaskStatus(a2a.TaskStateSubmitted),
		a2aStoredMessage{MessageID: "m1", Role: a2a.RoleUser, Text: "first"})
	view.RecordMessage("task-1", a2aStoredMessage{MessageID: "m2", Role: a2a.RoleUser, Text: "second"})
	view.RecordMessage("task-1", a2aStoredMessage{MessageID: "m3", Role: a2a.RoleUser, Text: "third"})

	rec, _ := view.Get("task-1")
	for _, c := range []struct {
		name  string
		limit *int
		want  []string
	}{
		{"no limit", nil, []string{"first", "second", "third"}},
		{"last two", ptr(2), []string{"second", "third"}},
		{"zero omits history", ptr(0), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			msgs := rec.task(a2aRenderOptions{historyLength: c.limit}).History
			var got []string
			for _, m := range msgs {
				text, _ := m.Parts[0].TextValue()
				got = append(got, text)
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("history = %v, want %v", got, c.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
