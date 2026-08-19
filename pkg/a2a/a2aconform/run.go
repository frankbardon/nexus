package a2aconform

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Run replays the whole corpus against one mapping.
//
// Each vector is a subtest named for its id, so a failure names the behaviour
// that drifted rather than a line number. Vectors the driver has not declared
// the features for are skipped and reported together at the end: a mapping's
// conformance claim is "these vectors passed AND these were not exercised", and
// hiding the second half would let a mapping claim conformance it does not have.
func Run(t *testing.T, d Driver) {
	t.Helper()

	supported := make(map[Feature]bool, len(d.Features()))
	for _, f := range d.Features() {
		if !f.Known() {
			t.Fatalf("%s declares unknown conformance feature %q", d.Name(), f)
		}
		supported[f] = true
	}

	var skipped []string
	for _, v := range Vectors() {
		if missing := missingFeatures(v, supported); len(missing) > 0 {
			skipped = append(skipped, fmt.Sprintf("%s (needs %s)", v.ID, strings.Join(missing, ", ")))
			t.Run(v.ID, func(t *testing.T) {
				t.Skipf("%s does not declare %s", d.Name(), strings.Join(missing, ", "))
			})
			continue
		}
		t.Run(v.ID, func(t *testing.T) { RunVector(t, d, v) })
	}

	if len(skipped) > 0 {
		sort.Strings(skipped)
		t.Logf("a2a conformance: %s exercised %d of %d vectors; not exercised: %s",
			d.Name(), len(Vectors())-len(skipped), len(Vectors()), strings.Join(skipped, "; "))
	}
}

// missingFeatures reports the features a vector needs and a driver lacks.
func missingFeatures(v Vector, supported map[Feature]bool) []string {
	var missing []string
	for _, f := range v.Features {
		if !supported[f] {
			missing = append(missing, string(f))
		}
	}
	return missing
}

// RunVector replays one vector against one mapping and reports every
// disagreement.
//
// Every difference is reported rather than only the first: a mapping that
// drifted usually drifted in more than one place, and fixing them one test run
// at a time is how a small divergence becomes a long afternoon.
func RunVector(t *testing.T, d Driver, v Vector) {
	t.Helper()

	env := Env{FileDir: t.TempDir()}
	if v.Policy != nil {
		env.Policy = *v.Policy
	}

	session, err := d.Begin(t, v, env)
	if err != nil {
		t.Fatalf("%s could not begin vector %s: %v", d.Name(), v.ID, err)
	}
	defer session.Close()

	obs := Observation{Marks: make(map[int]int)}
	for i, step := range v.Steps {
		if !step.Drives() {
			obs.Marks[i] = len(session.Frames())
			continue
		}
		if err := materialize(env, step); err != nil {
			t.Fatalf("step %d (%s): %v", i, step.Kind, err)
		}
		if err := session.Apply(expand(step)); err != nil {
			t.Fatalf("%s could not apply step %d (%s) of vector %s: %v",
				d.Name(), i, step.Kind, v.ID, err)
		}
	}
	obs.Frames = session.Frames()
	obs.Task = session.Task()

	errs := Check(v, obs)
	if len(errs) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s does not conform to vector %q (%s)\n", d.Name(), v.ID, v.Title)
	fmt.Fprintf(&b, "  why the vector expects what it does: %s\n", v.Rationale)
	for _, err := range errs {
		fmt.Fprintf(&b, "  - %v\n", err)
	}
	b.WriteString("  observed frames:\n")
	for i, f := range obs.Frames {
		fmt.Fprintf(&b, "    %2d %s\n", i, describe(f))
	}
	b.WriteString("  DO NOT weaken the vector to make this pass. Either the mapping has a bug, " +
		"or the expectation is wrong and the fix belongs in the vector's rationale.")
	t.Error(b.String())
}

// materialize writes the file a step says a tool wrote, so a mapping that
// publishes file artifacts has something real to stat and read.
//
// The runner does it rather than the driver because "a file of this size exists
// at this path" is part of the scenario, not part of any mapping.
func materialize(env Env, step Step) error {
	if step.File == nil {
		return nil
	}
	if filepath.IsAbs(step.File.Path) || strings.Contains(step.File.Path, "..") {
		return fmt.Errorf("file path %q must be relative and must not escape the vector's directory", step.File.Path)
	}
	full := filepath.Join(env.FileDir, filepath.FromSlash(step.File.Path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("creating the directory for %s: %w", step.File.Path, err)
	}
	if err := os.WriteFile(full, bytes.Repeat([]byte("x"), step.File.Bytes), 0o644); err != nil {
		return fmt.Errorf("materializing %s: %w", step.File.Path, err)
	}
	return nil
}

// expand resolves the repeat shorthand so a driver always receives literal
// content. A vector that needs a payload larger than its own budget says so
// with outputRepeat rather than embedding kilobytes of filler in the corpus.
func expand(step Step) Step {
	if step.OutputRepeat > 1 {
		step.Output = strings.Repeat(step.Output, step.OutputRepeat)
		step.OutputRepeat = 0
	}
	return step
}
