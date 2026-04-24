package events

// VectorDoc is a single document in a vector store: opaque ID, the embedding
// vector, the original textual content (optional — stores may persist it for
// re-ranking or provenance), and string-keyed metadata. Metadata is scoped to
// map[string]string to match the common denominator across backends (chromem,
// sqlite-vec, pgvector, Qdrant all support string→string).
type VectorDoc struct {
	ID       string
	Vector   []float32
	Content  string
	Metadata map[string]string
}

// VectorMatch is one hit returned by a nearest-neighbor query.
type VectorMatch struct {
	ID         string
	Content    string
	Metadata   map[string]string
	Similarity float32 // cosine similarity in [-1, 1] when the backend uses it
}

// VectorUpsert requests a batch insert/replace of documents into a namespace.
// Fired as a pointer payload on "vector.upsert"; the capability-resolved
// provider fills Provider / Error in place before Emit returns.
//
// Upsert semantics: documents with an ID already present in the namespace are
// replaced. Adapters without native upsert implement this as delete-then-add.
type VectorUpsert struct {
	Namespace string
	Docs      []VectorDoc

	Provider string
	Error    string
}

// VectorQuery requests a nearest-neighbor lookup in a namespace. Fired as a
// pointer payload on "vector.query"; the provider fills Matches / Provider /
// Error in place before Emit returns.
type VectorQuery struct {
	Namespace string
	Vector    []float32
	K         int
	Filter    map[string]string // exact-match metadata filter; empty means no filter

	Matches  []VectorMatch
	Provider string
	Error    string
}

// VectorDelete requests removal of documents by ID from a namespace. Fired
// as a pointer payload on "vector.delete".
type VectorDelete struct {
	Namespace string
	IDs       []string

	Provider string
	Error    string
}

// VectorNamespaceDrop requests removal of an entire namespace. Fired as a
// pointer payload on "vector.namespace.drop". Idempotent: dropping an
// unknown namespace is not an error.
type VectorNamespaceDrop struct {
	Namespace string

	Provider string
	Error    string
}
