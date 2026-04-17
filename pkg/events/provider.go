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

// ProviderFanoutTarget describes one provider in a fanout request.
type ProviderFanoutTarget struct {
	Provider string // plugin ID
	Model    string
}

// ProviderFanoutStart signals that a fanout request has been initiated.
type ProviderFanoutStart struct {
	FanoutID string
	Role     string
	Strategy string // "all", "llm_judge", "heuristic", "user"
	Targets  []ProviderFanoutTarget
}

// ProviderFanoutResponse signals that one provider in a fanout has responded.
type ProviderFanoutResponse struct {
	FanoutID string
	Provider string // plugin ID that responded
	Model    string
	Success  bool   // false if provider errored or timed out
	Error    string // non-empty on failure
}

// ProviderFanoutComplete signals that all fanout providers have responded
// (or the deadline was reached) and the final result has been emitted.
type ProviderFanoutComplete struct {
	FanoutID  string
	Role      string
	Strategy  string
	Succeeded int // number of providers that responded successfully
	Failed    int // number of providers that errored or timed out
}

// ProviderFanoutChoose presents fanout responses for user selection.
// IO plugins render a picker UI when they receive this event.
type ProviderFanoutChoose struct {
	FanoutID  string
	Role      string
	Responses []ProviderFanoutOption
}

// ProviderFanoutOption describes one response available for user selection.
type ProviderFanoutOption struct {
	Index    int
	Provider string
	Model    string
	Content  string  // response content preview
	CostUSD  float64
}

// ProviderFanoutChosen carries the user's selection from a fanout choice.
type ProviderFanoutChosen struct {
	FanoutID    string
	ChosenIndex int // -1 means user declined to choose (fall back to "all")
}
