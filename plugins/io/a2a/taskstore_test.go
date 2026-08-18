package a2a

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/storage"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// --- helpers ---

// openStoreAt opens a task store over dir and returns it with the func that
// releases the underlying SQLite handles. Calling that func and opening again
// over the same dir is a real restart: the second store reads the files the
// first one left behind, with no shared connection between them.
func openStoreAt(t *testing.T, dir string, policy retention) (*taskStore, func()) {
	t.Helper()
	opener, closeFn := storageAt(t, dir)
	st, err := opener(storage.ScopeSession)
	if err != nil {
		t.Fatalf("open session storage: %v", err)
	}
	store, err := openTaskStore(st, policy, discardLogger())
	if err != nil {
		t.Fatalf("openTaskStore: %v", err)
	}
	return store, closeFn
}

// newStore opens a task store over a fresh temp dir.
func newStore(t *testing.T, policy retention) *taskStore {
	t.Helper()
	store, _ := openStoreAt(t, t.TempDir(), policy)
	return store
}

// noRetention keeps everything, for the tests that are not about eviction.
var noRetention = retention{}

func principal(id string) nexusauth.Principal { return nexusauth.Principal{ID: id} }

// submitted is the opening status a task is created with.
func submitted() a2a.TaskStatus { return a2a.NewTaskStatus(a2a.TaskStateSubmitted) }

func userRef(text string) messageRef {
	return messageRef{MessageID: "m-" + text, Role: a2a.RoleUser, Text: text}
}

// mustCreate creates a task or fails the test.
func mustCreate(t *testing.T, view *principalTasks, taskID, contextID string, status a2a.TaskStatus) {
	t.Helper()
	if err := view.Create(taskID, contextID, status, userRef(taskID)); err != nil {
		t.Fatalf("Create(%s): %v", taskID, err)
	}
}

// taskIDsOf renders a record slice as its ids, for order-sensitive assertions.
func taskIDsOf(recs []taskRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.TaskID)
	}
	return out
}

// taskFromRPC decodes the Task a blocking SendMessage returned.
func taskFromRPC(t *testing.T, body []byte) a2a.Task {
	t.Helper()
	resp := rpcResponse(t, body)
	if resp.Error != nil {
		t.Fatalf("error response: %+v", resp.Error)
	}
	var result a2a.SendMessageResponse
	if err := resp.DecodeResult(&result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Task == nil {
		t.Fatal("SendMessage returned no task")
	}
	return *result.Task
}

// --- the structural scoping guarantee ---

// TestEverySelectIsPrincipalScoped is the structural half of the security
// constraint, checked against the SOURCE rather than against behaviour.
//
// The store's promise is that an unscoped read is not an expression this
// package can form. Behavioural tests can only show that the reads which exist
// today are scoped; this one shows that a read which is NOT scoped cannot be
// added without the test noticing, which is the property the story actually
// asks for. Every SQL literal that selects from a task table must name
// principal_id.
func TestEverySelectIsPrincipalScoped(t *testing.T) {
	src, err := os.ReadFile("taskstore.go")
	if err != nil {
		t.Fatalf("read taskstore.go: %v", err)
	}
	// SQL in this file is always a raw string literal, so splitting on the
	// backquote yields the statements at the odd indices.
	segments := strings.Split(string(src), "`")
	checked := 0
	for i := 1; i < len(segments); i += 2 {
		stmt := segments[i]
		if !strings.Contains(stmt, "SELECT") || !strings.Contains(stmt, "FROM task") {
			continue
		}
		checked++
		if !strings.Contains(stmt, "principal_id") {
			t.Errorf("an unscoped read exists in taskstore.go:\n%s", stmt)
		}
	}
	if checked < 5 {
		// Guards the guard: a refactor that moved the SQL out of raw literals
		// would silently make this test vacuous.
		t.Fatalf("only %d SQL reads were inspected; the scan no longer sees the store's statements", checked)
	}
}

// TestTaskSinkIsWriteOnly pins the other half: the surface a live run holds
// cannot read at all, so the hot path has no lookup within reach at all — let
// alone an unscoped one. Asserted against the INTERFACE's method set, since
// that is what a run is handed; the concrete type behind it has reads, and the
// point is that they are not reachable through this seam.
func TestTaskSinkIsWriteOnly(t *testing.T) {
	sink := reflect.TypeOf((*taskSink)(nil)).Elem()
	if sink.NumMethod() == 0 {
		t.Fatal("taskSink has no methods; the scan below would be vacuous")
	}
	for i := range sink.NumMethod() {
		name := sink.Method(i).Name
		if !strings.HasPrefix(name, "Record") {
			t.Errorf("taskSink.%s is not a write; a run must not be able to read tasks", name)
		}
	}
}

// --- durability ---

// TestTasksSurviveARestart is the acceptance criterion taken literally: the
// store is closed, reopened from the files on disk, and asked for a task it
// recorded before the close.
func TestTasksSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	owner := principal("partner-a")

	store, closeFn := openStoreAt(t, dir, noRetention)
	view := store.For(owner)
	mustCreate(t, view, "task-1", "ctx-1", submitted())
	if err := view.RecordStatus("task-1", a2a.NewTaskStatus(a2a.TaskStateWorking)); err != nil {
		t.Fatalf("RecordStatus working: %v", err)
	}
	if err := view.RecordArtifact("task-1", a2a.NewTextArtifact("art-1", "response", "the answer")); err != nil {
		t.Fatalf("RecordArtifact: %v", err)
	}
	if err := view.RecordMessage("task-1", messageRef{MessageID: "m-2", Role: a2a.RoleAgent, Text: "the answer"}); err != nil {
		t.Fatalf("RecordMessage: %v", err)
	}
	if err := view.RecordStatus("task-1", a2a.NewTaskStatus(a2a.TaskStateCompleted)); err != nil {
		t.Fatalf("RecordStatus completed: %v", err)
	}
	closeFn()

	// A second process, over the same files.
	restarted, _ := openStoreAt(t, dir, noRetention)
	rec, found, err := restarted.For(owner).Get("task-1")
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if !found {
		t.Fatal("the task did not survive the restart")
	}
	if rec.ContextID != "ctx-1" {
		t.Errorf("contextId = %q, want ctx-1", rec.ContextID)
	}
	if rec.Status.State != a2a.TaskStateCompleted {
		t.Errorf("state = %q, want COMPLETED", rec.Status.State)
	}
	if rec.Status.Timestamp == nil || rec.Status.Timestamp.Time.IsZero() {
		t.Error("the current status came back without a timestamp")
	}
	wantHistory := []a2a.TaskState{
		a2a.TaskStateSubmitted, a2a.TaskStateWorking, a2a.TaskStateCompleted,
	}
	if len(rec.StatusHistory) != len(wantHistory) {
		t.Fatalf("status history = %v, want %v", rec.StatusHistory, wantHistory)
	}
	for i, want := range wantHistory {
		if rec.StatusHistory[i].State != want {
			t.Errorf("status history[%d] = %q, want %q", i, rec.StatusHistory[i].State, want)
		}
	}
	if len(rec.Artifacts) != 1 || rec.Artifacts[0].ArtifactID != "art-1" {
		t.Errorf("artifacts = %+v, want one artifact art-1", rec.Artifacts)
	}
	if len(rec.Messages) != 2 || rec.Messages[0].Role != a2a.RoleUser || rec.Messages[1].Role != a2a.RoleAgent {
		t.Errorf("message references = %+v, want a user reference then an agent one", rec.Messages)
	}
	if rec.Principal.ID != owner.ID {
		t.Errorf("principal = %q, want %q", rec.Principal.ID, owner.ID)
	}
	if rec.CreatedAt.IsZero() || rec.UpdatedAt.IsZero() {
		t.Error("timestamps did not survive the restart")
	}
}

// TestPrincipalIsRecordedInFull pins that the record carries the identity, not
// just its id: Tenant, Scopes and Claims are audit material and are what a
// later authorization decision would read.
func TestPrincipalIsRecordedInFull(t *testing.T) {
	dir := t.TempDir()
	owner := nexusauth.Principal{
		ID:     "partner-a",
		Tenant: "acme",
		Scopes: []string{"a2a.write"},
		Claims: map[string]any{"sub": "partner-a"},
	}
	store, closeFn := openStoreAt(t, dir, noRetention)
	mustCreate(t, store.For(owner), "task-1", "ctx-1", submitted())
	closeFn()

	restarted, _ := openStoreAt(t, dir, noRetention)
	rec, found, err := restarted.For(owner).Get("task-1")
	if err != nil || !found {
		t.Fatalf("Get: %v (found=%v)", err, found)
	}
	if rec.Principal.Tenant != "acme" {
		t.Errorf("tenant = %q, want acme", rec.Principal.Tenant)
	}
	if !rec.Principal.HasScope("a2a.write") {
		t.Errorf("scopes = %v, want a2a.write", rec.Principal.Scopes)
	}
	if rec.Principal.Claims["sub"] != "partner-a" {
		t.Errorf("claims = %v, want sub=partner-a", rec.Principal.Claims)
	}
}

// --- principal isolation ---

// TestPrincipalIsolation walks every read and every write on the store and
// asserts that a second principal reaches none of the first one's task.
func TestPrincipalIsolation(t *testing.T) {
	store := newStore(t, noRetention)
	a := store.For(principal("partner-a"))
	b := store.For(principal("partner-b"))

	mustCreate(t, a, "task-1", "ctx-shared", submitted())
	if err := a.RecordArtifact("task-1", a2a.NewTextArtifact("art-1", "response", "a's answer")); err != nil {
		t.Fatalf("RecordArtifact: %v", err)
	}

	// Reads.
	if _, found, err := b.Get("task-1"); err != nil || found {
		t.Errorf("b.Get found another principal's task (found=%v, err=%v)", found, err)
	}
	recs, err := b.List("", 0)
	if err != nil {
		t.Fatalf("b.List: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("b.List returned %v, want nothing", taskIDsOf(recs))
	}
	// Naming the context explicitly must not widen the scope either.
	recs, err = b.List("ctx-shared", 0)
	if err != nil {
		t.Fatalf("b.List(ctx): %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("b.List(ctx-shared) returned %v, want nothing", taskIDsOf(recs))
	}

	// The child-table readers are scoped on their own terms, not on Get having
	// checked first.
	if hist, err := b.statusHistory("task-1"); err != nil || len(hist) != 0 {
		t.Errorf("b.statusHistory = %v (err=%v), want nothing", hist, err)
	}
	if arts, err := b.artifacts("task-1"); err != nil || len(arts) != 0 {
		t.Errorf("b.artifacts = %v (err=%v), want nothing", arts, err)
	}
	if msgs, err := b.messages("task-1"); err != nil || len(msgs) != 0 {
		t.Errorf("b.messages = %v (err=%v), want nothing", msgs, err)
	}

	// Writes.
	if err := b.RecordStatus("task-1", a2a.NewTaskStatus(a2a.TaskStateCanceled)); err == nil {
		t.Error("b moved another principal's task to CANCELED")
	}
	if err := b.RecordArtifact("task-1", a2a.NewTextArtifact("art-2", "x", "injected")); err == nil {
		t.Error("b appended an artifact to another principal's task")
	}
	if err := b.RecordMessage("task-1", userRef("injected")); err == nil {
		t.Error("b appended a message reference to another principal's task")
	}

	// And nothing b attempted left a mark.
	rec, found, err := a.Get("task-1")
	if err != nil || !found {
		t.Fatalf("a.Get: %v (found=%v)", err, found)
	}
	if rec.Status.State != a2a.TaskStateSubmitted {
		t.Errorf("state = %q, want SUBMITTED — a foreign write changed it", rec.Status.State)
	}
	if len(rec.StatusHistory) != 1 {
		t.Errorf("status history = %v, want the opening status only", rec.StatusHistory)
	}
	if len(rec.Artifacts) != 1 {
		t.Errorf("artifacts = %+v, want a's artifact only", rec.Artifacts)
	}
	if len(rec.Messages) != 1 {
		t.Errorf("message references = %+v, want a's inbound reference only", rec.Messages)
	}
}

// TestTwoPrincipalsMayHoldTheSameContext pins that scoping partitions the store
// rather than the conversation: the same contextId under two principals is two
// independent task sets.
func TestTwoPrincipalsMayHoldTheSameContext(t *testing.T) {
	store := newStore(t, noRetention)
	a := store.For(principal("partner-a"))
	b := store.For(principal("partner-b"))

	mustCreate(t, a, "task-a", "ctx-1", submitted())
	mustCreate(t, b, "task-b", "ctx-1", submitted())

	for name, tc := range map[string]struct {
		view *principalTasks
		want string
	}{
		"partner-a": {a, "task-a"},
		"partner-b": {b, "task-b"},
	} {
		recs, err := tc.view.List("ctx-1", 0)
		if err != nil {
			t.Fatalf("%s List: %v", name, err)
		}
		if got := taskIDsOf(recs); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s List = %v, want [%s]", name, got, tc.want)
		}
	}
}

// TestUnauthenticatedCallersShareOneBucket pins the sentinel partition key. An
// empty Principal.ID would be a poor partition key, so it maps to a value no
// credential this codebase accepts can produce.
func TestUnauthenticatedCallersShareOneBucket(t *testing.T) {
	store := newStore(t, noRetention)
	anon := store.For(nexusauth.Principal{})
	if anon.key != anonymousPrincipal {
		t.Errorf("anonymous key = %q, want %q", anon.key, anonymousPrincipal)
	}
	mustCreate(t, anon, "task-1", "ctx-1", submitted())

	// A second anonymous view sees it: with the chain disabled every caller is
	// anonymous, and partitioning them apart would lose the task on the next
	// request.
	if _, found, err := store.For(nexusauth.Principal{}).Get("task-1"); err != nil || !found {
		t.Errorf("a second unauthenticated view could not reach the task (found=%v, err=%v)", found, err)
	}
	// An authenticated caller still cannot.
	if _, found, _ := store.For(principal("partner-a")).Get("task-1"); found {
		t.Error("an authenticated caller reached an anonymous task")
	}
}

// --- retention ---

// TestRetentionTTLEvictsTerminalTasks pins age-based eviction, including the
// rule that a live task is never dropped by it.
func TestRetentionTTLEvictsTerminalTasks(t *testing.T) {
	store := newStore(t, retention{ttl: time.Hour})
	clock := time.Now()
	store.now = func() time.Time { return clock }
	view := store.For(principal("partner-a"))

	mustCreate(t, view, "task-done", "ctx-1", submitted())
	if err := view.RecordStatus("task-done", a2a.NewTaskStatus(a2a.TaskStateCompleted)); err != nil {
		t.Fatalf("RecordStatus: %v", err)
	}
	mustCreate(t, view, "task-live", "ctx-1", submitted())

	clock = clock.Add(2 * time.Hour)
	if err := store.evict(); err != nil {
		t.Fatalf("evict: %v", err)
	}

	if _, found, _ := view.Get("task-done"); found {
		t.Error("a terminal task older than the TTL was retained")
	}
	if _, found, _ := view.Get("task-live"); !found {
		t.Error("a live task was evicted by the TTL; only terminal tasks are evictable")
	}
}

// TestRetentionRunsOnOpen pins that a process which was down while tasks aged
// out does not carry them back in.
func TestRetentionRunsOnOpen(t *testing.T) {
	dir := t.TempDir()
	owner := principal("partner-a")

	store, closeFn := openStoreAt(t, dir, retention{ttl: time.Hour})
	clock := time.Now()
	store.now = func() time.Time { return clock }
	view := store.For(owner)
	mustCreate(t, view, "task-done", "ctx-1", submitted())
	if err := view.RecordStatus("task-done", a2a.NewTaskStatus(a2a.TaskStateCompleted)); err != nil {
		t.Fatalf("RecordStatus: %v", err)
	}
	closeFn()

	// The reopened store's clock is the real one, hours later than the stored
	// timestamps only if we say so — so open it with a TTL short enough that
	// the recorded task is already expired.
	restarted, _ := openStoreAt(t, dir, retention{ttl: time.Nanosecond})
	if _, found, _ := restarted.For(owner).Get("task-done"); found {
		t.Error("an expired task survived a restart; retention did not run on open")
	}
}

// TestRetentionCapsTasksPerContext pins the per-context cap and the two rules
// that make it safe: the newest survive, and a live task counts against the cap
// without being evictable by it.
func TestRetentionCapsTasksPerContext(t *testing.T) {
	store := newStore(t, retention{maxPerContext: 2})
	clock := time.Now()
	store.now = func() time.Time { return clock }
	view := store.For(principal("partner-a"))

	tick := func() { clock = clock.Add(time.Second) }

	// Oldest first: t1 terminal, t2 live, t3 and t4 terminal.
	mustCreate(t, view, "t1", "ctx-1", a2a.NewTaskStatus(a2a.TaskStateCompleted))
	tick()
	mustCreate(t, view, "t2", "ctx-1", submitted())
	tick()
	mustCreate(t, view, "t3", "ctx-1", a2a.NewTaskStatus(a2a.TaskStateCompleted))
	tick()
	mustCreate(t, view, "t4", "ctx-1", a2a.NewTaskStatus(a2a.TaskStateCompleted))

	for id, want := range map[string]bool{"t1": false, "t2": true, "t3": true, "t4": true} {
		_, found, err := view.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if found != want {
			t.Errorf("Get(%s) found = %v, want %v", id, found, want)
		}
	}
}

// TestRetentionCapIsPerContextAndPerPrincipal pins that the cap partitions the
// way its name says: one context cannot evict another's tasks, and one
// principal's traffic cannot evict another principal's — an eviction channel is
// still a channel.
func TestRetentionCapIsPerContextAndPerPrincipal(t *testing.T) {
	store := newStore(t, retention{maxPerContext: 1})
	clock := time.Now()
	store.now = func() time.Time { return clock }
	a := store.For(principal("partner-a"))
	b := store.For(principal("partner-b"))

	done := a2a.NewTaskStatus(a2a.TaskStateCompleted)
	mustCreate(t, a, "a-ctx1-old", "ctx-1", done)
	mustCreate(t, a, "a-ctx2", "ctx-2", done)
	mustCreate(t, b, "b-ctx1", "ctx-1", done)
	clock = clock.Add(time.Second)
	mustCreate(t, a, "a-ctx1-new", "ctx-1", done)

	cases := map[string]struct {
		view  *principalTasks
		id    string
		found bool
	}{
		"the overflowing context evicts its own oldest": {a, "a-ctx1-old", false},
		"the newest in that context survives":           {a, "a-ctx1-new", true},
		"another context of the same principal":         {a, "a-ctx2", true},
		"the same context under another principal":      {b, "b-ctx1", true},
	}
	for name, c := range cases {
		if _, found, err := c.view.Get(c.id); err != nil || found != c.found {
			t.Errorf("%s: Get(%s) found = %v (err=%v), want %v", name, c.id, found, err, c.found)
		}
	}
}

// TestEvictionRemovesChildRows pins the cascade. Retention is one DELETE
// against tasks; if the foreign keys were not enforced, the status history,
// artifacts and message references of an evicted task would be orphaned and the
// store would grow without bound in exactly the tables retention exists to
// bound.
func TestEvictionRemovesChildRows(t *testing.T) {
	store := newStore(t, retention{ttl: time.Hour})
	clock := time.Now()
	store.now = func() time.Time { return clock }
	view := store.For(principal("partner-a"))

	mustCreate(t, view, "task-1", "ctx-1", submitted())
	if err := view.RecordArtifact("task-1", a2a.NewTextArtifact("art-1", "response", "answer")); err != nil {
		t.Fatalf("RecordArtifact: %v", err)
	}
	if err := view.RecordStatus("task-1", a2a.NewTaskStatus(a2a.TaskStateCompleted)); err != nil {
		t.Fatalf("RecordStatus: %v", err)
	}

	clock = clock.Add(2 * time.Hour)
	if err := store.evict(); err != nil {
		t.Fatalf("evict: %v", err)
	}

	for _, table := range []string{"task_status_history", "task_artifacts", "task_messages"} {
		var n int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s still holds %d rows for an evicted task; the cascade did not fire", table, n)
		}
	}
}

// TestRetentionKnobsCanBeDisabled pins that zero means "keep everything" on
// both knobs, which is what the config documents.
func TestRetentionKnobsCanBeDisabled(t *testing.T) {
	store := newStore(t, retention{})
	clock := time.Now()
	store.now = func() time.Time { return clock }
	view := store.For(principal("partner-a"))

	for i := range 5 {
		mustCreate(t, view, fmt.Sprintf("t%d", i), "ctx-1", a2a.NewTaskStatus(a2a.TaskStateCompleted))
	}
	clock = clock.Add(365 * 24 * time.Hour)
	if err := store.evict(); err != nil {
		t.Fatalf("evict: %v", err)
	}
	recs, err := view.List("ctx-1", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recs) != 5 {
		t.Errorf("kept %d tasks with retention disabled, want 5", len(recs))
	}
}

// TestRetentionDefaultsMatchTheDocumentedNumbers pins the defaults the
// configuration reference publishes.
func TestRetentionDefaultsMatchTheDocumentedNumbers(t *testing.T) {
	cfg, err := parseConfig(map[string]any{"card": minimalCard()})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.retention.ttl != 24*time.Hour {
		t.Errorf("default tasks.ttl = %v, want 24h", cfg.retention.ttl)
	}
	if cfg.retention.maxPerContext != 200 {
		t.Errorf("default tasks.max_per_context = %d, want 200", cfg.retention.maxPerContext)
	}
}

// TestRetentionConfigIsParsed walks the accepted and rejected spellings.
func TestRetentionConfigIsParsed(t *testing.T) {
	t.Run("both knobs", func(t *testing.T) {
		cfg, err := parseConfig(map[string]any{
			"card":  minimalCard(),
			"tasks": map[string]any{"ttl": "90m", "max_per_context": 7},
		})
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if cfg.retention.ttl != 90*time.Minute || cfg.retention.maxPerContext != 7 {
			t.Errorf("retention = %+v, want 90m / 7", cfg.retention)
		}
	})
	t.Run("one knob leaves the other at its default", func(t *testing.T) {
		cfg, err := parseConfig(map[string]any{
			"card":  minimalCard(),
			"tasks": map[string]any{"ttl": "1h"},
		})
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if cfg.retention.ttl != time.Hour || cfg.retention.maxPerContext != defaultTasksPerContext {
			t.Errorf("retention = %+v, want 1h / %d", cfg.retention, defaultTasksPerContext)
		}
	})
	t.Run("zero disables", func(t *testing.T) {
		cfg, err := parseConfig(map[string]any{
			"card":  minimalCard(),
			"tasks": map[string]any{"ttl": "0s", "max_per_context": 0},
		})
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if cfg.retention.ttl != 0 || cfg.retention.maxPerContext != 0 {
			t.Errorf("retention = %+v, want both disabled", cfg.retention)
		}
	})
	for name, block := range map[string]any{
		"a bare number ttl":   map[string]any{"ttl": 600},
		"an unparsable ttl":   map[string]any{"ttl": "soon"},
		"a negative ttl":      map[string]any{"ttl": "-1h"},
		"a negative cap":      map[string]any{"max_per_context": -1},
		"a fractional cap":    map[string]any{"max_per_context": 1.5},
		"a non-mapping block": "24h",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig(map[string]any{"card": minimalCard(), "tasks": block}); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// --- concurrency ---

// TestConcurrentWrites drives every write path from many goroutines at once.
//
// SQLite admits one writer, and the store serializes writes behind its own
// mutex for that reason; this asserts the arrangement actually holds up rather
// than trading a busy timeout for a lost row. Run under -race it also covers
// the store's own field access.
func TestConcurrentWrites(t *testing.T) {
	store := newStore(t, noRetention)

	const principals = 4
	const perPrincipal = 8

	var wg sync.WaitGroup
	errs := make(chan error, principals*perPrincipal*4)

	for p := range principals {
		owner := principal(fmt.Sprintf("partner-%d", p))
		for i := range perPrincipal {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// A fresh view per goroutine, which is how the plugin uses it:
				// one per request.
				view := store.For(owner)
				taskID := fmt.Sprintf("task-%d-%d", p, i)
				if err := view.Create(taskID, "ctx-1", submitted(), userRef(taskID)); err != nil {
					errs <- fmt.Errorf("create %s: %w", taskID, err)
					return
				}
				if err := view.RecordStatus(taskID, a2a.NewTaskStatus(a2a.TaskStateWorking)); err != nil {
					errs <- fmt.Errorf("working %s: %w", taskID, err)
				}
				if err := view.RecordArtifact(taskID,
					a2a.NewTextArtifact(taskID+"-response", "response", "answer")); err != nil {
					errs <- fmt.Errorf("artifact %s: %w", taskID, err)
				}
				if err := view.RecordStatus(taskID, a2a.NewTaskStatus(a2a.TaskStateCompleted)); err != nil {
					errs <- fmt.Errorf("completed %s: %w", taskID, err)
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	for p := range principals {
		view := store.For(principal(fmt.Sprintf("partner-%d", p)))
		recs, err := view.List("", 0)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(recs) != perPrincipal {
			t.Errorf("partner-%d holds %d tasks, want %d", p, len(recs), perPrincipal)
		}
		for _, rec := range recs {
			if rec.Status.State != a2a.TaskStateCompleted {
				t.Errorf("%s ended %q, want COMPLETED", rec.TaskID, rec.Status.State)
			}
			if len(rec.StatusHistory) != 3 {
				t.Errorf("%s has %d status rows, want 3", rec.TaskID, len(rec.StatusHistory))
			}
			if len(rec.Artifacts) != 1 {
				t.Errorf("%s has %d artifacts, want 1", rec.TaskID, len(rec.Artifacts))
			}
		}
	}
}

// TestConcurrentWritesToOneTask pins the same-task case: repeated artifact
// writes under the same id replace rather than accumulate, and the status
// history stays consistent.
func TestConcurrentWritesToOneTask(t *testing.T) {
	store := newStore(t, noRetention)
	view := store.For(principal("partner-a"))
	mustCreate(t, view, "task-1", "ctx-1", submitted())

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := view.RecordArtifact("task-1",
				a2a.NewTextArtifact("task-1-response", "response", fmt.Sprintf("draft %d", i))); err != nil {
				t.Errorf("RecordArtifact: %v", err)
			}
			if err := view.RecordMessage("task-1",
				messageRef{MessageID: fmt.Sprintf("m-%d", i), Role: a2a.RoleAgent, Text: "chunk"}); err != nil {
				t.Errorf("RecordMessage: %v", err)
			}
		}()
	}
	wg.Wait()

	rec, found, err := view.Get("task-1")
	if err != nil || !found {
		t.Fatalf("Get: %v (found=%v)", err, found)
	}
	if len(rec.Artifacts) != 1 {
		t.Errorf("artifacts = %d, want 1 — a repeat artifact id must replace, not accumulate", len(rec.Artifacts))
	}
	// The inbound reference plus one per goroutine.
	if len(rec.Messages) != 17 {
		t.Errorf("message references = %d, want 17", len(rec.Messages))
	}
}

// --- the turn lifecycle ---

// newPluginAt boots a plugin whose storage lives at dir, and returns the func
// that releases the SQLite handles so a restart can be simulated.
func newPluginAt(t *testing.T, dir string, overrides map[string]any) (*Plugin, engine.EventBus, func()) {
	t.Helper()
	opener, closeFn := storageAt(t, dir)
	bus := engine.NewEventBus()
	p, ok := New().(*Plugin)
	if !ok {
		t.Fatal("New() did not return *Plugin")
	}
	if err := p.Init(engine.PluginContext{
		Config:  testConfig(t, overrides),
		Bus:     bus,
		Logger:  discardLogger(),
		Storage: opener,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(t.Context()) })
	return p, bus, closeFn
}

// TestSendMessageLandsInTheStore is the wiring assertion: a task created by
// SendMessage is durable, filed under the caller, and carries every transition
// the turn made — not just its final state.
func TestSendMessageLandsInTheStore(t *testing.T) {
	dir := t.TempDir()
	p, bus, closeFn := newPluginAt(t, dir, map[string]any{"bearer_token": "s3cret"})
	playAgent(t, bus, scriptedTurn("the answer is 42"))

	rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
		withBearer("s3cret"),
		jsonrpcBody(t, a2a.MethodSendMessage, sendMessageParams("what is the answer?", "ctx-1")))
	if rec.Code != http.StatusOK {
		t.Fatalf("SendMessage status = %d: %s", rec.Code, rec.Body)
	}
	task := taskFromRPC(t, rec.Body.Bytes())
	closeFn()

	// A restarted instance answers for the task it created before the restart.
	restarted, _ := openStoreAt(t, dir, noRetention)
	owner := nexusauth.Principal{ID: legacyBearerPrincipal}
	stored, found, err := restarted.For(owner).Get(task.ID)
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if !found {
		t.Fatalf("task %q did not reach the store", task.ID)
	}
	if stored.ContextID != "ctx-1" {
		t.Errorf("stored contextId = %q, want ctx-1", stored.ContextID)
	}
	if stored.Status.State != a2a.TaskStateCompleted {
		t.Errorf("stored state = %q, want COMPLETED", stored.Status.State)
	}
	wantHistory := []a2a.TaskState{
		a2a.TaskStateSubmitted, a2a.TaskStateWorking, a2a.TaskStateCompleted,
	}
	got := make([]a2a.TaskState, 0, len(stored.StatusHistory))
	for _, s := range stored.StatusHistory {
		got = append(got, s.State)
	}
	if len(got) != len(wantHistory) {
		t.Fatalf("stored status history = %v, want %v", got, wantHistory)
	}
	for i := range wantHistory {
		if got[i] != wantHistory[i] {
			t.Fatalf("stored status history = %v, want %v", got, wantHistory)
		}
	}
	if len(stored.Artifacts) != 1 {
		t.Fatalf("stored artifacts = %+v, want the turn's one artifact", stored.Artifacts)
	}
	if text, ok := stored.Artifacts[0].Parts[0].TextValue(); !ok || text != "the answer is 42" {
		t.Errorf("stored artifact text = %q, want the turn's answer", text)
	}
	if len(stored.Messages) != 2 {
		t.Fatalf("stored message references = %+v, want the inbound and the reply", stored.Messages)
	}
	if stored.Messages[0].Role != a2a.RoleUser || stored.Messages[0].Text != "what is the answer?" {
		t.Errorf("inbound reference = %+v", stored.Messages[0])
	}
	if stored.Messages[1].Role != a2a.RoleAgent || stored.Messages[1].Text != "the answer is 42" {
		t.Errorf("reply reference = %+v", stored.Messages[1])
	}
	if stored.Principal.ID != legacyBearerPrincipal {
		t.Errorf("stored principal = %q, want %q", stored.Principal.ID, legacyBearerPrincipal)
	}
}

// TestFailedTurnIsRecorded pins the other terminal path: a turn that dies is
// stored as FAILED, so a client that reconnects learns the outcome rather than
// finding a task frozen at WORKING.
func TestFailedTurnIsRecorded(t *testing.T) {
	dir := t.TempDir()
	p, bus, closeFn := newPluginAt(t, dir, nil)
	playAgent(t, bus, func(b engine.EventBus, in events.UserInput) {
		_ = b.Emit("agent.turn.start", events.TurnInfo{
			SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1", SessionID: in.SessionID,
		})
		_ = b.Emit("core.error", events.ErrorInfo{
			SchemaVersion: events.ErrorInfoVersion,
			Source:        "test.agent",
			Err:           fmt.Errorf("the provider gave up"),
			Fatal:         true,
		})
	})

	rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
		jsonrpcBody(t, a2a.MethodSendMessage, sendMessageParams("break", "ctx-1")))
	if rec.Code != http.StatusOK {
		t.Fatalf("SendMessage status = %d: %s", rec.Code, rec.Body)
	}
	task := taskFromRPC(t, rec.Body.Bytes())
	closeFn()

	restarted, _ := openStoreAt(t, dir, noRetention)
	stored, found, err := restarted.For(nexusauth.Principal{}).Get(task.ID)
	if err != nil || !found {
		t.Fatalf("Get after restart: %v (found=%v)", err, found)
	}
	if stored.Status.State != a2a.TaskStateFailed {
		t.Errorf("stored state = %q, want FAILED", stored.Status.State)
	}
	if stored.Status.Message == nil {
		t.Error("the FAILED status was stored without its reason")
	}
}

// TestStoreIsRequiredAtBoot pins the refusal to run without durable storage: a
// listener that could not record a task would answer for tasks it has no record
// of, which is the failure the store exists to prevent.
func TestStoreIsRequiredAtBoot(t *testing.T) {
	p := New().(*Plugin)
	err := p.Init(engine.PluginContext{
		Config: testConfig(t, nil),
		Bus:    engine.NewEventBus(),
		Logger: discardLogger(),
	})
	if err == nil {
		t.Fatal("the plugin booted without a task store")
	}
	if !strings.Contains(err.Error(), "durable") {
		t.Errorf("boot error does not explain the requirement: %v", err)
	}
}

// storeDirIsUnderTheSession pins the on-disk location the architecture doc
// promises, so a session archive disposes of its tasks with it.
func TestStoreLivesUnderTheSession(t *testing.T) {
	dir := t.TempDir()
	_, _, _ = newPluginAt(t, dir, nil)
	want := filepath.Join(dir, "session", "plugins", pluginID, "store.db")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("no task store at %s: %v", want, err)
	}
}

// --- pagination ---

// TestQueryPaginationIsStableUnderInsertion is why the cursor is a keyset rather
// than an offset: a task created between two pages must not shift the rows the
// client has not seen yet.
//
// With an offset cursor the insertion below would push one task down a slot and
// the second page would repeat a row the first page already returned. With a
// keyset cursor the second page continues from a POSITION, so the new task is
// simply newer than the position and is not seen at all.
func TestQueryPaginationIsStableUnderInsertion(t *testing.T) {
	store := newStore(t, noRetention)
	view := store.For(principal("partner-a"))
	for i := range 4 {
		mustCreate(t, view, fmt.Sprintf("task-%d", i), "ctx-1", submitted())
	}

	first, cursor, total, err := view.Query(taskQuery{limit: 2})
	if err != nil {
		t.Fatalf("Query page 1: %v", err)
	}
	if total != 4 {
		t.Errorf("totalSize = %d, want 4", total)
	}
	if got := taskIDsOf(first); len(got) != 2 || got[0] != "task-3" || got[1] != "task-2" {
		t.Fatalf("page 1 = %v, want the two newest", got)
	}
	if !cursor.set {
		t.Fatal("no cursor after a full page; the client cannot ask for the rest")
	}

	// A task arrives between the pages.
	mustCreate(t, view, "task-new", "ctx-1", submitted())

	second, next, total, err := view.Query(taskQuery{limit: 2, after: cursor})
	if err != nil {
		t.Fatalf("Query page 2: %v", err)
	}
	if got := taskIDsOf(second); len(got) != 2 || got[0] != "task-1" || got[1] != "task-0" {
		t.Fatalf("page 2 = %v, want the two oldest with no repeats", got)
	}
	if next.set {
		t.Errorf("a further page was offered after the last row")
	}
	if total != 5 {
		t.Errorf("totalSize = %d on page 2, want the current 5", total)
	}
}

// TestQueryFiltersAreScopedToOnePrincipal pins that no filter widens the result
// set: another principal's task matching every criterion is still invisible.
func TestQueryFiltersAreScopedToOnePrincipal(t *testing.T) {
	store := newStore(t, noRetention)
	a := store.For(principal("partner-a"))
	b := store.For(principal("partner-b"))
	mustCreate(t, a, "task-a", "ctx-shared", submitted())
	mustCreate(t, b, "task-b", "ctx-shared", submitted())

	recs, _, total, err := a.Query(taskQuery{contextID: "ctx-shared", state: a2a.TaskStateSubmitted})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := taskIDsOf(recs); len(got) != 1 || got[0] != "task-a" {
		t.Fatalf("Query returned %v, want only partner-a's task", got)
	}
	if total != 1 {
		t.Errorf("totalSize = %d, want 1; the count must carry the same predicate", total)
	}
}
