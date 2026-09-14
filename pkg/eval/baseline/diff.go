// Package baseline diffs two eval reports and produces a structured Diff
// + threshold-driven exit-code decision suitable for CI gating.
//
// Inputs may be a single report.json or a directory containing one (the CLI
// accepts both — most users point at tests/eval/reports/<run-id>/). The
// schema_version of both inputs is checked; mismatched versions hard-fail
// rather than silently misalign fields.
package baseline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/frankbardon/nexus/pkg/eval/report"
)

// Diff is the per-run summary of how Fresh compares to Against.
type Diff struct {
	SchemaVersion string       `json:"schema_version"`
	GeneratedAt   time.Time    `json:"generated_at"`
	AgainstRunID  string       `json:"against_run_id,omitempty"`
	FreshRunID    string       `json:"fresh_run_id,omitempty"`
	Summary       DiffSummary  `json:"summary"`
	Cases         []*CaseDelta `json:"cases"`
	// Threshold breaches surface as bools so `nexus eval baseline` can
	// translate them into exit codes without re-walking the structure.
	Breached  Breaches `json:"breaches"`
	NewCases  []string `json:"new_cases,omitempty"`
	GoneCases []string `json:"gone_cases,omitempty"`
}

// DiffSummary aggregates pass/fail movement across the union of cases.
type DiffSummary struct {
	AgainstTotal  int     `json:"against_total"`
	FreshTotal    int     `json:"fresh_total"`
	AgainstPassed int     `json:"against_passed"`
	FreshPassed   int     `json:"fresh_passed"`
	PassRateDelta float64 `json:"pass_rate_delta"`
}

// CaseDelta records how a single case moved between runs.
type CaseDelta struct {
	CaseID         string  `json:"case_id"`
	PassBefore     bool    `json:"pass_before"`
	PassAfter      bool    `json:"pass_after"`
	ScoreDelta     float64 `json:"score_delta"`
	LatencyP50Ms   int     `json:"latency_p50_ms_after,omitempty"`
	LatencyP95Ms   int     `json:"latency_p95_ms_after,omitempty"`
	LatencyP50Diff int     `json:"latency_p50_ms_delta"`
	LatencyP95Diff int     `json:"latency_p95_ms_delta"`
	TokensDelta    int     `json:"tokens_delta"`
	// CustomScoreDeltas holds candidate-minus-baseline for every name
	// present in either side's CustomScores (the union of both key sets).
	// A name present on only one side is treated as zero on the missing
	// side — introducing a new custom score reads as a full gain from 0,
	// and a custom score that disappears reads as a full drop to 0. This
	// mirrors how TokensDelta/latency already treat an absent diagnostic
	// as zero rather than skipping the case.
	CustomScoreDeltas map[string]float64 `json:"custom_score_deltas,omitempty"`
	// Notes are short human-readable explanations of state transitions —
	// "regression" (pass→fail), "recovery" (fail→pass), "" otherwise.
	Note string `json:"note,omitempty"`
}

// Breaches signals which thresholds were crossed; populated by Decide.
type Breaches struct {
	ScoreDrop       bool `json:"score_drop"`
	LatencyP95Drop  bool `json:"latency_p95_drop"`
	NewFailures     bool `json:"new_failures"`
	CustomScoreDrop bool `json:"custom_score_drop"`
}

// Thresholds encodes the eval.baseline config block.
type Thresholds struct {
	// FailOnScoreDrop is the absolute pass-rate drop that triggers a failure
	// exit (e.g. 0.05 = "5 percentage-point drop in pass-rate fails CI").
	// Zero or negative disables the gate.
	FailOnScoreDrop float64
	// FailOnLatencyP95Drop is the relative latency p95 increase ratio that
	// triggers a failure exit (0.20 = "20% slower fails CI"). Negative
	// values disable the gate.
	FailOnLatencyP95Drop float64
	// FailOnCustomScoreDrop is keyed by the same names used in
	// runner.Result.CustomScores / report.CaseEntry.CustomScores. Each
	// name's value is the absolute drop (baseline - candidate) that
	// triggers a failure exit for that name; a name absent from this map
	// is not gated at all. Zero or negative for a given name disables its
	// gate. Each configured name is evaluated independently — any one
	// name's breach, in any one case, fails the run.
	FailOnCustomScoreDrop map[string]float64
}

// Compute builds a Diff from two reports. against is the baseline; fresh is
// the new run. Stable ordering: cases are sorted by ID for snapshot stability.
func Compute(against, fresh *report.Report) (*Diff, error) {
	if against == nil || fresh == nil {
		return nil, fmt.Errorf("both reports required")
	}
	if against.SchemaVersion != fresh.SchemaVersion {
		return nil, fmt.Errorf(
			"schema mismatch: against=%q fresh=%q — bumping versions deliberately invalidates baselines",
			against.SchemaVersion, fresh.SchemaVersion,
		)
	}

	d := &Diff{
		SchemaVersion: against.SchemaVersion,
		GeneratedAt:   time.Now().UTC(),
		AgainstRunID:  against.RunID,
		FreshRunID:    fresh.RunID,
	}

	indexAgainst := map[string]*report.CaseEntry{}
	for _, c := range against.Cases {
		indexAgainst[c.CaseID] = c
	}
	indexFresh := map[string]*report.CaseEntry{}
	for _, c := range fresh.Cases {
		indexFresh[c.CaseID] = c
	}

	all := make(map[string]struct{})
	for k := range indexAgainst {
		all[k] = struct{}{}
	}
	for k := range indexFresh {
		all[k] = struct{}{}
	}
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, id := range keys {
		a := indexAgainst[id]
		f := indexFresh[id]
		switch {
		case a == nil && f != nil:
			d.NewCases = append(d.NewCases, id)
			d.Cases = append(d.Cases, &CaseDelta{
				CaseID:    id,
				PassAfter: f.Pass,
				Note:      "new",
			})
		case a != nil && f == nil:
			d.GoneCases = append(d.GoneCases, id)
			d.Cases = append(d.Cases, &CaseDelta{
				CaseID:     id,
				PassBefore: a.Pass,
				Note:       "missing",
			})
		default:
			cd := &CaseDelta{
				CaseID:     id,
				PassBefore: a.Pass,
				PassAfter:  f.Pass,
			}
			cd.ScoreDelta = boolToScore(f.Pass) - boolToScore(a.Pass)
			latA := latencyP50P95(a)
			latF := latencyP50P95(f)
			cd.LatencyP50Ms = latF.p50
			cd.LatencyP95Ms = latF.p95
			cd.LatencyP50Diff = latF.p50 - latA.p50
			cd.LatencyP95Diff = latF.p95 - latA.p95
			cd.TokensDelta = totalTokens(f) - totalTokens(a)
			cd.CustomScoreDeltas = customScoreDeltas(a, f)
			switch {
			case a.Pass && !f.Pass:
				cd.Note = "regression"
			case !a.Pass && f.Pass:
				cd.Note = "recovery"
			}
			d.Cases = append(d.Cases, cd)
		}
	}

	d.Summary = DiffSummary{
		AgainstTotal:  against.Summary.Total,
		FreshTotal:    fresh.Summary.Total,
		AgainstPassed: against.Summary.Passed,
		FreshPassed:   fresh.Summary.Passed,
		PassRateDelta: passRate(fresh) - passRate(against),
	}
	return d, nil
}

// Decide applies thresholds. Returns true when CI should fail.
func (d *Diff) Decide(t Thresholds) bool {
	d.Breached = Breaches{}

	// Score gate: a drop in pass-rate (negative delta) at or above the
	// threshold magnitude fails. PassRateDelta < 0 means fewer passes.
	if t.FailOnScoreDrop > 0 && -d.Summary.PassRateDelta >= t.FailOnScoreDrop {
		d.Breached.ScoreDrop = true
	}

	// Latency p95 gate: any case whose p95 grew by FailOnLatencyP95Drop
	// (relative to baseline) fails the run. Computed per-case so the diff
	// reflects which case actually moved.
	if t.FailOnLatencyP95Drop > 0 {
		for _, c := range d.Cases {
			// Recover the baseline p95 from after - delta.
			baselineP95 := c.LatencyP95Ms - c.LatencyP95Diff
			if baselineP95 <= 0 || c.LatencyP95Diff <= 0 {
				continue
			}
			ratio := float64(c.LatencyP95Diff) / float64(baselineP95)
			if ratio >= t.FailOnLatencyP95Drop {
				d.Breached.LatencyP95Drop = true
				break
			}
		}
	}

	// New-failure gate: any case that flipped pass→fail is a hard regression.
	for _, c := range d.Cases {
		if c.Note == "regression" {
			d.Breached.NewFailures = true
			break
		}
	}

	// Custom-score gates: each configured name is checked independently
	// against every case's CustomScoreDeltas for that name. A drop (negative
	// delta) whose magnitude is at or above that name's threshold fails the
	// run, exactly like the latency p95 gate above.
	for name, threshold := range t.FailOnCustomScoreDrop {
		if threshold <= 0 {
			continue
		}
		for _, c := range d.Cases {
			delta, ok := c.CustomScoreDeltas[name]
			if !ok {
				continue
			}
			if -delta >= threshold {
				d.Breached.CustomScoreDrop = true
				break
			}
		}
		if d.Breached.CustomScoreDrop {
			break
		}
	}

	return d.Breached.ScoreDrop || d.Breached.LatencyP95Drop || d.Breached.NewFailures || d.Breached.CustomScoreDrop
}

// LoadReport reads a report.json from path. path may be a file or a
// directory; for a dir we look for "report.json" inside.
func LoadReport(path string) (*report.Report, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %q: %w", path, err)
	}
	target := path
	if info.IsDir() {
		target = filepath.Join(path, "report.json")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", target, err)
	}
	var r report.Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode %q: %w", target, err)
	}
	return &r, nil
}

// -- helpers -----------------------------------------------------------------

func passRate(r *report.Report) float64 {
	if r == nil || r.Summary.Total == 0 {
		return 0
	}
	return float64(r.Summary.Passed) / float64(r.Summary.Total)
}

func boolToScore(p bool) float64 {
	if p {
		return 1
	}
	return 0
}

type latStats struct {
	p50, p95 int
}

// latencyP50P95 pulls a synthetic latency view from a case entry by reading
// the `latency` assertion's diagnostics. The runner emits p50_ms / p95_ms
// integers there. Absent assertion or absent keys → zeros.
func latencyP50P95(c *report.CaseEntry) latStats {
	if c == nil {
		return latStats{}
	}
	for _, a := range c.Assertions {
		if a.Kind != "latency" {
			continue
		}
		var s latStats
		if v, ok := a.Diagnostics["p50_ms"]; ok {
			s.p50 = anyToInt(v)
		}
		if v, ok := a.Diagnostics["p95_ms"]; ok {
			s.p95 = anyToInt(v)
		}
		return s
	}
	return latStats{}
}

// totalTokens pulls (input + output) from a case entry's token_budget
// assertion diagnostics.
func totalTokens(c *report.CaseEntry) int {
	if c == nil {
		return 0
	}
	for _, a := range c.Assertions {
		if a.Kind != "token_budget" {
			continue
		}
		var in, out int
		if v, ok := a.Diagnostics["total_input"]; ok {
			in = anyToInt(v)
		}
		if v, ok := a.Diagnostics["total_output"]; ok {
			out = anyToInt(v)
		}
		return in + out
	}
	return 0
}

// customScoreDeltas computes candidate-minus-baseline for the union of both
// case entries' CustomScores keys. A name missing from one side is treated
// as zero on that side — see CaseDelta.CustomScoreDeltas's doc comment.
func customScoreDeltas(against, fresh *report.CaseEntry) map[string]float64 {
	if against == nil && fresh == nil {
		return nil
	}
	var againstScores, freshScores map[string]float64
	if against != nil {
		againstScores = against.CustomScores
	}
	if fresh != nil {
		freshScores = fresh.CustomScores
	}
	if len(againstScores) == 0 && len(freshScores) == 0 {
		return nil
	}
	names := make(map[string]struct{}, len(againstScores)+len(freshScores))
	for name := range againstScores {
		names[name] = struct{}{}
	}
	for name := range freshScores {
		names[name] = struct{}{}
	}
	deltas := make(map[string]float64, len(names))
	for name := range names {
		deltas[name] = freshScores[name] - againstScores[name]
	}
	return deltas
}

func anyToInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	case uint64:
		return int(n)
	}
	return 0
}
