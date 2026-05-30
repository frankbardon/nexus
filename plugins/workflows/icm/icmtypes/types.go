// Package icmtypes holds tiny shared value types used by both the
// nexus.workflows.icm plugin (events.go) and its session subpackage
// (artifact sidecars + run state).
//
// It exists solely to break a would-be import cycle: the session package
// is a leaf that the main icm package imports, so the session package
// cannot import the icm package. ConditionResult is the only type both
// halves need to agree on, so it lives here.
package icmtypes

// ConditionResult is the persisted form of a predicate outcome. It
// appears in icm.* event payloads and in the per-artifact .icm.json
// sidecar.
type ConditionResult struct {
	Type     string   `json:"type"`
	Name     string   `json:"name,omitempty"`
	Verdict  string   `json:"verdict"`
	Feedback string   `json:"feedback,omitempty"`
	Score    *float64 `json:"score,omitempty"`
}
