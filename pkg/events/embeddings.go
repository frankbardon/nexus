package events

// Schema-version constants for embeddings.* payloads. See doc.go.
const (
	EmbeddingsRequestVersion = 1
)

// EmbeddingsRequest is an outbound embeddings request. The consumer fires it
// as a pointer payload on "embeddings.request"; the capability-resolved
// provider fills Vectors / Provider / Model / Error in place before Emit
// returns. Mirrors the synchronous fill pattern used by SearchRequest and
// HistoryQuery — embeddings are a one-shot lookup with no streaming surface.
type EmbeddingsRequest struct {
	SchemaVersion int `json:"_schema_version"`

	// Texts is the batch of strings to embed. Order is preserved in Vectors.
	Texts []string
	// Model is the embeddings model to use. Zero value means provider default.
	Model string
	// Dimensions optionally requests a specific vector dimensionality when
	// the provider supports truncation (e.g. OpenAI text-embedding-3-*). Zero
	// means provider default.
	Dimensions int

	// Vectors / Provider / Model (possibly echoed back) / Usage / Error are
	// populated by the provider handler. Callers should treat them as zero
	// on input.
	Vectors  [][]float32
	Provider string // plugin ID of the adapter that answered
	Usage    EmbeddingsUsage
	Error    string // non-empty when the call failed
}

// EmbeddingsUsage reports token accounting for an embeddings call when the
// provider returns it. Zero values are fine when unknown.
type EmbeddingsUsage struct {
	PromptTokens int
	TotalTokens  int
}
