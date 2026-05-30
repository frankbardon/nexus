package builtins

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/itchyny/gojq"

	"github.com/frankbardon/nexus/plugins/workflows/icm/predicates"
)

// HandlerJSONPathExists is the canonical handler name for JSONPathExists.
const HandlerJSONPathExists = "json_path_exists"

// JSONPathExists parses the artifact as JSON and runs a gojq query
// against it. It passes when the query yields at least one result; when
// must_be_non_empty is true (the default), null/empty-string/empty
// container results do not count.
//
// Args:
//
//	path (string, required): gojq-syntax query (e.g. ".items[0].id").
//	must_be_non_empty (bool, optional, default true): when true, only
//	    non-null, non-empty results count as matches.
//
// Compilation strategy: the gojq query string lives in predicate args,
// not in handler config, so it is only known at Evaluate time. The
// handler compiles per-Evaluate call. A single ICM run typically issues
// each predicate once or twice per stage iteration; per-call compile
// keeps the handler stateless and trivially safe for concurrent use. If
// future profiling shows compile overhead matters we can swap in a
// sync.Map cache keyed by the query string.
type JSONPathExists struct{}

// Evaluate implements predicates.NativeHandler.
func (JSONPathExists) Evaluate(_ context.Context, args map[string]any, artifact []byte) predicates.NativeResult {
	path, ok := args["path"].(string)
	if !ok {
		if _, present := args["path"]; !present {
			return predicates.NativeResult{Verdict: false, Feedback: `missing required arg "path"`}
		}
		return predicates.NativeResult{Verdict: false, Feedback: fmt.Sprintf(`arg "path" must be a string, got %T`, args["path"])}
	}
	if path == "" {
		return predicates.NativeResult{Verdict: false, Feedback: `arg "path" must be non-empty`}
	}

	mustNonEmpty, err := optionalBool(args, "must_be_non_empty", true)
	if err != nil {
		return predicates.NativeResult{Verdict: false, Feedback: err.Error()}
	}

	var doc any
	if err := json.Unmarshal(artifact, &doc); err != nil {
		return predicates.NativeResult{
			Verdict:  false,
			Feedback: fmt.Sprintf("artifact is not valid JSON: %v", err),
		}
	}

	query, err := gojq.Parse(path)
	if err != nil {
		return predicates.NativeResult{
			Verdict:  false,
			Feedback: fmt.Sprintf("invalid gojq path %q: %v", path, err),
		}
	}

	iter := query.Run(doc)
	var (
		anyResult   bool
		anyNonEmpty bool
		lastEvalErr error
	)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if e, isErr := v.(error); isErr {
			lastEvalErr = e
			continue
		}
		anyResult = true
		if !isEmptyValue(v) {
			anyNonEmpty = true
		}
	}

	if !anyResult {
		if lastEvalErr != nil {
			return predicates.NativeResult{
				Verdict:  false,
				Feedback: fmt.Sprintf("gojq path %q errored without yielding results: %v", path, lastEvalErr),
			}
		}
		return predicates.NativeResult{
			Verdict:  false,
			Feedback: fmt.Sprintf("gojq path %q yielded no results", path),
		}
	}
	if mustNonEmpty && !anyNonEmpty {
		return predicates.NativeResult{
			Verdict:  false,
			Feedback: fmt.Sprintf("gojq path %q matched but every result was null or empty", path),
		}
	}
	return predicates.NativeResult{Verdict: true}
}

// isEmptyValue reports whether v should count as "empty" for the
// must_be_non_empty check. Null, empty string, empty slice/map, and
// false-y numerics are NOT all conflated — only nil-ish and empty
// containers/strings are considered empty. Zero numbers and false bools
// are real values and count as non-empty.
func isEmptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}
