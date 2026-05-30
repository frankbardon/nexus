// Package icmtypes holds tiny shared value types used by both the
// nexus.workflows.icm plugin (events.go) and its session and predicates
// subpackages (artifact sidecars, run state, predicate failure events).
//
// It exists solely to break would-be import cycles: leaf packages
// (session, predicates) cannot import the main icm package because the
// main icm package imports them. Anything both halves need to agree on
// lives here.
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

// ICMPredicateFailedVersion is the schema version emitted with each
// ICMPredicateFailed event payload.
const ICMPredicateFailedVersion = 1

// ICMPredicateFailed fires whenever any predicate evaluation returns
// Verdict=false. Single source of truth for failure visibility — pass
// paths are not emitted.
//
// Lives in icmtypes (rather than the main icm package) so the
// predicates sub-package can emit it without forming an import cycle
// (predicates → icm → predicates). The main icm package re-exports this
// type via a type alias for ergonomic use by subscribers.
type ICMPredicateFailed struct {
	SchemaVersion int    `json:"_schema_version"`
	RunID         string `json:"run_id"`
	StageID       string `json:"stage_id"`
	ItemID        string `json:"item_id,omitempty"`
	Container     string `json:"container"` // output.validators | loop.until | verifier
	PredicateName string `json:"predicate_name"`
	PredicateType string `json:"predicate_type"`
	Feedback      string `json:"feedback,omitempty"`
}
