package events

// VectorMemoryStore is an explicit request to persist a piece of salient
// content into the vector memory for this agent. Fired as a pointer payload
// on "memory.vector.store"; the memory.vector plugin fills Provider / Error
// in place before Emit returns.
//
// Source is a short label recorded in metadata ("user", "agent", "compaction",
// "tool", …) and used for filtering on retrieval.
type VectorMemoryStore struct {
	Content  string
	Source   string
	Metadata map[string]string // optional extra metadata, merged into the stored doc

	Provider string
	Error    string
}
