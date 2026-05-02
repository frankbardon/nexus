package sampler

// EvalCandidateEventType is the dotted bus event type emitted whenever the
// sampler captures a session. Downstream tooling (`nexus eval list-candidates`,
// future ingestion backends) subscribes to this type to learn about new
// captures.
const EvalCandidateEventType = "eval.candidate"

// EvalCandidate is the payload of an eval.candidate emission. The struct is
// intentionally small and stable — external harnesses pin against it, so any
// addition here is a deliberate, documented event.
type EvalCandidate struct {
	// SessionID is the source session whose journal was sampled.
	SessionID string `json:"session_id"`
	// CaseDir is the absolute on-disk path the sample was written to,
	// typically `<out_dir>/<session-id>`. The directory contains a `journal/`
	// subtree plus a `metadata.json`.
	CaseDir string `json:"case_dir"`
	// Reason is the short tag explaining why the sample was captured.
	// Currently one of:
	//   "sampled"          — rate-based capture
	//   "failure_capture"  — failed session captured regardless of rate
	Reason string `json:"reason"`
	// Warnings carries non-fatal advisories surfaced during snapshotting
	// (e.g. partial copy, redactor errors). Empty on the happy path.
	Warnings []string `json:"warnings,omitempty"`
}
