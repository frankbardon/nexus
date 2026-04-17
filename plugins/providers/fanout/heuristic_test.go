package fanout

import (
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
)

func TestSelectByHeuristic_Longest(t *testing.T) {
	p := &Plugin{cfg: config{HeuristicPrefer: "longest"}}
	responses := []events.LLMResponse{
		{Content: "short", Model: "a"},
		{Content: "this is much longer content", Model: "b"},
		{Content: "medium length", Model: "c"},
	}
	now := time.Now()
	receivedAt := []time.Time{now, now, now}

	got := p.selectByHeuristic(responses, receivedAt)
	if got != 1 {
		t.Fatalf("expected index 1 (longest), got %d", got)
	}
}

func TestSelectByHeuristic_Shortest(t *testing.T) {
	p := &Plugin{cfg: config{HeuristicPrefer: "shortest"}}
	responses := []events.LLMResponse{
		{Content: "medium length", Model: "a"},
		{Content: "hi", Model: "b"},
		{Content: "this is much longer content", Model: "c"},
	}
	now := time.Now()
	receivedAt := []time.Time{now, now, now}

	got := p.selectByHeuristic(responses, receivedAt)
	if got != 1 {
		t.Fatalf("expected index 1 (shortest), got %d", got)
	}
}

func TestSelectByHeuristic_Fastest(t *testing.T) {
	p := &Plugin{cfg: config{HeuristicPrefer: "fastest"}}
	responses := []events.LLMResponse{
		{Content: "a", Model: "a"},
		{Content: "b", Model: "b"},
		{Content: "c", Model: "c"},
	}
	base := time.Now()
	receivedAt := []time.Time{
		base.Add(200 * time.Millisecond), // second
		base.Add(500 * time.Millisecond), // third
		base.Add(50 * time.Millisecond),  // first
	}

	got := p.selectByHeuristic(responses, receivedAt)
	if got != 2 {
		t.Fatalf("expected index 2 (fastest), got %d", got)
	}
}

func TestSelectByHeuristic_Cheapest(t *testing.T) {
	p := &Plugin{cfg: config{HeuristicPrefer: "cheapest"}}
	responses := []events.LLMResponse{
		{Content: "a", CostUSD: 0.05, Model: "a"},
		{Content: "b", CostUSD: 0.01, Model: "b"},
		{Content: "c", CostUSD: 0.10, Model: "c"},
	}
	now := time.Now()
	receivedAt := []time.Time{now, now, now}

	got := p.selectByHeuristic(responses, receivedAt)
	if got != 1 {
		t.Fatalf("expected index 1 (cheapest), got %d", got)
	}
}

func TestSelectByHeuristic_RequireFinish(t *testing.T) {
	p := &Plugin{cfg: config{
		HeuristicPrefer:        "longest",
		HeuristicRequireFinish: true,
	}}
	responses := []events.LLMResponse{
		{Content: "this is the longest response by far", FinishReason: "max_tokens", Model: "a"},
		{Content: "short", FinishReason: "end_turn", Model: "b"},
		{Content: "medium content here", FinishReason: "end_turn", Model: "c"},
	}
	now := time.Now()
	receivedAt := []time.Time{now, now, now}

	got := p.selectByHeuristic(responses, receivedAt)
	// Index 0 has longest content but finish_reason != "end_turn", so filtered.
	// Among filtered set [1, 2], index 2 has longest content.
	if got != 2 {
		t.Fatalf("expected index 2 (longest among end_turn), got %d", got)
	}
}

func TestSelectByHeuristic_RequireFinishFallback(t *testing.T) {
	p := &Plugin{cfg: config{
		HeuristicPrefer:        "longest",
		HeuristicRequireFinish: true,
	}}
	responses := []events.LLMResponse{
		{Content: "short", FinishReason: "max_tokens", Model: "a"},
		{Content: "this is the longest response", FinishReason: "max_tokens", Model: "b"},
		{Content: "medium", FinishReason: "stop", Model: "c"},
	}
	now := time.Now()
	receivedAt := []time.Time{now, now, now}

	got := p.selectByHeuristic(responses, receivedAt)
	// All filtered out (none have "end_turn"), so fall back to unfiltered.
	// Index 1 has longest content.
	if got != 1 {
		t.Fatalf("expected index 1 (longest, fallback to unfiltered), got %d", got)
	}
}

func TestSelectByHeuristic_DefaultIsLongest(t *testing.T) {
	p := &Plugin{cfg: config{HeuristicPrefer: ""}}
	responses := []events.LLMResponse{
		{Content: "short", Model: "a"},
		{Content: "this is the longest", Model: "b"},
	}
	now := time.Now()
	receivedAt := []time.Time{now, now}

	got := p.selectByHeuristic(responses, receivedAt)
	if got != 1 {
		t.Fatalf("expected index 1 (default=longest), got %d", got)
	}
}

func TestSelectByHeuristic_SingleResponse(t *testing.T) {
	p := &Plugin{cfg: config{HeuristicPrefer: "longest"}}
	responses := []events.LLMResponse{
		{Content: "only one", Model: "a"},
	}
	now := time.Now()
	receivedAt := []time.Time{now}

	got := p.selectByHeuristic(responses, receivedAt)
	if got != 0 {
		t.Fatalf("expected index 0 (only response), got %d", got)
	}
}

func TestSelectByHeuristic_RequireFinishWithFastest(t *testing.T) {
	p := &Plugin{cfg: config{
		HeuristicPrefer:        "fastest",
		HeuristicRequireFinish: true,
	}}
	responses := []events.LLMResponse{
		{Content: "a", FinishReason: "max_tokens", Model: "a"},
		{Content: "b", FinishReason: "end_turn", Model: "b"},
		{Content: "c", FinishReason: "end_turn", Model: "c"},
	}
	base := time.Now()
	receivedAt := []time.Time{
		base.Add(10 * time.Millisecond),  // fastest but filtered
		base.Add(300 * time.Millisecond), // slower
		base.Add(100 * time.Millisecond), // faster among end_turn
	}

	got := p.selectByHeuristic(responses, receivedAt)
	// Index 0 is fastest but filtered. Among [1, 2], index 2 arrived first.
	if got != 2 {
		t.Fatalf("expected index 2 (fastest among end_turn), got %d", got)
	}
}
