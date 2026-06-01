// Package builtins ships the four baked-in native predicate handlers
// for the ICM workflow plugin: word_count_under, word_count_over,
// contains_required_ids, and json_path_exists.
//
// Each handler implements predicates.NativeHandler. RegisterAll wires
// the lot into any predicates.Evaluator (or any other registrar
// implementing NativeRegistrar).
package builtins

import (
	"context"
	"fmt"
	"strings"

	"github.com/frankbardon/nexus/plugins/workflows/icm/predicates"
)

// Canonical handler names. Workspace authors reference these strings in
// predicate `handler` fields.
const (
	HandlerWordCountUnder = "word_count_under"
	HandlerWordCountOver  = "word_count_over"
)

// WordCountUnder passes when the artifact's word count is strictly less
// than the configured max_words.
//
// Args:
//
//	max_words (int, required, > 0): exclusive upper bound on word count.
//
// Word splitting follows strings.Fields semantics: any Unicode
// whitespace run separates words.
type WordCountUnder struct{}

// Evaluate implements predicates.NativeHandler.
func (WordCountUnder) Evaluate(_ context.Context, args map[string]any, artifact []byte) predicates.NativeResult {
	max, err := requirePositiveInt(args, "max_words")
	if err != nil {
		return predicates.NativeResult{Verdict: false, Feedback: err.Error()}
	}
	count := len(strings.Fields(string(artifact)))
	if count < max {
		return predicates.NativeResult{Verdict: true}
	}
	return predicates.NativeResult{
		Verdict:  false,
		Feedback: fmt.Sprintf("word count %d is not strictly less than max_words=%d", count, max),
	}
}

// WordCountOver passes when the artifact's word count is strictly
// greater than the configured min_words.
//
// Args:
//
//	min_words (int, required, >= 0): exclusive lower bound on word count.
//
// Word splitting follows strings.Fields semantics.
type WordCountOver struct{}

// Evaluate implements predicates.NativeHandler.
func (WordCountOver) Evaluate(_ context.Context, args map[string]any, artifact []byte) predicates.NativeResult {
	min, err := requireNonNegativeInt(args, "min_words")
	if err != nil {
		return predicates.NativeResult{Verdict: false, Feedback: err.Error()}
	}
	count := len(strings.Fields(string(artifact)))
	if count > min {
		return predicates.NativeResult{Verdict: true}
	}
	return predicates.NativeResult{
		Verdict:  false,
		Feedback: fmt.Sprintf("word count %d is not strictly greater than min_words=%d", count, min),
	}
}

// coerceInt accepts YAML's possible numeric landings (int, int64,
// float64) plus a few unsigned variants, returning a normalized int and
// true on success.
func coerceInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int8:
		return int(t), true
	case int16:
		return int(t), true
	case int32:
		return int(t), true
	case int64:
		return int(t), true
	case uint:
		return int(t), true
	case uint8:
		return int(t), true
	case uint16:
		return int(t), true
	case uint32:
		return int(t), true
	case uint64:
		return int(t), true
	case float32:
		if t != float32(int(t)) {
			return 0, false
		}
		return int(t), true
	case float64:
		if t != float64(int(t)) {
			return 0, false
		}
		return int(t), true
	}
	return 0, false
}

// requirePositiveInt returns args[key] as a strictly positive int, or a
// descriptive error.
func requirePositiveInt(args map[string]any, key string) (int, error) {
	raw, ok := args[key]
	if !ok {
		return 0, fmt.Errorf("missing required arg %q", key)
	}
	n, ok := coerceInt(raw)
	if !ok {
		return 0, fmt.Errorf("arg %q must be an integer, got %T", key, raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("arg %q must be > 0, got %d", key, n)
	}
	return n, nil
}

// requireNonNegativeInt returns args[key] as a non-negative int, or a
// descriptive error.
func requireNonNegativeInt(args map[string]any, key string) (int, error) {
	raw, ok := args[key]
	if !ok {
		return 0, fmt.Errorf("missing required arg %q", key)
	}
	n, ok := coerceInt(raw)
	if !ok {
		return 0, fmt.Errorf("arg %q must be an integer, got %T", key, raw)
	}
	if n < 0 {
		return 0, fmt.Errorf("arg %q must be >= 0, got %d", key, n)
	}
	return n, nil
}
