package events

import "time"

// Schema-version constants for memory.* payloads. See doc.go.
const (
	MemoryEntryVersion                 = 1
	MemoryQueryVersion                 = 1
	MemoryResultVersion                = 1
	HistoryQueryVersion                = 1
	LongTermMemoryEntryVersion         = 1
	LongTermMemoryLoadedVersion        = 1
	LongTermMemoryStoreRequestVersion  = 1
	LongTermMemoryStoredVersion        = 1
	LongTermMemoryReadRequestVersion   = 1
	LongTermMemoryReadResultVersion    = 1
	LongTermMemoryDeleteRequestVersion = 1
	LongTermMemoryDeletedVersion       = 1
	LongTermMemoryQueryVersion         = 1
	LongTermMemoryListResultVersion    = 1
	CompactionTriggeredVersion         = 1
	CompactionCompleteVersion          = 1

	// Idea 30 — live context curation events.
	MemoryToolResultClearedVersion  = 1
	MemoryToolDefPrunedVersion      = 1
	MemoryTopicShiftDetectedVersion = 1
	MemorySummaryReplacedVersion    = 1
	MemoryCuratedVersion            = 1
)

// MemoryEntry represents a single memory record.
type MemoryEntry struct {
	SchemaVersion int `json:"_schema_version"`

	Key       string
	Content   string
	Metadata  map[string]any
	SessionID string
}

// MemoryQuery describes a query against the memory store.
type MemoryQuery struct {
	SchemaVersion int `json:"_schema_version"`

	Query     string
	Limit     int
	SessionID string
}

// MemoryResult carries the results of a memory query.
type MemoryResult struct {
	SchemaVersion int `json:"_schema_version"`

	Entries []MemoryEntry
	Query   string
}

// HistoryQuery is a synchronous request for LLM-native conversation history.
// Emitted as a pointer payload on "memory.history.query"; the handler fills
// Messages in place before the Emit call returns. This follows the same
// pointer-mutation pattern as VetoablePayload. Callers consume Messages
// after Emit returns.
type HistoryQuery struct {
	SchemaVersion int `json:"_schema_version"`

	// SessionID scopes the query when multiple sessions share a bus. Empty
	// means "the current session". Set by caller.
	SessionID string
	// Messages is filled by the handler. Caller should treat it as nil on
	// input; a nil result after Emit means no history plugin answered.
	Messages []Message
}

// --- Long-term memory events ---

// LongTermMemoryIndex is a lightweight reference to a memory entry.
type LongTermMemoryIndex struct {
	Key     string            `json:"key"`
	Preview string            `json:"preview"` // first line of content
	Tags    map[string]string `json:"tags"`
	Updated time.Time         `json:"updated"`
}

// LongTermMemoryEntry is a full memory record.
type LongTermMemoryEntry struct {
	SchemaVersion int `json:"_schema_version"`

	Key           string            `json:"key"`
	Content       string            `json:"content"`
	Tags          map[string]string `json:"tags"`
	Created       time.Time         `json:"created"`
	Updated       time.Time         `json:"updated"`
	SourceSession string            `json:"source_session"`
}

// LongTermMemoryLoaded signals that the memory index has been injected into
// the system prompt.
type LongTermMemoryLoaded struct {
	SchemaVersion int                   `json:"_schema_version"`
	Entries       []LongTermMemoryIndex `json:"entries"`
	Scope         string                `json:"scope"` // "agent", "global", or "both"
}

// LongTermMemoryStoreRequest is a request to write or update a memory entry.
type LongTermMemoryStoreRequest struct {
	SchemaVersion int               `json:"_schema_version"`
	Key           string            `json:"key"`
	Content       string            `json:"content"`
	Tags          map[string]string `json:"tags,omitempty"`
}

// LongTermMemoryStored confirms a memory entry was written.
type LongTermMemoryStored struct {
	SchemaVersion int    `json:"_schema_version"`
	Key           string `json:"key"`
}

// LongTermMemoryReadRequest is a request to read a memory entry by key.
type LongTermMemoryReadRequest struct {
	SchemaVersion int    `json:"_schema_version"`
	Key           string `json:"key"`
}

// LongTermMemoryReadResult carries the full content of a memory entry.
type LongTermMemoryReadResult struct {
	SchemaVersion int               `json:"_schema_version"`
	Key           string            `json:"key"`
	Content       string            `json:"content"`
	Tags          map[string]string `json:"tags"`
}

// LongTermMemoryDeleteRequest is a request to delete a memory entry.
type LongTermMemoryDeleteRequest struct {
	SchemaVersion int    `json:"_schema_version"`
	Key           string `json:"key"`
}

// LongTermMemoryDeleted confirms a memory entry was deleted.
type LongTermMemoryDeleted struct {
	SchemaVersion int    `json:"_schema_version"`
	Key           string `json:"key"`
}

// LongTermMemoryQuery is a request to list or filter memories.
type LongTermMemoryQuery struct {
	SchemaVersion int               `json:"_schema_version"`
	Tags          map[string]string `json:"tags,omitempty"`
}

// LongTermMemoryListResult carries filtered memory index entries.
type LongTermMemoryListResult struct {
	SchemaVersion int                   `json:"_schema_version"`
	Entries       []LongTermMemoryIndex `json:"entries"`
}

// --- Compaction events ---

// CompactionTriggered signals that context compaction is starting.
type CompactionTriggered struct {
	SchemaVersion int `json:"_schema_version"`

	Reason       string // human-readable trigger reason
	MessageCount int    // number of messages before compaction
	BackupPath   string // session-relative path to the backup file
}

// CompactionComplete signals that context compaction has finished.
// Subscribers should replace their conversation history with Messages.
type CompactionComplete struct {
	SchemaVersion int `json:"_schema_version"`

	Messages     []Message // the compacted conversation
	BackupPath   string    // session-relative path to the backup file
	MessageCount int       // number of messages after compaction
	PrevCount    int       // number of messages before compaction
}

// --- Idea 30: live context curation events ---

// MemoryToolResultCleared signals that a tool result body has been replaced
// with an envelope marker by the live curator. The call/result pairing stays
// in history; only the body is gone.
type MemoryToolResultCleared struct {
	SchemaVersion int `json:"_schema_version"`

	ToolCallID    string `json:"tool_call_id"`
	Tool          string `json:"tool"`
	OriginalSize  int    `json:"original_size"`
	ClearedAtTurn int    `json:"cleared_at_turn"`
	Reason        string `json:"reason"` // "age", "subsequent_call", "no_citation", "synthesis_detected"
}

// MemoryToolDefPruned signals that a tool definition has been demoted out
// of the LLM-visible tool list because it has been idle too long. Restoration
// happens implicitly via discovery on next mention.
type MemoryToolDefPruned struct {
	SchemaVersion int `json:"_schema_version"`

	ToolID         string `json:"tool_id"`
	LastUsedTurn   int    `json:"last_used_turn"`
	DefinitionSize int    `json:"definition_size"`
}

// MemoryTopicShiftDetected signals the topic pruner observed a topic
// boundary in the conversation.
type MemoryTopicShiftDetected struct {
	SchemaVersion int `json:"_schema_version"`

	FromTurn   int     `json:"from_turn"`
	ToTurn     int     `json:"to_turn"`
	Similarity float64 `json:"similarity"`
	Signal     string  `json:"signal"` // "embedding" | "phrase" | "user_explicit"
}

// MemorySummaryReplaced signals that a span of the conversation has been
// replaced with a reasoning-preserving summary. Distinct from
// CompactionComplete — that event reports a full-buffer rewrite; this one
// reports a span-level replacement.
type MemorySummaryReplaced struct {
	SchemaVersion int `json:"_schema_version"`

	FromTurns      [2]int   `json:"from_turns"` // [start, end] inclusive turn range
	OriginalTokens int      `json:"original_tokens"`
	SummaryTokens  int      `json:"summary_tokens"`
	PreservedKinds []string `json:"preserved_kinds"` // e.g. ["decision","rationale","error","next_step"]
}

// CurationSection describes one section of context touched by a curation
// pass. Used by MemoryCurated to convey stability impact.
type CurationSection struct {
	// SectionID is an opaque identifier for the section that was edited.
	// Convention: "<plugin-id>/<scope>" e.g. "nexus.memory.tool_result_clear/turn-12"
	// or "nexus.memory.summary_buffer/range-3-8".
	SectionID string `json:"section_id"`
	// Kind is the cache-stability category. "volatile" = recent turns
	// (no cache impact); "session" = session-long content like compaction
	// summaries (controlled re-cache); "static" = system prompt / tool defs
	// (curator must not touch these).
	Kind string `json:"kind"`
	// TokensDelta is the post-edit minus pre-edit token estimate. Negative
	// for shrinkage (the common case); positive for replacements that grew.
	TokensDelta int `json:"tokens_delta"`
}

// MemoryCurated is the envelope event emitted by every curation layer.
// It carries the stability-impact descriptor consumed by the cache-aware
// prompt builder (Idea 05) so cache invalidation cost is scoped.
type MemoryCurated struct {
	SchemaVersion int `json:"_schema_version"`

	// Layer names the curation layer that ran (e.g. "tool_result_clear",
	// "tool_def_pruner", "topic_pruner", "summary_buffer").
	Layer string `json:"layer"`
	// SectionsTouched describes every section the layer edited.
	SectionsTouched []CurationSection `json:"sections_touched"`
	// CacheInvalidates is true when at least one touched section is in
	// the cached prefix and the prompt builder must re-cache. False when
	// every touched section is volatile (no impact).
	CacheInvalidates bool `json:"cache_invalidates"`
	// AtTurn is the turn boundary at which the curation ran. Curations
	// batch at turn boundaries to keep cache invalidations predictable.
	AtTurn int `json:"at_turn"`
}
