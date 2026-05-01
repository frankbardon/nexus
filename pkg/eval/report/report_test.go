package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	evalcase "github.com/frankbardon/nexus/pkg/eval/case"
	"github.com/frankbardon/nexus/pkg/eval/runner"
)

func TestAggregate_Stable(t *testing.T) {
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	results := []*runner.Result{
		{
			CaseID:    "alpha",
			Pass:      true,
			StartedAt: t0,
			EndedAt:   t0.Add(time.Second),
			Assertions: []evalcase.AssertionResult{
				{Kind: "event_emitted", Pass: true},
			},
			Counts: map[string]int{"io.input": 2},
		},
		{
			CaseID: "beta",
			Pass:   false,
			Assertions: []evalcase.AssertionResult{
				{Kind: "event_emitted", Pass: false, Message: "missing"},
			},
		},
	}

	r := Aggregate("deterministic", results)
	if r.Summary.Total != 2 || r.Summary.Passed != 1 || r.Summary.Failed != 1 {
		t.Errorf("Summary=%+v", r.Summary)
	}
	if r.Mode != "deterministic" {
		t.Errorf("Mode=%q", r.Mode)
	}
	if r.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion=%q", r.SchemaVersion)
	}
	if r.Cases[0].CaseID != "alpha" {
		t.Errorf("Cases not sorted: first=%q", r.Cases[0].CaseID)
	}
}

func TestWriteJSON_SchemaShape(t *testing.T) {
	r := &Report{
		SchemaVersion: SchemaVersion,
		Mode:          "deterministic",
		GeneratedAt:   time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		Summary:       Summary{Total: 1, Passed: 1, Failed: 0},
		Cases: []*CaseEntry{
			{
				CaseID: "alpha",
				Pass:   true,
				Assertions: []AssertionEntry{
					{Kind: "event_emitted", Pass: true, Diagnostics: map[string]any{"count": 2}},
				},
				Counts: map[string]int{"io.input": 2},
			},
		},
	}

	var buf bytes.Buffer
	if err := WriteJSON(&buf, r); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	// Parse back and check stable top-level keys.
	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"schema_version", "generated_at", "mode", "summary", "cases"} {
		if _, ok := decoded[k]; !ok {
			t.Errorf("missing top-level key %q", k)
		}
	}
	if decoded["schema_version"] != SchemaVersion {
		t.Errorf("schema_version=%v", decoded["schema_version"])
	}

	// Snapshot a few key fields exactly.
	cases, _ := decoded["cases"].([]any)
	if len(cases) != 1 {
		t.Fatalf("cases len=%d", len(cases))
	}
	c0 := cases[0].(map[string]any)
	if c0["case_id"] != "alpha" {
		t.Errorf("case_id=%v", c0["case_id"])
	}
	asserts := c0["assertions"].([]any)
	a0 := asserts[0].(map[string]any)
	if a0["kind"] != "event_emitted" || a0["pass"] != true {
		t.Errorf("assertion shape: %+v", a0)
	}
}

func TestWriteSummary_Human(t *testing.T) {
	r := Aggregate("deterministic", []*runner.Result{
		{
			CaseID: "alpha",
			Pass:   false,
			Assertions: []evalcase.AssertionResult{
				{Kind: "event_emitted", Pass: false, Message: "missing tool.invoke"},
				{Kind: "latency", Pass: true},
			},
		},
	})
	var buf bytes.Buffer
	if err := WriteSummary(&buf, r); err != nil {
		t.Fatalf("WriteSummary: %v", err)
	}
	s := buf.String()
	for _, want := range []string{"FAIL", "alpha", "event_emitted", "missing tool.invoke", "latency"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q: %s", want, s)
		}
	}
}
