package internalflow

import "testing"

func TestSkipForHistory(t *testing.T) {
	tests := []struct {
		name string
		meta map[string]any
		want bool
	}{
		{"nil_meta", nil, false},
		{"empty_meta", map[string]any{}, false},
		{"react_main_records", map[string]any{"task_kind": "react_main", "_source": "nexus.agent.react"}, false},
		{"planexec_step_records", map[string]any{"task_kind": "planexec_step", "_source": "nexus.agent.planexec"}, false},
		{"orchestrator_decompose_records", map[string]any{"task_kind": "orchestrator_decompose"}, false},
		{"plan_skipped", map[string]any{"task_kind": "plan"}, true},
		{"summarise_skipped", map[string]any{"task_kind": "summarise"}, true},
		{"classify_skipped", map[string]any{"task_kind": "classify"}, true},
		{"compact_skipped", map[string]any{"task_kind": "compact"}, true},
		{"subagent_skipped", map[string]any{"task_kind": "subagent"}, true},
		{"unknown_kind_records", map[string]any{"task_kind": "future_agent"}, false},
		{"source_alone_records", map[string]any{"_source": "nexus.agent.react"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SkipForHistory(tc.meta); got != tc.want {
				t.Fatalf("SkipForHistory(%v) = %v, want %v", tc.meta, got, tc.want)
			}
		})
	}
}
