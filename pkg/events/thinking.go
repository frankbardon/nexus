package events

import "time"

// Schema-version constants for thinking.* payloads. See doc.go.
const (
	ThinkingStepVersion = 1
)

// ThinkingStep represents an intermediate reasoning step visible to the user.
type ThinkingStep struct {
	SchemaVersion int `json:"_schema_version"`

	TurnID    string
	Source    string // plugin ID that generated this thinking step
	Content   string
	Phase     string // "planning", "executing", "reasoning"
	Timestamp time.Time
	// Index is the sequence number of this step within a single TurnID.
	// Providers that emit one event per logical thinking block (or chunk
	// thereof) use this to order steps for reconstruction; planners that
	// emit a single thinking event per turn leave it at 0.
	Index int
}
