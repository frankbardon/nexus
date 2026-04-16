package events

import "time"

// ThinkingStep represents an intermediate reasoning step visible to the user.
type ThinkingStep struct {
	TurnID    string
	Source    string // plugin ID that generated this thinking step
	Content   string
	Phase     string // "planning", "executing", "reasoning"
	Timestamp time.Time
}
