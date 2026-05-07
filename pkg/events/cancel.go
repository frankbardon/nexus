package events

// Schema-version constants for cancel.* payloads. See doc.go.
const (
	CancelRequestVersion  = 1
	CancelActiveVersion   = 1
	CancelCompleteVersion = 1
	CancelResumeVersion   = 1
)

// CancelRequest asks the system to cancel the current operation.
type CancelRequest struct {
	SchemaVersion int `json:"_schema_version"`

	TurnID string
	Source string // "tui", "browser", etc.
}

// CancelActive signals that cancellation is in progress.
type CancelActive struct {
	SchemaVersion int `json:"_schema_version"`

	TurnID string
}

// CancelComplete signals that cancellation has finished.
// Resumable indicates whether the cancelled operation can be resumed.
type CancelComplete struct {
	SchemaVersion int `json:"_schema_version"`

	TurnID    string
	Resumable bool
}

// CancelResume requests resuming a previously cancelled operation.
type CancelResume struct {
	SchemaVersion int `json:"_schema_version"`

	TurnID string
}
