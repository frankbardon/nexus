package main

import (
	"testing"
)

func TestDecodeUsage_PromotesTagsAndCost(t *testing.T) {
	payload := map[string]any{
		"Model":   "claude-haiku-4-5-20251001",
		"CostUSD": 0.0042,
		"Usage": map[string]any{
			"PromptTokens":     1000,
			"CompletionTokens": 200,
			"TotalTokens":      1200,
		},
		"Tags": map[string]any{
			"tenant":        "acme",
			"source_plugin": "nexus.agent.react",
		},
		"Metadata": map[string]any{"task_kind": "react_main"},
	}
	rec, ok := decodeUsage(payload)
	if !ok {
		t.Fatal("expected decode ok")
	}
	if rec.tenant() != "acme" {
		t.Errorf("tenant: got %q", rec.tenant())
	}
	if rec.plugin() != "nexus.agent.react" {
		t.Errorf("plugin: got %q", rec.plugin())
	}
	if rec.taskKind() != "react_main" {
		t.Errorf("task_kind: got %q", rec.taskKind())
	}
	if rec.CostUSD != 0.0042 {
		t.Errorf("cost: got %v", rec.CostUSD)
	}
}

func TestDecodeUsage_SkipsEmpty(t *testing.T) {
	if _, ok := decodeUsage(map[string]any{}); ok {
		t.Fatal("empty payload should not decode")
	}
}

func TestCostReport_GroupingAndSort(t *testing.T) {
	r := newCostReport("tenant")
	r.add(usageRecord{Tags: map[string]string{"tenant": "a"}, CostUSD: 0.1})
	r.add(usageRecord{Tags: map[string]string{"tenant": "a"}, CostUSD: 0.2})
	r.add(usageRecord{Tags: map[string]string{"tenant": "b"}, CostUSD: 0.05})
	r.add(usageRecord{Tags: nil, CostUSD: 0.99}) // (unset) bucket

	rows := r.sortedRows()
	if len(rows) != 3 {
		t.Fatalf("expected 3 buckets, got %d", len(rows))
	}
	if rows[0].Key != "(unset)" || rows[0].CostUSD != 0.99 {
		t.Errorf("expected (unset) first by cost, got %+v", rows[0])
	}
	if rows[1].Key != "a" || rows[1].CostUSD != 0.30000000000000004 && rows[1].CostUSD != 0.3 {
		t.Errorf("expected tenant a aggregated, got %+v", rows[1])
	}
	if rows[2].Key != "b" {
		t.Errorf("expected tenant b last, got %+v", rows[2])
	}
}

func TestCostReport_KeyByModel(t *testing.T) {
	r := newCostReport("model")
	r.add(usageRecord{Model: "haiku", CostUSD: 0.01})
	r.add(usageRecord{Model: "sonnet", CostUSD: 1.0})
	r.add(usageRecord{Model: "haiku", CostUSD: 0.02})

	rows := r.sortedRows()
	if len(rows) != 2 || rows[0].Key != "sonnet" || rows[1].Key != "haiku" {
		t.Fatalf("unexpected rows: %+v", rows)
	}
	if rows[1].Calls != 2 {
		t.Errorf("haiku call count: got %d", rows[1].Calls)
	}
}
