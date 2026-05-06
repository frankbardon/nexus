package events

// Schema-version constants for hybrid.* payloads. See doc.go.
const (
	HybridQueryVersion = 1
)

// HybridMatch is one fused result returned by a hybrid search. Score is the
// fused relevance (RRF or weighted, per orchestrator config); Similarity and
// Lexical preserve the raw per-backend scores so downstream consumers can
// re-weight or report them. Sources lists which backends contributed (e.g.
// ["vector"], ["lexical"], ["vector","lexical"]).
type HybridMatch struct {
	ID         string
	Content    string
	Metadata   map[string]string
	Score      float32
	Similarity float32
	Lexical    float32
	Sources    []string
}

// HybridQuery requests a fused retrieval against both vector and lexical
// stores in a single namespace. Fired as a pointer payload on
// "hybrid.query"; the search.hybrid provider fills Matches / Provider /
// Error in place before Emit returns.
//
// The orchestrator embeds Query when Vector is empty. Callers that have
// already embedded (e.g. memory recall reusing a turn-level vector) pass
// Vector to skip the embed call. LexicalBias in [-1, 1] tilts the per-query
// fusion weights — positive favors lexical, negative favors vector.
type HybridQuery struct {
	SchemaVersion int `json:"_schema_version"`

	Namespace   string
	Query       string
	Vector      []float32
	K           int
	Filter      map[string]string
	LexicalBias float32

	Matches  []HybridMatch
	Provider string
	Error    string
}
