package a2a

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine/storage"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// This file is the durable task store behind nexus.io.a2a.
//
// # Why it exists
//
// A2A tasks outlive the call that created them: GetTask, ListTasks and
// SubscribeToTask all assume a task is still there after its SendMessage has
// returned, and a client that reconnects after an agent restart expects the
// same. Holding tasks in a map would make every one of those operations a lie
// the moment the process exited, so the record lives in SQLite from the instant
// the task is created.
//
// # Where it lives
//
// On the engine's per-plugin storage capability at session scope —
// <session>/plugins/nexus.io.a2a/store.db — reached through
// PluginContext.Storage(storage.ScopeSession). Session scope is the honest one:
// a standalone listener serves exactly one Nexus session and binds exactly one
// A2A context to it (see bindContextLocked), so the task set and the session
// have identical lifetimes, and archiving the session disposes of its tasks
// without a second retention policy to keep in step. It is emphatically NOT a
// bespoke file format: the engine already owns connection pooling, WAL mode,
// busy timeouts and the on-disk path convention.
//
// # Principal scoping
//
// Scoping is structural, not a convention every call site has to remember.
// taskStore exposes no query at all: the only thing it can do is hand out a
// *principalTasks for a specific nexusauth.Principal, and every read and write
// lives on that type with `principal_id = ?` compiled into the statement. There
// is no sibling method that omits the predicate, so "list every task" is not an
// expression this package can form — a caller cannot reach another principal's
// task by forgetting a filter, because there is no unfiltered path to forget.
//
// The rule holds all the way down. The child tables are read through a join
// back onto tasks under the same predicate rather than "safely" by task id
// alone, and the child writes are INSERT..SELECT statements whose source row
// comes FROM tasks under that predicate — so each statement is scoped on its own
// terms rather than on a sibling statement having checked first. The invariant
// is one line long and machine-checkable: every SQL read in this file names
// principal_id. TestEverySelectIsPrincipalScoped enforces exactly that against
// the source, so an unscoped read cannot be added quietly.
//
// A live run never holds a *principalTasks directly; it holds a taskSink, which
// is the write-only subset. There is therefore no lookup at all within reach of
// the bus goroutines that translate a turn.

// taskStoreSchema is the durable shape of a task.
//
// The parent row carries the identity and the CURRENT status, denormalized out
// of the history table so the common read (what state is this task in?) is one
// indexed lookup rather than an ordered scan. task_status_history keeps the full
// transition trail; artifacts and message references hang off the same key and
// cascade on delete, which is what makes retention a single DELETE against
// tasks.
//
// terminal is a stored column rather than a computed predicate because eviction
// is driven by it: SQLite can index a column, not an IN-list evaluated per row,
// and retention runs on every task creation.
const taskStoreSchema = `
CREATE TABLE IF NOT EXISTS tasks (
	task_id        TEXT PRIMARY KEY,
	context_id     TEXT NOT NULL,
	principal_id   TEXT NOT NULL,
	principal      TEXT NOT NULL,
	state          TEXT NOT NULL,
	state_at       INTEGER NOT NULL,
	terminal       INTEGER NOT NULL,
	status_message TEXT,
	created_at     INTEGER NOT NULL,
	updated_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS tasks_by_principal_context
	ON tasks(principal_id, context_id, created_at DESC);
CREATE INDEX IF NOT EXISTS tasks_by_retention
	ON tasks(terminal, updated_at);

CREATE TABLE IF NOT EXISTS task_status_history (
	task_id TEXT NOT NULL REFERENCES tasks(task_id) ON DELETE CASCADE,
	seq     INTEGER NOT NULL,
	state   TEXT NOT NULL,
	at      INTEGER NOT NULL,
	message TEXT,
	PRIMARY KEY (task_id, seq)
);

CREATE TABLE IF NOT EXISTS task_artifacts (
	task_id     TEXT NOT NULL REFERENCES tasks(task_id) ON DELETE CASCADE,
	artifact_id TEXT NOT NULL,
	seq         INTEGER NOT NULL,
	artifact    TEXT NOT NULL,
	PRIMARY KEY (task_id, artifact_id)
);

CREATE TABLE IF NOT EXISTS task_messages (
	task_id    TEXT NOT NULL REFERENCES tasks(task_id) ON DELETE CASCADE,
	seq        INTEGER NOT NULL,
	message_id TEXT NOT NULL,
	role       TEXT NOT NULL,
	text       TEXT NOT NULL,
	PRIMARY KEY (task_id, seq)
);
`

// anonymousPrincipal is the principal key an unauthenticated caller is filed
// under.
//
// A listener with no bearer token and no auth: block admits every caller and
// nexusauth hands back the zero Principal, whose ID is empty. An empty string is
// a poor partition key — it reads as "missing" rather than "nobody" — so it is
// mapped to this sentinel instead. The parentheses are deliberate: no bearer
// token, JWT subject or proxy header this codebase accepts can produce a
// principal id shaped like that, so an authenticated caller can never land in
// the anonymous bucket. And the two cannot mix within one process anyway: the
// zero principal only occurs when the chain is disabled, in which case every
// caller is anonymous.
const anonymousPrincipal = "(unauthenticated)"

// principalKey reduces a Principal to the string tasks are partitioned by. ID is
// the only field authorization compares (see the nexusauth.Principal doc), so it
// is the only field the partition keys on.
func principalKey(p nexusauth.Principal) string {
	if id := strings.TrimSpace(p.ID); id != "" {
		return id
	}
	return anonymousPrincipal
}

// retention is the resolved task-retention policy. Both knobs are configurable
// and both default to a non-zero value; see defaultTaskTTL and
// defaultTasksPerContext for the reasoning behind the numbers.
type retention struct {
	// ttl is how long a terminal task is kept after its last transition. Zero
	// disables age-based eviction.
	ttl time.Duration
	// maxPerContext is how many tasks are kept per (principal, contextId).
	// Zero disables the cap.
	maxPerContext int
}

// messageRef is the record of one message in a task's history.
//
// It is a REFERENCE, not a second copy of the conversation: the authoritative
// transcript is memory.history's, and duplicating it here would give a task a
// divergent copy of the same turn. What is kept is the identity of the message —
// the client-assigned messageId and the role — plus the text it carried, because
// nothing in Nexus is addressable by A2A message id, so a reference that recorded
// only the id could never be resolved back to anything. Retention bounds the
// growth of this table, which is what makes that affordable once tool-result
// artifacts and inline file parts land on top of the same store.
type messageRef struct {
	MessageID string
	Role      a2a.Role
	Text      string
}

// taskRecord is one stored task, reassembled.
type taskRecord struct {
	TaskID    string
	ContextID string
	Principal nexusauth.Principal
	// Status is the current status: state, timestamp and any status message.
	Status a2a.TaskStatus
	// StatusHistory is every status this task has held, oldest first. The last
	// entry equals Status.
	StatusHistory []a2a.TaskStatus
	Artifacts     []a2a.Artifact
	Messages      []messageRef
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// renderOptions tunes how a stored record is projected onto the wire.
//
// Both knobs come straight off the request objects: GetTask and ListTasks each
// carry a historyLength, and ListTasks additionally defaults artifacts OFF so a
// page of tasks does not drag every artifact body with it.
type renderOptions struct {
	// historyLength caps how many of the MOST RECENT history messages the
	// rendered task carries. Nil means "no client-imposed limit" and zero means
	// "omit history entirely" — the presence distinction the specification
	// draws in section 3.2.4, which is why it is a pointer.
	historyLength *int
	// omitArtifacts drops the artifact bodies. ListTasks sets it unless the
	// caller asked for artifacts explicitly.
	omitArtifacts bool
}

// Task renders the record in the wire shape.
//
// # What History contains, stated plainly
//
// It is the trail of message REFERENCES this store retained, rendered as text
// messages — not a replay of Nexus's conversation. The authoritative transcript
// belongs to memory.history; what is kept here is, per exchange, the
// client-assigned messageId, the role and the text that travelled. Because this
// transport accepts text parts only and emits text artifacts only, that
// rendering is lossless for everything this agent can currently say or hear.
// What it would NOT survive is a part this transport does not yet accept: a
// file or data part would be recorded by its text (which is empty) and so is
// skipped rather than rendered as an empty message, since a2a.Message requires
// non-empty Parts.
//
// Section 3.7 explicitly leaves it to the server which messages are persisted
// and warns clients not to assume all of them are present, so a bounded
// reference trail is a conforming History rather than a partial one — and
// retention (tasks.max_per_context, tasks.ttl) is what bounds it.
//
// Each message is stamped with this task's id and context so a client can
// correlate it, exactly as the messages that produced it were.
func (r taskRecord) Task(opt renderOptions) a2a.Task {
	task := a2a.Task{
		ID:        r.TaskID,
		ContextID: r.ContextID,
		Status:    r.Status,
		History:   r.history(opt.historyLength),
	}
	if !opt.omitArtifacts {
		task.Artifacts = r.Artifacts
	}
	return task
}

// history renders the retained message references, honouring a client-supplied
// cap. The cap keeps the MOST RECENT messages, which is what "historyLength"
// means everywhere the specification uses it: a client asking for 2 wants the
// last exchange, not the first.
func (r taskRecord) history(limit *int) []a2a.Message {
	if limit != nil && *limit <= 0 {
		return nil
	}
	out := make([]a2a.Message, 0, len(r.Messages))
	for _, ref := range r.Messages {
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

// taskStore is the durable store. It deliberately exposes NO query surface: see
// the principal-scoping note at the top of this file. The only way to reach a
// task is through For.
type taskStore struct {
	db        *sql.DB
	retention retention
	logger    *slog.Logger
	// now is the clock, injectable so retention tests do not sleep.
	now func() time.Time

	// mu serializes every write.
	//
	// The storage handle is a pooled *sql.DB with several connections, and
	// SQLite admits exactly one writer at a time; concurrent write transactions
	// would otherwise spend the busy timeout contending instead of committing.
	// A task write is a few small statements, so serializing them costs nothing
	// measurable and removes an entire class of intermittent SQLITE_BUSY.
	// Reads are not serialized: WAL lets them proceed alongside the writer.
	mu sync.Mutex
}

// openTaskStore prepares the schema on a per-plugin storage handle and returns
// the store. It evicts once on open, so a process that was down while tasks aged
// out does not carry them back in.
func openTaskStore(st storage.Storage, policy retention, logger *slog.Logger) (*taskStore, error) {
	if st == nil {
		return nil, fmt.Errorf("%s: the task store requires a storage handle", pluginID)
	}
	db := st.DB()
	if _, err := db.Exec(taskStoreSchema); err != nil {
		return nil, fmt.Errorf("%s: creating the task store schema: %w", pluginID, err)
	}
	s := &taskStore{db: db, retention: policy, logger: logger, now: time.Now}
	// Order matters: settle the tasks a previous process abandoned FIRST, so the
	// retention sweep that follows can evict them. Retention only ever drops
	// terminal tasks, so a zombie left non-terminal would be immortal.
	if err := s.settleOrphans(); err != nil {
		return nil, fmt.Errorf("%s: settling tasks left in flight by a previous process: %w", pluginID, err)
	}
	if err := s.evict(); err != nil {
		return nil, fmt.Errorf("%s: applying task retention on open: %w", pluginID, err)
	}
	return s, nil
}

// orphanReason is the status message a task left in flight by a stopped process
// is settled with.
const orphanReason = "the agent stopped while this task was still running, so it will never complete"

// settleOrphans drives every non-terminal task to FAILED when the store opens.
//
// # Why a task can be non-terminal at open
//
// A task's state lives in two places while it runs: this store, and the in-memory
// run driving it. Only the store survives a process exit, so a task the process
// was serving when it stopped — a crash, a SIGKILL, an ordinary restart mid-turn —
// stays recorded in whatever state it last reached. Nothing will ever move it
// again: the run is gone, and no bus event will arrive for a turn that no longer
// exists.
//
// Leaving it as it stands is the worst option. A client polling GetTask sees
// WORKING for ever; SubscribeToTask can only hand it a snapshot and hang up,
// which reads like a dropped connection rather than an ending; and retention
// cannot evict it, because only terminal tasks are evictable — so a crash loop
// accumulates immortal rows that count against the per-context cap and push
// real tasks out of it.
//
// FAILED is the honest state. The work did not complete and nobody canceled it,
// which is exactly what FAILED means; CANCELED would claim a decision nobody
// made. The status message says what happened, so a client reading it back is
// told the difference between "the agent failed at this" and "the agent went
// away".
//
// # Why it is written through the principal-scoped path
//
// The repair does the same writes a live transition does — RecordStatus on a
// principal-scoped view — rather than one broad UPDATE. The read below PROJECTS
// each task's principal instead of filtering by one, which is what a
// process-wide repair needs and what no request-serving path may ever do; every
// write it drives then goes through the ordinary scoped statement, so the
// store's "no unscoped write" invariant holds through the repair as well.
func (s *taskStore) settleOrphans() error {
	rows, err := s.db.Query(
		`SELECT task_id, principal_id, principal FROM tasks WHERE terminal = 0`)
	if err != nil {
		return fmt.Errorf("reading tasks left in flight: %w", err)
	}
	type orphan struct {
		taskID    string
		principal nexusauth.Principal
	}
	var orphans []orphan
	for rows.Next() {
		var (
			taskID        string
			principalID   string
			principalJSON string
		)
		if err := rows.Scan(&taskID, &principalID, &principalJSON); err != nil {
			rows.Close()
			return fmt.Errorf("reading tasks left in flight: %w", err)
		}
		var p nexusauth.Principal
		if err := json.Unmarshal([]byte(principalJSON), &p); err != nil {
			rows.Close()
			return fmt.Errorf("decoding the principal of task %q: %w", taskID, err)
		}
		orphans = append(orphans, orphan{taskID: taskID, principal: p})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("reading tasks left in flight: %w", err)
	}
	rows.Close()

	for _, o := range orphans {
		status := a2a.NewTaskStatus(a2a.TaskStateFailed).WithMessage(
			a2a.NewMessage(newMessageID(), a2a.RoleAgent, a2a.TextPart(orphanReason)))
		if err := s.For(o.principal).RecordStatus(o.taskID, status); err != nil {
			return fmt.Errorf("settling task %q: %w", o.taskID, err)
		}
	}
	if len(orphans) > 0 && s.logger != nil {
		s.logger.Warn("a2a tasks were left in flight by a previous process and have been failed",
			"tasks", len(orphans))
	}
	return nil
}

// For returns the view of the store belonging to one principal. It is the ONLY
// way to reach a task, which is what makes an unscoped read impossible to write
// rather than merely discouraged.
func (s *taskStore) For(p nexusauth.Principal) *principalTasks {
	return &principalTasks{store: s, principal: p.Clone(), key: principalKey(p)}
}

// evict applies both retention knobs. It runs on open and after every task
// creation, so the store is bounded by the event that grows it.
//
// Only TERMINAL tasks are evictable. A live task is the one thing a client is
// certain to ask about — it is the task it is streaming right now — and dropping
// it mid-turn to satisfy a cap would turn a retention policy into a correctness
// bug. Non-terminal tasks still COUNT against the per-context cap, so an agent
// wedged with stale in-flight tasks shows up as pressure on retention rather
// than silently exempting itself from it.
func (s *taskStore) evict() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evictLocked()
}

func (s *taskStore) evictLocked() error {
	if s.retention.ttl > 0 {
		cutoff := s.now().Add(-s.retention.ttl).UnixNano()
		if _, err := s.db.Exec(
			`DELETE FROM tasks WHERE terminal = 1 AND updated_at < ?`, cutoff); err != nil {
			return fmt.Errorf("evicting expired tasks: %w", err)
		}
	}
	if s.retention.maxPerContext > 0 {
		// The window ranks every task in the (principal, context) partition,
		// newest first, and the delete takes the overflow that is also terminal.
		// Ranking over ALL tasks and then filtering to terminal ones is what
		// makes a live task count against the cap without being evictable.
		//
		// The cap is per (principal, context) rather than per context alone so
		// that one principal's traffic cannot push another principal's tasks out
		// of the store — an eviction channel is still a channel.
		if _, err := s.db.Exec(`
			DELETE FROM tasks WHERE task_id IN (
				SELECT task_id FROM (
					SELECT task_id, terminal, ROW_NUMBER() OVER (
						PARTITION BY principal_id, context_id
						ORDER BY created_at DESC, rowid DESC
					) AS rn
					FROM tasks
				) WHERE rn > ? AND terminal = 1
			)`, s.retention.maxPerContext); err != nil {
			return fmt.Errorf("evicting tasks over the per-context cap: %w", err)
		}
	}
	return nil
}

// taskSink is the write-through surface a live run holds.
//
// It is deliberately WRITE-ONLY. A run translates bus events into A2A frames on
// arbitrary engine goroutines; giving it a read method would put an unscoped
// lookup one autocomplete away from the hot path. The only implementation is
// *principalTasks, so a run can only ever write to the task of the principal
// that created it.
type taskSink interface {
	RecordStatus(taskID string, status a2a.TaskStatus) error
	RecordArtifact(taskID string, artifact a2a.Artifact) error
	RecordMessage(taskID string, ref messageRef) error
}

var _ taskSink = (*principalTasks)(nil)

// principalTasks is the principal-scoped view of the store. Every statement it
// issues carries `principal_id = ?` bound from key, and no method omits it.
type principalTasks struct {
	store     *taskStore
	principal nexusauth.Principal
	key       string
}

// Principal returns the identity this view is scoped to.
func (t *principalTasks) Principal() nexusauth.Principal { return t.principal.Clone() }

// Create records a new task and its opening status, then applies retention.
//
// The inbound message reference is written in the same transaction as the task:
// a task whose originating message is missing is a task nobody can explain, and
// two statements outside a transaction is exactly how that happens.
func (t *principalTasks) Create(taskID, contextID string, status a2a.TaskStatus, inbound messageRef) error {
	if taskID == "" {
		return fmt.Errorf("%s: creating a task requires a task id", pluginID)
	}
	principalJSON, err := json.Marshal(t.principal)
	if err != nil {
		return fmt.Errorf("%s: encoding the principal for task %q: %w", pluginID, taskID, err)
	}
	statusMessage, err := encodeStatusMessage(status)
	if err != nil {
		return fmt.Errorf("%s: encoding the status message for task %q: %w", pluginID, taskID, err)
	}
	at := statusTime(status, t.store.now())
	now := t.store.now().UnixNano()

	t.store.mu.Lock()
	defer t.store.mu.Unlock()

	if err := withTx(t.store.db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			INSERT INTO tasks(task_id, context_id, principal_id, principal, state, state_at,
			                  terminal, status_message, created_at, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			taskID, contextID, t.key, string(principalJSON), string(status.State), at,
			boolToInt(status.State.IsTerminal()), statusMessage, now, now); err != nil {
			return fmt.Errorf("inserting the task: %w", err)
		}
		if _, err := tx.Exec(`
			INSERT INTO task_status_history(task_id, seq, state, at, message) VALUES(?, 0, ?, ?, ?)`,
			taskID, string(status.State), at, statusMessage); err != nil {
			return fmt.Errorf("inserting the opening status: %w", err)
		}
		if inbound.MessageID != "" || inbound.Text != "" {
			if _, err := tx.Exec(`
				INSERT INTO task_messages(task_id, seq, message_id, role, text) VALUES(?, 0, ?, ?, ?)`,
				taskID, inbound.MessageID, string(inbound.Role), inbound.Text); err != nil {
				return fmt.Errorf("inserting the inbound message reference: %w", err)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("%s: recording task %q: %w", pluginID, taskID, err)
	}

	// Retention runs after the insert, not before: the task that just arrived is
	// the newest in its partition and so is never its own eviction candidate,
	// and running it here means the store is bounded by the same event that
	// grows it.
	if err := t.store.evictLocked(); err != nil {
		return fmt.Errorf("%s: %w", pluginID, err)
	}
	return nil
}

// RecordStatus appends a transition and updates the task's current status.
//
// The UPDATE matches on principal_id as well as task_id, so a view belonging to
// one principal cannot move another principal's task — and because the history
// row is written in the same transaction, and only once the update has matched,
// it cannot leave a history entry on a task it does not own either.
func (t *principalTasks) RecordStatus(taskID string, status a2a.TaskStatus) error {
	statusMessage, err := encodeStatusMessage(status)
	if err != nil {
		return fmt.Errorf("%s: encoding the status message for task %q: %w", pluginID, taskID, err)
	}
	at := statusTime(status, t.store.now())
	now := t.store.now().UnixNano()

	t.store.mu.Lock()
	defer t.store.mu.Unlock()

	if err := withTx(t.store.db, func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			UPDATE tasks SET state = ?, state_at = ?, terminal = ?, status_message = ?, updated_at = ?
			WHERE task_id = ? AND principal_id = ?`,
			string(status.State), at, boolToInt(status.State.IsTerminal()), statusMessage, now,
			taskID, t.key)
		if err != nil {
			return fmt.Errorf("updating the current status: %w", err)
		}
		if err := requireRow(res); err != nil {
			return err
		}
		// The history row is selected FROM tasks under the same predicate, so it
		// is scoped on its own terms rather than on the update above having
		// matched. Every statement in this file that touches a task carries
		// principal_id; none of them relies on a sibling statement for it.
		if _, err := tx.Exec(`
			INSERT INTO task_status_history(task_id, seq, state, at, message)
			SELECT t.task_id,
			       COALESCE((SELECT MAX(h.seq) + 1 FROM task_status_history h WHERE h.task_id = t.task_id), 0),
			       ?, ?, ?
			FROM tasks t WHERE t.task_id = ? AND t.principal_id = ?`,
			string(status.State), at, statusMessage, taskID, t.key); err != nil {
			return fmt.Errorf("appending the status history: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("%s: recording the %s status of task %q: %w", pluginID, status.State, taskID, err)
	}
	return nil
}

// RecordArtifact stores an artifact against a task. Artifact ids are unique
// within a task, so a repeat write of the same id replaces it — which is what a
// re-rendered artifact should do, rather than accumulate duplicates.
func (t *principalTasks) RecordArtifact(taskID string, artifact a2a.Artifact) error {
	encoded, err := json.Marshal(artifact)
	if err != nil {
		return fmt.Errorf("%s: encoding artifact %q of task %q: %w", pluginID, artifact.ArtifactID, taskID, err)
	}
	now := t.store.now().UnixNano()

	t.store.mu.Lock()
	defer t.store.mu.Unlock()

	if err := withTx(t.store.db, func(tx *sql.Tx) error {
		// Selecting the parent row FROM tasks under the principal predicate is
		// what scopes the write: an unowned task selects nothing, so nothing is
		// inserted and RowsAffected reports the refusal.
		res, err := tx.Exec(`
			INSERT INTO task_artifacts(task_id, artifact_id, seq, artifact)
			SELECT t.task_id, ?,
			       COALESCE((SELECT MAX(a.seq) + 1 FROM task_artifacts a WHERE a.task_id = t.task_id), 0),
			       ?
			FROM tasks t WHERE t.task_id = ? AND t.principal_id = ?
			ON CONFLICT(task_id, artifact_id) DO UPDATE SET artifact = excluded.artifact`,
			artifact.ArtifactID, string(encoded), taskID, t.key)
		if err != nil {
			return fmt.Errorf("inserting the artifact: %w", err)
		}
		if err := requireRow(res); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE tasks SET updated_at = ? WHERE task_id = ? AND principal_id = ?`,
			now, taskID, t.key); err != nil {
			return fmt.Errorf("touching the task: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("%s: recording artifact %q of task %q: %w", pluginID, artifact.ArtifactID, taskID, err)
	}
	return nil
}

// RecordMessage appends a message reference to a task's history.
func (t *principalTasks) RecordMessage(taskID string, ref messageRef) error {
	now := t.store.now().UnixNano()

	t.store.mu.Lock()
	defer t.store.mu.Unlock()

	if err := withTx(t.store.db, func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			INSERT INTO task_messages(task_id, seq, message_id, role, text)
			SELECT t.task_id,
			       COALESCE((SELECT MAX(m.seq) + 1 FROM task_messages m WHERE m.task_id = t.task_id), 0),
			       ?, ?, ?
			FROM tasks t WHERE t.task_id = ? AND t.principal_id = ?`,
			ref.MessageID, string(ref.Role), ref.Text, taskID, t.key)
		if err != nil {
			return fmt.Errorf("inserting the message reference: %w", err)
		}
		if err := requireRow(res); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE tasks SET updated_at = ? WHERE task_id = ? AND principal_id = ?`,
			now, taskID, t.key); err != nil {
			return fmt.Errorf("touching the task: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("%s: recording a message reference on task %q: %w", pluginID, taskID, err)
	}
	return nil
}

// Get returns one task belonging to this principal. The bool reports presence;
// another principal's task is reported ABSENT rather than forbidden, because a
// distinct "exists but is not yours" answer is an existence oracle for task ids
// this caller was never told.
func (t *principalTasks) Get(taskID string) (taskRecord, bool, error) {
	row := t.store.db.QueryRow(`
		SELECT context_id, principal, state, state_at, status_message, created_at, updated_at
		FROM tasks WHERE task_id = ? AND principal_id = ?`, taskID, t.key)

	var (
		contextID     string
		principalJSON string
		state         string
		stateAt       int64
		statusMessage sql.NullString
		createdAt     int64
		updatedAt     int64
	)
	switch err := row.Scan(&contextID, &principalJSON, &state, &stateAt, &statusMessage,
		&createdAt, &updatedAt); {
	case err == sql.ErrNoRows:
		return taskRecord{}, false, nil
	case err != nil:
		return taskRecord{}, false, fmt.Errorf("%s: reading task %q: %w", pluginID, taskID, err)
	}

	rec := taskRecord{
		TaskID:    taskID,
		ContextID: contextID,
		CreatedAt: time.Unix(0, createdAt).UTC(),
		UpdatedAt: time.Unix(0, updatedAt).UTC(),
	}
	if err := json.Unmarshal([]byte(principalJSON), &rec.Principal); err != nil {
		return taskRecord{}, false, fmt.Errorf("%s: decoding the principal of task %q: %w", pluginID, taskID, err)
	}
	status, err := decodeStatus(state, stateAt, statusMessage)
	if err != nil {
		return taskRecord{}, false, fmt.Errorf("%s: decoding the status of task %q: %w", pluginID, taskID, err)
	}
	rec.Status = status

	if rec.StatusHistory, err = t.statusHistory(taskID); err != nil {
		return taskRecord{}, false, err
	}
	if rec.Artifacts, err = t.artifacts(taskID); err != nil {
		return taskRecord{}, false, err
	}
	if rec.Messages, err = t.messages(taskID); err != nil {
		return taskRecord{}, false, err
	}
	return rec, true, nil
}

// taskQuery is one ListTasks request reduced to what the store can answer.
//
// Every field is a NARROWING filter; none of them can widen the result set past
// the principal the view is bound to, because that predicate is not part of this
// struct at all — it is compiled into the statement. There is deliberately no
// "principal" field here for a caller to set, forget or get wrong.
type taskQuery struct {
	// contextID restricts the listing to one conversational context. Empty means
	// every context this principal has.
	contextID string
	// state restricts the listing to tasks currently in one state. Empty means
	// any state.
	state a2a.TaskState
	// changedAfter restricts the listing to tasks whose CURRENT status was
	// observed at or after this instant. The zero time means no lower bound.
	changedAfter time.Time
	// after is the keyset cursor a previous page ended on. The zero value starts
	// at the newest task.
	after listCursor
	// limit caps the page. Zero or less means no limit.
	limit int
}

// listCursor is the keyset position a page of tasks ended on.
//
// It is a keyset — (created_at, rowid), the exact tuple the listing is ordered
// by — rather than an offset, because an offset cursor silently skips or repeats
// rows when the underlying set changes between pages, and this set changes on
// every turn and on every retention sweep. A keyset cursor names a POSITION, so
// a task created or evicted while a client pages through cannot shift the rows
// it has not seen yet.
//
// rowid breaks ties: created_at is nanosecond-precision but two tasks created
// inside one clock tick would otherwise be an ambiguous position, and an
// ambiguous cursor is an infinite loop or a dropped row.
type listCursor struct {
	createdAt int64
	rowID     int64
	// set distinguishes "start from the beginning" from a cursor that happens to
	// carry zeroes.
	set bool
}

// List returns this principal's tasks, newest first. An empty contextID means
// "every context this principal has" — which is still only this principal's
// tasks, because the predicate on principal_id is not optional.
//
// A limit of zero or less means no limit. It is the unfiltered, unpaginated
// shorthand for Query.
func (t *principalTasks) List(contextID string, limit int) ([]taskRecord, error) {
	recs, _, _, err := t.Query(taskQuery{contextID: contextID, limit: limit})
	return recs, err
}

// Query returns one page of this principal's tasks, newest first, along with the
// cursor for the following page and the total number of tasks MATCHING THE
// FILTERS before pagination.
//
// The total is computed under the identical predicate, in the same call, so a
// client's "3 of 40" is 40 of the same thing it is paging through rather than a
// count of some looser set.
//
// The returned cursor's set field reports whether a further page exists: the
// page query asks for one row more than the limit and the extra row is dropped,
// so "is there more" is answered by the same statement that answers "what is on
// this page" instead of by a second round trip that could disagree with it.
func (t *principalTasks) Query(q taskQuery) ([]taskRecord, listCursor, int, error) {
	filter, args := q.predicate()

	// Both statements below repeat `WHERE principal_id = ?` in their own literal
	// rather than sharing a fragment that carries it. That is what keeps the
	// invariant checkable by reading the file: every SQL literal here that reads
	// a task names principal_id itself.
	var total int
	if err := t.store.db.QueryRow(
		`SELECT COUNT(*) FROM tasks WHERE principal_id = ?`+filter,
		append([]any{t.key}, args...)...,
	).Scan(&total); err != nil {
		return nil, listCursor{}, 0, fmt.Errorf("%s: counting tasks: %w", pluginID, err)
	}

	pageArgs := append([]any{t.key}, args...)
	page := `SELECT task_id, created_at, rowid FROM tasks WHERE principal_id = ?` + filter
	if q.after.set {
		// Strictly AFTER the cursor in the listing's own order, which is
		// descending: a smaller (created_at, rowid) is a later row.
		page += ` AND (created_at < ? OR (created_at = ? AND rowid < ?))`
		pageArgs = append(pageArgs, q.after.createdAt, q.after.createdAt, q.after.rowID)
	}
	page += ` ORDER BY created_at DESC, rowid DESC`
	if q.limit > 0 {
		page += ` LIMIT ?`
		pageArgs = append(pageArgs, q.limit+1)
	}

	ids, cursors, err := t.page(page, pageArgs)
	if err != nil {
		return nil, listCursor{}, 0, err
	}

	var next listCursor
	if q.limit > 0 && len(ids) > q.limit {
		ids = ids[:q.limit]
		cursors = cursors[:q.limit]
		next = cursors[len(cursors)-1]
	}

	out := make([]taskRecord, 0, len(ids))
	for _, id := range ids {
		rec, found, err := t.Get(id)
		if err != nil {
			return nil, listCursor{}, 0, err
		}
		if found {
			out = append(out, rec)
		}
	}
	return out, next, total, nil
}

// predicate renders the query's narrowing filters as a SQL fragment appended to
// a statement that has ALREADY named principal_id. It never emits a principal
// predicate of its own, so it cannot accidentally replace one.
func (q taskQuery) predicate() (string, []any) {
	var (
		filter string
		args   []any
	)
	if q.contextID != "" {
		filter += ` AND context_id = ?`
		args = append(args, q.contextID)
	}
	if q.state != "" {
		filter += ` AND state = ?`
		args = append(args, string(q.state))
	}
	if !q.changedAfter.IsZero() {
		// state_at is the instant the CURRENT status was observed, which is what
		// the specification's statusTimestampAfter filter selects on. The bound
		// is inclusive, as the field name's "after" is defined to be in section
		// 3.2 ("at or after").
		filter += ` AND state_at >= ?`
		args = append(args, q.changedAfter.UTC().UnixNano())
	}
	return filter, args
}

// page runs a scoped page query, returning the task ids in order alongside the
// cursor position of each. It is separate only so the rows cursor is closed
// before the per-task assembly reads run, which keeps a single pooled connection
// from being held open across them.
func (t *principalTasks) page(query string, args []any) ([]string, []listCursor, error) {
	rows, err := t.store.db.Query(query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: listing tasks: %w", pluginID, err)
	}
	defer rows.Close()

	var (
		ids     []string
		cursors []listCursor
	)
	for rows.Next() {
		var (
			id        string
			createdAt int64
			rowID     int64
		)
		if err := rows.Scan(&id, &createdAt, &rowID); err != nil {
			return nil, nil, fmt.Errorf("%s: listing tasks: %w", pluginID, err)
		}
		ids = append(ids, id)
		cursors = append(cursors, listCursor{createdAt: createdAt, rowID: rowID, set: true})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("%s: listing tasks: %w", pluginID, err)
	}
	return ids, cursors, nil
}

// --- record assembly ---
//
// Each helper below joins its child table back onto tasks under the principal
// predicate rather than trusting Get to have checked ownership first. They are
// unexported and only Get calls them today, but "safe because of who calls it"
// is the property that decays; "safe because the statement says principal_id"
// is the property that does not.

func (t *principalTasks) statusHistory(taskID string) ([]a2a.TaskStatus, error) {
	rows, err := t.store.db.Query(`
		SELECT h.state, h.at, h.message
		FROM task_status_history h JOIN tasks t ON t.task_id = h.task_id
		WHERE h.task_id = ? AND t.principal_id = ?
		ORDER BY h.seq`, taskID, t.key)
	if err != nil {
		return nil, fmt.Errorf("%s: reading the status history of task %q: %w", pluginID, taskID, err)
	}
	defer rows.Close()

	var out []a2a.TaskStatus
	for rows.Next() {
		var (
			state   string
			at      int64
			message sql.NullString
		)
		if err := rows.Scan(&state, &at, &message); err != nil {
			return nil, fmt.Errorf("%s: reading the status history of task %q: %w", pluginID, taskID, err)
		}
		status, err := decodeStatus(state, at, message)
		if err != nil {
			return nil, fmt.Errorf("%s: decoding the status history of task %q: %w", pluginID, taskID, err)
		}
		out = append(out, status)
	}
	return out, rows.Err()
}

func (t *principalTasks) artifacts(taskID string) ([]a2a.Artifact, error) {
	rows, err := t.store.db.Query(`
		SELECT a.artifact
		FROM task_artifacts a JOIN tasks t ON t.task_id = a.task_id
		WHERE a.task_id = ? AND t.principal_id = ?
		ORDER BY a.seq`, taskID, t.key)
	if err != nil {
		return nil, fmt.Errorf("%s: reading the artifacts of task %q: %w", pluginID, taskID, err)
	}
	defer rows.Close()

	var out []a2a.Artifact
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, fmt.Errorf("%s: reading the artifacts of task %q: %w", pluginID, taskID, err)
		}
		var artifact a2a.Artifact
		if err := json.Unmarshal([]byte(encoded), &artifact); err != nil {
			return nil, fmt.Errorf("%s: decoding an artifact of task %q: %w", pluginID, taskID, err)
		}
		out = append(out, artifact)
	}
	return out, rows.Err()
}

func (t *principalTasks) messages(taskID string) ([]messageRef, error) {
	rows, err := t.store.db.Query(`
		SELECT m.message_id, m.role, m.text
		FROM task_messages m JOIN tasks t ON t.task_id = m.task_id
		WHERE m.task_id = ? AND t.principal_id = ?
		ORDER BY m.seq`, taskID, t.key)
	if err != nil {
		return nil, fmt.Errorf("%s: reading the message references of task %q: %w", pluginID, taskID, err)
	}
	defer rows.Close()

	var out []messageRef
	for rows.Next() {
		var (
			ref  messageRef
			role string
		)
		if err := rows.Scan(&ref.MessageID, &role, &ref.Text); err != nil {
			return nil, fmt.Errorf("%s: reading the message references of task %q: %w", pluginID, taskID, err)
		}
		ref.Role = a2a.Role(role)
		out = append(out, ref)
	}
	return out, rows.Err()
}

// --- helpers ---

// errNoSuchTask is the sentinel a scoped write returns when no row matched BOTH
// the task id and the principal. The caller wraps it with context; it is not
// surfaced as a distinguishable condition to a client, for the same reason Get
// reports a foreign task as absent.
var errNoSuchTask = fmt.Errorf("no task with that id belongs to this principal")

// requireRow turns "the scoped statement matched nothing" into errNoSuchTask.
//
// Every write in this file is expressed so that the principal predicate decides
// whether a row is produced at all — an UPDATE with the predicate in its WHERE,
// or an INSERT whose SELECT reads the parent row FROM tasks under it. A zero
// row count therefore means exactly one thing: no task with that id belongs to
// this principal. That is the whole ownership check; there is no separate
// pre-flight lookup that a later edit could forget to call.
func requireRow(res sql.Result) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("counting the affected rows: %w", err)
	}
	if affected == 0 {
		return errNoSuchTask
	}
	return nil
}

// withTx runs fn inside a transaction, committing on success and rolling back on
// error or panic.
//
// It exists rather than storage.Storage.Tx because the store holds only the
// *sql.DB: the handle is opened once in Init and the schema is this package's,
// so threading the Storage interface through purely to reach its Tx wrapper
// would add an indirection without adding a guarantee.
func withTx(db *sql.DB, fn func(*sql.Tx) error) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("committing: %w", err)
	}
	return nil
}

// encodeStatusMessage renders a status's optional message for storage. An absent
// message is stored as SQL NULL rather than an empty string, so "no message" and
// "a message that encoded to nothing" stay distinguishable.
func encodeStatusMessage(status a2a.TaskStatus) (any, error) {
	if status.Message == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(status.Message)
	if err != nil {
		return nil, err
	}
	return string(encoded), nil
}

// decodeStatus rebuilds a TaskStatus from its stored columns.
func decodeStatus(state string, at int64, message sql.NullString) (a2a.TaskStatus, error) {
	status := a2a.TaskStatus{
		State:     a2a.TaskState(state),
		Timestamp: a2a.NewTimestamp(time.Unix(0, at)),
	}
	if message.Valid && message.String != "" {
		var m a2a.Message
		if err := json.Unmarshal([]byte(message.String), &m); err != nil {
			return a2a.TaskStatus{}, err
		}
		status.Message = &m
	}
	return status, nil
}

// statusTime returns the instant a status was observed, falling back to the
// store's clock when the status carries no timestamp. Every status this plugin
// produces is stamped by a2a.NewTaskStatus, so the fallback covers a status that
// arrived from somewhere else rather than any normal path.
func statusTime(status a2a.TaskStatus, fallback time.Time) int64 {
	if status.Timestamp != nil && !status.Timestamp.Time.IsZero() {
		return status.Timestamp.Time.UTC().UnixNano()
	}
	return fallback.UTC().UnixNano()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
