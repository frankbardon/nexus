package events

// CancelRequest asks the system to cancel the current operation.
type CancelRequest struct {
	TurnID string
	Source string // "tui", "browser", etc.
}

// CancelActive signals that cancellation is in progress.
type CancelActive struct {
	TurnID string
}

// CancelComplete signals that cancellation has finished.
// Resumable indicates whether the cancelled operation can be resumed.
type CancelComplete struct {
	TurnID    string
	Resumable bool
}

// CancelResume requests resuming a previously cancelled operation.
type CancelResume struct {
	TurnID string
}
