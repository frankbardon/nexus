package promote

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/frankbardon/nexus/pkg/engine/journal"
	"gopkg.in/yaml.v3"
)

// nonReplayableTypes flags envelope types whose presence implies the source
// session relied on a side-effect failure — promotion still produces a case
// directory, but the user gets a warning that the stash-replay path will
// not exercise the same path. Keep this set narrow; false positives create
// noise.
//
// provider.fallback.* events fire when a primary provider errored and the
// fallback coordinator advanced the chain. Replay short-circuits the
// primary's stashed response and never raises the error, so the fallback
// branch is silent on replay.
var nonReplayableTypes = map[string]bool{
	"provider.fallback.error":       true,
	"provider.fallback.advance":     true,
	"provider.fallback.exhausted":   true,
	"llm.error":                     true,
	"tool.error":                    true,
	"agent.iteration.exceeded":      true,
}

// observed is the per-promotion projection of the source journal: counts,
// per-tool stats, token totals, and latency samples. Pure data — no engine
// dependency — so the scaffold synthesis is testable in isolation.
type observed struct {
	// EventCounts is per-event-type total count across the journal.
	EventCounts map[string]int
	// ToolCounts is per-tool name count derived from tool.invoke envelopes.
	ToolCounts map[string]int
	// ToolArgKeys is the per-tool union of argument keys observed.
	ToolArgKeys map[string]map[string]struct{}
	// PromptTokens / CompletionTokens are session totals across llm.response
	// payloads.
	PromptTokens     int
	CompletionTokens int
	// TurnDurationsMs is the wall-clock-equivalent duration of each
	// agent.turn.start..agent.turn.end pair (computed from Envelope.Ts deltas,
	// not host wall-clock).
	TurnDurationsMs []int
	// NonReplayableHits names envelope types in nonReplayableTypes that the
	// source session emitted. Surfaces in PromoteResult.Warnings.
	NonReplayableHits []string
}

// observe walks the journal envelopes once and produces a fully-populated
// observed snapshot. Splitting this from the YAML synthesis keeps both
// halves pure and unit-testable.
func observe(envs []journal.Envelope) observed {
	o := observed{
		EventCounts: make(map[string]int),
		ToolCounts:  make(map[string]int),
		ToolArgKeys: make(map[string]map[string]struct{}),
	}
	hits := make(map[string]struct{})
	var turnStarts []int64

	for _, e := range envs {
		o.EventCounts[e.Type]++
		if nonReplayableTypes[e.Type] {
			hits[e.Type] = struct{}{}
		}
		switch e.Type {
		case "tool.invoke":
			name, args := readToolInvoke(e.Payload)
			if name == "" {
				continue
			}
			o.ToolCounts[name]++
			if o.ToolArgKeys[name] == nil {
				o.ToolArgKeys[name] = make(map[string]struct{})
			}
			for k := range args {
				o.ToolArgKeys[name][k] = struct{}{}
			}
		case "llm.response":
			pt, ct := readUsage(e.Payload)
			o.PromptTokens += pt
			o.CompletionTokens += ct
		case "agent.turn.start":
			turnStarts = append(turnStarts, e.Ts.UnixNano())
		case "agent.turn.end":
			if len(turnStarts) == 0 {
				continue
			}
			start := turnStarts[len(turnStarts)-1]
			turnStarts = turnStarts[:len(turnStarts)-1]
			delta := e.Ts.UnixNano() - start
			if delta < 0 {
				delta = 0
			}
			o.TurnDurationsMs = append(o.TurnDurationsMs, int(delta/1_000_000))
		}
	}

	for k := range hits {
		o.NonReplayableHits = append(o.NonReplayableHits, k)
	}
	sort.Strings(o.NonReplayableHits)
	return o
}

// readToolInvoke extracts (name, arguments-as-map) from a journaled
// tool.invoke payload. Mirrors how pkg/eval/case/assertions.go reads the
// same envelopes — duplicated here to avoid an import cycle on evalcase.
func readToolInvoke(payload any) (string, map[string]any) {
	flat, ok := payload.(map[string]any)
	if !ok {
		// Marshal struct payloads through JSON-equivalent flatten via reflection.
		flat = jsonFlatten(payload)
		if flat == nil {
			return "", nil
		}
	}
	name := ""
	if v, ok := flat["Name"]; ok {
		if s, sok := v.(string); sok {
			name = s
		}
	}
	if name == "" {
		if v, ok := flat["name"]; ok {
			if s, sok := v.(string); sok {
				name = s
			}
		}
	}
	var args map[string]any
	if v, ok := flat["Arguments"]; ok {
		args = readArgsAsMap(v)
	}
	if args == nil {
		if v, ok := flat["arguments"]; ok {
			args = readArgsAsMap(v)
		}
	}
	return name, args
}

// readArgsAsMap normalizes a tool-invoke arguments field into a map. The
// engine's typed events.ToolCall stores arguments as a map[string]any, but
// llm.response.tool_calls carry arguments as a JSON-encoded string. Promote
// has to handle the latter shape too because tool.invoke envelopes can
// arrive either way depending on which plugin path emitted them.
func readArgsAsMap(v any) map[string]any {
	switch a := v.(type) {
	case map[string]any:
		return a
	case string:
		if a == "" {
			return nil
		}
		var m map[string]any
		if err := yaml.Unmarshal([]byte(a), &m); err == nil {
			return m
		}
	}
	return nil
}

// readUsage extracts (prompt, completion) tokens from an llm.response
// payload. Mirrors pkg/eval/case/assertions.go:readUsage but on the
// post-reload map shape only — Promote's caller (the scaffold pipeline)
// reads journals from disk where every payload is already map[string]any.
func readUsage(payload any) (int, int) {
	flat, ok := payload.(map[string]any)
	if !ok {
		flat = jsonFlatten(payload)
		if flat == nil {
			return 0, 0
		}
	}
	usage, ok := flat["Usage"]
	if !ok {
		usage, ok = flat["usage"]
	}
	if !ok {
		return 0, 0
	}
	uf, ok := usage.(map[string]any)
	if !ok {
		uf = jsonFlatten(usage)
		if uf == nil {
			return 0, 0
		}
	}
	in := anyToInt(uf["PromptTokens"])
	if in == 0 {
		in = anyToInt(uf["prompt_tokens"])
	}
	out := anyToInt(uf["CompletionTokens"])
	if out == 0 {
		out = anyToInt(uf["completion_tokens"])
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
	case float32:
		return int(n)
	case float64:
		return int(n)
	case uint64:
		return int(n)
	}
	return 0
}

// jsonFlatten round-trips an arbitrary value through JSON to coerce structs
// into map[string]any. Used as a fallback when a payload arrives typed (the
// caller is reading directly off the bus, not from the journal).
func jsonFlatten(v any) map[string]any {
	if v == nil {
		return nil
	}
	data, err := jsonMarshal(v)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := jsonUnmarshal(data, &out); err != nil {
		return nil
	}
	return out
}

// SynthesizeAssertions returns the YAML body for assertions.yaml derived from
// the journal observations. The output is pure data + comments; the caller
// is expected to launch $EDITOR and tighten the bounds.
//
// The scaffold uses exact-count event_count_bounds (min=max=count) so the
// case is initially a strict regression-replay. The user loosens bounds as
// they pull the case into a broader test scenario. Token and latency
// budgets get 10% / 50% slack respectively.
func SynthesizeAssertions(envs []journal.Envelope) ([]byte, error) {
	o := observe(envs)
	return synthesizeAssertionsYAML(o)
}

// synthesizeAssertionsYAML renders the YAML against an observed snapshot.
// Split out so tests can drive it with a hand-crafted snapshot without
// constructing a full envelope slice.
func synthesizeAssertionsYAML(o observed) ([]byte, error) {
	doc := &yaml.Node{Kind: yaml.DocumentNode}
	root := &yaml.Node{
		Kind:        yaml.MappingNode,
		HeadComment: "Auto-generated by `nexus eval promote`.\nReview every assertion: bounds default to exact-match (min=max).\nTighten or loosen as your case evolves; remove what isn't load-bearing.",
	}
	doc.Content = []*yaml.Node{root}

	deterministic := &yaml.Node{
		Kind:        yaml.SequenceNode,
		HeadComment: "Deterministic assertions — checked against the live (replayed) event\nstream. No LLM calls; gates every PR.",
	}
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "deterministic", Tag: "!!str"},
		deterministic,
	)

	// 1) event_count_bounds — one block, all distinct types from the journal
	//    with exact-match bounds. The user widens what should drift.
	if len(o.EventCounts) > 0 {
		entry := &yaml.Node{
			Kind:        yaml.MappingNode,
			HeadComment: "Event-count bounds: every distinct event type observed in the source\njournal, with min=max=count. Loosen the ranges that legitimately\nvary across runs (status updates, ticks, thinking).",
		}
		appendKV(entry, "kind", "event_count_bounds")
		bounds := &yaml.Node{Kind: yaml.MappingNode}
		typeNames := make([]string, 0, len(o.EventCounts))
		for k := range o.EventCounts {
			typeNames = append(typeNames, k)
		}
		sort.Strings(typeNames)
		for _, t := range typeNames {
			c := o.EventCounts[t]
			rng := &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
			appendKV(rng, "min", fmt.Sprintf("%d", c))
			appendKV(rng, "max", fmt.Sprintf("%d", c))
			bounds.Content = append(bounds.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: t, Tag: "!!str"},
				rng,
			)
		}
		entry.Content = append(entry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "bounds", Tag: "!!str"},
			bounds,
		)
		deterministic.Content = append(deterministic.Content, entry)
	}

	// 2) token_budget — observed totals + 10% slack.
	{
		entry := &yaml.Node{
			Kind:        yaml.MappingNode,
			HeadComment: "Session-level token cap with 10% slack on the observed totals.\nFlip per_turn: true to enforce per-turn caps instead.",
		}
		appendKV(entry, "kind", "token_budget")
		appendKV(entry, "max_input_tokens", fmt.Sprintf("%d", withSlack(o.PromptTokens, 0.10)))
		appendKV(entry, "max_output_tokens", fmt.Sprintf("%d", withSlack(o.CompletionTokens, 0.10)))
		appendKVBool(entry, "per_turn", false)
		deterministic.Content = append(deterministic.Content, entry)
	}

	// 3) latency — observed p50/p95 + 50% slack, clamped at 0.
	{
		p50 := percentile(o.TurnDurationsMs, 0.50)
		p95 := percentile(o.TurnDurationsMs, 0.95)
		entry := &yaml.Node{
			Kind:        yaml.MappingNode,
			HeadComment: "Latency caps from observed p50/p95 with 50% slack. Computed from\nevent timestamps, not wall-clock — replay reproduces the same deltas.",
		}
		appendKV(entry, "kind", "latency")
		appendKV(entry, "p50_ms", fmt.Sprintf("%d", withSlack(p50, 0.50)))
		appendKV(entry, "p95_ms", fmt.Sprintf("%d", withSlack(p95, 0.50)))
		deterministic.Content = append(deterministic.Content, entry)
	}

	// 4) tool_invocation_parity — one block per tool seen.
	if len(o.ToolCounts) > 0 {
		toolNames := make([]string, 0, len(o.ToolCounts))
		for k := range o.ToolCounts {
			toolNames = append(toolNames, k)
		}
		sort.Strings(toolNames)
		comment := "Per-tool invocation parity. count_tolerance=0 means exact match;\narg_keys=true asserts the union of argument keys is identical.\nObserved tools: " + commaJoin(toolNames)
		entry := &yaml.Node{
			Kind:        yaml.MappingNode,
			HeadComment: comment,
		}
		appendKV(entry, "kind", "tool_invocation_parity")
		appendKV(entry, "count_tolerance", "0")
		appendKVBool(entry, "arg_keys", true)
		deterministic.Content = append(deterministic.Content, entry)
	}

	// Semantic block: empty list with a TODO. Phase 5 will turn this into a
	// real rubric authoring surface.
	semantic := &yaml.Node{
		Kind:        yaml.SequenceNode,
		HeadComment: "Semantic assertions (Phase 5) — LLM-judge rubrics.\nTODO: add an llm_judge rubric here once the case stabilizes.",
		Style:       yaml.FlowStyle,
	}
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "semantic", Tag: "!!str"},
		semantic,
	)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close yaml encoder: %w", err)
	}
	return buf.Bytes(), nil
}

// withSlack returns v + ceil(v * pct), clamped at zero. Used so the scaffold
// produces a generous-but-not-huge budget the author can tighten by hand.
func withSlack(v int, pct float64) int {
	if v <= 0 {
		return 0
	}
	slack := int(float64(v) * pct)
	if slack < 1 {
		slack = 1
	}
	return v + slack
}

// percentile returns the p-th percentile of an unsorted int slice. p in [0,1].
// Returns 0 for an empty slice.
func percentile(xs []int, p float64) int {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]int(nil), xs...)
	sort.Ints(cp)
	idx := int(p * float64(len(cp)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

// appendKV inserts a key/value pair under parent. value is rendered as YAML
// int by default (matches the assertion-spec fields, which are all
// numeric), with the `kind` key as the one documented exception — its
// values are strings.
func appendKV(parent *yaml.Node, key, value string) {
	tag := "!!int"
	if key == "kind" {
		tag = "!!str"
	}
	parent.Content = append(parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"},
		&yaml.Node{Kind: yaml.ScalarNode, Value: value, Tag: tag},
	)
}

func appendKVBool(parent *yaml.Node, key string, value bool) {
	v := "false"
	if value {
		v = "true"
	}
	parent.Content = append(parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"},
		&yaml.Node{Kind: yaml.ScalarNode, Value: v, Tag: "!!bool"},
	)
}

func commaJoin(xs []string) string {
	out := ""
	for i, s := range xs {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
