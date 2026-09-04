package events

import "time"

// Schema-version constants for session.* payloads. See doc.go.
const (
	// SessionFileVersion is 2 because version 1 named an Action field and no
	// SessionID, Offset or BytesAdded -- but nothing ever emitted it, so 2 is
	// the first version any subscriber will see. See SessionFile.
	//
	// No compat migrator is registered for 1->2, deliberately. The struct
	// changed; the wire did not, beyond gaining _schema_version itself. A
	// journaled payload written before this carries the same five keys under
	// the same names, so it deserializes to a v2 payload with the version
	// reading 0 -- which the v0 == v1 rule in pkg/events/compat already covers,
	// and which every subscriber reads by key regardless.
	SessionFileVersion            = 2
	SessionSnapshotRequestVersion = 1
	SessionSnapshotResultVersion  = 1
	SessionOwnerConflictVersion   = 1

	SessionStorageDegradedVersion  = 1
	SessionStorageRecoveredVersion = 1
)

// SessionFile describes a file event within a session workspace: the payload of
// session.file.created and session.file.updated.
//
// It goes on the wire as a map rather than as this struct, via Map below. That
// is not an oversight -- every existing subscriber type-asserts
// map[string]any, and changing the payload type would break all of them for no
// gain. The struct is the *definition* of that map, and Map is the only place
// the keys are spelled.
//
// Before that, this struct was declared and nothing used it, while four call
// sites each spelled the same keys out by hand. `make check-events` guards
// structs in this package, so it was guarding a type no subscriber could ever
// see, and guarding nothing at all about the shape that actually shipped. Two
// hand-built emitters had already drifted -- one publishing a bare basename as
// path, one an absolute host path with no session_id -- and the guard could not
// have caught either.
//
// There is deliberately no Action field. The action is the event type
// (session.file.created vs .updated), and a duplicate of it in the payload is a
// second source of truth that can disagree with the first -- which is exactly
// what nexus.tool.pdf did, hardcoding "created" on every write including
// updates.
type SessionFile struct {
	SchemaVersion int `json:"_schema_version"`

	SessionID string // the session this file belongs to
	// Path is relative to the session root and slash-separated, so it can be
	// used directly as an object key under the session's prefix.
	Path string
	// Size is the file's size after the write.
	//
	// int, not int64, because int is what has always been on the wire: this
	// struct never reached a subscriber, and every subscriber reading the map
	// type-asserts .(int). Widening the field here would widen the payload and
	// turn every one of those assertions into a silent zero -- a wire break
	// dressed up as a type cleanup. The declared int64 was decorative.
	Size int
	// Offset is where the write began: 0 for a whole-file write, the previous
	// length for an append. BytesAdded is how much it added. Together they let
	// a subscriber upload a delta instead of re-reading the whole file; a
	// subscriber that ignores both is still correct, just slower.
	Offset     int
	BytesAdded int
}

// Map renders the payload in the form that goes on the bus.
//
// The keys here are the wire contract. TestSessionFileMapCoversEveryField
// asserts one key per exported field, so a field added to the struct without a
// key here fails the build rather than shipping a payload that silently omits
// it; a field renamed or retyped trips `make check-events`, which is what binds
// the guard to the shape that actually ships.
func (f SessionFile) Map() map[string]any {
	return map[string]any{
		"_schema_version": f.SchemaVersion,
		"session_id":      f.SessionID,
		"path":            f.Path,
		"size":            f.Size,
		"offset":          f.Offset,
		"bytes_added":     f.BytesAdded,
	}
}

// SessionSnapshotRequest asks the engine to snapshot the whole session tree to
// the configured object store and wait for it to be durable.
//
// The engine snapshots at every turn boundary on its own, so nothing needs to
// emit this in a normal run. It exists for the two cases the turn boundary does
// not cover: an embedder driving the engine outside an agent loop, and a
// custom agent that does not emit agent.turn.end. Ignored — not an error — when
// no object-store backend is configured.
type SessionSnapshotRequest struct {
	SchemaVersion int `json:"_schema_version"`

	// Reason is free-form and appears in the log line and in the resulting
	// SessionSnapshotResult, so an operator can tell one caller from another.
	Reason string `json:"reason,omitempty"`
}

// SessionSnapshotResult reports the outcome of one whole-tree snapshot.
//
// Emitted after the snapshot has either been made durable or failed, so a
// subscriber observing OK can rely on the remote copy being restorable. Not
// emitted at all when no object-store backend is configured.
type SessionSnapshotResult struct {
	SchemaVersion int `json:"_schema_version"`

	SessionID string `json:"session_id"`
	// Trigger is what caused the snapshot: "turn", "shutdown", "request" or
	// "retry" (the recovery worker healing a degraded store).
	Trigger string `json:"trigger"`
	// Sequence is the per-run snapshot counter, starting at 1.
	Sequence uint64 `json:"sequence"`
	// Generation is the session's commit generation: unlike Sequence it is
	// seeded from the manifest the previous holder of the session committed,
	// so it keeps increasing across a resume onto a different host. It is the
	// stamp the commit marker and the per-object manifest in the bucket both
	// carry, so a subscriber can name the exact remote state a snapshot
	// produced. Zero when a snapshot failed before it could claim one.
	Generation uint64 `json:"generation"`
	// TurnID is the turn whose boundary triggered the snapshot. Empty for the
	// shutdown and request triggers.
	TurnID string `json:"turn_id,omitempty"`
	// Objects is the size of the committed object set — every object the
	// snapshot asserts is durably present, excluding the commit marker. It
	// counts objects that immutable-skip did not re-upload, because they are
	// still part of the stored session.
	Objects int `json:"objects"`
	// Bytes is the total size of those objects: how big the stored session is.
	Bytes int64 `json:"bytes"`
	// ObjectsUploaded and BytesUploaded are the share of the set this snapshot
	// actually transferred. This, not Bytes, is the per-turn cost.
	ObjectsUploaded int   `json:"objects_uploaded"`
	BytesUploaded   int64 `json:"bytes_uploaded"`
	// ObjectsSkipped and BytesSkipped are what immutable-skip saved: files
	// whose identity proves they cannot have changed (sealed journal segments,
	// content-addressed blobs) and which the store was listed to confirm it
	// already holds.
	ObjectsSkipped int   `json:"objects_skipped"`
	BytesSkipped   int64 `json:"bytes_skipped"`
	// DurationMs is the wall time of the whole snapshot, staging included.
	DurationMs float64 `json:"duration_ms"`
	// OK reports whether the snapshot was made durable. When false the remote
	// commit marker still names the previous snapshot.
	OK bool `json:"ok"`
	// ErrorMessage is empty on success.
	ErrorMessage string `json:"error,omitempty"`
}

// SessionOwnerConflict reports that a second host appears to hold the session
// this process just opened against the object store.
//
// # Detection, not prevention
//
// Single-writer is assumed by the object-store design and deliberately not
// enforced. Nothing about this event refuses, blocks, retries or fences: by the
// time it is emitted the engine has already claimed the session and is running
// normally. It exists because the failure it describes is otherwise completely
// silent — two hosts snapshot the same session at their own turn boundaries and
// the loser's whole tree, per-plugin databases included, is overwritten with no
// error anywhere. A subscriber that wants to act (page an operator, stop the
// run) has to do so itself.
//
// Emitted at most once per run, and only when an object-store backend is
// configured.
type SessionOwnerConflict struct {
	SchemaVersion int `json:"_schema_version"`

	SessionID string `json:"session_id"`

	// HolderHost, HolderPID and HolderInstanceID identify the process that
	// wrote the owner marker found in the store. HolderInstanceID is unique
	// per engine run, which is what makes two containers that happen to share
	// a hostname and a PID still distinguishable.
	HolderHost       string `json:"holder_host"`
	HolderPID        int    `json:"holder_pid"`
	HolderInstanceID string `json:"holder_instance_id"`
	// HolderHeartbeatAt is the last heartbeat the holder recorded, by the
	// holder's own clock.
	HolderHeartbeatAt time.Time `json:"holder_heartbeat_at"`
	// HeartbeatAgeSeconds is how old that heartbeat looked from here. Carried
	// alongside the timestamp because the two clocks are not the same one, and
	// the age is the number the staleness decision was actually made on.
	HeartbeatAgeSeconds float64 `json:"heartbeat_age_seconds"`

	// LocalHost, LocalPID and LocalInstanceID identify this process — the one
	// that went ahead anyway.
	LocalHost       string `json:"local_host"`
	LocalPID        int    `json:"local_pid"`
	LocalInstanceID string `json:"local_instance_id"`
}

// SessionStorageDegraded reports that the configured object store stopped
// accepting this session's state, and that the engine is running against the
// local working copy while it retries.
//
// Emitted at most once per outage — an "episode" runs from the first failure to
// the first success after it, so a subscriber counts outages rather than
// failed requests — and only when `core.object_store.backend` names a backend.
// `session.storage.recovered` closes the episode.
//
// # What it means under each failure policy
//
// Under `degrade` the session keeps taking turns. The honest caveat is that the
// durability guarantee is *not* being met for as long as this state lasts, even
// though nothing is failing: turns the user watched happen exist only on local
// disk. That is the trade the operator chose.
//
// Under `strict` `TurnsBlocked` is true and every subsequent `io.input` is
// vetoed until the state is durably stored. Note carefully what that does and
// does not mean: the turn that hit the outage ALREADY RAN — its output was
// streamed, its tools executed and its side effects are in the world. The
// engine cannot un-run it. What `strict` guarantees is that no *further* turn
// runs against state whose predecessor is not stored, and that the divergence
// is never silent.
//
// Recovery is automatic under both policies. Nothing here needs an operator.
type SessionStorageDegraded struct {
	SchemaVersion int `json:"_schema_version"`

	SessionID string `json:"session_id"`
	// Backend is the registered backend name from core.object_store.backend.
	Backend string `json:"backend"`
	// FailurePolicy is the resolved core.object_store.failure_policy:
	// "degrade" or "strict".
	FailurePolicy string `json:"failure_policy"`

	// Since is when the episode opened — the first failure, not this event.
	Since time.Time `json:"since"`
	// ConsecutiveFailures is how many persistence failures the episode has
	// seen so far.
	ConsecutiveFailures uint64 `json:"consecutive_failures"`

	// QueuedPushes is the depth of the bounded retry queue at emission.
	QueuedPushes int `json:"queued_pushes"`
	// DroppedPushes counts individual pushes discarded because the queue was
	// full. A drop is not lost work: it escalates to a whole-tree snapshot,
	// which re-uploads everything the store does not already hold.
	DroppedPushes uint64 `json:"dropped_pushes"`
	// SnapshotPending reports that a whole-tree snapshot is owed — the
	// backstop that covers anything the queue could not.
	SnapshotPending bool `json:"snapshot_pending"`

	// TurnsBlocked reports that further input is being refused. Only ever true
	// under failure_policy: strict.
	TurnsBlocked bool `json:"turns_blocked"`

	// Error is the most recent failure's message.
	Error string `json:"error,omitempty"`
}

// SessionStorageRecovered closes the episode a SessionStorageDegraded opened:
// the backlog drained, the session is durably stored again, and under `strict`
// input is accepted again.
//
// Emitted only when the matching degraded event went out, so the two always
// pair. Recovery needs no operator action — the retry worker gets there on its
// own, and an outage that heals before the next turn boundary is closed by that
// turn's ordinary snapshot with no retry at all.
type SessionStorageRecovered struct {
	SchemaVersion int `json:"_schema_version"`

	SessionID     string `json:"session_id"`
	Backend       string `json:"backend"`
	FailurePolicy string `json:"failure_policy"`

	// DegradedForSeconds is the wall time from the first failure to recovery.
	DegradedForSeconds float64 `json:"degraded_for_seconds"`
	// Failures is how many persistence failures the episode saw in total.
	Failures uint64 `json:"failures"`
	// RetryAttempts is how many backoff attempts the recovery worker made.
	// Zero when an ordinary turn-boundary snapshot healed it first.
	RetryAttempts uint64 `json:"retry_attempts"`
	// DrainedPushes is how many deferred pushes the retries got through.
	DrainedPushes uint64 `json:"drained_pushes"`
	// DroppedPushes is how many were discarded on queue overflow and covered
	// by a whole-tree snapshot instead.
	DroppedPushes uint64 `json:"dropped_pushes"`
}
