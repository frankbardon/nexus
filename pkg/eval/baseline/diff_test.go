package baseline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/eval/report"
)

func makeReport(runID string, cases ...*report.CaseEntry) *report.Report {
	r := &report.Report{
		SchemaVersion: report.SchemaVersion,
		GeneratedAt:   time.Now().UTC(),
		RunID:         runID,
		Mode:          "deterministic",
		Cases:         cases,
	}
	for _, c := range cases {
		r.Summary.Total++
		if c.Pass {
			r.Summary.Passed++
		} else {
			r.Summary.Failed++
		}
	}
	return r
}

func passCase(id string) *report.CaseEntry {
	return &report.CaseEntry{
		CaseID: id, Pass: true,
		Assertions: []report.AssertionEntry{
			{Kind: "latency", Pass: true, Diagnostics: map[string]any{"p50_ms": 100, "p95_ms": 200}},
			{Kind: "token_budget", Pass: true, Diagnostics: map[string]any{"total_input": 500, "total_output": 100}},
		},
	}
}

func failCase(id string) *report.CaseEntry {
	c := passCase(id)
	c.Pass = false
	return c
}

func TestCompute_NoMovement(t *testing.T) {
	a := makeReport("run-a", passCase("c1"), passCase("c2"))
	b := makeReport("run-b", passCase("c1"), passCase("c2"))
	d, err := Compute(a, b)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if d.Summary.PassRateDelta != 0 {
		t.Errorf("pass-rate delta should be 0, got %v", d.Summary.PassRateDelta)
	}
	for _, c := range d.Cases {
		if c.Note != "" {
			t.Errorf("case %s had note %q with no movement", c.CaseID, c.Note)
		}
	}
	if d.Decide(Thresholds{FailOnScoreDrop: 0.05, FailOnLatencyP95Drop: 0.20}) {
		t.Error("expected pass with no movement")
	}
}

func TestCompute_Regression(t *testing.T) {
	a := makeReport("run-a", passCase("c1"), passCase("c2"))
	b := makeReport("run-b", passCase("c1"), failCase("c2"))
	d, err := Compute(a, b)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	var found bool
	for _, c := range d.Cases {
		if c.CaseID == "c2" && c.Note == "regression" {
			found = true
		}
	}
	if !found {
		t.Error("expected c2 regression note")
	}
	if !d.Decide(Thresholds{FailOnScoreDrop: 0.05}) {
		t.Error("expected score-drop breach")
	}
	if !d.Breached.NewFailures {
		t.Error("expected NewFailures breach")
	}
}

func TestCompute_AddRemove(t *testing.T) {
	a := makeReport("run-a", passCase("c1"), passCase("removed"))
	b := makeReport("run-b", passCase("c1"), passCase("added"))
	d, err := Compute(a, b)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(d.NewCases) != 1 || d.NewCases[0] != "added" {
		t.Errorf("new cases: %v", d.NewCases)
	}
	if len(d.GoneCases) != 1 || d.GoneCases[0] != "removed" {
		t.Errorf("gone cases: %v", d.GoneCases)
	}
}

func TestDecide_LatencyP95Drop(t *testing.T) {
	a := &report.Report{
		SchemaVersion: report.SchemaVersion,
		Cases: []*report.CaseEntry{
			{CaseID: "c1", Pass: true, Assertions: []report.AssertionEntry{
				{Kind: "latency", Pass: true, Diagnostics: map[string]any{"p50_ms": 100, "p95_ms": 200}},
			}},
		},
		Summary: report.Summary{Total: 1, Passed: 1},
	}
	b := &report.Report{
		SchemaVersion: report.SchemaVersion,
		Cases: []*report.CaseEntry{
			{CaseID: "c1", Pass: true, Assertions: []report.AssertionEntry{
				// p95 grew from 200 → 280 = +40% (>= 20% threshold).
				{Kind: "latency", Pass: true, Diagnostics: map[string]any{"p50_ms": 100, "p95_ms": 280}},
			}},
		},
		Summary: report.Summary{Total: 1, Passed: 1},
	}
	d, err := Compute(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Decide(Thresholds{FailOnLatencyP95Drop: 0.20}) {
		t.Errorf("expected latency breach; diff=%+v", d)
	}
	if !d.Breached.LatencyP95Drop {
		t.Error("LatencyP95Drop flag not set")
	}
}

func TestSchemaMismatch(t *testing.T) {
	a := makeReport("a", passCase("x"))
	b := makeReport("b", passCase("x"))
	b.SchemaVersion = "999"
	if _, err := Compute(a, b); err == nil {
		t.Error("expected schema mismatch error")
	}
}

func TestLoadReport_FileAndDir(t *testing.T) {
	dir := t.TempDir()
	r := makeReport("test-run", passCase("c1"))
	data, _ := json.Marshal(r)

	// File path.
	filePath := filepath.Join(dir, "report.json")
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadReport(filePath)
	if err != nil {
		t.Fatalf("file load: %v", err)
	}
	if got.RunID != "test-run" {
		t.Errorf("got %q", got.RunID)
	}

	// Dir path.
	subdir := filepath.Join(dir, "run-1")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "report.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = LoadReport(subdir)
	if err != nil {
		t.Fatalf("dir load: %v", err)
	}
	if got.RunID != "test-run" {
		t.Errorf("got %q", got.RunID)
	}
}
