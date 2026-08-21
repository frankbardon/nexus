package a2aconform

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
)

// This file is the harness's own honesty test.
//
// A conformance suite that passes everything is worse than no suite at all: it
// reports conformance a mapping has not earned, and the next mapping is built
// against an oracle that never says no. Check is a pure function of a vector and
// an observation precisely so this file can exist — it builds the observation a
// CONFORMING mapping would produce, asserts Check is silent on it, then breaks
// that observation one way at a time and asserts each break is reported.
//
// Every mutation below corresponds to a rule the corpus exists to hold: the
// opening frame, exact frame comparison, the terminal-close rule, the
// artifacts-before-terminal ordering, the interleaving deviation, the
// assert_* steps and the agreement between the stream and the task snapshot.
// If a rule stops firing, a test here fails rather than a mapping silently
// gaining permission to drift.

const (
	conformTaskID    = "task-conform"
	conformContextID = "ctx-conform"
)

// goodTurnObservation is what a conforming mapping produces for the
// turn-completes vector: SUBMITTED, WORKING, the answer as an artifact, then
// COMPLETED.
func goodTurnObservation() Observation {
	frames := []a2a.StreamResponse{
		a2a.StreamTask(a2a.NewTask(conformTaskID, conformContextID)),
		a2a.StreamStatusUpdate(a2a.NewStatusUpdate(conformTaskID, conformContextID,
			a2a.NewTaskStatus(a2a.TaskStateWorking))),
		a2a.StreamArtifactUpdate(a2a.NewArtifactUpdate(conformTaskID, conformContextID,
			a2a.NewTextArtifact(conformTaskID+"-response", "response", "the answer is 42"))),
		a2a.StreamStatusUpdate(a2a.NewStatusUpdate(conformTaskID, conformContextID,
			a2a.NewTaskStatus(a2a.TaskStateCompleted))),
	}
	return Observation{
		Frames: frames,
		Task:   foldTask(frames),
		// Step 3 of the vector is assert_active, reached after the opening
		// snapshot and the WORKING transition: the model has spoken, the output
		// gates have not run, and the task is still live.
		Marks: map[int]int{3: 2},
	}
}

// goodParkedObservation is what a conforming mapping produces for the
// hitl-parks-stream-open vector: the question arrives, and nothing follows it.
func goodParkedObservation() Observation {
	question := a2a.NewAgentMessage("msg-1", "Approve the destructive migration?").
		InContext(conformContextID).ForTask(conformTaskID)
	question.Metadata = map[string]any{"nexus.hitl.requestId": "q-1"}
	frames := []a2a.StreamResponse{
		a2a.StreamTask(a2a.NewTask(conformTaskID, conformContextID)),
		a2a.StreamStatusUpdate(a2a.NewStatusUpdate(conformTaskID, conformContextID,
			a2a.NewTaskStatus(a2a.TaskStateWorking))),
		a2a.StreamStatusUpdate(a2a.NewStatusUpdate(conformTaskID, conformContextID,
			a2a.NewTaskStatus(a2a.TaskStateInputRequired).WithMessage(question))),
	}
	return Observation{
		Frames: frames,
		Task:   foldTask(frames),
		// Step 3 is assert_parked, reached with all three frames observed.
		Marks: map[int]int{3: 3},
	}
}

// foldTask renders the task snapshot a mapping's own non-streaming view would
// report, by folding the frames exactly as a client would.
func foldTask(frames []a2a.StreamResponse) a2a.Task {
	task := a2a.NewTask(conformTaskID, conformContextID)
	for _, f := range frames {
		switch f.Kind() {
		case a2a.StreamPayloadTask:
			task.Status = f.Task.Status
		case a2a.StreamPayloadStatusUpdate:
			task.Status = f.StatusUpdate.Status
		case a2a.StreamPayloadArtifactUpdate:
			task.Artifacts = append(task.Artifacts, f.ArtifactUpdate.Artifact)
		}
	}
	return task
}

// mustVector fetches a corpus vector, failing loudly if the corpus lost it: a
// test silently skipping the vector it was written for is the failure mode this
// whole file exists to prevent.
func mustVector(t *testing.T, id string) Vector {
	t.Helper()
	v, ok := VectorByID(id)
	if !ok {
		t.Fatalf("vector %q is missing from the corpus", id)
	}
	return v
}

// TestCheckAcceptsAConformingObservation is the control. Without it, every
// mutation below could be "caught" by an oracle that rejects everything, which
// would be just as useless as one that accepts everything.
func TestCheckAcceptsAConformingObservation(t *testing.T) {
	for _, tc := range []struct {
		vector string
		obs    Observation
	}{
		{"turn-completes", goodTurnObservation()},
		{"hitl-parks-stream-open", goodParkedObservation()},
	} {
		t.Run(tc.vector, func(t *testing.T) {
			if errs := Check(mustVector(t, tc.vector), tc.obs); len(errs) > 0 {
				t.Fatalf("Check rejected a conforming observation:\n%s", joinErrors(errs))
			}
		})
	}
}

// TestCheckCatchesDrift breaks a conforming observation one way at a time and
// asserts the break is reported. Each case names the rule it removes.
func TestCheckCatchesDrift(t *testing.T) {
	tests := []struct {
		name string
		// vector is the corpus entry the mutated observation is judged against.
		vector string
		// mutate produces the wrong observation.
		mutate func() Observation
		// want is a substring the reported disagreement must contain, so the
		// test pins WHICH rule fired rather than merely that something did.
		want string
	}{
		{
			name:   "no frames at all",
			vector: "turn-completes",
			mutate: func() Observation { return Observation{} },
			want:   "no frames were produced",
		},
		{
			name:   "the stream does not open with a task snapshot",
			vector: "turn-completes",
			mutate: func() Observation {
				obs := goodTurnObservation()
				obs.Frames = obs.Frames[1:]
				return obs
			},
			want: "specification section 11.7 requires a task snapshot",
		},
		{
			name:   "the turn's answer is never published",
			vector: "turn-completes",
			mutate: func() Observation {
				obs := goodTurnObservation()
				obs.Frames = []a2a.StreamResponse{obs.Frames[0], obs.Frames[1], obs.Frames[3]}
				obs.Task = foldTask(obs.Frames)
				return obs
			},
			want: "missing frame",
		},
		{
			name:   "the answer's text is not what the turn produced",
			vector: "turn-completes",
			mutate: func() Observation {
				obs := goodTurnObservation()
				obs.Frames[2] = a2a.StreamArtifactUpdate(a2a.NewArtifactUpdate(
					conformTaskID, conformContextID,
					a2a.NewTextArtifact(conformTaskID+"-response", "response", "the answer is 41")))
				obs.Task = foldTask(obs.Frames)
				return obs
			},
			want: `want "the answer is 42"`,
		},
		{
			name:   "the artifact carries the wrong id",
			vector: "turn-completes",
			mutate: func() Observation {
				obs := goodTurnObservation()
				obs.Frames[2] = a2a.StreamArtifactUpdate(a2a.NewArtifactUpdate(
					conformTaskID, conformContextID,
					a2a.NewTextArtifact("some-other-id", "response", "the answer is 42")))
				obs.Task = foldTask(obs.Frames)
				return obs
			},
			want: "artifactId",
		},
		{
			name:   "an artifact frame names a different task",
			vector: "turn-completes",
			mutate: func() Observation {
				obs := goodTurnObservation()
				obs.Frames[2] = a2a.StreamArtifactUpdate(a2a.NewArtifactUpdate(
					"task-somebody-else", conformContextID,
					a2a.NewTextArtifact(conformTaskID+"-response", "response", "the answer is 42")))
				return obs
			},
			want: "artifact names task",
		},
		{
			name:   "the answer arrives after the terminal status",
			vector: "turn-completes",
			mutate: func() Observation {
				obs := goodTurnObservation()
				obs.Frames = []a2a.StreamResponse{
					obs.Frames[0], obs.Frames[1], obs.Frames[3], obs.Frames[2],
				}
				return obs
			},
			// SSEWriter refuses anything after a terminal frame, which is the
			// independent half of the oracle catching it first.
			want: "not admissible on an A2A stream",
		},
		{
			name:   "the turn never reaches a terminal state",
			vector: "turn-completes",
			mutate: func() Observation {
				obs := goodTurnObservation()
				obs.Frames = obs.Frames[:3]
				obs.Task = foldTask(obs.Frames)
				return obs
			},
			want: "the stream is still open",
		},
		{
			name:   "an extra frame nobody expected",
			vector: "turn-completes",
			mutate: func() Observation {
				obs := goodTurnObservation()
				obs.Frames = append([]a2a.StreamResponse{obs.Frames[0], obs.Frames[1], obs.Frames[2]},
					a2a.StreamArtifactUpdate(a2a.NewArtifactUpdate(conformTaskID, conformContextID,
						a2a.NewTextArtifact(conformTaskID+"-extra", "extra", "surplus"))),
					obs.Frames[3])
				obs.Task = foldTask(obs.Frames)
				return obs
			},
			want: "unexpected extra frame",
		},
		{
			name:   "the task completed at the model response instead of the end of the turn",
			vector: "turn-completes",
			// The mark moves past the terminal frame, which is exactly what a
			// mapping that completed at llm.response would produce: the
			// assert_active step would be reached with the task already ended.
			mutate: func() Observation {
				obs := goodTurnObservation()
				obs.Marks[3] = len(obs.Frames)
				return obs
			},
			want: "assert_active",
		},
		{
			name:   "the runner never reached an assertion step",
			vector: "turn-completes",
			mutate: func() Observation {
				obs := goodTurnObservation()
				obs.Marks = map[int]int{}
				return obs
			},
			want: "recorded no frame mark",
		},
		{
			name:   "the task snapshot disagrees with the stream",
			vector: "turn-completes",
			mutate: func() Observation {
				obs := goodTurnObservation()
				obs.Task.Artifacts = nil
				return obs
			},
			want: "the task snapshot is missing artifact",
		},
		{
			name:   "the snapshot's final state is not the one the vector pins",
			vector: "turn-completes",
			mutate: func() Observation {
				obs := goodTurnObservation()
				obs.Task.Status = a2a.NewTaskStatus(a2a.TaskStateFailed)
				return obs
			},
			want: "final task state",
		},
		{
			name:   "the snapshot names a different task from the stream",
			vector: "turn-completes",
			mutate: func() Observation {
				obs := goodTurnObservation()
				obs.Task.ID = "task-somebody-else"
				return obs
			},
			want: "the task snapshot names task",
		},
		{
			name:   "a parked task is reported as terminated",
			vector: "hitl-parks-stream-open",
			mutate: func() Observation {
				obs := goodParkedObservation()
				obs.Frames[2] = a2a.StreamStatusUpdate(a2a.NewStatusUpdate(
					conformTaskID, conformContextID, a2a.NewTaskStatus(a2a.TaskStateCompleted)))
				obs.Task = foldTask(obs.Frames)
				return obs
			},
			want: "assert_parked",
		},
		{
			name:   "the stream closes on a non-terminal state",
			vector: "hitl-parks-stream-open",
			// A mapping that "tidied up" by closing the stream at
			// INPUT_REQUIRED renders that as a terminal frame, which is
			// indistinguishable client-side from a dropped connection.
			mutate: func() Observation {
				obs := goodParkedObservation()
				obs.Frames = append(obs.Frames, a2a.StreamStatusUpdate(a2a.NewStatusUpdate(
					conformTaskID, conformContextID, a2a.NewTaskStatus(a2a.TaskStateCanceled))))
				obs.Task = foldTask(obs.Frames)
				return obs
			},
			want: "the stream closed",
		},
		{
			name:   "the question is not carried on the status message",
			vector: "hitl-parks-stream-open",
			mutate: func() Observation {
				obs := goodParkedObservation()
				obs.Frames[2] = a2a.StreamStatusUpdate(a2a.NewStatusUpdate(
					conformTaskID, conformContextID, a2a.NewTaskStatus(a2a.TaskStateInputRequired)))
				return obs
			},
			want: "status carries no message",
		},
		{
			name:   "the question loses its originating hitl request id",
			vector: "hitl-parks-stream-open",
			mutate: func() Observation {
				obs := goodParkedObservation()
				msg := a2a.NewAgentMessage("msg-1", "Approve the destructive migration?").
					InContext(conformContextID).ForTask(conformTaskID)
				obs.Frames[2] = a2a.StreamStatusUpdate(a2a.NewStatusUpdate(
					conformTaskID, conformContextID,
					a2a.NewTaskStatus(a2a.TaskStateInputRequired).WithMessage(msg)))
				return obs
			},
			want: "nexus.hitl.requestId",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := Check(mustVector(t, tc.vector), tc.mutate())
			if len(errs) == 0 {
				t.Fatalf("Check accepted a broken observation; it must report %q", tc.want)
			}
			if joined := joinErrors(errs); !strings.Contains(joined, tc.want) {
				t.Errorf("Check reported:\n%s\nwant a disagreement containing %q", joined, tc.want)
			}
		})
	}
}

// TestOrderingRulesAreAssertedByTheCorpus pins the two deliberate ordering
// deviations as rules the corpus actually exercises. A vector that stopped
// asserting them would leave the deviations undefended, which is how a second
// mapping "fixes" them into divergence.
func TestOrderingRulesAreAssertedByTheCorpus(t *testing.T) {
	var precede, follows int
	for _, v := range Vectors() {
		if v.Expect.ArtifactsPrecedeTerminal {
			precede++
		}
		if v.Expect.StatusFollowsArtifact {
			follows++
		}
	}
	if precede == 0 {
		t.Error("no vector asserts artifactsPrecedeTerminal; the artifacts-before-terminal rule is undefended")
	}
	if follows == 0 {
		t.Error("no vector asserts statusFollowsArtifact; the interleaving deviation is undefended")
	}
}

// TestOrderingChecksFire proves the two ordering rules are more than declarative
// flags: an observation that violates each is reported.
func TestOrderingChecksFire(t *testing.T) {
	v := mustVector(t, "turn-completes")
	if !v.Expect.ArtifactsPrecedeTerminal || !v.Expect.StatusFollowsArtifact {
		t.Fatal("turn-completes no longer asserts the ordering rules this test exercises")
	}

	// Artifacts, but no terminal frame for them to precede.
	obs := goodTurnObservation()
	obs.Frames = obs.Frames[:3]
	obs.Task = foldTask(obs.Frames)
	if !containsError(Check(v, obs), "no terminal frame was produced") {
		t.Error("checkOrdering did not report artifacts with no terminal status to precede")
	}

	// A phase-gated stream: every status ahead of every artifact, which is the
	// literal reading of section 11.7 this corpus exists to keep open.
	obs = goodTurnObservation()
	obs.Frames = []a2a.StreamResponse{obs.Frames[0], obs.Frames[1], obs.Frames[3], obs.Frames[2]}
	if !containsError(Check(v, obs), "this vector exists to pin the interleaving deviation open") {
		t.Error("checkOrdering did not report a stream with no status after an artifact")
	}
}

// TestUnsupportedFeatureSkipsRatherThanPasses pins the feature gate: a mapping
// declaring nothing must SKIP the corpus, visibly, rather than sail through it.
// A gate that silently passed would let a mapping claim conformance it never
// exercised.
func TestUnsupportedFeatureSkipsRatherThanPasses(t *testing.T) {
	supported := map[Feature]bool{FeatureTurn: true}
	var skipped, exercised int
	for _, v := range Vectors() {
		if len(missingFeatures(v, supported)) > 0 {
			skipped++
			continue
		}
		exercised++
	}
	if skipped == 0 {
		t.Error("a turn-only mapping skips nothing; the feature gate is not doing anything")
	}
	if exercised == 0 {
		t.Error("a turn-only mapping exercises nothing; the baseline vector must not be feature-gated away")
	}
}

// containsError reports whether any reported disagreement mentions want.
func containsError(errs []error, want string) bool {
	return strings.Contains(joinErrors(errs), want)
}

// joinErrors renders a disagreement list for a failure message.
func joinErrors(errs []error) string {
	var b strings.Builder
	for _, err := range errs {
		b.WriteString("  - ")
		b.WriteString(err.Error())
		b.WriteString("\n")
	}
	return b.String()
}
