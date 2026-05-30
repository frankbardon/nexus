package builtins

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/plugins/workflows/icm/predicates"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newEvaluator() *predicates.Evaluator {
	l := discardLogger()
	return predicates.NewEvaluator(engine.NewSchemaRegistry(l), nil, nil, l)
}

// ---------------------------------------------------------------------
// WordCountUnder
// ---------------------------------------------------------------------

func TestWordCountUnder_PassUnderThreshold(t *testing.T) {
	h := WordCountUnder{}
	r := h.Evaluate(context.Background(), map[string]any{"max_words": 5}, []byte("one two three"))
	if !r.Verdict {
		t.Fatalf("expected verdict=true, got false (feedback=%q)", r.Feedback)
	}
}

func TestWordCountUnder_FailAtThreshold(t *testing.T) {
	h := WordCountUnder{}
	r := h.Evaluate(context.Background(), map[string]any{"max_words": 3}, []byte("one two three"))
	if r.Verdict {
		t.Fatalf("expected verdict=false at threshold (strict <)")
	}
	if !strings.Contains(r.Feedback, "3") {
		t.Fatalf("feedback should include actual count, got %q", r.Feedback)
	}
}

func TestWordCountUnder_FailOverThreshold(t *testing.T) {
	h := WordCountUnder{}
	r := h.Evaluate(context.Background(), map[string]any{"max_words": 2}, []byte("one two three"))
	if r.Verdict {
		t.Fatalf("expected verdict=false")
	}
}

func TestWordCountUnder_EmptyArtifactPasses(t *testing.T) {
	h := WordCountUnder{}
	r := h.Evaluate(context.Background(), map[string]any{"max_words": 1}, []byte(""))
	if !r.Verdict {
		t.Fatalf("expected verdict=true for empty artifact, got %q", r.Feedback)
	}
}

func TestWordCountUnder_MissingMaxWords(t *testing.T) {
	h := WordCountUnder{}
	r := h.Evaluate(context.Background(), map[string]any{}, []byte("a b"))
	if r.Verdict {
		t.Fatalf("expected verdict=false on missing arg")
	}
	if !strings.Contains(r.Feedback, "max_words") {
		t.Fatalf("feedback should mention missing arg, got %q", r.Feedback)
	}
}

func TestWordCountUnder_ZeroAndNegative(t *testing.T) {
	h := WordCountUnder{}
	for _, n := range []int{0, -1, -100} {
		r := h.Evaluate(context.Background(), map[string]any{"max_words": n}, []byte("a"))
		if r.Verdict {
			t.Fatalf("expected verdict=false for max_words=%d", n)
		}
		if !strings.Contains(r.Feedback, "max_words") {
			t.Fatalf("feedback for max_words=%d should mention arg name, got %q", n, r.Feedback)
		}
	}
}

func TestWordCountUnder_FloatCoerce(t *testing.T) {
	h := WordCountUnder{}
	// YAML may land integers as float64.
	r := h.Evaluate(context.Background(), map[string]any{"max_words": float64(5)}, []byte("one two"))
	if !r.Verdict {
		t.Fatalf("expected float64 coerce to succeed, got %q", r.Feedback)
	}
}

func TestWordCountUnder_NonNumeric(t *testing.T) {
	h := WordCountUnder{}
	r := h.Evaluate(context.Background(), map[string]any{"max_words": "five"}, []byte("a"))
	if r.Verdict {
		t.Fatalf("expected verdict=false for string max_words")
	}
}

// ---------------------------------------------------------------------
// WordCountOver
// ---------------------------------------------------------------------

func TestWordCountOver_PassOverThreshold(t *testing.T) {
	h := WordCountOver{}
	r := h.Evaluate(context.Background(), map[string]any{"min_words": 2}, []byte("one two three"))
	if !r.Verdict {
		t.Fatalf("expected verdict=true (3 > 2), got %q", r.Feedback)
	}
}

func TestWordCountOver_FailAtThreshold(t *testing.T) {
	h := WordCountOver{}
	r := h.Evaluate(context.Background(), map[string]any{"min_words": 3}, []byte("one two three"))
	if r.Verdict {
		t.Fatalf("expected verdict=false at threshold (strict >)")
	}
}

func TestWordCountOver_FailUnderThreshold(t *testing.T) {
	h := WordCountOver{}
	r := h.Evaluate(context.Background(), map[string]any{"min_words": 10}, []byte("one two"))
	if r.Verdict {
		t.Fatalf("expected verdict=false")
	}
}

func TestWordCountOver_EmptyArtifactFailsByDefault(t *testing.T) {
	h := WordCountOver{}
	// 0 > 0 is false; min_words=0 with empty artifact still fails (strict).
	r := h.Evaluate(context.Background(), map[string]any{"min_words": 0}, []byte(""))
	if r.Verdict {
		t.Fatalf("expected verdict=false for empty artifact (0 not > 0)")
	}
}

func TestWordCountOver_MissingMinWords(t *testing.T) {
	h := WordCountOver{}
	r := h.Evaluate(context.Background(), map[string]any{}, []byte("a b c"))
	if r.Verdict {
		t.Fatalf("expected verdict=false on missing arg")
	}
	if !strings.Contains(r.Feedback, "min_words") {
		t.Fatalf("feedback should mention min_words, got %q", r.Feedback)
	}
}

func TestWordCountOver_NegativeRejected(t *testing.T) {
	h := WordCountOver{}
	r := h.Evaluate(context.Background(), map[string]any{"min_words": -1}, []byte("a"))
	if r.Verdict {
		t.Fatalf("expected verdict=false for negative min_words")
	}
}

func TestWordCountOver_ZeroAllowed(t *testing.T) {
	h := WordCountOver{}
	// min_words=0 with at least one word should pass (1 > 0).
	r := h.Evaluate(context.Background(), map[string]any{"min_words": 0}, []byte("hello"))
	if !r.Verdict {
		t.Fatalf("expected verdict=true (1 > 0), got %q", r.Feedback)
	}
}

// ---------------------------------------------------------------------
// ContainsRequiredIDs
// ---------------------------------------------------------------------

func TestContainsRequiredIDs_AllPresent(t *testing.T) {
	h := ContainsRequiredIDs{}
	r := h.Evaluate(context.Background(),
		map[string]any{"ids": []any{"REQ-1", "REQ-2"}},
		[]byte("see REQ-1 and also REQ-2 for context"))
	if !r.Verdict {
		t.Fatalf("expected pass, got %q", r.Feedback)
	}
}

func TestContainsRequiredIDs_SomeMissing(t *testing.T) {
	h := ContainsRequiredIDs{}
	r := h.Evaluate(context.Background(),
		map[string]any{"ids": []any{"REQ-1", "REQ-2", "REQ-3"}},
		[]byte("only REQ-2 is here"))
	if r.Verdict {
		t.Fatalf("expected fail")
	}
	if !strings.Contains(r.Feedback, "REQ-1") || !strings.Contains(r.Feedback, "REQ-3") {
		t.Fatalf("feedback should list missing ids REQ-1 and REQ-3, got %q", r.Feedback)
	}
	if strings.Contains(r.Feedback, "REQ-2") {
		t.Fatalf("feedback should NOT list present id REQ-2, got %q", r.Feedback)
	}
}

func TestContainsRequiredIDs_EmptyIDsRejected(t *testing.T) {
	// Decision: empty ids array is rejected as malformed args.
	h := ContainsRequiredIDs{}
	r := h.Evaluate(context.Background(),
		map[string]any{"ids": []any{}},
		[]byte("anything"))
	if r.Verdict {
		t.Fatalf("expected fail for empty ids array")
	}
	if !strings.Contains(r.Feedback, "ids") {
		t.Fatalf("feedback should mention ids arg, got %q", r.Feedback)
	}
}

func TestContainsRequiredIDs_MissingIDsArg(t *testing.T) {
	h := ContainsRequiredIDs{}
	r := h.Evaluate(context.Background(), map[string]any{}, []byte("hello"))
	if r.Verdict {
		t.Fatalf("expected fail on missing ids arg")
	}
	if !strings.Contains(r.Feedback, "ids") {
		t.Fatalf("feedback should mention ids arg, got %q", r.Feedback)
	}
}

func TestContainsRequiredIDs_CaseInsensitive(t *testing.T) {
	h := ContainsRequiredIDs{}
	r := h.Evaluate(context.Background(),
		map[string]any{
			"ids":              []any{"req-1", "REQ-2"},
			"case_insensitive": true,
		},
		[]byte("REQ-1 and req-2 appear"))
	if !r.Verdict {
		t.Fatalf("expected pass with case_insensitive=true, got %q", r.Feedback)
	}
}

func TestContainsRequiredIDs_CaseSensitiveByDefault(t *testing.T) {
	h := ContainsRequiredIDs{}
	r := h.Evaluate(context.Background(),
		map[string]any{"ids": []any{"REQ-1"}},
		[]byte("only req-1 in lowercase"))
	if r.Verdict {
		t.Fatalf("expected fail with default case sensitivity")
	}
}

func TestContainsRequiredIDs_StringSliceNative(t *testing.T) {
	h := ContainsRequiredIDs{}
	r := h.Evaluate(context.Background(),
		map[string]any{"ids": []string{"A", "B"}},
		[]byte("A and B"))
	if !r.Verdict {
		t.Fatalf("expected pass with []string ids, got %q", r.Feedback)
	}
}

func TestContainsRequiredIDs_NonStringEntry(t *testing.T) {
	h := ContainsRequiredIDs{}
	r := h.Evaluate(context.Background(),
		map[string]any{"ids": []any{"A", 42}},
		[]byte("A"))
	if r.Verdict {
		t.Fatalf("expected fail when ids contains a non-string entry")
	}
}

func TestContainsRequiredIDs_BadCaseInsensitiveType(t *testing.T) {
	h := ContainsRequiredIDs{}
	r := h.Evaluate(context.Background(),
		map[string]any{
			"ids":              []any{"A"},
			"case_insensitive": "yes",
		},
		[]byte("A"))
	if r.Verdict {
		t.Fatalf("expected fail for non-bool case_insensitive")
	}
}

// ---------------------------------------------------------------------
// JSONPathExists
// ---------------------------------------------------------------------

func TestJSONPathExists_SimpleMatch(t *testing.T) {
	h := JSONPathExists{}
	r := h.Evaluate(context.Background(),
		map[string]any{"path": ".name"},
		[]byte(`{"name":"alice"}`))
	if !r.Verdict {
		t.Fatalf("expected pass, got %q", r.Feedback)
	}
}

func TestJSONPathExists_NoMatch(t *testing.T) {
	h := JSONPathExists{}
	r := h.Evaluate(context.Background(),
		map[string]any{"path": ".missing"},
		[]byte(`{"name":"alice"}`))
	if r.Verdict {
		// gojq returns null for missing keys; with default must_be_non_empty=true this fails.
		t.Fatalf("expected fail (null result with default must_be_non_empty), got pass")
	}
}

func TestJSONPathExists_NullWithMustBeNonEmptyFalse(t *testing.T) {
	h := JSONPathExists{}
	r := h.Evaluate(context.Background(),
		map[string]any{"path": ".missing", "must_be_non_empty": false},
		[]byte(`{"name":"alice"}`))
	if !r.Verdict {
		t.Fatalf("expected pass with must_be_non_empty=false (null counts), got %q", r.Feedback)
	}
}

func TestJSONPathExists_NullWithMustBeNonEmptyTrue(t *testing.T) {
	h := JSONPathExists{}
	r := h.Evaluate(context.Background(),
		map[string]any{"path": ".n", "must_be_non_empty": true},
		[]byte(`{"n":null}`))
	if r.Verdict {
		t.Fatalf("expected fail for null with must_be_non_empty=true")
	}
}

func TestJSONPathExists_EmptyArrayFails(t *testing.T) {
	h := JSONPathExists{}
	r := h.Evaluate(context.Background(),
		map[string]any{"path": ".items"},
		[]byte(`{"items":[]}`))
	if r.Verdict {
		t.Fatalf("expected fail for empty array under must_be_non_empty=true")
	}
}

func TestJSONPathExists_EmptyStringFails(t *testing.T) {
	h := JSONPathExists{}
	r := h.Evaluate(context.Background(),
		map[string]any{"path": ".s"},
		[]byte(`{"s":""}`))
	if r.Verdict {
		t.Fatalf("expected fail for empty string under must_be_non_empty=true")
	}
}

func TestJSONPathExists_ZeroNumberPasses(t *testing.T) {
	h := JSONPathExists{}
	r := h.Evaluate(context.Background(),
		map[string]any{"path": ".n"},
		[]byte(`{"n":0}`))
	if !r.Verdict {
		t.Fatalf("expected pass for 0 (real value, not empty), got %q", r.Feedback)
	}
}

func TestJSONPathExists_FalseBoolPasses(t *testing.T) {
	h := JSONPathExists{}
	r := h.Evaluate(context.Background(),
		map[string]any{"path": ".b"},
		[]byte(`{"b":false}`))
	if !r.Verdict {
		t.Fatalf("expected pass for false (real value, not empty), got %q", r.Feedback)
	}
}

func TestJSONPathExists_ArrayIndex(t *testing.T) {
	h := JSONPathExists{}
	r := h.Evaluate(context.Background(),
		map[string]any{"path": ".items[0].id"},
		[]byte(`{"items":[{"id":"x"}]}`))
	if !r.Verdict {
		t.Fatalf("expected pass, got %q", r.Feedback)
	}
}

func TestJSONPathExists_BadJSON(t *testing.T) {
	h := JSONPathExists{}
	r := h.Evaluate(context.Background(),
		map[string]any{"path": ".name"},
		[]byte(`not json {`))
	if r.Verdict {
		t.Fatalf("expected fail")
	}
	if !strings.Contains(r.Feedback, "not valid JSON") {
		t.Fatalf("feedback should mention parse error, got %q", r.Feedback)
	}
}

func TestJSONPathExists_BadQuerySyntax(t *testing.T) {
	h := JSONPathExists{}
	r := h.Evaluate(context.Background(),
		map[string]any{"path": ".[bad syntax"},
		[]byte(`{"name":"alice"}`))
	if r.Verdict {
		t.Fatalf("expected fail")
	}
	if !strings.Contains(r.Feedback, "gojq") && !strings.Contains(r.Feedback, "invalid") {
		t.Fatalf("feedback should describe the syntax error, got %q", r.Feedback)
	}
}

func TestJSONPathExists_MissingPathArg(t *testing.T) {
	h := JSONPathExists{}
	r := h.Evaluate(context.Background(), map[string]any{}, []byte(`{}`))
	if r.Verdict {
		t.Fatalf("expected fail on missing path")
	}
	if !strings.Contains(r.Feedback, "path") {
		t.Fatalf("feedback should mention path arg, got %q", r.Feedback)
	}
}

func TestJSONPathExists_EmptyPathArg(t *testing.T) {
	h := JSONPathExists{}
	r := h.Evaluate(context.Background(), map[string]any{"path": ""}, []byte(`{}`))
	if r.Verdict {
		t.Fatalf("expected fail on empty path")
	}
}

func TestJSONPathExists_PathWrongType(t *testing.T) {
	h := JSONPathExists{}
	r := h.Evaluate(context.Background(), map[string]any{"path": 42}, []byte(`{}`))
	if r.Verdict {
		t.Fatalf("expected fail on non-string path")
	}
}

// ---------------------------------------------------------------------
// RegisterAll
// ---------------------------------------------------------------------

func TestRegisterAll_RegistersAllHandlers(t *testing.T) {
	e := newEvaluator()
	if err := RegisterAll(e); err != nil {
		t.Fatalf("RegisterAll returned error: %v", err)
	}
	for _, name := range []string{
		HandlerWordCountUnder,
		HandlerWordCountOver,
		HandlerContainsRequiredIDs,
		HandlerJSONPathExists,
	} {
		if _, ok := e.LookupNative(name); !ok {
			t.Errorf("handler %q was not registered", name)
		}
	}
}

// captureRegistrar records registrations for assertion without a full
// Evaluator. Confirms RegisterAll uses only the interface surface.
type captureRegistrar struct {
	got map[string]predicates.NativeHandler
}

func (c *captureRegistrar) RegisterNative(name string, h predicates.NativeHandler) {
	if c.got == nil {
		c.got = make(map[string]predicates.NativeHandler)
	}
	c.got[name] = h
}

func TestRegisterAll_UsesInterfaceOnly(t *testing.T) {
	c := &captureRegistrar{}
	if err := RegisterAll(c); err != nil {
		t.Fatalf("RegisterAll returned error: %v", err)
	}
	want := []string{
		HandlerWordCountUnder,
		HandlerWordCountOver,
		HandlerContainsRequiredIDs,
		HandlerJSONPathExists,
	}
	if len(c.got) != len(want) {
		t.Fatalf("expected %d registrations, got %d", len(want), len(c.got))
	}
	for _, name := range want {
		if _, ok := c.got[name]; !ok {
			t.Errorf("missing registration for %q", name)
		}
	}
}
