package a2aconform

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
)

// Feature is a capability a mapping either has or does not. It exists so a
// mapping that genuinely cannot express part of the model — the broker's IO
// envelope carries no tool results, so it can produce no tool-result artifacts —
// declares that ONCE, visibly, instead of each vector growing a special case.
//
// A missing feature skips a vector and the skip is reported. It is not an
// escape hatch: a mapping that CAN do something must declare the feature and
// pass the vectors, and a mapping that later gains a capability must add the
// feature and satisfy them.
type Feature string

// The capability vocabulary.
const (
	// FeatureTurn is the baseline: a message drives a turn that produces text.
	// Every mapping has it.
	FeatureTurn Feature = "turn"
	// FeatureFailure is rendering an unrecoverable error as TASK_STATE_FAILED.
	FeatureFailure Feature = "failure"
	// FeatureCancel is client-initiated cancellation to TASK_STATE_CANCELED.
	FeatureCancel Feature = "cancel"
	// FeatureHITL is parking at TASK_STATE_INPUT_REQUIRED and resuming.
	FeatureHITL Feature = "hitl"
	// FeatureToolArtifacts is publishing tool results as artifacts.
	FeatureToolArtifacts Feature = "tool_artifacts"
	// FeatureFileArtifacts is publishing files a turn wrote as artifacts,
	// including the oversized-file degradation.
	FeatureFileArtifacts Feature = "file_artifacts"
	// FeatureArtifactBudget is the per-task artifact ceiling and its suppression
	// notice.
	FeatureArtifactBudget Feature = "artifact_budget"
)

var allFeatures = []Feature{
	FeatureTurn, FeatureFailure, FeatureCancel, FeatureHITL,
	FeatureToolArtifacts, FeatureFileArtifacts, FeatureArtifactBudget,
}

// Features returns every declared feature.
func Features() []Feature {
	out := make([]Feature, len(allFeatures))
	copy(out, allFeatures)
	return out
}

// Known reports whether f is a declared feature.
func (f Feature) Known() bool {
	for _, known := range allFeatures {
		if f == known {
			return true
		}
	}
	return false
}

// Env is what the runner hands a driver for one vector.
type Env struct {
	// FileDir is the directory the runner materializes StepFile paths into. A
	// mapping that publishes file artifacts must resolve reported paths against
	// it. Fresh per vector.
	FileDir string
	// Policy is the artifact bound this vector runs under. Nil fields mean the
	// mapping's own default.
	Policy Policy
}

// Driver is one mapping under test. It is implemented in the mapping's own
// package, where its internals are reachable; this package never imports a
// mapping.
type Driver interface {
	// Name identifies the mapping in failure messages.
	Name() string
	// Features declares what this mapping can express. Vectors requiring
	// anything absent are skipped, and the skips are reported.
	Features() []Feature
	// Begin prepares one vector's scenario. It must NOT start the task: the
	// vector's first step does that, so the opening frame is produced by the
	// mapping rather than by the harness.
	Begin(t *testing.T, v Vector, env Env) (Session, error)
}

// Session is one vector's replay against one mapping.
//
// Frames must return the CANONICAL frame stream a non-extension A2A client would
// see, opening Task snapshot included as the first element. Apply must be
// synchronous: when it returns, every frame the step caused must already be
// visible to Frames.
type Session interface {
	// Apply realizes one abstract step. Assertion steps never reach it.
	Apply(step Step) error
	// Frames returns every canonical frame produced so far, in order.
	Frames() []a2a.StreamResponse
	// Task returns the mapping's own current task snapshot, which is what a
	// non-streaming client would be answered with.
	Task() a2a.Task
	// Close releases whatever Begin acquired. It must be safe to call on a task
	// that never reached a terminal state.
	Close()
}

// Observation is everything the runner collected from one replay. It is split
// out from the checking so Check is a pure function that a test can feed a
// deliberately-wrong observation to — which is how this harness proves it is not
// vacuous.
type Observation struct {
	// Frames is the canonical frame stream, in order.
	Frames []a2a.StreamResponse
	// Task is the mapping's final task snapshot.
	Task a2a.Task
	// Marks records, per step index, how many frames had been observed when the
	// runner reached that step. Only assertion steps have an entry.
	Marks map[int]int
}
