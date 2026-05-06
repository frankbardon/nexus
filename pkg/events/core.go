package events

import "time"

// Schema-version constants for core.* payloads. See doc.go for the
// versioning convention.
const (
	BootConfigVersion     = 1
	ShutdownReasonVersion = 1
	ErrorInfoVersion      = 1
	TickInfoVersion       = 1
)

// BootConfig carries bootstrap configuration for system startup.
type BootConfig struct {
	SchemaVersion int `json:"_schema_version"`

	ConfigPath string
	Profile    string
}

// ShutdownReason describes why the system is shutting down.
type ShutdownReason struct {
	SchemaVersion int `json:"_schema_version"`

	Reason string // "user", "error", "signal"
	Error  error
}

// ErrorInfo describes an error originating from a specific source.
type ErrorInfo struct {
	SchemaVersion int `json:"_schema_version"`

	Source           string // plugin ID
	Err              error
	Fatal            bool
	Retryable        bool           // whether this error class is retryable (429, 5xx)
	RetriesExhausted bool           // provider's own retry logic gave up
	RequestMeta      map[string]any // echo of LLMRequest.Metadata for correlation
	// EventType is set when ErrorInfo describes a panic recovered during
	// event dispatch — names the event whose handler panicked. Empty for
	// errors that did not originate from a recovered handler panic.
	EventType string
	// Stack is the goroutine stack (debug.Stack output) captured at panic
	// recovery time. Empty for non-panic ErrorInfo records.
	Stack string
}

// TickInfo carries periodic tick metadata.
type TickInfo struct {
	SchemaVersion int `json:"_schema_version"`

	Sequence int
	Time     time.Time
}
