package evalcase

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Assertions is the in-memory schema of assertions.yaml. All deterministic
// kinds live under `deterministic:`. Phase 1 has no semantic kinds yet — the
// `semantic:` block is parsed and stored as raw maps so a future phase can
// add LLM-judge support without a breaking migration.
type Assertions struct {
	Deterministic []Assertion      `yaml:"-"`
	Semantic      []map[string]any `yaml:"-"`
}

// Assertion is the union type for every concrete deterministic assertion.
// Exactly one of the embedded *Spec pointers is non-nil.
type Assertion struct {
	Kind                  string
	EventEmitted          *EventEmittedSpec
	EventSequenceDistance *EventSequenceDistanceSpec
	ToolInvocationParity  *ToolInvocationParitySpec
	EventCountBounds      *EventCountBoundsSpec
	EventSequenceStrict   *EventSequenceStrictSpec
	TokenBudget           *TokenBudgetSpec
	Latency               *LatencySpec
	ResponseContains      *ResponseContainsSpec
	ContainsValue         *ContainsValueSpec
}

// EventEmittedSpec passes when at least Count.Min and at most Count.Max
// envelopes match Type and (optionally) Where.
type EventEmittedSpec struct {
	Type  string         `yaml:"type"`
	Where map[string]any `yaml:"where,omitempty"`
	Count *CountRange    `yaml:"count,omitempty"`
}

// CountRange is a closed range. A zero Min is "no minimum"; a zero Max is
// "no maximum" (matches plan.md semantics — count: { min: 1 } with no max
// reads as "at least 1").
type CountRange struct {
	Min int `yaml:"min,omitempty"`
	Max int `yaml:"max,omitempty"`
}

// EventSequenceDistanceSpec asserts that the Levenshtein ratio between the
// observed event-type stream and the journal's event-type stream is at most
// Threshold (0.0–1.0). 0.0 = identical, 1.0 = totally different.
type EventSequenceDistanceSpec struct {
	Threshold float64 `yaml:"threshold"`
	// Filter, when non-empty, restricts the streams to envelopes whose Type
	// is in the set. Lets a case ignore high-frequency noise (core.tick).
	Filter []string `yaml:"filter,omitempty"`
}

// ToolInvocationParitySpec asserts per-tool invocation count parity (within
// CountTolerance) and, when ArgKeys is true, that the set of argument keys
// observed matches the journal's set per tool. Arg-value parity is
// intentionally not checked — values vary across model upgrades.
type ToolInvocationParitySpec struct {
	CountTolerance int  `yaml:"count_tolerance,omitempty"`
	ArgKeys        bool `yaml:"arg_keys,omitempty"`
}

// EventCountBoundsSpec asserts a per-event-type count range over the observed
// stream.
type EventCountBoundsSpec struct {
	Bounds map[string]CountRange `yaml:"bounds"`
}

// EventSequenceStrictSpec asserts the exact (filtered) event-type sequence
// equals Pattern.
type EventSequenceStrictSpec struct {
	Pattern []string `yaml:"pattern"`
	Filter  []string `yaml:"filter,omitempty"`
}

// TokenBudgetSpec caps tokens read off llm.response events. When PerTurn is
// set, each turn must stay below the limits. Unset (zero) limits are unbounded.
type TokenBudgetSpec struct {
	MaxInputTokens  int  `yaml:"max_input_tokens,omitempty"`
	MaxOutputTokens int  `yaml:"max_output_tokens,omitempty"`
	PerTurn         bool `yaml:"per_turn,omitempty"`
}

// LatencySpec caps p50/p95 turn latency, computed from event timestamp deltas
// (NOT wall-clock). A turn = agent.turn.start..agent.turn.end. Values in ms.
type LatencySpec struct {
	P50Ms int `yaml:"p50_ms,omitempty"`
	P95Ms int `yaml:"p95_ms,omitempty"`
}

// ResponseContainsSpec asserts that the text content of a selected event
// (by default, the final assistant response) contains the required
// substring(s). Matching is case-insensitive.
type ResponseContainsSpec struct {
	// EventType selects which event's Content field to search. Empty means
	// "the final assistant response" — the last llm.response event that has
	// no pending tool calls and non-empty Content (mirrors
	// protocol.isFinalAssistant's "real final turn" definition).
	EventType   string   `yaml:"event_type,omitempty"`
	Contains    []string `yaml:"contains,omitempty"`
	ContainsAny []string `yaml:"contains_any,omitempty"`
}

// ContainsValueSpec asserts that a selected event's text content (default
// selection: same as ResponseContainsSpec — the final assistant response,
// via selectResponseText) contains at least one numeric token within
// Tolerance of Value.
//
// Tolerance defaulting: when Tolerance is zero (unset — the struct's own
// zero value, so an explicit `tolerance: 0` in YAML is indistinguishable
// from omitting it and gets the same inferred default), it defaults to half
// the smallest decimal unit implied by how many digits were written after
// Value's decimal point in assertions.yaml — e.g. `value: 42.3` infers
// tolerance 0.05; `value: 100` infers tolerance 0.5. A plain Go float64
// field cannot itself remember "how many digits were written" (42.3 and
// 42.30 are the same float64), and by the time ParseAssertions reaches this
// spec's decode step the entry has already round-tripped once through
// map[string]any (see rawAssertionsFile / ParseAssertions's entryBytes
// re-marshal) — so any "original YAML text" capture done here would already
// be reading a normalized, shortest-round-trip re-marshal, not the user's
// literal keystrokes. Given that, this implementation infers digit count
// directly from Value itself at evaluation time, via
// strconv.FormatFloat(Value, 'f', -1, 64) (Go's own shortest string that
// round-trips back to the identical float64) — which is provably the same
// string ParseAssertions's yaml.v3 re-marshal would have produced for this
// field, so capturing "raw text" earlier in the pipeline would infer an
// identical default. This is a deliberate simplification documented here
// rather than added machinery (a yaml.Node walk) that would only reproduce
// the same result through more code. Practical consequence: `value: 42.30`
// infers the same default tolerance as `value: 42.3` (both are the float64
// 42.3) — set Tolerance explicitly when that distinction matters.
//
// Unit (e.g. "%", "$") is stripped from the selected text, along with
// thousands-separator commas, before numeric tokens are extracted — see
// extractNumericTokens.
//
// Extraction is word-boundary-aware: a numeric token immediately preceded
// or followed by another digit is rejected, so `value: 4.2` never matches
// text containing "24.2" (see extractNumericTokens).
//
// A candidate passes if its absolute difference from Value is <= Tolerance
// — the tolerance boundary is INCLUSIVE (a value exactly Tolerance away
// passes).
type ContainsValueSpec struct {
	EventType string  `yaml:"event_type,omitempty"`
	Value     float64 `yaml:"value"`
	Tolerance float64 `yaml:"tolerance,omitempty"`
	Unit      string  `yaml:"unit,omitempty"`
}

// rawAssertionsFile is the YAML schema we unmarshal into; the kind is read
// from each entry's `kind` field and the rest of the entry is decoded into
// the matching Spec.
type rawAssertionsFile struct {
	Deterministic []map[string]any `yaml:"deterministic"`
	Semantic      []map[string]any `yaml:"semantic"`
}

// ParseAssertions decodes assertions.yaml bytes into an Assertions value.
// Each deterministic entry must have a `kind` field; unknown kinds error out
// rather than silently degrade — typos in test bundles must be loud.
func ParseAssertions(data []byte) (Assertions, error) {
	var raw rawAssertionsFile
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Assertions{}, fmt.Errorf("yaml: %w", err)
	}

	out := Assertions{Semantic: raw.Semantic}
	for i, entry := range raw.Deterministic {
		kindRaw, ok := entry["kind"]
		if !ok {
			return Assertions{}, fmt.Errorf("deterministic[%d]: missing 'kind'", i)
		}
		kind, ok := kindRaw.(string)
		if !ok {
			return Assertions{}, fmt.Errorf("deterministic[%d]: 'kind' must be string", i)
		}

		// Re-marshal then unmarshal into the typed spec — simplest path that
		// honors yaml tags and tolerates unknown fields.
		entryBytes, err := yaml.Marshal(entry)
		if err != nil {
			return Assertions{}, fmt.Errorf("deterministic[%d]: re-marshal: %w", i, err)
		}

		a := Assertion{Kind: kind}
		switch kind {
		case "event_emitted":
			spec := &EventEmittedSpec{}
			if err := yaml.Unmarshal(entryBytes, spec); err != nil {
				return Assertions{}, fmt.Errorf("deterministic[%d] event_emitted: %w", i, err)
			}
			if spec.Type == "" {
				return Assertions{}, fmt.Errorf("deterministic[%d] event_emitted: missing 'type'", i)
			}
			a.EventEmitted = spec
		case "event_sequence_distance":
			spec := &EventSequenceDistanceSpec{}
			if err := yaml.Unmarshal(entryBytes, spec); err != nil {
				return Assertions{}, fmt.Errorf("deterministic[%d] event_sequence_distance: %w", i, err)
			}
			if spec.Threshold < 0 || spec.Threshold > 1 {
				return Assertions{}, fmt.Errorf("deterministic[%d] event_sequence_distance: threshold must be in [0,1]", i)
			}
			a.EventSequenceDistance = spec
		case "tool_invocation_parity":
			spec := &ToolInvocationParitySpec{}
			if err := yaml.Unmarshal(entryBytes, spec); err != nil {
				return Assertions{}, fmt.Errorf("deterministic[%d] tool_invocation_parity: %w", i, err)
			}
			a.ToolInvocationParity = spec
		case "event_count_bounds":
			spec := &EventCountBoundsSpec{}
			if err := yaml.Unmarshal(entryBytes, spec); err != nil {
				return Assertions{}, fmt.Errorf("deterministic[%d] event_count_bounds: %w", i, err)
			}
			if len(spec.Bounds) == 0 {
				return Assertions{}, fmt.Errorf("deterministic[%d] event_count_bounds: 'bounds' is empty", i)
			}
			a.EventCountBounds = spec
		case "event_sequence_strict":
			spec := &EventSequenceStrictSpec{}
			if err := yaml.Unmarshal(entryBytes, spec); err != nil {
				return Assertions{}, fmt.Errorf("deterministic[%d] event_sequence_strict: %w", i, err)
			}
			if len(spec.Pattern) == 0 {
				return Assertions{}, fmt.Errorf("deterministic[%d] event_sequence_strict: 'pattern' is empty", i)
			}
			a.EventSequenceStrict = spec
		case "token_budget":
			spec := &TokenBudgetSpec{}
			if err := yaml.Unmarshal(entryBytes, spec); err != nil {
				return Assertions{}, fmt.Errorf("deterministic[%d] token_budget: %w", i, err)
			}
			a.TokenBudget = spec
		case "latency":
			spec := &LatencySpec{}
			if err := yaml.Unmarshal(entryBytes, spec); err != nil {
				return Assertions{}, fmt.Errorf("deterministic[%d] latency: %w", i, err)
			}
			a.Latency = spec
		case "response_contains":
			spec := &ResponseContainsSpec{}
			if err := yaml.Unmarshal(entryBytes, spec); err != nil {
				return Assertions{}, fmt.Errorf("deterministic[%d] response_contains: %w", i, err)
			}
			if len(spec.Contains) == 0 && len(spec.ContainsAny) == 0 {
				return Assertions{}, fmt.Errorf("deterministic[%d] response_contains: at least one of 'contains'/'contains_any' is required", i)
			}
			a.ResponseContains = spec
		case "contains_value":
			spec := &ContainsValueSpec{}
			if err := yaml.Unmarshal(entryBytes, spec); err != nil {
				return Assertions{}, fmt.Errorf("deterministic[%d] contains_value: %w", i, err)
			}
			a.ContainsValue = spec
		default:
			return Assertions{}, fmt.Errorf("deterministic[%d]: unknown kind %q", i, kind)
		}
		out.Deterministic = append(out.Deterministic, a)
	}
	return out, nil
}

// -- evaluation --------------------------------------------------------------

// ObservedEvent is the minimal projection of a bus event the assertion engine
// needs. The runner converts engine.Event[any] to ObservedEvent so the case
// package has no engine dependency.
type ObservedEvent struct {
	Type      string
	Timestamp time.Time
	Payload   any
}

// AssertionResult captures the outcome of evaluating one assertion.
type AssertionResult struct {
	Kind        string         `json:"kind"`
	Pass        bool           `json:"pass"`
	Message     string         `json:"message,omitempty"`
	Diagnostics map[string]any `json:"diagnostics,omitempty"`
}

// Evaluate runs the assertion against observed and (where applicable) golden
// event streams. Golden may be nil for assertions that do not need it
// (token_budget, event_emitted, latency, event_count_bounds).
func (a Assertion) Evaluate(observed, golden []ObservedEvent) AssertionResult {
	switch a.Kind {
	case "event_emitted":
		return evalEventEmitted(*a.EventEmitted, observed)
	case "event_sequence_distance":
		return evalEventSequenceDistance(*a.EventSequenceDistance, observed, golden)
	case "tool_invocation_parity":
		return evalToolInvocationParity(*a.ToolInvocationParity, observed, golden)
	case "event_count_bounds":
		return evalEventCountBounds(*a.EventCountBounds, observed)
	case "event_sequence_strict":
		return evalEventSequenceStrict(*a.EventSequenceStrict, observed)
	case "token_budget":
		return evalTokenBudget(*a.TokenBudget, observed)
	case "latency":
		return evalLatency(*a.Latency, observed)
	case "response_contains":
		return evalResponseContains(*a.ResponseContains, observed)
	case "contains_value":
		return evalContainsValue(*a.ContainsValue, observed)
	default:
		return AssertionResult{Kind: a.Kind, Pass: false, Message: "unknown assertion kind"}
	}
}

// -- per-kind implementations ------------------------------------------------

func evalEventEmitted(spec EventEmittedSpec, observed []ObservedEvent) AssertionResult {
	count := 0
	for _, e := range observed {
		if e.Type != spec.Type {
			continue
		}
		if !matchesWhere(e.Payload, spec.Where) {
			continue
		}
		count++
	}

	min, max := 1, 0 // default: at least 1, no upper bound
	if spec.Count != nil {
		min, max = spec.Count.Min, spec.Count.Max
	}

	pass := count >= min && (max == 0 || count <= max)
	msg := ""
	if !pass {
		msg = fmt.Sprintf("type=%q count=%d not in [min=%d, max=%d]", spec.Type, count, min, max)
	}
	return AssertionResult{
		Kind:        "event_emitted",
		Pass:        pass,
		Message:     msg,
		Diagnostics: map[string]any{"count": count, "type": spec.Type},
	}
}

// matchesWhere does a shallow equality check between where keys/values and
// the payload. Payload may be a struct (live, typed) or map[string]any
// (replay, post JSON round-trip). Keys are matched case-insensitively so
// users can write `where: {name: shell}` against an events.ToolCall
// payload whose JSON shape uses `Name`.
func matchesWhere(payload any, where map[string]any) bool {
	if len(where) == 0 {
		return true
	}
	flat := flattenPayload(payload)
	for k, want := range where {
		got, ok := lookupCI(flat, k)
		if !ok {
			return false
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			return false
		}
	}
	return true
}

// lookupCI does a case-insensitive single-key lookup. Returns the first
// match by iteration order (Go map iteration is random — for our usage,
// payloads only have one key per name in practice, so collisions are not a
// concern).
func lookupCI(m map[string]any, key string) (any, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	for k, v := range m {
		if equalFold(k, key) {
			return v, true
		}
	}
	return nil, false
}

// equalFold is a no-allocation ASCII-only case-insensitive compare.
// strings.EqualFold pulls in unicode tables we don't need.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func evalEventSequenceDistance(spec EventSequenceDistanceSpec, observed, golden []ObservedEvent) AssertionResult {
	o := projectTypes(observed, spec.Filter)
	g := projectTypes(golden, spec.Filter)
	dist := levenshtein(o, g)
	maxLen := max(len(o), len(g))
	ratio := 0.0
	if maxLen > 0 {
		ratio = float64(dist) / float64(maxLen)
	}
	pass := ratio <= spec.Threshold
	msg := ""
	if !pass {
		msg = fmt.Sprintf("levenshtein ratio %.3f exceeds threshold %.3f (dist=%d, max=%d)", ratio, spec.Threshold, dist, maxLen)
	}
	return AssertionResult{
		Kind:    "event_sequence_distance",
		Pass:    pass,
		Message: msg,
		Diagnostics: map[string]any{
			"observed_len": len(o),
			"golden_len":   len(g),
			"distance":     dist,
			"ratio":        ratio,
			"threshold":    spec.Threshold,
		},
	}
}

func evalToolInvocationParity(spec ToolInvocationParitySpec, observed, golden []ObservedEvent) AssertionResult {
	obsCounts, obsKeys := perToolStats(observed)
	goldCounts, goldKeys := perToolStats(golden)

	tolerance := spec.CountTolerance
	mismatches := []string{}

	// Count parity per tool seen in either side.
	all := make(map[string]struct{}, len(obsCounts)+len(goldCounts))
	for k := range obsCounts {
		all[k] = struct{}{}
	}
	for k := range goldCounts {
		all[k] = struct{}{}
	}
	for tool := range all {
		o := obsCounts[tool]
		g := goldCounts[tool]
		diff := o - g
		if diff < 0 {
			diff = -diff
		}
		if diff > tolerance {
			mismatches = append(mismatches, fmt.Sprintf("count[%s] obs=%d golden=%d diff=%d tol=%d", tool, o, g, diff, tolerance))
		}
		if spec.ArgKeys {
			if !stringSetEqual(obsKeys[tool], goldKeys[tool]) {
				mismatches = append(mismatches, fmt.Sprintf("arg_keys[%s] obs=%v golden=%v", tool, sortedKeys(obsKeys[tool]), sortedKeys(goldKeys[tool])))
			}
		}
	}

	pass := len(mismatches) == 0
	msg := ""
	if !pass {
		msg = fmt.Sprintf("%d mismatch(es): %v", len(mismatches), mismatches)
	}
	return AssertionResult{
		Kind:        "tool_invocation_parity",
		Pass:        pass,
		Message:     msg,
		Diagnostics: map[string]any{"observed_counts": obsCounts, "golden_counts": goldCounts},
	}
}

// perToolStats returns per-tool invocation counts and the union of arg-keys
// seen for each tool. Reads tool.invoke envelopes; understands both typed
// events.ToolCall payloads and post-JSON-roundtrip map[string]any payloads.
func perToolStats(events []ObservedEvent) (map[string]int, map[string]map[string]struct{}) {
	counts := make(map[string]int)
	keys := make(map[string]map[string]struct{})
	for _, e := range events {
		if e.Type != "tool.invoke" {
			continue
		}
		flat := flattenPayload(e.Payload)
		nameRaw, ok := lookupCI(flat, "name")
		if !ok {
			continue
		}
		name := fmt.Sprint(nameRaw)
		counts[name]++

		if argsAny, ok := lookupCI(flat, "arguments"); ok {
			collectArgKeys(name, argsAny, keys)
		}
	}
	return counts, keys
}

func collectArgKeys(tool string, args any, dst map[string]map[string]struct{}) {
	if dst[tool] == nil {
		dst[tool] = make(map[string]struct{})
	}
	switch v := args.(type) {
	case map[string]any:
		for k := range v {
			dst[tool][k] = struct{}{}
		}
	case string:
		// A JSON-encoded args blob; pull keys without panic on bad JSON.
		if v == "" {
			return
		}
		var m map[string]any
		if err := yaml.Unmarshal([]byte(v), &m); err == nil {
			for k := range m {
				dst[tool][k] = struct{}{}
			}
		}
	}
}

func evalEventCountBounds(spec EventCountBoundsSpec, observed []ObservedEvent) AssertionResult {
	counts := make(map[string]int)
	for _, e := range observed {
		counts[e.Type]++
	}
	violations := []string{}
	for typ, rng := range spec.Bounds {
		c := counts[typ]
		if c < rng.Min || (rng.Max != 0 && c > rng.Max) {
			violations = append(violations, fmt.Sprintf("%s: count=%d not in [%d,%d]", typ, c, rng.Min, rng.Max))
		}
	}
	pass := len(violations) == 0
	msg := ""
	if !pass {
		msg = fmt.Sprintf("%d violation(s): %v", len(violations), violations)
	}
	return AssertionResult{
		Kind:        "event_count_bounds",
		Pass:        pass,
		Message:     msg,
		Diagnostics: map[string]any{"counts": counts},
	}
}

func evalEventSequenceStrict(spec EventSequenceStrictSpec, observed []ObservedEvent) AssertionResult {
	o := projectTypes(observed, spec.Filter)
	pass := stringSliceEqual(o, spec.Pattern)
	msg := ""
	if !pass {
		msg = fmt.Sprintf("sequence mismatch: observed=%v want=%v", o, spec.Pattern)
	}
	return AssertionResult{
		Kind:        "event_sequence_strict",
		Pass:        pass,
		Message:     msg,
		Diagnostics: map[string]any{"observed": o, "pattern": spec.Pattern},
	}
}

func evalTokenBudget(spec TokenBudgetSpec, observed []ObservedEvent) AssertionResult {
	type usage struct{ in, out int }
	var totals usage
	var perTurn []usage
	var current usage
	inTurn := false
	for _, e := range observed {
		switch e.Type {
		case "agent.turn.start":
			if inTurn {
				perTurn = append(perTurn, current)
			}
			current = usage{}
			inTurn = true
		case "agent.turn.end":
			if inTurn {
				perTurn = append(perTurn, current)
				inTurn = false
			}
			current = usage{}
		case "llm.response":
			in, out := readUsage(e.Payload)
			totals.in += in
			totals.out += out
			current.in += in
			current.out += out
		}
	}
	if inTurn {
		perTurn = append(perTurn, current)
	}

	violations := []string{}
	if spec.PerTurn {
		for i, t := range perTurn {
			if spec.MaxInputTokens > 0 && t.in > spec.MaxInputTokens {
				violations = append(violations, fmt.Sprintf("turn[%d] input %d > %d", i, t.in, spec.MaxInputTokens))
			}
			if spec.MaxOutputTokens > 0 && t.out > spec.MaxOutputTokens {
				violations = append(violations, fmt.Sprintf("turn[%d] output %d > %d", i, t.out, spec.MaxOutputTokens))
			}
		}
	} else {
		if spec.MaxInputTokens > 0 && totals.in > spec.MaxInputTokens {
			violations = append(violations, fmt.Sprintf("session input %d > %d", totals.in, spec.MaxInputTokens))
		}
		if spec.MaxOutputTokens > 0 && totals.out > spec.MaxOutputTokens {
			violations = append(violations, fmt.Sprintf("session output %d > %d", totals.out, spec.MaxOutputTokens))
		}
	}
	pass := len(violations) == 0
	msg := ""
	if !pass {
		msg = fmt.Sprintf("%d violation(s): %v", len(violations), violations)
	}
	diag := map[string]any{
		"total_input":  totals.in,
		"total_output": totals.out,
	}
	if spec.PerTurn {
		turnsDiag := make([]map[string]int, 0, len(perTurn))
		for _, t := range perTurn {
			turnsDiag = append(turnsDiag, map[string]int{"input": t.in, "output": t.out})
		}
		diag["per_turn"] = turnsDiag
	}
	return AssertionResult{Kind: "token_budget", Pass: pass, Message: msg, Diagnostics: diag}
}

func evalLatency(spec LatencySpec, observed []ObservedEvent) AssertionResult {
	durations := turnDurationsMs(observed)
	if len(durations) == 0 {
		// No turns observed — pass trivially. There's nothing to measure.
		return AssertionResult{
			Kind:        "latency",
			Pass:        true,
			Diagnostics: map[string]any{"note": "no turn pairs observed"},
		}
	}
	sorted := append([]int(nil), durations...)
	sort.Ints(sorted)
	p50 := percentile(sorted, 0.50)
	p95 := percentile(sorted, 0.95)
	violations := []string{}
	if spec.P50Ms > 0 && p50 > spec.P50Ms {
		violations = append(violations, fmt.Sprintf("p50 %dms > %dms", p50, spec.P50Ms))
	}
	if spec.P95Ms > 0 && p95 > spec.P95Ms {
		violations = append(violations, fmt.Sprintf("p95 %dms > %dms", p95, spec.P95Ms))
	}
	pass := len(violations) == 0
	msg := ""
	if !pass {
		msg = fmt.Sprintf("%d violation(s): %v", len(violations), violations)
	}
	return AssertionResult{
		Kind:    "latency",
		Pass:    pass,
		Message: msg,
		Diagnostics: map[string]any{
			"durations_ms": durations,
			"p50_ms":       p50,
			"p95_ms":       p95,
		},
	}
}

func evalResponseContains(spec ResponseContainsSpec, observed []ObservedEvent) AssertionResult {
	text, found := selectResponseText(spec.EventType, observed)
	if !found {
		return AssertionResult{
			Kind:    "response_contains",
			Pass:    false,
			Message: fmt.Sprintf("no matching event with text content found (event_type=%q)", defaultDisplay(spec.EventType)),
		}
	}

	missing := []string{}
	for _, want := range spec.Contains {
		if !containsFold(text, want) {
			missing = append(missing, want)
		}
	}

	anyOK := len(spec.ContainsAny) == 0
	matched := ""
	for _, want := range spec.ContainsAny {
		if containsFold(text, want) {
			anyOK = true
			matched = want
			break
		}
	}

	pass := len(missing) == 0 && anyOK
	msg := ""
	if !pass {
		var parts []string
		if len(missing) > 0 {
			parts = append(parts, fmt.Sprintf("missing required substring(s): %v", missing))
		}
		if !anyOK {
			parts = append(parts, fmt.Sprintf("none of contains_any present: %v", spec.ContainsAny))
		}
		msg = strings.Join(parts, "; ")
	}
	return AssertionResult{
		Kind:    "response_contains",
		Pass:    pass,
		Message: msg,
		Diagnostics: map[string]any{
			"missing":      missing,
			"matched_any":  matched,
			"event_type":   defaultDisplay(spec.EventType),
			"text_preview": truncateForDiagnostics(text, 200),
		},
	}
}

// defaultDisplay renders eventType for diagnostics/messages, naming the
// implicit default explicitly rather than showing an empty string.
func defaultDisplay(eventType string) string {
	if eventType == "" {
		return "llm.response (final assistant response)"
	}
	return eventType
}

// truncateForDiagnostics caps s at n bytes for diagnostics payloads, without
// the UTF-8-safety ceremony truncateForSummary (protocol package) has —
// diagnostics are for humans reading a report, not for a wire response.
func truncateForDiagnostics(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// selectResponseText locates the text content a response_contains assertion
// searches. When eventType is empty it defaults to the final assistant
// response: the last llm.response event with no pending tool calls and
// non-empty Content, mirroring protocol.isFinalAssistant's "real final
// turn" definition. Otherwise it returns the last eventType event's Content
// field. ok is false when no event both matches and carries text.
func selectResponseText(eventType string, observed []ObservedEvent) (text string, ok bool) {
	target := eventType
	if target == "" {
		target = "llm.response"
	}
	for i := len(observed) - 1; i >= 0; i-- {
		e := observed[i]
		if e.Type != target {
			continue
		}
		flat := flattenPayload(e.Payload)
		if eventType == "" {
			if tc, has := lookupCI(flat, "ToolCalls"); has && !isEmptySlice(tc) {
				// Tool-call turn — not the final assistant message.
				continue
			}
		}
		content, has := lookupCI(flat, "Content")
		if !has {
			continue
		}
		s, isString := content.(string)
		if !isString || s == "" {
			continue
		}
		return s, true
	}
	return "", false
}

// isEmptySlice reports whether v (a flattenPayload-normalized field value,
// so always []any or nil after the JSON round-trip) is empty.
func isEmptySlice(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.([]any)
	if !ok {
		return false
	}
	return len(s) == 0
}

// containsFold reports whether s contains substr, ASCII case-insensitively.
// Mirrors equalFold's rationale: avoid strings.EqualFold/ToLower's unicode
// tables for a check this file only ever needs on ASCII assertion text.
func containsFold(s, substr string) bool {
	if substr == "" {
		return true
	}
	return strings.Contains(asciiLower(s), asciiLower(substr))
}

// asciiLower lowercases the ASCII letters in s, leaving everything else
// (including any non-ASCII bytes) untouched.
func asciiLower(s string) string {
	b := []byte(s)
	for i := range b {
		if 'A' <= b[i] && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func evalContainsValue(spec ContainsValueSpec, observed []ObservedEvent) AssertionResult {
	text, found := selectResponseText(spec.EventType, observed)
	if !found {
		return AssertionResult{
			Kind:    "contains_value",
			Pass:    false,
			Message: fmt.Sprintf("no matching event with text content found (event_type=%q)", defaultDisplay(spec.EventType)),
		}
	}

	tolerance := spec.Tolerance
	if tolerance == 0 {
		tolerance = defaultValueTolerance(spec.Value)
	}

	candidates := extractNumericTokens(text, spec.Unit)

	pass := false
	closestDiff := math.Inf(1)
	closestRaw := ""
	rawTokens := make([]string, 0, len(candidates))
	for _, c := range candidates {
		rawTokens = append(rawTokens, c.raw)
		diff := math.Abs(c.value - spec.Value)
		if diff < closestDiff {
			closestDiff = diff
			closestRaw = c.raw
		}
		if diff <= tolerance {
			pass = true
		}
	}

	msg := ""
	if !pass {
		if len(candidates) == 0 {
			msg = fmt.Sprintf("no numeric tokens found in text (event_type=%q)", defaultDisplay(spec.EventType))
		} else {
			msg = fmt.Sprintf("no numeric token within tolerance %v of %v (closest match %q, off by %v)", tolerance, spec.Value, closestRaw, closestDiff)
		}
	}
	return AssertionResult{
		Kind:    "contains_value",
		Pass:    pass,
		Message: msg,
		Diagnostics: map[string]any{
			"value":       spec.Value,
			"tolerance":   tolerance,
			"unit":        spec.Unit,
			"event_type":  defaultDisplay(spec.EventType),
			"candidates":  rawTokens,
			"closest_raw": closestRaw,
		},
	}
}

// defaultValueTolerance infers ContainsValueSpec's default Tolerance from
// how many digits appear after the decimal point in value's own
// shortest-round-trip string form (see ContainsValueSpec's doc comment for
// why this, rather than a captured "raw YAML text," is the chosen source).
// Half the smallest implied unit: 0 decimal digits -> 0.5, 1 -> 0.05, 2 ->
// 0.005, and so on.
func defaultValueTolerance(value float64) float64 {
	s := strconv.FormatFloat(value, 'f', -1, 64)
	digits := 0
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		digits = len(s) - dot - 1
	}
	return 0.5 / math.Pow10(digits)
}

// numericToken is one word-boundary-correct numeric candidate extracted
// from a ContainsValueSpec's selected text.
type numericToken struct {
	raw   string // the matched substring, post-unit-stripping, pre-comma-stripping
	value float64
}

// numericTokenPattern matches a numeric run: either comma-thousands-grouped
// digits (e.g. "1,234") or a plain digit run (e.g. "12345"), each with an
// optional decimal fraction. It intentionally does not match a leading
// sign — this file has no requirement to distinguish "-4.2" from a hyphen
// in prose (e.g. a "12-34" range), and treating '-' as a boundary character
// keeps that ambiguous case out of scope rather than guessing at it.
var numericTokenPattern = regexp.MustCompile(`(?:[0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)(?:\.[0-9]+)?`)

// extractNumericTokens finds every word-boundary-correct numeric token in
// text and parses it to a float64.
//
// unit (e.g. "%", "$"), when non-empty, is stripped from text before
// matching — since unit characters are never part of numericTokenPattern's
// digit/comma/dot character classes, this only matters for units that
// would otherwise sit directly adjacent to a token and could confuse a
// boundary check (see below); it has no effect on which digits are
// captured.
//
// Word-boundary awareness: because numericTokenPattern's digit run is
// greedy, a leftmost match at a run's actual start already consumes the
// whole run (e.g. matching "24.2" begins at the '2', not "4.2" — there is
// no separate attempt to start mid-run). This function additionally
// double-checks that invariant explicitly: a match whose immediately
// preceding or following byte is itself an ASCII digit is rejected. That
// makes the guarantee "a search for 4.2 never matches inside 24.2" hold by
// construction, not by incidental regex greediness alone.
//
// Thousands-separator commas are stripped from each matched token before
// strconv.ParseFloat.
func extractNumericTokens(text, unit string) []numericToken {
	scan := text
	if unit != "" {
		scan = strings.ReplaceAll(scan, unit, "")
	}
	var out []numericToken
	for _, loc := range numericTokenPattern.FindAllStringIndex(scan, -1) {
		start, end := loc[0], loc[1]
		if start > 0 && isASCIIDigit(scan[start-1]) {
			continue
		}
		if end < len(scan) && isASCIIDigit(scan[end]) {
			continue
		}
		raw := scan[start:end]
		cleaned := strings.ReplaceAll(raw, ",", "")
		v, err := strconv.ParseFloat(cleaned, 64)
		if err != nil {
			continue
		}
		out = append(out, numericToken{raw: raw, value: v})
	}
	return out
}

func isASCIIDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

// turnDurationsMs returns the wall-clock-equivalent duration in milliseconds
// of each agent.turn.start..agent.turn.end pair, computed from event
// timestamps (NOT clock). Unmatched starts/ends are dropped.
func turnDurationsMs(events []ObservedEvent) []int {
	var stack []time.Time
	var out []int
	for _, e := range events {
		switch e.Type {
		case "agent.turn.start":
			stack = append(stack, e.Timestamp)
		case "agent.turn.end":
			if len(stack) == 0 {
				continue
			}
			start := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			d := max(e.Timestamp.Sub(start), 0)
			out = append(out, int(d/time.Millisecond))
		}
	}
	return out
}

func percentile(sorted []int, p float64) int {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	idx = max(idx, 0)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// -- helpers -----------------------------------------------------------------

// projectTypes returns the event-type stream after applying the optional
// filter set. An empty filter passes everything through.
func projectTypes(events []ObservedEvent, filter []string) []string {
	var allow map[string]struct{}
	if len(filter) > 0 {
		allow = make(map[string]struct{}, len(filter))
		for _, f := range filter {
			allow[f] = struct{}{}
		}
	}
	out := make([]string, 0, len(events))
	for _, e := range events {
		if allow != nil {
			if _, ok := allow[e.Type]; !ok {
				continue
			}
		}
		out = append(out, e.Type)
	}
	return out
}

// levenshtein is a minimal O(n*m) edit-distance implementation. ~30 lines —
// not worth a dependency.
func levenshtein(a, b []string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := 0; j <= len(b); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			curr[j] = min(del, ins, sub)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

// flattenPayload returns a map[string]any view of the payload regardless of
// whether it's a struct (live) or map (post JSON round-trip). It's a
// shallow flatten — nested fields stay as their underlying type.
//
// JSON, not YAML, because:
//   - The journal already round-trips payloads via JSON, so post-replay
//     payloads are already map[string]any with Go field names as keys.
//   - JSON marshal of an untagged Go struct preserves the field name
//     exactly; YAML lowercases.
//
// The where-clause matcher therefore matches both sides on Go field names
// (Name, Arguments, Usage, ...). Payloads with explicit json tags (e.g.
// events.ToolCallRequest's `id`/`name`/`arguments`) follow those tags.
func flattenPayload(p any) map[string]any {
	switch v := p.(type) {
	case nil:
		return nil
	case map[string]any:
		return v
	default:
		data, err := json.Marshal(p)
		if err != nil {
			return nil
		}
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			return nil
		}
		return out
	}
}

// readUsage extracts (PromptTokens, CompletionTokens) from an llm.response
// payload regardless of whether it's a typed events.LLMResponse or a
// post-JSON-roundtrip map. Both shapes go through flattenPayload (JSON
// marshal) so the keys are the Go field names.
func readUsage(payload any) (in, out int) {
	flat := flattenPayload(payload)
	usage, ok := lookupCI(flat, "Usage")
	if !ok {
		return 0, 0
	}
	uf := flattenPayload(usage)
	if uf == nil {
		return 0, 0
	}
	if v, ok := lookupCI(uf, "PromptTokens"); ok {
		in = anyToInt(v)
	}
	if v, ok := lookupCI(uf, "CompletionTokens"); ok {
		out = anyToInt(v)
	}
	return in, out
}

func anyToInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	case uint64:
		return int(n)
	}
	return 0
}

func stringSetEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
