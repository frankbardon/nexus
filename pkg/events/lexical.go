package events

// LexicalDoc is a single document in a lexical (keyword / BM25) index.
// Identifiers and metadata mirror VectorDoc so the dual-write ingest pipeline
// can build both records from one chunk. Vector is intentionally absent —
// lexical stores rank by token overlap, not similarity.
type LexicalDoc struct {
	ID       string
	Content  string
	Metadata map[string]string
}

// LexicalMatch is one hit returned by a lexical query. Score is BM25-style
// relevance — higher is better, range backend-dependent.
type LexicalMatch struct {
	ID       string
	Content  string
	Metadata map[string]string
	Score    float32
}

// LexicalUpsert requests a batch insert/replace of documents into a namespace.
// Fired as a pointer payload on "lexical.upsert"; the search.lexical provider
// fills Provider / Error in place before Emit returns.
//
// Upsert semantics: documents with an ID already present in the namespace are
// replaced. Adapters without native upsert implement this as delete-then-add.
type LexicalUpsert struct {
	Namespace string
	Docs      []LexicalDoc

	Provider string
	Error    string
}

// LexicalQuery requests a BM25 nearest-keyword lookup in a namespace. Fired
// as a pointer payload on "lexical.query"; the provider fills Matches /
// Provider / Error in place before Emit returns.
type LexicalQuery struct {
	Namespace string
	Query     string
	K         int
	Filter    map[string]string // exact-match metadata filter; empty means no filter

	Matches  []LexicalMatch
	Provider string
	Error    string
}

// LexicalDelete requests removal of documents by ID from a namespace. Fired
// as a pointer payload on "lexical.delete".
type LexicalDelete struct {
	Namespace string
	IDs       []string

	Provider string
	Error    string
}

// LexicalNamespaceDrop requests removal of an entire namespace. Fired as a
// pointer payload on "lexical.namespace.drop". Idempotent: dropping an
// unknown namespace is not an error.
type LexicalNamespaceDrop struct {
	Namespace string

	Provider string
	Error    string
}
