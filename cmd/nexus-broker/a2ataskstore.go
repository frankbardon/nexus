package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// This file is the broker's DURABLE A2A TASK STORE: the record that lets
// GetTask, ListTasks and SubscribeToTask answer after the instance that ran the
// task has been released, has crashed, or was never started by this process at
// all.
//
// # Why it is a fourth JSONL file and not a database
//
// It sits beside leases.jsonl, session-binaries.jsonl and a2a-contexts.jsonl in
// state_dir, and it is written exactly the way a2a-contexts.jsonl is: append one
// JSON record per change, fold the file into memory on open, rewrite in place
// periodically to deduplicate and prune. That is a deliberate choice over the
// obvious alternative.
//
// nexus.io.a2a keeps its tasks in SQLite, because a plugin is handed a per-plugin
// storage handle by the engine and gets pooling, WAL and the on-disk convention
// for free. This binary has no engine: it would have to open a database itself,
// and doing so would make broker state live in TWO mechanisms — three JSONL
// journals and one database — with two failure policies, two recovery stories
// and two things an operator has to know about a state_dir. One mechanism that
// every existing broker index already uses is worth more here than the query
// engine, because the queries this store answers (one task by id, one page of a
// principal's tasks) are a map lookup and a sort.
//
// The cost of that choice is stated rather than hidden: the fold is in memory,
// so the store is bounded by RECORD COUNT and by the size of what one record may
// carry — see maxA2ATaskRecords and maxA2AStoredTextBytes — instead of by disk.
//
// # Principal scoping
//
// The scoping is STRUCTURAL, and in one respect stronger than the SQL version
// next door. There is no predicate to remember, because a task is not reachable
// by its id: the fold is keyed by OWNER FIRST (byOwner[ownerKey][taskID]), and
// the only way to obtain an owner key is to hand a Principal to For. A caller
// holding a task id and no principal cannot form a lookup at all.
//
// A view is bound to a PROFILE as well as a principal, and a task addressed to
// another profile is absent from it. That is the same key a2aContextRecord uses
// and for the same reason: two profiles are two different public agents with two
// different configs, so a task belongs to one of them. Without it, ListTasks on
// the research agent would list the caller's support conversations.
//
// Every read and write therefore lives on *a2aPrincipalTasks and goes through
// ownedLocked, which indexes byOwner with t.ownerKey and nothing else.
// TestEveryTaskReadIsOwnerScoped enforces that against the SOURCE: a method on
// the scoped view that does not mention ownerKey, or any other function in this
// file that reaches into byOwner outside the housekeeping allowlist, fails the
// build.
//
// A live task never holds a *a2aPrincipalTasks. It holds an a2aTaskSink, which
// is the write-only subset, so the read pump translating a turn has no lookup
// within reach — let alone an unscoped one.
//
// # Durability failures never fail a turn
//
// Every write here returns NOTHING. That is the broker's own precedent
// (Registry.appendRecord, sessionBinaryIndex.record, a2aContextIndex.record): a
// durability index must not become a new way for an agent turn to fail. The
// instance is already running and the client is already streaming; refusing the
// turn over a full disk would turn a degraded read-back into an outage. Failures
// are logged, loudly, naming what will not be readable later.

// a2aTaskStoreName is the file inside state_dir that holds the task records.
const a2aTaskStoreName = "a2a-tasks.jsonl"

// a2aTaskCompactEvery is how many appends may accumulate before the store is
// rewritten in place, deduplicated to one record per task and pruned. It matches
// a2aContextCompactEvery because the two files see the same order of write rate:
// a task writes a handful of records over its life, a context writes one.
const a2aTaskCompactEvery = 256

// maxA2ATaskRecords is the hard ceiling on how many task records the store
// keeps, across every principal and every context.
//
// It exists because the per-context cap alone does not bound anything a broker
// cares about: a broker fronts an unbounded number of conversations, so
// "50 tasks per context" multiplied by "as many contexts as clients ask for" is
// not a bound. This is the number that makes the store's footprint statable.
//
// 2048 is chosen against the worst case rather than the typical one. A record
// carries at most one response artifact and a short message trail, each capped
// at maxA2AStoredTextBytes, so a fully-loaded store is on the order of tens of
// megabytes in memory and on disk — an amount a gateway process can hold without
// anyone having to think about it. Eviction takes the OLDEST TERMINAL records
// first, so pressure is felt by the tasks least likely to be read back.
const maxA2ATaskRecords = 2048

// maxA2AStoredTextBytes caps the text of one stored artifact or one stored
// history message.
//
// This is the store's real growth term. A turn's answer is unbounded — an agent
// asked to summarize a repository can emit megabytes — and the record is
// rewritten on each of the handful of transitions a task makes, so an uncapped
// answer would be written several times at whatever size it happened to be.
//
// 16 KiB is roughly four thousand words: longer than any answer a person reads
// in one go, and long enough that the cap is not reached by ordinary traffic. It
// is a constant rather than a config key because it is a property of THIS
// storage substrate — an in-memory fold of a JSONL file — rather than a
// deployment choice, and the honest thing to expose to an operator is the
// retention policy, not the byte budget of the encoding.
//
// The degradation is visible rather than silent: a truncated stored copy carries
// a2aTruncationNotice, so a client reading a task back can tell that what it is
// holding is an excerpt. THE STREAM IS NEVER TRUNCATED — only the stored copy
// is, so a client that was attached when the turn ran received the whole answer.
const maxA2AStoredTextBytes = 16 << 10

// a2aTruncationNotice is appended to any stored text that had to be cut.
const a2aTruncationNotice = "\n\n[the stored copy of this text was truncated by the broker's task store]"

// maxA2AStoredMessages and maxA2AStoredArtifacts bound the two repeated fields
// of a record, so a task that is asked a hundred questions — or an agent that
// republishes an artifact under new ids — cannot make one record grow without
// limit inside an otherwise bounded store. Both keep the NEWEST entries, which
// is what a client reading a task back is looking for.
const (
	maxA2AStoredMessages  = 20
	maxA2AStoredArtifacts = 8
)

// a2aTaskOrphanReason is the status message a task left in flight by a stopped
// broker is settled with.
//
// FAILED is the honest state: the work did not complete and nobody canceled it.
// Leaving it as it stood would be worse in three separate ways — a client
// polling GetTask would see WORKING for ever, SubscribeToTask could only hand
// over a snapshot and hang up, and retention could never evict it, because only
// terminal tasks are evictable. A crash loop would then accumulate immortal
// records that count against the per-context cap and push real tasks out of it.
const a2aTaskOrphanReason = "the broker stopped while this task was still running, so it will never complete"

// a2aTaskRetention is the resolved retention policy. Both knobs are
// configurable; see defaultA2ATaskTTL and defaultA2ATasksPerContext for the
// numbers and the reasoning.
type a2aTaskRetention struct {
	// ttl is how long a terminal task is kept after its last transition. Zero
	// disables age-based eviction.
	ttl time.Duration
	// maxPerContext is how many tasks are kept per (owner, contextId). Zero
	// disables the cap.
	maxPerContext int
}

// a2aStoredMessage is one message in a task's history.
//
// It is a REFERENCE rather than a second copy of the conversation: the
// authoritative transcript lives in the engine session the instance holds, and
// duplicating it here would give a task a divergent view of the same turn. What
// is kept is the identity of the message and the text that travelled, because
// nothing in the broker is addressable by A2A message id — a reference that
// recorded only the id could never be resolved back to anything.
type a2aStoredMessage struct {
	MessageID string   `json:"message_id,omitempty"`
	Role      a2a.Role `json:"role"`
	Text      string   `json:"text"`
}

// a2aTaskRecord is one line of the task store: a complete task, as of the write
// that produced the line.
//
// Whole-record-per-append rather than a delta log, exactly as a2aContextRecord
// is: the fold is then "last record for this key wins", which needs no replay
// ordering and cannot be corrupted by a lost intermediate line.
//
// NO SECRET MAY EVER BE ADDED TO THIS STRUCT. It outlives the process and is
// written to be read back, and — unlike the lease journal — it is written for
// every task a client ever ran. That is also why the OWNER is stored as an id
// and not as a nexusauth.Principal: a Principal carries Claims, which for a JWT
// validator is the decoded token body. An id is all the store needs and all it
// is entitled to keep.
type a2aTaskRecord struct {
	// OwnerID is the principal id of the caller the task belongs to, and it is
	// part of the KEY rather than a note on the record — see the principal
	// scoping section at the top of this file. Empty is the anonymous owner,
	// which is every caller on a broker with no `auth:` block.
	OwnerID string `json:"owner_id,omitempty"`

	// Profile is the `agents:` profile the task was addressed to, and it is part
	// of the scoped view's key alongside the owner — see the scoping section at
	// the top of this file.
	Profile string `json:"profile,omitempty"`

	TaskID    string `json:"task_id"`
	ContextID string `json:"context_id"`

	// Seq is the store-assigned creation order, and it is the TIEBREAK the
	// listing cursor is built on. CreatedAt alone is not a position: two tasks
	// created inside one clock tick would be an ambiguous cursor, and an
	// ambiguous cursor is either an infinite loop or a dropped row.
	Seq int64 `json:"seq"`

	State         string       `json:"state"`
	StateAt       time.Time    `json:"state_at"`
	StatusMessage *a2a.Message `json:"status_message,omitempty"`

	// Terminal is stored rather than recomputed so that a record written by a
	// broker that knew a state this one does not still evicts correctly.
	Terminal bool `json:"terminal"`

	Artifacts []a2a.Artifact     `json:"artifacts,omitempty"`
	History   []a2aStoredMessage `json:"history,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// status rebuilds the wire TaskStatus from the stored columns.
func (r a2aTaskRecord) status() a2a.TaskStatus {
	status := a2a.TaskStatus{
		State:     a2a.TaskState(r.State),
		Timestamp: a2a.NewTimestamp(r.StateAt),
	}
	if r.StatusMessage != nil {
		msg := *r.StatusMessage
		status.Message = &msg
	}
	return status
}

// a2aRenderOptions tunes how a stored record is projected onto the wire. Both
// knobs come straight off the request objects: GetTask and ListTasks each carry
// a historyLength, and ListTasks additionally defaults artifacts OFF so a page
// of tasks does not drag every artifact body with it.
type a2aRenderOptions struct {
	// historyLength caps how many of the MOST RECENT history messages the
	// rendered task carries. Nil means "no client-imposed limit" and zero means
	// "omit history entirely" — the presence distinction specification section
	// 3.2.4 draws, which is why it is a pointer.
	historyLength *int
	// omitArtifacts drops the artifact bodies.
	omitArtifacts bool
}

// task renders the record in the wire shape.
//
// History is the trail of message references this store retained, rendered as
// text messages. Section 3.7 explicitly leaves it to the server which messages
// are persisted and warns clients not to assume all of them are present, so a
// bounded reference trail is a conforming History rather than a partial one —
// and retention is what bounds it.
func (r a2aTaskRecord) task(opt a2aRenderOptions) a2a.Task {
	task := a2a.Task{
		ID:        r.TaskID,
		ContextID: r.ContextID,
		Status:    r.status(),
		History:   r.history(opt.historyLength),
	}
	if !opt.omitArtifacts && len(r.Artifacts) > 0 {
		task.Artifacts = append([]a2a.Artifact(nil), r.Artifacts...)
	}
	return task
}

// history renders the retained message references, honouring a client-supplied
// cap. The cap keeps the MOST RECENT messages, which is what "historyLength"
// means everywhere the specification uses it: a client asking for 2 wants the
// last exchange, not the first.
func (r a2aTaskRecord) history(limit *int) []a2a.Message {
	if limit != nil && *limit <= 0 {
		return nil
	}
	out := make([]a2a.Message, 0, len(r.History))
	for _, ref := range r.History {
		if ref.Text == "" || !ref.Role.Valid() {
			// A reference with no text cannot be rendered: a Message requires
			// non-empty Parts. Skipping is honest; an empty message would not be.
			continue
		}
		out = append(out, a2a.NewMessage(ref.MessageID, ref.Role, a2a.TextPart(ref.Text)).
			InContext(r.ContextID).
			ForTask(r.TaskID))
	}
	if len(out) == 0 {
		return nil
	}
	if limit != nil && *limit < len(out) {
		out = out[len(out)-*limit:]
	}
	return out
}

// a2aTaskSink is the write-through surface a LIVE task holds.
//
// It is deliberately WRITE-ONLY. A task folds instance payloads into frames on
// the gateway's read-pump goroutine; giving it a read method would put a lookup
// on the hot path, and the whole point of the owner-keyed fold is that a lookup
// requires a principal. The only implementation is *a2aPrincipalTasks, so a task
// can only ever write to the record of the principal that created it.
type a2aTaskSink interface {
	RecordStatus(taskID string, status a2a.TaskStatus)
	RecordArtifact(taskID string, artifact a2a.Artifact)
	RecordMessage(taskID string, msg a2aStoredMessage)
}

var _ a2aTaskSink = (*a2aPrincipalTasks)(nil)

// a2aTaskStore is the durable task store: an append-only JSONL file under
// state_dir with an owner-keyed in-memory fold.
//
// It exposes NO query surface at all. The only thing it can do is hand out a
// view bound to one principal; see the scoping note at the top of this file.
type a2aTaskStore struct {
	logger *slog.Logger

	// path is the store file inside state_dir, EMPTY when the broker runs
	// without one.
	//
	// An empty path is a fully supported, deliberately chosen mode rather than a
	// degenerate one: the fold still works, so GetTask, ListTasks and
	// SubscribeToTask all answer for the life of the process, and only durability
	// across a restart is lost. That is exactly the bargain a broker with no
	// state_dir has already made for leases and for context continuity, and it
	// keeps the operations honest instead of making them refuse.
	path string

	// now is the clock stamped on records and used by retention. Tests swap it
	// for a deterministic one so TTL eviction needs no sleeps.
	now func() time.Time

	retention a2aTaskRetention

	mu sync.Mutex

	// f is the append handle. Nil for a memory-only store, after Close, and
	// briefly during a rewrite.
	f *os.File

	// byOwner is the fold: owner key → task id → most recent record. Keyed by
	// owner FIRST, which is what makes an unscoped read unformable.
	byOwner map[string]map[string]a2aTaskRecord

	// nextSeq assigns the listing tiebreak.
	nextSeq int64

	// sinceCompact counts appends since the last rewrite.
	sinceCompact int
}

// openA2ATaskStore builds the broker's A2A task store from config.
//
// An EMPTY state_dir yields a MEMORY-ONLY store rather than a nil one. That is
// the one place this file departs from a2aContextIndex, and the reason is that
// the two degrade differently: a missing context binding costs a resumed
// conversation, which the client cannot see, whereas a missing task record makes
// GetTask answer "unknown task" for a task the client is streaming right now,
// which it very much can. So the store always exists, and state_dir decides
// whether it survives a restart.
//
// Like openA2AContextIndex, an unusable FILE is not a boot failure — the caller
// logs and carries on with the memory-only store it is handed instead.
func openA2ATaskStore(logger *slog.Logger, cfg Config) (*a2aTaskStore, error) {
	if logger == nil {
		logger = slog.Default()
	}
	store := newA2ATaskStore(logger, cfg.A2ATaskRetention)
	if cfg.StateDir == "" {
		return store, nil
	}
	// 0o700 to match the lease journal and the two indexes: this file names
	// principal ids and carries agent output. It is not a secret store, but it is
	// nobody else's business either.
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return store, fmt.Errorf("creating broker state_dir %q: %w", cfg.StateDir, err)
	}
	store.path = filepath.Join(cfg.StateDir, a2aTaskStoreName)

	recs, skipped, err := readA2ATaskStore(store.path)
	if err != nil {
		store.path = ""
		return store, fmt.Errorf("reading a2a task store: %w", err)
	}
	if skipped > 0 {
		logger.Warn("skipped unreadable a2a task records; the broker was most likely killed mid-write",
			"path", store.path, "skipped", skipped, "read", len(recs))
	}
	store.foldLocked(recs)

	// Order is load-bearing: settle the tasks a previous process abandoned FIRST,
	// so the retention sweep that follows can evict them. Retention only ever
	// drops terminal tasks, so a zombie left non-terminal would be immortal.
	orphans := store.settleOrphans()

	store.mu.Lock()
	store.evictLocked()
	// Rewrite-on-open deduplicates, applies retention and truncates any torn
	// trailing record, so the file a running broker appends to always starts from
	// a clean, fully parseable, already-bounded state.
	rewriteErr := store.rewriteLocked()
	live := store.countLocked()
	store.mu.Unlock()

	if rewriteErr != nil {
		store.path = ""
		return store, rewriteErr
	}
	logger.Info("a2a task store opened",
		"path", store.path, "tasks", live, "settled_orphans", orphans, "skipped_records", skipped)
	return store, nil
}

// newA2ATaskStore builds a memory-only store. It is what NewA2AServer installs
// by default, so an ingress always has somewhere to write.
func newA2ATaskStore(logger *slog.Logger, policy a2aTaskRetention) *a2aTaskStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &a2aTaskStore{
		logger:    logger,
		now:       time.Now,
		retention: policy,
		byOwner:   make(map[string]map[string]a2aTaskRecord),
	}
}

// For returns the view of the store belonging to one principal on one profile.
// It is the ONLY way to reach a task, which is what makes an unscoped read
// impossible to write rather than merely discouraged.
func (s *a2aTaskStore) For(p nexusauth.Principal, profile string) *a2aPrincipalTasks {
	return s.forOwner(p.ID, profile)
}

// forOwner builds a scoped view from the key components already in hand. It
// exists for the housekeeping paths — orphan settlement reads a record's stored
// owner and profile and has no Principal to rebuild — and it is no weaker than
// For, which any caller could hand a fabricated Principal to. What makes the
// scoping real is that every request handler derives its principal from
// callerPrincipal(r) and its profile from the route it was addressed to.
func (s *a2aTaskStore) forOwner(ownerKey, profile string) *a2aPrincipalTasks {
	return &a2aPrincipalTasks{store: s, ownerKey: ownerKey, profile: profile}
}

// Close closes the append handle. It is idempotent and nil-receiver safe.
func (s *a2aTaskStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	if err != nil {
		return fmt.Errorf("closing a2a task store: %w", err)
	}
	return nil
}

// settleOrphans drives every non-terminal task to FAILED when the store opens.
//
// The repair does the same write a live transition does — RecordStatus on an
// owner-scoped view — rather than one broad sweep over the fold, so the store's
// "no unscoped write" invariant holds through the repair as well. It returns how
// many it settled, for the boot log.
func (s *a2aTaskStore) settleOrphans() int {
	type orphan struct {
		ownerKey string
		profile  string
		taskID   string
	}
	var orphans []orphan

	s.mu.Lock()
	for ownerKey, tasks := range s.byOwner {
		for taskID, rec := range tasks {
			if !rec.Terminal {
				orphans = append(orphans, orphan{ownerKey: ownerKey, profile: rec.Profile, taskID: taskID})
			}
		}
	}
	s.mu.Unlock()

	// Sorted so a broker that settles several reports them in a stable order and
	// the rewritten file is byte-stable for equal timestamps.
	sort.Slice(orphans, func(i, j int) bool {
		if orphans[i].ownerKey == orphans[j].ownerKey {
			return orphans[i].taskID < orphans[j].taskID
		}
		return orphans[i].ownerKey < orphans[j].ownerKey
	})

	status := a2a.NewTaskStatus(a2a.TaskStateFailed).WithMessage(
		a2a.NewAgentMessage(newA2AMessageID(), a2aTaskOrphanReason))
	for _, o := range orphans {
		s.forOwner(o.ownerKey, o.profile).RecordStatus(o.taskID, status)
	}
	if len(orphans) > 0 {
		s.logger.Warn("a2a tasks were left in flight by a previous broker and have been failed",
			"tasks", len(orphans))
	}
	return len(orphans)
}

// ---- the scoped view ----

// a2aPrincipalTasks is the owner-scoped view of the store. Every method reaches
// the fold through ownedLocked, which indexes byOwner with ownerKey and nothing
// else.
type a2aPrincipalTasks struct {
	store *a2aTaskStore
	// ownerKey is the principal id this view is bound to. Empty is the anonymous
	// owner, which is every caller on a broker with no `auth:` block; two
	// anonymous callers therefore share a partition, which is exactly what
	// "authentication is disabled" already means for lease ownership everywhere
	// else in this binary.
	ownerKey string

	// profile is the `agents:` profile this view is bound to. A task addressed
	// to another profile is ABSENT from it, exactly as another principal's is:
	// same code path, same answer, no oracle.
	profile string
}

// owns reports whether a record belongs to this view. It is the profile half of
// the key; the owner half is structural, since the record was found in this
// owner's partition to begin with.
func (t *a2aPrincipalTasks) owns(rec a2aTaskRecord) bool {
	return rec.Profile == t.profile
}

// ownedLocked returns this principal's partition, creating it when create is
// set. Caller MUST hold store.mu.
//
// It is the single point at which byOwner is indexed by a scoped method, which
// is what makes "every read names the owner" checkable by reading the file.
func (t *a2aPrincipalTasks) ownedLocked(create bool) map[string]a2aTaskRecord {
	owned, ok := t.store.byOwner[t.ownerKey]
	if !ok && create {
		owned = make(map[string]a2aTaskRecord)
		t.store.byOwner[t.ownerKey] = owned
	}
	return owned
}

// Create records a new task with its opening status and the message that
// started it, then applies retention.
//
// Retention runs AFTER the insert, not before: the task that just arrived is the
// newest in its partition and so is never its own eviction candidate, and
// running it here means the store is bounded by the same event that grows it.
func (t *a2aPrincipalTasks) Create(taskID, contextID string, status a2a.TaskStatus, inbound a2aStoredMessage) {
	if taskID == "" {
		return
	}
	s := t.store
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now().UTC()
	s.nextSeq++
	rec := a2aTaskRecord{
		OwnerID:       t.ownerKey,
		Profile:       t.profile,
		TaskID:        taskID,
		ContextID:     contextID,
		Seq:           s.nextSeq,
		State:         string(status.State),
		StateAt:       a2aStatusTime(status, now),
		StatusMessage: status.Message,
		Terminal:      status.State.IsTerminal(),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if inbound.Text != "" {
		rec.History = appendA2AStoredMessage(nil, inbound)
	}
	t.ownedLocked(true)[taskID] = rec
	s.appendLocked(rec)
	s.evictLocked()
}

// RecordStatus moves the task to a new status. A task this principal does not
// own is not found, so nothing is written — the same non-answer a read gets.
func (t *a2aPrincipalTasks) RecordStatus(taskID string, status a2a.TaskStatus) {
	t.mutate(taskID, func(rec *a2aTaskRecord) {
		rec.State = string(status.State)
		rec.StateAt = a2aStatusTime(status, t.store.now().UTC())
		rec.StatusMessage = status.Message
		rec.Terminal = status.State.IsTerminal()
	})
}

// RecordArtifact stores an artifact against a task. Artifact ids are unique
// within a task, so a repeat write of the same id REPLACES it — which is what a
// re-rendered artifact should do rather than accumulate duplicates.
func (t *a2aPrincipalTasks) RecordArtifact(taskID string, artifact a2a.Artifact) {
	t.mutate(taskID, func(rec *a2aTaskRecord) {
		rec.Artifacts = appendA2AStoredArtifact(rec.Artifacts, truncateA2AArtifact(artifact))
	})
}

// RecordMessage appends a message reference to a task's history.
func (t *a2aPrincipalTasks) RecordMessage(taskID string, msg a2aStoredMessage) {
	if msg.Text == "" {
		return
	}
	t.mutate(taskID, func(rec *a2aTaskRecord) {
		rec.History = appendA2AStoredMessage(rec.History, msg)
	})
}

// mutate applies a change to one of this principal's tasks and persists the
// result.
//
// The lookup is the ordinary owner-scoped one, so a task belonging to somebody
// else is simply absent and the mutation is a no-op. There is no separate
// ownership check to forget: the map this reads is the owner's own partition.
func (t *a2aPrincipalTasks) mutate(taskID string, apply func(*a2aTaskRecord)) {
	if taskID == "" {
		return
	}
	s := t.store
	s.mu.Lock()
	defer s.mu.Unlock()

	owned := t.ownedLocked(false)
	rec, ok := owned[taskID]
	if !ok || !t.owns(rec) {
		return
	}
	apply(&rec)
	rec.UpdatedAt = s.now().UTC()
	owned[taskID] = rec
	s.appendLocked(rec)
}

// Get returns one of this principal's tasks. The bool reports presence; another
// principal's task is reported ABSENT rather than forbidden, because a distinct
// "exists but is not yours" answer is an existence oracle for task ids this
// caller was never told.
func (t *a2aPrincipalTasks) Get(taskID string) (a2aTaskRecord, bool) {
	s := t.store
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := t.ownedLocked(false)[taskID]
	if !ok || !t.owns(rec) {
		return a2aTaskRecord{}, false
	}
	return cloneA2ATaskRecord(rec), true
}

// a2aTaskQuery is one ListTasks request reduced to what the store can answer.
//
// Every field is a NARROWING filter; none of them can widen the result past the
// principal the view is bound to, because that key is not part of this struct at
// all. There is deliberately no "owner" field for a caller to set, forget or get
// wrong.
type a2aTaskQuery struct {
	// contextID restricts the listing to one conversational context. Empty means
	// every context this principal has.
	contextID string
	// state restricts the listing to tasks currently in one state.
	state a2a.TaskState
	// changedAfter restricts the listing to tasks whose CURRENT status was
	// observed at or after this instant. The zero time means no lower bound.
	changedAfter time.Time
	// after is the keyset cursor a previous page ended on.
	after a2aListCursor
	// limit caps the page. Zero or less means no limit.
	limit int
}

// a2aListCursor is the keyset position a page of tasks ended on.
//
// It is a keyset — (createdAt, seq), the exact tuple the listing is ordered by —
// rather than an offset, because an offset cursor silently skips or repeats rows
// when the underlying set changes between pages, and this set changes on every
// turn and on every retention sweep.
type a2aListCursor struct {
	createdAt int64
	seq       int64
	// set distinguishes "start from the beginning" from a cursor that happens to
	// carry zeroes.
	set bool
}

// Query returns one page of this principal's tasks, newest first, with the
// cursor for the following page and the total number of tasks MATCHING THE
// FILTERS before pagination.
//
// The total is computed under the identical filters, in the same call, so a
// client's "3 of 40" is 40 of the same thing it is paging through.
func (t *a2aPrincipalTasks) Query(q a2aTaskQuery) ([]a2aTaskRecord, a2aListCursor, int) {
	s := t.store
	s.mu.Lock()
	matched := make([]a2aTaskRecord, 0, len(t.ownedLocked(false)))
	for _, rec := range t.ownedLocked(false) {
		if t.owns(rec) && q.matches(rec) {
			matched = append(matched, cloneA2ATaskRecord(rec))
		}
	}
	s.mu.Unlock()

	sortA2ATaskRecordsNewestFirst(matched)
	total := len(matched)

	if q.after.set {
		// Strictly AFTER the cursor in the listing's own order, which is
		// descending: a smaller (createdAt, seq) is a later row.
		for len(matched) > 0 {
			head := matched[0]
			if a2aCursorAfter(head, q.after) {
				break
			}
			matched = matched[1:]
		}
	}

	var next a2aListCursor
	if q.limit > 0 && len(matched) > q.limit {
		matched = matched[:q.limit]
		last := matched[len(matched)-1]
		next = a2aListCursor{createdAt: last.CreatedAt.UnixNano(), seq: last.Seq, set: true}
	}
	return matched, next, total
}

// matches reports whether one record satisfies the query's narrowing filters.
func (q a2aTaskQuery) matches(rec a2aTaskRecord) bool {
	if q.contextID != "" && rec.ContextID != q.contextID {
		return false
	}
	if q.state != "" && rec.State != string(q.state) {
		return false
	}
	if !q.changedAfter.IsZero() && rec.StateAt.Before(q.changedAfter) {
		// The bound is inclusive, as the specification's statusTimestampAfter
		// filter is defined to be ("at or after").
		return false
	}
	return true
}

// a2aCursorAfter reports whether a record sits strictly after a cursor position
// in the newest-first listing order.
func a2aCursorAfter(rec a2aTaskRecord, c a2aListCursor) bool {
	created := rec.CreatedAt.UnixNano()
	if created != c.createdAt {
		return created < c.createdAt
	}
	return rec.Seq < c.seq
}

// sortA2ATaskRecordsNewestFirst orders a listing, newest first, with Seq as the
// tiebreak so the order is total.
func sortA2ATaskRecordsNewestFirst(recs []a2aTaskRecord) {
	sort.Slice(recs, func(i, j int) bool {
		a, b := recs[i].CreatedAt.UnixNano(), recs[j].CreatedAt.UnixNano()
		if a == b {
			return recs[i].Seq > recs[j].Seq
		}
		return a > b
	})
}

// ---- housekeeping ----
//
// The functions below are the ONLY ones in this file that reach into byOwner
// without an owner key, and they are process-wide maintenance rather than
// request-serving reads: folding a file on open, evicting under retention, and
// rewriting the file. None of them returns a record to a caller.

// foldLocked seeds the fold from records read off disk. It runs before anything
// else can touch the store, so it takes no lock.
func (s *a2aTaskStore) foldLocked(recs []a2aTaskRecord) {
	for _, rec := range recs {
		owned, ok := s.byOwner[rec.OwnerID]
		if !ok {
			owned = make(map[string]a2aTaskRecord)
			s.byOwner[rec.OwnerID] = owned
		}
		owned[rec.TaskID] = rec
		if rec.Seq > s.nextSeq {
			s.nextSeq = rec.Seq
		}
	}
}

// countLocked reports how many tasks the fold holds. Caller MUST hold s.mu.
func (s *a2aTaskStore) countLocked() int {
	n := 0
	for _, owned := range s.byOwner {
		n += len(owned)
	}
	return n
}

// appendLocked writes one record to the file and schedules compaction. Caller
// MUST hold s.mu.
//
// A memory-only store writes nothing and is not an error. A write failure is
// LOGGED AND SWALLOWED: see the durability note at the top of this file.
func (s *a2aTaskStore) appendLocked(rec a2aTaskRecord) {
	if s.path == "" {
		return
	}
	if s.f == nil {
		if err := s.reopenLocked(); err != nil {
			s.logger.Error("the a2a task store is closed; the task will not be readable after a restart",
				"path", s.path, "task_id", rec.TaskID, "error", err)
			return
		}
	}
	line, err := json.Marshal(rec)
	if err != nil {
		s.logger.Error("encoding an a2a task record failed; the task will not be readable after a restart",
			"path", s.path, "task_id", rec.TaskID, "error", err)
		return
	}
	line = append(line, '\n')
	// One Write per record, straight to the file descriptor with no buffering in
	// front of it, so a record is either in the file or it is not — the only
	// partial record a reader can meet is the one the kernel was mid-way through,
	// which readA2ATaskStore skips.
	if _, err := s.f.Write(line); err != nil {
		s.logger.Error("appending an a2a task record failed; the task will not be readable after a restart",
			"path", s.path, "task_id", rec.TaskID, "error", err)
		return
	}

	s.sinceCompact++
	if s.sinceCompact >= a2aTaskCompactEvery {
		if err := s.rewriteLocked(); err != nil {
			// The record IS written; a failed rewrite only means the file stays
			// larger than we would like.
			s.logger.Error("compacting the a2a task store failed; the file will keep growing",
				"path", s.path, "error", err)
		}
	}
}

// evictLocked applies retention. Caller MUST hold s.mu.
//
// Only TERMINAL tasks are evictable. A live task is the one thing a client is
// certain to ask about — it is the task it is streaming right now — and dropping
// it mid-turn to satisfy a cap would turn a retention policy into a correctness
// bug. Non-terminal tasks still COUNT against the caps, so a broker wedged with
// stale in-flight tasks shows up as pressure on retention rather than silently
// exempting itself from it.
//
// Eviction touches the FOLD only. The evicted record's last line stays in the
// file until the next rewrite, and that is deliberate rather than sloppy: a
// rewrite on every task creation would be the store's dominant cost, and a
// resurrected record is re-evicted by the identical sweep openA2ATaskStore runs
// before it serves anything, so no evicted task can come back from the dead.
func (s *a2aTaskStore) evictLocked() {
	if s.retention.ttl > 0 {
		cutoff := s.now().UTC().Add(-s.retention.ttl)
		for _, owned := range s.byOwner {
			for id, rec := range owned {
				if rec.Terminal && rec.UpdatedAt.Before(cutoff) {
					delete(owned, id)
				}
			}
		}
	}

	if s.retention.maxPerContext > 0 {
		for _, owned := range s.byOwner {
			byContext := make(map[string][]a2aTaskRecord)
			for _, rec := range owned {
				byContext[rec.ContextID] = append(byContext[rec.ContextID], rec)
			}
			for _, recs := range byContext {
				if len(recs) <= s.retention.maxPerContext {
					continue
				}
				sortA2ATaskRecordsNewestFirst(recs)
				for _, rec := range recs[s.retention.maxPerContext:] {
					if rec.Terminal {
						delete(owned, rec.TaskID)
					}
				}
			}
		}
	}

	// The global ceiling, applied last: it is the backstop that makes the store's
	// footprint statable regardless of how many contexts a broker meets.
	if s.countLocked() > maxA2ATaskRecords {
		all := make([]a2aTaskRecord, 0, s.countLocked())
		for _, owned := range s.byOwner {
			for _, rec := range owned {
				all = append(all, rec)
			}
		}
		sortA2ATaskRecordsNewestFirst(all)
		for i := len(all) - 1; i >= 0 && s.countLocked() > maxA2ATaskRecords; i-- {
			rec := all[i]
			if !rec.Terminal {
				continue
			}
			delete(s.byOwner[rec.OwnerID], rec.TaskID)
		}
	}

	for ownerKey, owned := range s.byOwner {
		if len(owned) == 0 {
			delete(s.byOwner, ownerKey)
		}
	}
}

// recordsLocked returns the surviving records in creation order, oldest first,
// which is the order the rewritten file reads in. Caller MUST hold s.mu.
func (s *a2aTaskStore) recordsLocked() []a2aTaskRecord {
	out := make([]a2aTaskRecord, 0, s.countLocked())
	for _, owned := range s.byOwner {
		for _, rec := range owned {
			out = append(out, rec)
		}
	}
	sortA2ATaskRecordsNewestFirst(out)
	// Reverse into oldest-first so the file reads chronologically.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// rewriteLocked replaces the store file with exactly the surviving records, one
// line each. Caller MUST hold s.mu.
//
// It writes a temp file and renames it over the store, so a crash mid-rewrite
// leaves the previous file intact rather than a half-written one: rename is
// atomic within a directory, and the temp file is in the same directory for
// exactly that reason.
func (s *a2aTaskStore) rewriteLocked() error {
	if s.path == "" {
		s.sinceCompact = 0
		return nil
	}
	kept := s.recordsLocked()

	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("creating a2a task store temp file: %w", err)
	}
	var buf bytes.Buffer
	for _, rec := range kept {
		line, err := json.Marshal(rec)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("encoding an a2a task record: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("writing a2a task store temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("closing a2a task store temp file: %w", err)
	}

	// The append handle has to let go of the old inode before the rename, and be
	// reopened on the new one after it, or subsequent appends would land in a
	// file nothing reads.
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = s.reopenLocked()
		_ = os.Remove(tmp)
		return fmt.Errorf("replacing a2a task store: %w", err)
	}
	if err := s.reopenLocked(); err != nil {
		return err
	}
	s.sinceCompact = 0
	return nil
}

// reopenLocked opens the store for appending. Caller MUST hold s.mu.
func (s *a2aTaskStore) reopenLocked() error {
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening a2a task store %q: %w", s.path, err)
	}
	s.f = f
	return nil
}

// readA2ATaskStore parses a store file, returning the records it could read and
// how many segments it had to skip.
//
// TOLERANCE IS THE POINT, exactly as in readLeaseJournal and
// readA2AContextIndex. A broker killed mid-write leaves a final line with no
// terminating newline, and a full stop over that would cost every task in the
// file. So a segment that does not parse is skipped and counted; a record with
// no task id says nothing and is skipped; and the final segment is skipped even
// if it parses whenever the file does not end in a newline, because a torn
// record is not made trustworthy by happening to be valid JSON up to the cut.
func readA2ATaskStore(path string) ([]a2aTaskRecord, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("reading %q: %w", path, err)
	}
	if len(data) == 0 {
		return nil, 0, nil
	}

	torn := data[len(data)-1] != '\n'
	segments := bytes.Split(data, []byte{'\n'})
	if !torn {
		segments = segments[:len(segments)-1]
	}

	var (
		recs    []a2aTaskRecord
		skipped int
	)
	for i, seg := range segments {
		if len(bytes.TrimSpace(seg)) == 0 {
			continue
		}
		if torn && i == len(segments)-1 {
			skipped++
			continue
		}
		var rec a2aTaskRecord
		if err := json.Unmarshal(seg, &rec); err != nil || rec.TaskID == "" || rec.State == "" {
			skipped++
			continue
		}
		recs = append(recs, rec)
	}
	return recs, skipped, nil
}

// ---- record helpers ----

// cloneA2ATaskRecord copies a record's slices and its status message so a
// snapshot handed to a caller cannot alias the fold's own, which keeps a later
// append off a slice another goroutine is reading.
func cloneA2ATaskRecord(rec a2aTaskRecord) a2aTaskRecord {
	if len(rec.Artifacts) > 0 {
		rec.Artifacts = append([]a2a.Artifact(nil), rec.Artifacts...)
	}
	if len(rec.History) > 0 {
		rec.History = append([]a2aStoredMessage(nil), rec.History...)
	}
	if rec.StatusMessage != nil {
		msg := *rec.StatusMessage
		rec.StatusMessage = &msg
	}
	return rec
}

// appendA2AStoredMessage adds one message reference, truncating its text and
// keeping only the newest maxA2AStoredMessages entries.
func appendA2AStoredMessage(history []a2aStoredMessage, msg a2aStoredMessage) []a2aStoredMessage {
	msg.Text = truncateA2AStoredText(msg.Text)
	history = append(history, msg)
	if len(history) > maxA2AStoredMessages {
		history = history[len(history)-maxA2AStoredMessages:]
	}
	return history
}

// appendA2AStoredArtifact adds or replaces an artifact by id, keeping only the
// newest maxA2AStoredArtifacts entries.
func appendA2AStoredArtifact(artifacts []a2a.Artifact, artifact a2a.Artifact) []a2a.Artifact {
	for i, existing := range artifacts {
		if existing.ArtifactID == artifact.ArtifactID {
			artifacts[i] = artifact
			return artifacts
		}
	}
	artifacts = append(artifacts, artifact)
	if len(artifacts) > maxA2AStoredArtifacts {
		artifacts = artifacts[len(artifacts)-maxA2AStoredArtifacts:]
	}
	return artifacts
}

// truncateA2AArtifact bounds the text an artifact carries into the store. Only
// text parts are touched, because a text part is the only kind this mapping
// produces.
func truncateA2AArtifact(artifact a2a.Artifact) a2a.Artifact {
	if len(artifact.Parts) == 0 {
		return artifact
	}
	parts := make([]a2a.Part, len(artifact.Parts))
	copy(parts, artifact.Parts)
	for i, part := range parts {
		text, ok := part.TextValue()
		if !ok {
			continue
		}
		if trimmed := truncateA2AStoredText(text); trimmed != text {
			parts[i] = a2a.TextPart(trimmed)
		}
	}
	artifact.Parts = parts
	return artifact
}

// truncateA2AStoredText caps one stored string at maxA2AStoredTextBytes,
// marking any cut it makes so a client reading the task back can tell that what
// it holds is an excerpt.
//
// The cut is made on a byte boundary that does not split a UTF-8 rune, so the
// stored text stays valid JSON-encodable text rather than becoming a string with
// a replacement character glued to the end.
func truncateA2AStoredText(text string) string {
	if len(text) <= maxA2AStoredTextBytes {
		return text
	}
	cut := maxA2AStoredTextBytes
	for cut > 0 && !utf8RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + a2aTruncationNotice
}

// utf8RuneStart reports whether b begins a UTF-8 rune (i.e. is not a
// continuation byte).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// a2aStatusTime returns the instant a status was observed, falling back to the
// store's clock when the status carries no timestamp. Every status this ingress
// produces is stamped by a2a.NewTaskStatus, so the fallback covers a status that
// arrived from somewhere else rather than any normal path.
func a2aStatusTime(status a2a.TaskStatus, fallback time.Time) time.Time {
	if status.Timestamp != nil && !status.Timestamp.Time.IsZero() {
		return status.Timestamp.Time.UTC()
	}
	return fallback.UTC()
}
