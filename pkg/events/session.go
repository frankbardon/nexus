package events

import "time"

// Schema-version constants for session.* payloads. See doc.go.
const (
	SessionFileVersion            = 1
	SessionSnapshotRequestVersion = 1
	SessionSnapshotResultVersion  = 1
	SessionOwnerConflictVersion   = 1
)

// SessionFile describes a file event within a session workspace.
type SessionFile struct {
	SchemaVersion int `json:"_schema_version"`

	Path   string // relative to session root
	Action string // "created", "updated"
	Size   int64
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
	// Trigger is what caused the snapshot: "turn", "shutdown" or "request".
	Trigger string `json:"trigger"`
	// Sequence is the per-run snapshot counter, starting at 1.
	Sequence uint64 `json:"sequence"`
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
