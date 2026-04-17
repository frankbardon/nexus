package events

import "time"

// MemoryEntry represents a single memory record.
type MemoryEntry struct {
	Key       string
	Content   string
	Metadata  map[string]any
	SessionID string
}

// MemoryQuery describes a query against the memory store.
type MemoryQuery struct {
	Query     string
	Limit     int
	SessionID string
}

// MemoryResult carries the results of a memory query.
type MemoryResult struct {
	Entries []MemoryEntry
	Query   string
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
	Entries []LongTermMemoryIndex `json:"entries"`
	Scope   string                `json:"scope"` // "agent", "global", or "both"
}

// LongTermMemoryStoreRequest is a request to write or update a memory entry.
type LongTermMemoryStoreRequest struct {
	Key     string            `json:"key"`
	Content string            `json:"content"`
	Tags    map[string]string `json:"tags,omitempty"`
}

// LongTermMemoryStored confirms a memory entry was written.
type LongTermMemoryStored struct {
	Key string `json:"key"`
}

// LongTermMemoryReadRequest is a request to read a memory entry by key.
type LongTermMemoryReadRequest struct {
	Key string `json:"key"`
}

// LongTermMemoryReadResult carries the full content of a memory entry.
type LongTermMemoryReadResult struct {
	Key     string            `json:"key"`
	Content string            `json:"content"`
	Tags    map[string]string `json:"tags"`
}

// LongTermMemoryDeleteRequest is a request to delete a memory entry.
type LongTermMemoryDeleteRequest struct {
	Key string `json:"key"`
}

// LongTermMemoryDeleted confirms a memory entry was deleted.
type LongTermMemoryDeleted struct {
	Key string `json:"key"`
}

// LongTermMemoryQuery is a request to list or filter memories.
type LongTermMemoryQuery struct {
	Tags map[string]string `json:"tags,omitempty"`
}

// LongTermMemoryListResult carries filtered memory index entries.
type LongTermMemoryListResult struct {
	Entries []LongTermMemoryIndex `json:"entries"`
}

// --- Compaction events ---

// CompactionTriggered signals that context compaction is starting.
type CompactionTriggered struct {
	Reason       string // human-readable trigger reason
	MessageCount int    // number of messages before compaction
	BackupPath   string // session-relative path to the backup file
}

// CompactionComplete signals that context compaction has finished.
// Subscribers should replace their conversation history with Messages.
type CompactionComplete struct {
	Messages     []Message // the compacted conversation
	BackupPath   string    // session-relative path to the backup file
	MessageCount int       // number of messages after compaction
	PrevCount    int       // number of messages before compaction
}
