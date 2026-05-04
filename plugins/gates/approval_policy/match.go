package approvalpolicy

import (
	"fmt"
	"strings"
)

// rule is a parsed approval-policy rule. Each rule is matched against a
// flattened action-payload map (built by the gate handler). The first
// matching rule wins.
type rule struct {
	match            map[string]any
	mode             string
	choices          []choiceCfg
	defaultChoice    string
	prompt           string
	promptSynthesizer string
	timeoutSeconds   int
}

// choiceCfg is a minimal in-config choice description. Mapped to
// events.HITLChoice at request-emit time.
type choiceCfg struct {
	id    string
	label string
	kind  string
}

// matches returns true iff every key/value pair in the rule's match map
// has a matching entry in payload. String values are treated as glob
// patterns (path.Match semantics: `*`, `?`, `[set]`); non-string values
// must be deep-equal at runtime via fmt formatting.
//
// Match keys may be dotted (e.g. "args.command") to address nested
// values inside payload.
func (r *rule) matches(payload map[string]any) bool {
	for key, want := range r.match {
		got, ok := lookupNested(payload, key)
		if !ok {
			return false
		}
		if !valueMatches(want, got) {
			return false
		}
	}
	return true
}

// lookupNested walks a dotted path through nested map[string]any values.
// Returns (nil, false) when any segment is missing or the path leaves
// map territory before exhaustion.
func lookupNested(m map[string]any, dotted string) (any, bool) {
	if m == nil {
		return nil, false
	}
	parts := strings.Split(dotted, ".")
	var cur any = m
	for _, p := range parts {
		mp, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, present := mp[p]
		if !present {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// valueMatches compares a configured match value against a runtime
// value. String matches are glob via globMatch (* matches any run of
// chars including /, ? matches a single char); other types fall back
// to string-formatted equality so simple scalars (numbers, bools)
// work without ceremony.
func valueMatches(want, got any) bool {
	switch w := want.(type) {
	case string:
		gs, ok := got.(string)
		if !ok {
			return false
		}
		return globMatch(w, gs)
	default:
		return formatScalar(want) == formatScalar(got)
	}
}

// globMatch is a tiny shell-style globber. Supports `*` (any run,
// including slashes — unlike path.Match) and `?` (single char).
// Backslash escapes the next char. No bracket sets — keep it simple;
// add them when a real rule needs them.
func globMatch(pattern, s string) bool {
	// Iterative DP-free matcher. Two cursors, with the * cursor saved
	// so we can backtrack on mismatch. Standard textbook glob impl.
	pi, si := 0, 0
	starPi, starSi := -1, 0
	for si < len(s) {
		if pi < len(pattern) {
			c := pattern[pi]
			switch c {
			case '*':
				starPi = pi
				starSi = si
				pi++
				continue
			case '?':
				pi++
				si++
				continue
			case '\\':
				if pi+1 < len(pattern) {
					if pattern[pi+1] == s[si] {
						pi += 2
						si++
						continue
					}
				}
			default:
				if c == s[si] {
					pi++
					si++
					continue
				}
			}
		}
		// Mismatch — fall back to last `*` if any.
		if starPi >= 0 {
			pi = starPi + 1
			starSi++
			si = starSi
			continue
		}
		return false
	}
	// Drain trailing `*`s in pattern.
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// formatScalar coerces a few common scalar types to their canonical
// string form for cross-type equality (YAML int parses to int, JSON
// might give float64, etc).
func formatScalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		// fmt renders numeric types consistently (1 int vs 1 float64 both
		// render as "1"). Match maps are tiny; the cost is irrelevant.
		return fmt.Sprint(x)
	}
}
