package evalcase

import (
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
)

// helper: synthesize a stream with explicit timestamps starting at t0.
func mkStream(types ...string) []ObservedEvent {
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	out := make([]ObservedEvent, len(types))
	for i, typ := range types {
		out[i] = ObservedEvent{
			Type:      typ,
			Timestamp: t0.Add(time.Duration(i) * time.Millisecond),
		}
	}
	return out
}

// -- event_emitted -----------------------------------------------------------

func TestEventEmitted_Pass(t *testing.T) {
	stream := mkStream("io.input", "io.input", "agent.turn.start", "agent.turn.end")
	a := Assertion{
		Kind:         "event_emitted",
		EventEmitted: &EventEmittedSpec{Type: "io.input", Count: &CountRange{Min: 1, Max: 5}},
	}
	r := a.Evaluate(stream, nil)
	if !r.Pass {
		t.Fatalf("expected pass, msg=%s", r.Message)
	}
}

func TestEventEmitted_FailZero(t *testing.T) {
	stream := mkStream("io.input")
	a := Assertion{
		Kind:         "event_emitted",
		EventEmitted: &EventEmittedSpec{Type: "tool.invoke", Count: &CountRange{Min: 1}},
	}
	r := a.Evaluate(stream, nil)
	if r.Pass {
		t.Fatal("expected fail")
	}
}

func TestEventEmitted_EmptyStream(t *testing.T) {
	a := Assertion{
		Kind:         "event_emitted",
		EventEmitted: &EventEmittedSpec{Type: "io.input"},
	}
	r := a.Evaluate(nil, nil)
	if r.Pass {
		t.Fatal("expected fail on empty stream (default min=1)")
	}
}

func TestEventEmitted_WithWhere(t *testing.T) {
	stream := []ObservedEvent{
		{Type: "tool.invoke", Payload: events.ToolCall{Name: "shell"}, Timestamp: time.Now()},
		{Type: "tool.invoke", Payload: events.ToolCall{Name: "read_file"}, Timestamp: time.Now()},
	}
	a := Assertion{
		Kind:         "event_emitted",
		EventEmitted: &EventEmittedSpec{Type: "tool.invoke", Where: map[string]any{"name": "shell"}},
	}
	r := a.Evaluate(stream, nil)
	if !r.Pass {
		t.Fatalf("expected pass: %s", r.Message)
	}

	a.EventEmitted.Where = map[string]any{"name": "missing"}
	r = a.Evaluate(stream, nil)
	if r.Pass {
		t.Fatal("expected fail")
	}
}

// -- event_sequence_distance -------------------------------------------------

func TestEventSequenceDistance_Identical(t *testing.T) {
	s := mkStream("a", "b", "c")
	a := Assertion{
		Kind:                  "event_sequence_distance",
		EventSequenceDistance: &EventSequenceDistanceSpec{Threshold: 0.0},
	}
	r := a.Evaluate(s, s)
	if !r.Pass {
		t.Fatalf("expected pass: %s", r.Message)
	}
}

func TestEventSequenceDistance_OneEdit(t *testing.T) {
	g := mkStream("a", "b", "c")
	o := mkStream("a", "b", "d") // 1 substitution; ratio = 1/3 ≈ 0.333
	a := Assertion{
		Kind:                  "event_sequence_distance",
		EventSequenceDistance: &EventSequenceDistanceSpec{Threshold: 0.5},
	}
	r := a.Evaluate(o, g)
	if !r.Pass {
		t.Fatalf("expected pass: %s", r.Message)
	}
	a.EventSequenceDistance.Threshold = 0.1
	r = a.Evaluate(o, g)
	if r.Pass {
		t.Fatal("expected fail at threshold 0.1")
	}
}

func TestEventSequenceDistance_BothEmpty(t *testing.T) {
	a := Assertion{
		Kind:                  "event_sequence_distance",
		EventSequenceDistance: &EventSequenceDistanceSpec{Threshold: 0.0},
	}
	r := a.Evaluate(nil, nil)
	if !r.Pass {
		t.Fatal("empty/empty should pass at any threshold")
	}
}

func TestEventSequenceDistance_FilterIgnoresNoise(t *testing.T) {
	g := mkStream("a", "b", "c")
	o := mkStream("a", "core.tick", "b", "core.tick", "c")
	a := Assertion{
		Kind: "event_sequence_distance",
		EventSequenceDistance: &EventSequenceDistanceSpec{
			Threshold: 0.0,
			Filter:    []string{"a", "b", "c"},
		},
	}
	r := a.Evaluate(o, g)
	if !r.Pass {
		t.Fatalf("expected pass after filter: %s", r.Message)
	}
}

// -- tool_invocation_parity --------------------------------------------------

func TestToolInvocationParity_Pass(t *testing.T) {
	g := []ObservedEvent{
		{Type: "tool.invoke", Payload: events.ToolCall{Name: "shell", Arguments: map[string]any{"cmd": "ls"}}},
		{Type: "tool.invoke", Payload: events.ToolCall{Name: "read_file", Arguments: map[string]any{"path": "x"}}},
	}
	o := []ObservedEvent{
		{Type: "tool.invoke", Payload: events.ToolCall{Name: "shell", Arguments: map[string]any{"cmd": "different"}}},
		{Type: "tool.invoke", Payload: events.ToolCall{Name: "read_file", Arguments: map[string]any{"path": "y"}}},
	}
	a := Assertion{
		Kind:                 "tool_invocation_parity",
		ToolInvocationParity: &ToolInvocationParitySpec{ArgKeys: true},
	}
	r := a.Evaluate(o, g)
	if !r.Pass {
		t.Fatalf("expected pass: %s", r.Message)
	}
}

func TestToolInvocationParity_CountDiff(t *testing.T) {
	g := []ObservedEvent{
		{Type: "tool.invoke", Payload: events.ToolCall{Name: "shell"}},
	}
	o := []ObservedEvent{
		{Type: "tool.invoke", Payload: events.ToolCall{Name: "shell"}},
		{Type: "tool.invoke", Payload: events.ToolCall{Name: "shell"}},
		{Type: "tool.invoke", Payload: events.ToolCall{Name: "shell"}},
	}
	a := Assertion{
		Kind:                 "tool_invocation_parity",
		ToolInvocationParity: &ToolInvocationParitySpec{CountTolerance: 1},
	}
	r := a.Evaluate(o, g)
	if r.Pass {
		t.Fatal("expected fail")
	}
}

func TestToolInvocationParity_ArgKeysDiffer(t *testing.T) {
	g := []ObservedEvent{
		{Type: "tool.invoke", Payload: events.ToolCall{Name: "shell", Arguments: map[string]any{"cmd": "x"}}},
	}
	o := []ObservedEvent{
		{Type: "tool.invoke", Payload: events.ToolCall{Name: "shell", Arguments: map[string]any{"cmd": "x", "extra": 1}}},
	}
	a := Assertion{
		Kind:                 "tool_invocation_parity",
		ToolInvocationParity: &ToolInvocationParitySpec{ArgKeys: true},
	}
	r := a.Evaluate(o, g)
	if r.Pass {
		t.Fatal("expected fail (arg keys diverge)")
	}
}

// -- event_count_bounds ------------------------------------------------------

func TestEventCountBounds_Pass(t *testing.T) {
	s := mkStream("io.input", "io.input", "tool.invoke")
	a := Assertion{
		Kind: "event_count_bounds",
		EventCountBounds: &EventCountBoundsSpec{Bounds: map[string]CountRange{
			"io.input":    {Min: 1, Max: 5},
			"tool.invoke": {Min: 1},
		}},
	}
	r := a.Evaluate(s, nil)
	if !r.Pass {
		t.Fatalf("expected pass: %s", r.Message)
	}
}

func TestEventCountBounds_FailMax(t *testing.T) {
	s := mkStream("io.input", "io.input", "io.input")
	a := Assertion{
		Kind: "event_count_bounds",
		EventCountBounds: &EventCountBoundsSpec{Bounds: map[string]CountRange{
			"io.input": {Min: 1, Max: 2},
		}},
	}
	r := a.Evaluate(s, nil)
	if r.Pass {
		t.Fatal("expected fail")
	}
}

func TestEventCountBounds_DuplicateEventsAreCounted(t *testing.T) {
	s := mkStream("x", "x", "x", "x")
	a := Assertion{
		Kind: "event_count_bounds",
		EventCountBounds: &EventCountBoundsSpec{Bounds: map[string]CountRange{
			"x": {Min: 4, Max: 4},
		}},
	}
	r := a.Evaluate(s, nil)
	if !r.Pass {
		t.Fatalf("expected pass on exact match, msg=%s", r.Message)
	}
}

// -- event_sequence_strict ---------------------------------------------------

func TestEventSequenceStrict_Pass(t *testing.T) {
	s := mkStream("a", "b", "c")
	a := Assertion{
		Kind:                "event_sequence_strict",
		EventSequenceStrict: &EventSequenceStrictSpec{Pattern: []string{"a", "b", "c"}},
	}
	r := a.Evaluate(s, nil)
	if !r.Pass {
		t.Fatalf("expected pass: %s", r.Message)
	}
}

func TestEventSequenceStrict_Fail(t *testing.T) {
	s := mkStream("a", "b", "d")
	a := Assertion{
		Kind:                "event_sequence_strict",
		EventSequenceStrict: &EventSequenceStrictSpec{Pattern: []string{"a", "b", "c"}},
	}
	r := a.Evaluate(s, nil)
	if r.Pass {
		t.Fatal("expected fail")
	}
}

func TestEventSequenceStrict_FilterDropsCoreTick(t *testing.T) {
	s := mkStream("a", "core.tick", "b", "core.tick")
	a := Assertion{
		Kind: "event_sequence_strict",
		EventSequenceStrict: &EventSequenceStrictSpec{
			Pattern: []string{"a", "b"},
			Filter:  []string{"a", "b"},
		},
	}
	r := a.Evaluate(s, nil)
	if !r.Pass {
		t.Fatalf("expected pass: %s", r.Message)
	}
}

func TestEventSequenceStrict_EmptyStream(t *testing.T) {
	a := Assertion{
		Kind:                "event_sequence_strict",
		EventSequenceStrict: &EventSequenceStrictSpec{Pattern: []string{"a"}},
	}
	r := a.Evaluate(nil, nil)
	if r.Pass {
		t.Fatal("empty stream should not match non-empty pattern")
	}
}

// -- token_budget ------------------------------------------------------------

func TestTokenBudget_Pass(t *testing.T) {
	t0 := time.Now()
	stream := []ObservedEvent{
		{Type: "agent.turn.start", Timestamp: t0},
		{Type: "llm.response", Timestamp: t0.Add(time.Millisecond), Payload: events.LLMResponse{Usage: events.Usage{PromptTokens: 100, CompletionTokens: 50}}},
		{Type: "agent.turn.end", Timestamp: t0.Add(2 * time.Millisecond)},
	}
	a := Assertion{
		Kind:        "token_budget",
		TokenBudget: &TokenBudgetSpec{MaxInputTokens: 1000, MaxOutputTokens: 1000},
	}
	r := a.Evaluate(stream, nil)
	if !r.Pass {
		t.Fatalf("expected pass: %s", r.Message)
	}
}

func TestTokenBudget_SessionFails(t *testing.T) {
	t0 := time.Now()
	stream := []ObservedEvent{
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{Usage: events.Usage{PromptTokens: 1500}}},
	}
	a := Assertion{
		Kind:        "token_budget",
		TokenBudget: &TokenBudgetSpec{MaxInputTokens: 1000},
	}
	r := a.Evaluate(stream, nil)
	if r.Pass {
		t.Fatal("expected fail")
	}
}

func TestTokenBudget_PerTurn(t *testing.T) {
	t0 := time.Now()
	stream := []ObservedEvent{
		{Type: "agent.turn.start", Timestamp: t0},
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{Usage: events.Usage{PromptTokens: 200}}},
		{Type: "agent.turn.end", Timestamp: t0},
		{Type: "agent.turn.start", Timestamp: t0},
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{Usage: events.Usage{PromptTokens: 800}}},
		{Type: "agent.turn.end", Timestamp: t0},
	}
	a := Assertion{
		Kind:        "token_budget",
		TokenBudget: &TokenBudgetSpec{MaxInputTokens: 500, PerTurn: true},
	}
	r := a.Evaluate(stream, nil)
	if r.Pass {
		t.Fatal("expected fail (turn 2 exceeds 500)")
	}
}

// -- latency -----------------------------------------------------------------

func TestLatency_FromTimestamps(t *testing.T) {
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	stream := []ObservedEvent{
		{Type: "agent.turn.start", Timestamp: t0},
		{Type: "agent.turn.end", Timestamp: t0.Add(100 * time.Millisecond)},
		{Type: "agent.turn.start", Timestamp: t0.Add(200 * time.Millisecond)},
		{Type: "agent.turn.end", Timestamp: t0.Add(700 * time.Millisecond)},
	}
	a := Assertion{
		Kind:    "latency",
		Latency: &LatencySpec{P50Ms: 1000, P95Ms: 1000},
	}
	r := a.Evaluate(stream, nil)
	if !r.Pass {
		t.Fatalf("expected pass: %s", r.Message)
	}

	a.Latency = &LatencySpec{P50Ms: 50}
	r = a.Evaluate(stream, nil)
	if r.Pass {
		t.Fatal("expected fail")
	}
}

func TestLatency_NoTurns(t *testing.T) {
	a := Assertion{Kind: "latency", Latency: &LatencySpec{P50Ms: 100}}
	r := a.Evaluate(nil, nil)
	if !r.Pass {
		t.Fatal("no turns should pass trivially")
	}
}

// -- ParseAssertions round-trip ---------------------------------------------

func TestParseAssertions_AllKinds(t *testing.T) {
	yaml := `deterministic:
  - kind: event_emitted
    type: io.input
  - kind: event_sequence_distance
    threshold: 0.2
  - kind: tool_invocation_parity
    arg_keys: true
  - kind: event_count_bounds
    bounds:
      io.input: { min: 1, max: 3 }
  - kind: event_sequence_strict
    pattern: [a, b]
  - kind: token_budget
    max_input_tokens: 8000
  - kind: latency
    p95_ms: 5000
`
	a, err := ParseAssertions([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseAssertions: %v", err)
	}
	if len(a.Deterministic) != 7 {
		t.Fatalf("got %d kinds want 7", len(a.Deterministic))
	}
	expectedKinds := []string{
		"event_emitted",
		"event_sequence_distance",
		"tool_invocation_parity",
		"event_count_bounds",
		"event_sequence_strict",
		"token_budget",
		"latency",
	}
	for i, k := range expectedKinds {
		if a.Deterministic[i].Kind != k {
			t.Errorf("kind[%d]=%q want %q", i, a.Deterministic[i].Kind, k)
		}
	}
}
