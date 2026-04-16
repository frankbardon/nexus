package events

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
