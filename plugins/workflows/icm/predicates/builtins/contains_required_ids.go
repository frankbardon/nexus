package builtins

import (
	"context"
	"fmt"
	"strings"

	"github.com/frankbardon/nexus/plugins/workflows/icm/predicates"
)

// HandlerContainsRequiredIDs is the canonical handler name for
// ContainsRequiredIDs.
const HandlerContainsRequiredIDs = "contains_required_ids"

// ContainsRequiredIDs passes when every string in args["ids"] appears at
// least once in the artifact text.
//
// Args:
//
//	ids ([]string, required): IDs that must all appear in the artifact.
//	case_insensitive (bool, optional, default false): fold case when matching.
//
// Decision on empty ids: an empty ids array is treated as a MALFORMED
// args error rather than a vacuous-truth pass. Rationale: a predicate
// asserting "contains required IDs" with zero IDs is almost certainly an
// authoring mistake (e.g. a config-substitution dropped values); silently
// passing such predicates risks hiding a broken workspace. Authors who
// truly want a no-op can omit the predicate entirely.
type ContainsRequiredIDs struct{}

// Evaluate implements predicates.NativeHandler.
func (ContainsRequiredIDs) Evaluate(_ context.Context, args map[string]any, artifact []byte) predicates.NativeResult {
	ids, err := requireStringSlice(args, "ids")
	if err != nil {
		return predicates.NativeResult{Verdict: false, Feedback: err.Error()}
	}
	if len(ids) == 0 {
		return predicates.NativeResult{
			Verdict:  false,
			Feedback: `arg "ids" must contain at least one entry`,
		}
	}
	ci, err := optionalBool(args, "case_insensitive", false)
	if err != nil {
		return predicates.NativeResult{Verdict: false, Feedback: err.Error()}
	}

	hay := string(artifact)
	if ci {
		hay = strings.ToLower(hay)
	}

	var missing []string
	for _, id := range ids {
		needle := id
		if ci {
			needle = strings.ToLower(needle)
		}
		if !strings.Contains(hay, needle) {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return predicates.NativeResult{Verdict: true}
	}
	return predicates.NativeResult{
		Verdict:  false,
		Feedback: fmt.Sprintf("missing required ids: %s", strings.Join(missing, ", ")),
	}
}

// requireStringSlice coerces args[key] into []string. Accepts either a
// []string directly or YAML's typical []any landing of strings.
func requireStringSlice(args map[string]any, key string) ([]string, error) {
	raw, ok := args[key]
	if !ok {
		return nil, fmt.Errorf("missing required arg %q", key)
	}
	switch t := raw.(type) {
	case []string:
		out := make([]string, len(t))
		copy(out, t)
		return out, nil
	case []any:
		out := make([]string, 0, len(t))
		for i, v := range t {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("arg %q[%d] must be a string, got %T", key, i, v)
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("arg %q must be a list of strings, got %T", key, raw)
}

// optionalBool returns args[key] as a bool, falling back to def when the
// key is absent. Non-bool values produce an error.
func optionalBool(args map[string]any, key string, def bool) (bool, error) {
	raw, ok := args[key]
	if !ok {
		return def, nil
	}
	b, ok := raw.(bool)
	if !ok {
		return false, fmt.Errorf("arg %q must be a bool, got %T", key, raw)
	}
	return b, nil
}
