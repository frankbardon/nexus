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
		{Type: "tool.invoke", Payload: events.ToolCall{SchemaVersion: events.ToolCallVersion, Name: "shell"}, Timestamp: time.Now()},
		{Type: "tool.invoke", Payload: events.ToolCall{SchemaVersion: events.ToolCallVersion, Name: "read_file"}, Timestamp: time.Now()},
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
		{Type: "tool.invoke", Payload: events.ToolCall{SchemaVersion: events.ToolCallVersion, Name: "shell", Arguments: map[string]any{"cmd": "ls"}}},
		{Type: "tool.invoke", Payload: events.ToolCall{SchemaVersion: events.ToolCallVersion, Name: "read_file", Arguments: map[string]any{"path": "x"}}},
	}
	o := []ObservedEvent{
		{Type: "tool.invoke", Payload: events.ToolCall{SchemaVersion: events.ToolCallVersion, Name: "shell", Arguments: map[string]any{"cmd": "different"}}},
		{Type: "tool.invoke", Payload: events.ToolCall{SchemaVersion: events.ToolCallVersion, Name: "read_file", Arguments: map[string]any{"path": "y"}}},
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
		{Type: "tool.invoke", Payload: events.ToolCall{SchemaVersion: events.ToolCallVersion, Name: "shell"}},
	}
	o := []ObservedEvent{
		{Type: "tool.invoke", Payload: events.ToolCall{SchemaVersion: events.ToolCallVersion, Name: "shell"}},
		{Type: "tool.invoke", Payload: events.ToolCall{SchemaVersion: events.ToolCallVersion, Name: "shell"}},
		{Type: "tool.invoke", Payload: events.ToolCall{SchemaVersion: events.ToolCallVersion, Name: "shell"}},
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
		{Type: "tool.invoke", Payload: events.ToolCall{SchemaVersion: events.ToolCallVersion, Name: "shell", Arguments: map[string]any{"cmd": "x"}}},
	}
	o := []ObservedEvent{
		{Type: "tool.invoke", Payload: events.ToolCall{SchemaVersion: events.ToolCallVersion, Name: "shell", Arguments: map[string]any{"cmd": "x", "extra": 1}}},
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
		{Type: "llm.response", Timestamp: t0.Add(time.Millisecond), Payload: events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Usage: events.Usage{PromptTokens: 100, CompletionTokens: 50}}},
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
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Usage: events.Usage{PromptTokens: 1500}}},
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
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Usage: events.Usage{PromptTokens: 200}}},
		{Type: "agent.turn.end", Timestamp: t0},
		{Type: "agent.turn.start", Timestamp: t0},
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Usage: events.Usage{PromptTokens: 800}}},
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

// -- response_contains --------------------------------------------------------

func TestResponseContains_AllPresent(t *testing.T) {
	t0 := time.Now()
	stream := []ObservedEvent{
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       "The total revenue was $4.2M, up 12% year over year.",
			FinishReason:  "stop",
		}},
	}
	a := Assertion{
		Kind:             "response_contains",
		ResponseContains: &ResponseContainsSpec{Contains: []string{"REVENUE", "$4.2M"}},
	}
	r := a.Evaluate(stream, nil)
	if !r.Pass {
		t.Fatalf("expected pass: %s", r.Message)
	}
}

func TestResponseContains_OneMissing(t *testing.T) {
	t0 := time.Now()
	stream := []ObservedEvent{
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       "The total revenue was $4.2M.",
			FinishReason:  "stop",
		}},
	}
	a := Assertion{
		Kind:             "response_contains",
		ResponseContains: &ResponseContainsSpec{Contains: []string{"revenue", "profit"}},
	}
	r := a.Evaluate(stream, nil)
	if r.Pass {
		t.Fatal("expected fail: 'profit' is absent")
	}
}

func TestResponseContains_ContainsAny_OnePresent(t *testing.T) {
	t0 := time.Now()
	stream := []ObservedEvent{
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       "Here is the chart you asked for.",
			FinishReason:  "stop",
		}},
	}
	a := Assertion{
		Kind:             "response_contains",
		ResponseContains: &ResponseContainsSpec{ContainsAny: []string{"CHART", "graph", "table"}},
	}
	r := a.Evaluate(stream, nil)
	if !r.Pass {
		t.Fatalf("expected pass: %s", r.Message)
	}
}

func TestResponseContains_ContainsAny_NonePresent(t *testing.T) {
	t0 := time.Now()
	stream := []ObservedEvent{
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       "Here is a summary.",
			FinishReason:  "stop",
		}},
	}
	a := Assertion{
		Kind:             "response_contains",
		ResponseContains: &ResponseContainsSpec{ContainsAny: []string{"chart", "graph", "table"}},
	}
	r := a.Evaluate(stream, nil)
	if r.Pass {
		t.Fatal("expected fail: none of contains_any present")
	}
}

func TestResponseContains_DefaultsToFinalAssistantResponse(t *testing.T) {
	t0 := time.Now()
	stream := []ObservedEvent{
		// A tool-call-only turn must be skipped by the default selection.
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			ToolCalls:     []events.ToolCallRequest{{ID: "1", Name: "search", Arguments: "{}"}},
		}},
		{Type: "tool.invoke", Timestamp: t0.Add(time.Millisecond)},
		{Type: "tool.result", Timestamp: t0.Add(2 * time.Millisecond)},
		{Type: "llm.response", Timestamp: t0.Add(3 * time.Millisecond), Payload: events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       "The answer is 42.",
			FinishReason:  "stop",
		}},
	}
	a := Assertion{
		Kind:             "response_contains",
		ResponseContains: &ResponseContainsSpec{Contains: []string{"42"}},
	}
	r := a.Evaluate(stream, nil)
	if !r.Pass {
		t.Fatalf("expected pass (final assistant response should be the last non-tool-call turn): %s", r.Message)
	}
}

func TestResponseContains_NonDefaultEventType(t *testing.T) {
	t0 := time.Now()
	stream := []ObservedEvent{
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       "The final answer is 42.",
			FinishReason:  "stop",
		}},
		{Type: "io.output", Timestamp: t0.Add(time.Millisecond), Payload: map[string]any{
			"Content": "Rendered card: Revenue Summary",
		}},
	}
	a := Assertion{
		Kind: "response_contains",
		ResponseContains: &ResponseContainsSpec{
			EventType: "io.output",
			Contains:  []string{"Revenue Summary"},
		},
	}
	r := a.Evaluate(stream, nil)
	if !r.Pass {
		t.Fatalf("expected pass selecting io.output content: %s", r.Message)
	}

	// The same substring is absent from the default (final assistant
	// response) selection, proving EventType actually redirected the search.
	a.ResponseContains = &ResponseContainsSpec{Contains: []string{"Revenue Summary"}}
	r = a.Evaluate(stream, nil)
	if r.Pass {
		t.Fatal("expected fail: 'Revenue Summary' is not in the final assistant response")
	}
}

// -- contains_value -----------------------------------------------------------

func TestContainsValue_DefaultTolerance_OneDecimalPlace(t *testing.T) {
	if got, want := defaultValueTolerance(42.3), 0.05; got != want {
		t.Fatalf("defaultValueTolerance(42.3) = %v, want %v", got, want)
	}
}

func TestContainsValue_DefaultTolerance_NoDecimalPlace(t *testing.T) {
	if got, want := defaultValueTolerance(100), 0.5; got != want {
		t.Fatalf("defaultValueTolerance(100) = %v, want %v", got, want)
	}
}

func TestContainsValue_DefaultTolerance_TwoDecimalPlaces(t *testing.T) {
	if got, want := defaultValueTolerance(3.14), 0.005; got != want {
		t.Fatalf("defaultValueTolerance(3.14) = %v, want %v", got, want)
	}
}

func TestContainsValue_FormattingVariance_Passes(t *testing.T) {
	t0 := time.Now()
	stream := []ObservedEvent{
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       "Conversion rate held steady at 42.30%.",
			FinishReason:  "stop",
		}},
	}
	a := Assertion{
		Kind:          "contains_value",
		ContainsValue: &ContainsValueSpec{Value: 42.3, Unit: "%"},
	}
	r := a.Evaluate(stream, nil)
	if !r.Pass {
		t.Fatalf("expected pass (42.30%% formats the same value as 42.3): %s", r.Message)
	}
}

func TestContainsValue_MaterialDifference_Fails(t *testing.T) {
	t0 := time.Now()
	stream := []ObservedEvent{
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       "Conversion rate held steady at 43.30%.",
			FinishReason:  "stop",
		}},
	}
	a := Assertion{
		Kind:          "contains_value",
		ContainsValue: &ContainsValueSpec{Value: 42.3, Unit: "%"},
	}
	r := a.Evaluate(stream, nil)
	if r.Pass {
		t.Fatal("expected fail: 43.3 is materially different from 42.3, despite matching formatting")
	}
}

func TestContainsValue_UnitStripped_Dollar(t *testing.T) {
	t0 := time.Now()
	stream := []ObservedEvent{
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       "Total spend came to $100 this month.",
			FinishReason:  "stop",
		}},
	}
	a := Assertion{
		Kind:          "contains_value",
		ContainsValue: &ContainsValueSpec{Value: 100, Unit: "$"},
	}
	r := a.Evaluate(stream, nil)
	if !r.Pass {
		t.Fatalf("expected pass: %s", r.Message)
	}
}

func TestContainsValue_ThousandsSeparatorStripped(t *testing.T) {
	t0 := time.Now()
	stream := []ObservedEvent{
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       "Revenue was $1,234.50 for the quarter.",
			FinishReason:  "stop",
		}},
	}
	a := Assertion{
		Kind:          "contains_value",
		ContainsValue: &ContainsValueSpec{Value: 1234.5, Unit: "$"},
	}
	r := a.Evaluate(stream, nil)
	if !r.Pass {
		t.Fatalf("expected pass (comma thousands separator stripped): %s", r.Message)
	}
}

// TestContainsValue_WordBoundary_DoesNotMatchEmbeddedNumber is the explicit
// regression test the story calls for: a search for 4.2 must not match text
// that only contains 24.2 as a substring.
func TestContainsValue_WordBoundary_DoesNotMatchEmbeddedNumber(t *testing.T) {
	t0 := time.Now()
	stream := []ObservedEvent{
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       "The measurement was 24.2 units.",
			FinishReason:  "stop",
		}},
	}
	a := Assertion{
		Kind:          "contains_value",
		ContainsValue: &ContainsValueSpec{Value: 4.2},
	}
	r := a.Evaluate(stream, nil)
	if r.Pass {
		t.Fatal("expected fail: 4.2 must not match inside 24.2")
	}
}

func TestContainsValue_WordBoundary_StandaloneNumberStillMatches(t *testing.T) {
	t0 := time.Now()
	stream := []ObservedEvent{
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       "The measurement was 4.2 units, not 24.2.",
			FinishReason:  "stop",
		}},
	}
	a := Assertion{
		Kind:          "contains_value",
		ContainsValue: &ContainsValueSpec{Value: 4.2},
	}
	r := a.Evaluate(stream, nil)
	if !r.Pass {
		t.Fatalf("expected pass: a standalone 4.2 is present alongside 24.2: %s", r.Message)
	}
}

// TestContainsValue_ToleranceBoundary covers a value exactly at the
// tolerance edge, just inside it, and just outside it. The boundary is
// documented (ContainsValueSpec's doc comment, evalContainsValue) as
// INCLUSIVE: a candidate exactly Tolerance away from Value passes.
func TestContainsValue_ToleranceBoundary(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"exactly at edge (inclusive)", "reading: 105", true},
		{"just inside", "reading: 104", true},
		{"just outside", "reading: 106", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t0 := time.Now()
			stream := []ObservedEvent{
				{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{
					SchemaVersion: events.LLMResponseVersion,
					Content:       tt.text,
					FinishReason:  "stop",
				}},
			}
			a := Assertion{
				Kind:          "contains_value",
				ContainsValue: &ContainsValueSpec{Value: 100, Tolerance: 5},
			}
			r := a.Evaluate(stream, nil)
			if r.Pass != tt.want {
				t.Fatalf("text %q: Pass = %v, want %v (%s)", tt.text, r.Pass, tt.want, r.Message)
			}
		})
	}
}

func TestContainsValue_ExplicitToleranceOverridesDefault(t *testing.T) {
	t0 := time.Now()
	// Default tolerance for Value 42.3 would be ±0.05 — 42.5 would fail
	// against the default. An explicit wide tolerance should let it pass.
	stream := []ObservedEvent{
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       "Value came in at 42.5.",
			FinishReason:  "stop",
		}},
	}
	withDefault := Assertion{
		Kind:          "contains_value",
		ContainsValue: &ContainsValueSpec{Value: 42.3},
	}
	if r := withDefault.Evaluate(stream, nil); r.Pass {
		t.Fatal("expected fail under the default (narrow) inferred tolerance")
	}

	withExplicit := Assertion{
		Kind:          "contains_value",
		ContainsValue: &ContainsValueSpec{Value: 42.3, Tolerance: 1},
	}
	if r := withExplicit.Evaluate(stream, nil); !r.Pass {
		t.Fatalf("expected pass under an explicit, wider tolerance: %s", r.Message)
	}
}

func TestContainsValue_NoNumericTokenFound(t *testing.T) {
	t0 := time.Now()
	stream := []ObservedEvent{
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       "No numbers here at all.",
			FinishReason:  "stop",
		}},
	}
	a := Assertion{
		Kind:          "contains_value",
		ContainsValue: &ContainsValueSpec{Value: 42.3},
	}
	r := a.Evaluate(stream, nil)
	if r.Pass {
		t.Fatal("expected fail: no numeric tokens in text")
	}
}

func TestContainsValue_NonDefaultEventType(t *testing.T) {
	t0 := time.Now()
	stream := []ObservedEvent{
		{Type: "llm.response", Timestamp: t0, Payload: events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			Content:       "The final answer is 7.",
			FinishReason:  "stop",
		}},
		{Type: "io.output", Timestamp: t0.Add(time.Millisecond), Payload: map[string]any{
			"Content": "Rendered card total: 42.3%",
		}},
	}
	a := Assertion{
		Kind: "contains_value",
		ContainsValue: &ContainsValueSpec{
			EventType: "io.output",
			Value:     42.3,
			Unit:      "%",
		},
	}
	r := a.Evaluate(stream, nil)
	if !r.Pass {
		t.Fatalf("expected pass selecting io.output content: %s", r.Message)
	}
}

func TestParseAssertions_ContainsValue(t *testing.T) {
	yamlSrc := `deterministic:
  - kind: contains_value
    value: 42.3
    unit: "%"
`
	a, err := ParseAssertions([]byte(yamlSrc))
	if err != nil {
		t.Fatalf("ParseAssertions: %v", err)
	}
	if len(a.Deterministic) != 1 {
		t.Fatalf("got %d entries want 1", len(a.Deterministic))
	}
	cv := a.Deterministic[0].ContainsValue
	if cv == nil {
		t.Fatal("expected ContainsValue spec to be populated")
	}
	if cv.Value != 42.3 {
		t.Errorf("Value = %v, want 42.3", cv.Value)
	}
	if cv.Unit != "%" {
		t.Errorf("Unit = %q, want \"%%\"", cv.Unit)
	}
	if cv.Tolerance != 0 {
		t.Errorf("Tolerance = %v, want 0 (unset, defaulted at eval time)", cv.Tolerance)
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
  - kind: response_contains
    contains: [hello]
    contains_any: [world, there]
  - kind: contains_value
    value: 42.3
    tolerance: 0.1
    unit: "%"
`
	a, err := ParseAssertions([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseAssertions: %v", err)
	}
	if len(a.Deterministic) != 9 {
		t.Fatalf("got %d kinds want 9", len(a.Deterministic))
	}
	expectedKinds := []string{
		"event_emitted",
		"event_sequence_distance",
		"tool_invocation_parity",
		"event_count_bounds",
		"event_sequence_strict",
		"token_budget",
		"latency",
		"response_contains",
		"contains_value",
	}
	for i, k := range expectedKinds {
		if a.Deterministic[i].Kind != k {
			t.Errorf("kind[%d]=%q want %q", i, a.Deterministic[i].Kind, k)
		}
	}
	rc := a.Deterministic[7].ResponseContains
	if rc == nil {
		t.Fatal("expected ResponseContains spec to be populated")
	}
	if len(rc.Contains) != 1 || rc.Contains[0] != "hello" {
		t.Errorf("Contains = %v, want [hello]", rc.Contains)
	}
	if len(rc.ContainsAny) != 2 {
		t.Errorf("ContainsAny = %v, want 2 entries", rc.ContainsAny)
	}
	cv := a.Deterministic[8].ContainsValue
	if cv == nil {
		t.Fatal("expected ContainsValue spec to be populated")
	}
	if cv.Value != 42.3 {
		t.Errorf("Value = %v, want 42.3", cv.Value)
	}
	if cv.Tolerance != 0.1 {
		t.Errorf("Tolerance = %v, want 0.1", cv.Tolerance)
	}
	if cv.Unit != "%" {
		t.Errorf("Unit = %q, want \"%%\"", cv.Unit)
	}
}

func TestParseAssertions_ResponseContains_RequiresContainsOrContainsAny(t *testing.T) {
	yaml := `deterministic:
  - kind: response_contains
`
	_, err := ParseAssertions([]byte(yaml))
	if err == nil {
		t.Fatal("expected error when neither 'contains' nor 'contains_any' is set")
	}
}
