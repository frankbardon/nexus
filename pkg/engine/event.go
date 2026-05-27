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
	// Causation carries provenance for this event: which event caused it,
	// which session and agent it belongs to, and its monotonic per-session
	// sequence. Populated automatically by the bus on dispatch — callers do
	// not need to set it. See EventCausation.
	Causation EventCausation
}

// Meta returns the untyped metadata for this event.
func (e Event[T]) Meta() EventMeta {
	return EventMeta{
		Type:      e.Type,
		ID:        e.ID,
		Timestamp: e.Timestamp,
		Source:    e.Source,
		Causation: e.Causation,
	}
}

// EventMeta carries untyped event metadata for wildcard and filter use.
type EventMeta struct {
	Type      string
	ID        string
	Timestamp time.Time
	Source    string
	Causation EventCausation
}

// EventCausation records the provenance of an event. The bus assigns Sequence
// monotonically per session on dispatch; ParentID is the ID of the event whose
// handler triggered this emission (zero for root events); SessionID and AgentID
// come from the active causation context pushed by callers (engine/session,
// agent loops, sub-agent runtime). All fields are best-effort: handlers
// emitting outside a known session/agent context see zero values.
//
// Causation is purely descriptive — it never affects dispatch ordering or
// filtering. Replay and observability are the consumers.
type EventCausation struct {
	// ParentID is the ID of the event whose handler emitted this event.
	// Empty for root events (boot, user input arriving from IO transport).
	ParentID string
	// ParentSeq mirrors ParentID via the per-session monotonic sequence.
	// Zero when no parent is detectable.
	ParentSeq uint64
	// SessionID is the session this event belongs to. Empty when emitted
	// outside any session context (engine boot, shutdown).
	SessionID string
	// AgentID identifies the agent that produced the event. For sub-agent
	// activity this is the sub-agent's identity, not the parent agent's,
	// so causation chains expose specialist attribution.
	AgentID string
	// Sequence is monotonic per session, assigned by the bus on publish.
	// Starts at 1 within a session; never reused.
	Sequence uint64
	// Depth is the sub-agent recursion depth at the time of emission.
	// Zero for the top-level agent, 1 for its first-level sub-agent, etc.
	Depth int
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

// GenerateID produces a random hex-encoded UUID-style identifier.
func GenerateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
