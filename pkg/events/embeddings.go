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
	// Used when Inputs is empty — keeps existing text-only callers and
	// adapters working unchanged.
	Texts []string
	// Inputs is the polymorphic batch (text and/or image inputs) for
	// multimodal-aware providers. Additive over Texts: when empty, providers
	// continue to read Texts. When non-empty, providers must consume Inputs
	// in order (one vector per Input) and either honor every Input kind or
	// return a clear error (e.g. an OpenAI text-only embedding model
	// receiving an image input). Adapters must not silently downgrade an
	// image input to a text fallback.
	Inputs []EmbeddingsInput
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

// EmbeddingsInput is one polymorphic embedding input. Exactly one of Text,
// Image, or ImageURI is populated. Adapters that don't support image inputs
// must return a clear error when they encounter an image-bearing input
// rather than silently downgrading.
//
// The ImageURI form supports the engine's content-addressed blob scheme
// ("nexus-blob:<sha>") and plain external URLs. A provider that can't
// resolve a particular scheme must error rather than skip the input.
type EmbeddingsInput struct {
	// Text is set when this input is a text snippet. Mutually exclusive
	// with Image and ImageURI.
	Text string
	// Image is the inline image bytes. Mutually exclusive with Text and
	// ImageURI. Requires MimeType.
	Image []byte
	// ImageURI references an image by URI: "nexus-blob:<sha>" for the
	// engine blob store, or a plain HTTPS URL the provider can fetch.
	// Mutually exclusive with Text and Image.
	ImageURI string
	// MimeType is the IANA media type (e.g. "image/png", "image/jpeg")
	// describing Image / ImageURI bytes. Required when Image is set;
	// recommended for ImageURI.
	MimeType string
}

// EmbeddingsUsage reports token accounting for an embeddings call when the
// provider returns it. Zero values are fine when unknown.
type EmbeddingsUsage struct {
	PromptTokens int
	TotalTokens  int
}
