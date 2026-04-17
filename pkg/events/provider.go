package events

// ProviderFallback notifies IO plugins and observers that a provider switch
// occurred due to the primary provider failing.
type ProviderFallback struct {
	Role           string // model role being resolved (e.g. "balanced")
	FailedProvider string // plugin ID of the provider that failed
	FailedModel    string // model that failed
	Error          string // error description
	NextProvider   string // plugin ID of the fallback provider
	NextModel      string // model being tried next
	Attempt        int    // 0-based index in the fallback chain
}
