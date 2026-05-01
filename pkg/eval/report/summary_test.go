package report

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestWriteTerminalSummary_NoColor writes to a *bytes.Buffer (not a tty), so
// the colorizer disables ANSI escapes regardless of NO_COLOR. This is the
// goal: the "plain" branch is what CI logs and golden snapshots assert on.
func TestWriteTerminalSummary_NoColor(t *testing.T) {
	r := &Report{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		Mode:          "deterministic",
		RunID:         "run-test",
		Summary:       Summary{Total: 2, Passed: 1, Failed: 1},
		Cases: []*CaseEntry{
			{CaseID: "ok-case", Pass: true, Assertions: []AssertionEntry{
				{Kind: "event_emitted", Pass: true},
			}},
			{CaseID: "bad-case", Pass: false, Assertions: []AssertionEntry{
				{Kind: "token_budget", Pass: false, Message: "input 9999 > 1000"},
			}},
		},
	}
	var buf bytes.Buffer
	if err := WriteTerminalSummary(&buf, r); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := buf.String()

	// No ANSI escapes when target isn't a terminal.
	if strings.Contains(got, "\033[") {
		t.Errorf("non-tty output should not contain ANSI escapes:\n%s", got)
	}
	for _, want := range []string{
		"eval report (deterministic mode)",
		"run_id=run-test",
		"total=2 passed=1 failed=1",
		"[PASS] ok-case",
		"[FAIL] bad-case",
		"event_emitted",
		"token_budget",
		"input 9999 > 1000",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\nfull output:\n%s", want, got)
		}
	}
}

func TestColorizer_NonTTY(t *testing.T) {
	var buf bytes.Buffer
	c := newColorizer(&buf)
	if c.color {
		t.Error("colorizer should disable color for non-tty")
	}
	if got := c.bold("x"); got != "x" {
		t.Errorf("bold should be plain, got %q", got)
	}
}
