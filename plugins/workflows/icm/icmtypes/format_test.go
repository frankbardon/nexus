package icmtypes

import (
	"strings"
	"testing"
)

func TestFormatStageStarted(t *testing.T) {
	phase, content := FormatStageStarted(ICMStageStarted{
		StageID:     "04_assemble",
		PostureName: "blog_editor",
		Order:       4,
	})
	if phase != "icm.stage" {
		t.Errorf("phase = %q, want icm.stage", phase)
	}
	for _, want := range []string{"4", "04_assemble", "blog_editor"} {
		if !strings.Contains(content, want) {
			t.Errorf("content %q missing %q", content, want)
		}
	}
}

func TestFormatStageIteration_WithFailures(t *testing.T) {
	_, content := FormatStageIteration(ICMStageIteration{
		StageID:       "04_assemble",
		Iteration:     2,
		MaxIterations: 3,
		ExitFailures: []ConditionResult{
			{Type: "native", Name: "above_total_floor", Verdict: "fail"},
			{Type: "command", Name: "link_audit", Verdict: "fail"},
			// Pass entries must be filtered.
			{Type: "regex", Name: "starts_with_h1", Verdict: "pass"},
		},
	})
	for _, want := range []string{"2/3", "above_total_floor", "link_audit"} {
		if !strings.Contains(content, want) {
			t.Errorf("content %q missing %q", content, want)
		}
	}
	if strings.Contains(content, "starts_with_h1") {
		t.Errorf("pass entry leaked into failure list: %q", content)
	}
}

func TestFormatFanoutItem_StatusMarkers(t *testing.T) {
	cases := []struct {
		status string
		marker string
	}{
		{"active", "▸"},
		{"completed", "✓"},
		{"failed", "✗"},
	}
	for _, tc := range cases {
		_, content := FormatFanoutItem(ICMFanoutItem{
			StageID: "03_draft_sections",
			ItemID:  "intro",
			Index:   1,
			Total:   4,
			Status:  tc.status,
		})
		if !strings.HasPrefix(content, tc.marker) {
			t.Errorf("status %q: content %q missing marker %q", tc.status, content, tc.marker)
		}
	}
}

func TestFormatPredicateFailed_IncludesFeedback(t *testing.T) {
	_, content := FormatPredicateFailed(ICMPredicateFailed{
		StageID:       "04_assemble",
		Container:     "loop.until",
		PredicateName: "link_audit",
		PredicateType: "command",
		Feedback:      "link_audit: 1 suspicious link(s) found",
	})
	for _, want := range []string{"loop.until", "link_audit", "command", "suspicious"} {
		if !strings.Contains(content, want) {
			t.Errorf("content %q missing %q", content, want)
		}
	}
}

func TestFormatRunHalted_CancelledVerb(t *testing.T) {
	_, content := FormatRunHalted(ICMRunHalted{
		Reason:    "context cancelled",
		Cancelled: true,
	})
	if !strings.Contains(content, "cancelled") {
		t.Errorf("expected 'cancelled' verb, got %q", content)
	}
}
