package engine

import (
	"crypto/rand"
	"fmt"
	"time"
)

// Event is a typed event with payload.
type Event[T any] struct {
	Type      string
	ID        string
	Timestamp time.Time
	Source    string
	Payload   T
}

// Meta returns the untyped metadata for this event.
func (e Event[T]) Meta() EventMeta {
	return EventMeta{
		Type:      e.Type,
		ID:        e.ID,
		Timestamp: e.Timestamp,
		Source:    e.Source,
	}
}

// EventMeta carries untyped event metadata for wildcard and filter use.
type EventMeta struct {
	Type      string
	ID        string
	Timestamp time.Time
	Source    string
}

// EventFilter is a predicate that returns true to accept an event.
type EventFilter func(EventMeta) bool

// EventSubscription declares a handler's interest in a particular event type.
type EventSubscription struct {
	EventType string
	Priority  int // lower = earlier
	Filter    EventFilter
}

// VetoResult is the outcome of a vetoable (before:*) event dispatch.
type VetoResult struct {
	Vetoed bool
	Reason string
}

// VetoablePayload wraps the original domain payload for before:* events.
// Handlers inspect Original (e.g. *events.ToolCall) and set Veto to block.
// The wrapper is a pointer so handler mutations propagate back to EmitVetoable.
type VetoablePayload struct {
	Original any        // domain payload passed by the caller
	Veto     VetoResult // handlers set this to veto the action
}

// generateID produces a random hex-encoded UUID-style identifier.
func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
