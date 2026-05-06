package events

// Schema-version constants for citations.* payloads. See doc.go.
const (
	RetrievalContextVersion = 1
	CitedResponseVersion    = 1
)

// RetrievedChunk is one piece of context a retrieval plugin pulled for the
// current turn. The citation plugin uses it as the validation set: cited
// references must point at chunks that were actually retrieved during the
// turn or they are dropped.
type RetrievedChunk struct {
	Source    string
	DocID     string
	ChunkIdx  string
	TrustTier string // optional, per Idea 03
}

// RetrievalContext is emitted by knowledge_search and memory.vector after
// each successful retrieval pass so the citation plugin can tally which
// chunks the LLM had access to. Multiple emissions per turn merge.
//
// Bus event type: "rag.retrieved".
type RetrievalContext struct {
	SchemaVersion int `json:"_schema_version"`

	TurnID string
	Source string // emitter plugin ID for debug
	Chunks []RetrievedChunk
}

// CitationRef is one parsed source attribution attached to an LLM response.
// Lighter than the provider-native Citation struct (which carries Anthropic-
// specific char/page indices) — works for both tag-based parsing and the
// Anthropic native path.
type CitationRef struct {
	Source    string
	DocID     string
	ChunkIdx  string
	Snippet   string
	TrustTier string

	// Span is the [start, end) byte offset into CitedResponse.Text where the
	// citation applied. Zeros when the source format does not provide it.
	SpanStart int
	SpanEnd   int
}

// CitedResponse is the post-processed LLM response with citation tags
// stripped from user-visible text and structured CitationRefs collected.
// Emitted as "llm.response.cited" after every llm.response that resolves
// against an active retrieval context.
type CitedResponse struct {
	SchemaVersion int `json:"_schema_version"`

	TurnID    string
	Text      string
	Citations []CitationRef
	// Mode tracks how the citations were obtained: "tag" (parsed from
	// <cite/> markers) or "anthropic_native" (read from Citations[] on the
	// underlying response).
	Mode string
}
