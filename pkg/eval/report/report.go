// Package report aggregates per-case Results into a stable JSON shape +
// human-readable summary. Phase 2's baseline differ depends on this shape
// being stable — schema_version below is bumped explicitly.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/eval/runner"
)

// SchemaVersion of the report JSON. Bump on breaking shape changes.
const SchemaVersion = "1"

// Report is the top-level eval-run report. Stable JSON shape — Phase 2's
// baseline differ keys off field names exactly.
type Report struct {
	SchemaVersion string       `json:"schema_version"`
	GeneratedAt   time.Time    `json:"generated_at"`
	RunID         string       `json:"run_id,omitempty"`
	Mode          string       `json:"mode"` // "deterministic" | "full"
	Summary       Summary      `json:"summary"`
	Cases         []*CaseEntry `json:"cases"`
}

// Summary is the aggregate pass/fail across all cases.
type Summary struct {
	Total  int `json:"total"`
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

// CaseEntry is the projected result for one case.
type CaseEntry struct {
	CaseID     string           `json:"case_id"`
	Pass       bool             `json:"pass"`
	StartedAt  time.Time        `json:"started_at"`
	EndedAt    time.Time        `json:"ended_at"`
	Assertions []AssertionEntry `json:"assertions"`
	Counts     map[string]int   `json:"counts,omitempty"`
}

// AssertionEntry mirrors evalcase.AssertionResult but with deterministic
// ordering of diagnostic keys for snapshot stability.
type AssertionEntry struct {
	Kind        string         `json:"kind"`
	Pass        bool           `json:"pass"`
	Message     string         `json:"message,omitempty"`
	Diagnostics map[string]any `json:"diagnostics,omitempty"`
}

// Aggregate folds a slice of runner.Results into a Report.
func Aggregate(mode string, results []*runner.Result) *Report {
	r := &Report{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   time.Now().UTC(),
		Mode:          mode,
	}
	for _, res := range results {
		entry := &CaseEntry{
			CaseID:    res.CaseID,
			Pass:      res.Pass,
			StartedAt: res.StartedAt,
			EndedAt:   res.EndedAt,
			Counts:    res.Counts,
		}
		for _, a := range res.Assertions {
			entry.Assertions = append(entry.Assertions, AssertionEntry{
				Kind:        a.Kind,
				Pass:        a.Pass,
				Message:     a.Message,
				Diagnostics: a.Diagnostics,
			})
		}
		r.Cases = append(r.Cases, entry)
		r.Summary.Total++
		if res.Pass {
			r.Summary.Passed++
		} else {
			r.Summary.Failed++
		}
	}
	// Stable order for snapshot tests.
	sort.SliceStable(r.Cases, func(i, j int) bool { return r.Cases[i].CaseID < r.Cases[j].CaseID })
	return r
}

// WriteJSON writes the report as indented JSON to w.
func WriteJSON(w io.Writer, r *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteSummary writes a short human-readable summary to w.
func WriteSummary(w io.Writer, r *Report) error {
	var b strings.Builder
	fmt.Fprintf(&b, "eval report (%s mode)\n", r.Mode)
	fmt.Fprintf(&b, "  total=%d passed=%d failed=%d\n", r.Summary.Total, r.Summary.Passed, r.Summary.Failed)
	for _, c := range r.Cases {
		mark := "PASS"
		if !c.Pass {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "  [%s] %s\n", mark, c.CaseID)
		for _, a := range c.Assertions {
			amark := "ok"
			if !a.Pass {
				amark = "fail"
			}
			fmt.Fprintf(&b, "    - %-26s %s", a.Kind, amark)
			if a.Message != "" {
				fmt.Fprintf(&b, ": %s", a.Message)
			}
			fmt.Fprintln(&b)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}
