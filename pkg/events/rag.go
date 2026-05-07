package events

// Schema-version constants for rag.* payloads. See doc.go.
const (
	RAGIngestVersion       = 1
	RAGIngestDeleteVersion = 1
)

// RAGIngest is an ingestion request for a single file. Fired as a pointer
// payload on "rag.ingest"; the ingest plugin fills Provider / Chunks /
// SkippedCached / Error in place before Emit returns. A notification event
// "rag.ingest.result" is emitted after the fill so observers (UI, logger)
// can react without coupling to the caller.
type RAGIngest struct {
	SchemaVersion int `json:"_schema_version"`

	Path      string
	Namespace string
	Metadata  map[string]string // extra metadata merged into every chunk's metadata

	Provider      string // plugin ID of the ingest plugin that answered
	Chunks        int    // number of chunks upserted
	SkippedCached int    // number of chunks served from the embedding cache
	Error         string
}

// RAGIngestDelete requests removal of all chunks produced from a given path
// in a given namespace. Fired when a watched file is deleted. Fills in place
// just like RAGIngest.
type RAGIngestDelete struct {
	SchemaVersion int `json:"_schema_version"`

	Path      string
	Namespace string

	Provider string
	Deleted  int
	Error    string
}
