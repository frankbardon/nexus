package events

// Schema-version constants for session.* payloads. See doc.go.
const (
	SessionFileVersion            = 1
	SessionSnapshotRequestVersion = 1
	SessionSnapshotResultVersion  = 1
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
