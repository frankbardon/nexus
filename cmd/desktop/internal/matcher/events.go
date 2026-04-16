package matcher

// This file defines the domain event types for the staffing-match
// matcher. These are NOT in pkg/events because they are specific to
// one distributable application, not core Nexus concepts. Any code
// that emits or consumes these events imports this package
// directly — which, per the internal/ boundary, is only the
// cmd/desktop host binary.

// Event type constants. Kept as const strings in one place so the
// plugin's Subscriptions/Emissions lists and the bus Emit calls cannot
// drift.
const (
	EventMatchRequest = "match.request"
	EventMatchResult  = "match.result"
)

// MatchRequest asks the staffing-match plugin to rank the candidate
// pool against a job description.
//
// RequestID is caller-generated and echoed in the matching
// MatchResult so multiple concurrent requests can be disambiguated.
// For the scaffold, concurrency is not exercised — the host emits one
// request at a time from a Wails-bound method and waits synchronously
// — but the field is here now so we do not have to rewrite every
// consumer the first time the agent loop fires two matches in
// parallel.
//
// JobText is the matcher's source of truth. The plugin ranks against
// JobText and ignores PDFPath for that purpose. When PDF parsing
// lands in a later slice, the plugin will itself populate JobText
// from the PDF before ranking — the bus contract does not change.
// Callers that already have structured job text (pasted from the
// clipboard, typed into a textarea) should set JobText directly and
// leave PDFPath empty.
type MatchRequest struct {
	RequestID string
	// JobText is the job description as plain text. Ranking runs
	// against this field. Required: if empty, the plugin returns an
	// error result.
	JobText string
	// PDFPath is an absolute path on the local filesystem, retained
	// as provenance metadata and as the hook for the future
	// PDF-parsing slice. The current ranker ignores it completely.
	// When PDF parsing lands, a non-empty PDFPath with an empty
	// JobText will trigger parse-then-rank; a non-empty JobText
	// always wins regardless of PDFPath.
	PDFPath string
	// TopK caps the number of candidates returned. Zero means "use
	// the plugin default" (currently 5).
	TopK int
}

// MatchResult is emitted once per MatchRequest. Exactly one of
// Candidates or Error will be meaningful. Cost is always populated
// on the success path (Candidates non-empty) and zero on the
// error path — if an LLM call failed before returning usage
// data, there is nothing to report.
type MatchResult struct {
	RequestID  string
	Candidates []Candidate
	Cost       Cost
	Error      string
}

// Cost reports the LLM spend and latency for a single match. All
// fields are populated on the success path; the struct is
// zero-valued when MatchResult.Error is non-empty. USD is a plain
// float64 rather than a dedicated money type because PoC spend is
// always in the cents-per-call range and storing it as a rounded
// fixed-point value would hide the precision we actually care
// about ("is this call $0.002 or $0.02").
type Cost struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	USD              float64
	LatencyMs        int64
}

// Candidate is a single ranked entry in a MatchResult.
//
// Score is a float in [0, 1] for UI sorting and display. Reasoning is
// the two-to-three-sentence human-readable justification the PRD
// calls out in §6. ResumePath is the absolute path to the candidate's
// source resume so the UI can open it on click without the plugin
// having to round-trip the whole file contents through an event.
type Candidate struct {
	ID         string
	Name       string
	Score      float64
	Reasoning  string
	ResumePath string
}
