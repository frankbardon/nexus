// Package journal is the engine's durable, append-only event log.
//
// One journal exists per session at <session_root>/journal/. Every event
// dispatched on the bus is recorded with a monotonic per-session sequence
// number, the dispatching event's parent sequence (best-effort), and the
// vetoed flag for before:* events. The journal is the source of truth for
// crash recovery, deterministic replay, and observability projections — the
// in-memory bus ring is only for the boot-time pre-subscription gap.
package journal

import (
	"time"
)

// SchemaVersion is the on-disk format version. Readers reject journals with
// any other value rather than attempting silent migration.
const SchemaVersion = "1"

// FsyncMode controls how often the writer flushes the kernel buffer to disk.
type FsyncMode string

const (
	// FsyncEveryEvent fsyncs after every appended envelope. Strongest crash
	// guarantee, slowest throughput.
	FsyncEveryEvent FsyncMode = "every-event"
	// FsyncTurnBoundary fsyncs only when an agent.turn.end envelope is
	// appended. Default — most turns are 5–30 events, amortizing the disk
	// flush across the burst.
	FsyncTurnBoundary FsyncMode = "turn-boundary"
	// FsyncNone never fsyncs explicitly. The OS buffer is still flushed on
	// Close. Suitable for ephemeral test runs.
	FsyncNone FsyncMode = "none"
)

// ParseFsyncMode normalizes a config string to a known mode. Unknown values
// fall back to FsyncTurnBoundary so a typo cannot silently disable durability.
func ParseFsyncMode(s string) FsyncMode {
	switch FsyncMode(s) {
	case FsyncEveryEvent, FsyncTurnBoundary, FsyncNone:
		return FsyncMode(s)
	default:
		return FsyncTurnBoundary
	}
}

// Envelope is the on-disk record for a single dispatched event. One envelope
// is one JSONL line in events.jsonl (or events-NNN.jsonl.zst once rotated).
//
// Field naming uses snake_case to match the rest of the on-disk JSON in the
// session workspace and to keep tail-style external tooling readable.
type Envelope struct {
	// Seq is monotonic per-session, starts at 1, gap-free.
	Seq uint64 `json:"seq"`
	// Ts is when the event was dispatched on the bus, not when it landed
	// on disk. Replay tools key off this.
	Ts time.Time `json:"ts"`
	// Type is the event type (e.g. "llm.request", "tool.result").
	Type string `json:"type"`
	// EventID is the bus-assigned random ID. Carried so external tools can
	// correlate journal entries with otel spans and logger output.
	EventID string `json:"event_id,omitempty"`
	// Source is the plugin ID that emitted the event, when known.
	Source string `json:"source,omitempty"`
	// TraceID is reserved for cross-system correlation (otel). Empty for now.
	TraceID string `json:"trace_id,omitempty"`
	// ParentSeq is the seq of the event whose handler emitted this one,
	// best-effort. Zero means no detectable parent.
	ParentSeq uint64 `json:"parent_seq,omitempty"`
	// SideEffect marks events whose handlers performed real-world I/O
	// (LLM call billed, file written, shell run). Replay must short-circuit
	// these. Computed from event type, not the handler.
	SideEffect bool `json:"side_effect,omitempty"`
	// Vetoed is true for before:* events that a handler vetoed.
	Vetoed bool `json:"vetoed,omitempty"`
	// VetoReason is the handler-supplied reason when Vetoed is true.
	VetoReason string `json:"veto_reason,omitempty"`
	// Payload is the event payload, marshaled as-is. Marshalling errors
	// fall back to a placeholder so the envelope still records the event.
	Payload any `json:"payload,omitempty"`
}

// Header is the journal directory's manifest. Written once at construction,
// re-read on every open to validate compatibility.
type Header struct {
	SchemaVersion string    `json:"schema_version"`
	CreatedAt     time.Time `json:"created_at"`
	FsyncMode     string    `json:"fsync_mode"`
	SessionID     string    `json:"session_id,omitempty"`
}

// sideEffectTypes is the set of event types whose handlers we know to be
// non-deterministic side effects. Used to populate Envelope.SideEffect at
// write time so replay logic does not have to re-derive it.
var sideEffectTypes = map[string]bool{
	"llm.request":      true,
	"llm.response":     true,
	"tool.invoke":      true,
	"tool.result":      true,
	"io.ask":           true,
	"io.ask.response":  true,
	"web.search":       true,
	"web.fetch":        true,
	"shell.execute":    true,
	"file.write":       true,
	"code.execute":     true,
	"rag.ingest":       true,
	"embeddings.embed": true,
	"vector.upsert":    true,
	"vector.query":     true,
}

// IsSideEffect reports whether the event type is recorded as a side effect.
// Centralized so providers and tools can extend the list as new categories
// arrive without each writer site re-implementing the rule.
func IsSideEffect(eventType string) bool {
	return sideEffectTypes[eventType]
}
