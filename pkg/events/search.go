package events

import "time"

// Schema-version constants for search.* payloads. See doc.go.
const (
	SearchRequestVersion = 1
)

// SearchRequest is an outbound web search request. The tool plugin fires it
// as a pointer payload on "search.request"; the capability-resolved provider
// fills Results / Provider / Error in place before Emit returns. Mirrors the
// synchronous fill pattern used by ToolCatalogQuery and HistoryQuery rather
// than the async LLMRequest/LLMResponse pair, since search is a one-shot
// lookup with no streaming surface.
type SearchRequest struct {
	SchemaVersion int `json:"_schema_version"`

	Query      string
	Count      int    // max results; zero means provider default
	SafeSearch string // "off" | "moderate" | "strict"
	Language   string // BCP-47 tag, e.g. "en", "en-US"
	Freshness  string // "day" | "week" | "month" | "" — best-effort, provider-dependent

	// Results / Provider / Error are populated by the provider handler.
	// Callers should treat them as zero on input.
	Results  []SearchResult
	Provider string // plugin ID of the adapter that answered
	Error    string // non-empty when the lookup failed
}

// SearchResult is one hit returned by a search provider.
type SearchResult struct {
	Title       string
	URL         string
	Snippet     string
	PublishedAt time.Time // zero when the provider does not supply a date
	Source      string    // provider-specific hostname or domain, optional
}
