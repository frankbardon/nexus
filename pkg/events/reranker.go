package events

// Schema-version constants for reranker.* payloads. See doc.go.
const (
	RerankRequestVersion = 1
)

// RerankDoc is one document handed to a cross-encoder reranker. Content is
// the text the reranker scores against the query; ID is opaque so the caller
// can map results back to their own structures.
type RerankDoc struct {
	ID      string
	Content string
}

// RerankResult is one scored document returned by a reranker. Index is the
// position the doc held in the input slice, useful when the caller wants to
// preserve original metadata not echoed in RerankDoc.
type RerankResult struct {
	ID    string
	Index int
	Score float32
}

// RerankRequest asks the active search.reranker provider to re-score a
// candidate pool against a query and return the top-N. Fired as a pointer
// payload on "reranker.rerank"; the provider fills Results / Provider /
// Error in place before Emit returns.
type RerankRequest struct {
	SchemaVersion int `json:"_schema_version"`

	Query string
	Docs  []RerankDoc
	TopN  int
	Model string

	Results  []RerankResult
	Provider string
	Error    string
}
