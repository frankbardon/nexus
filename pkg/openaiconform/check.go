package openaiconform

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Divergence is one key a surface may legitimately produce and another may not.
//
// It is the explicit allowlist the story asks for, and it is enforced in BOTH
// directions: a key is deleted before the body comparison on a surface named in
// Only, and REQUIRED ABSENT on every surface that is not. So neither "the
// provider stopped writing it" nor "batch quietly started writing it" passes.
//
// Adding an entry is a deliberate act a reviewer sees in a diff. Deleting a key
// from a vector's expected body to make a run green is not a substitute and is
// refused by validateVector.
type Divergence struct {
	// Key is the top-level body key, or — for a reply — the name of the
	// LLMResponse field.
	Key string
	// Only lists the surfaces allowed to produce it. Every other surface must
	// not.
	Only []Surface
	// Why records the reason, which is printed when the rule is broken.
	Why string
}

func (d Divergence) allows(s Surface) bool {
	for _, only := range d.Only {
		if only == s {
			return true
		}
	}
	return false
}

// RequestDivergences is every request-body key that legitimately differs
// between the two surfaces.
//
// It is short on purpose. Everything else the provider does and the coordinator
// does not — multimodal parts, prompt decoration, tool_choice, tool filtering,
// reasoning-Item replay — is absent from the corpus scope entirely rather than
// allowlisted here, because those are whole features rather than wire
// divergences. See the package doc.
func RequestDivergences() []Divergence {
	return []Divergence{
		{
			Key:  "stream",
			Only: []Surface{SurfaceProvider},
			Why: "A batch line is not a stream: the OpenAI Batch API reads a file of " +
				"complete requests and returns a file of complete replies, so the " +
				"coordinator never writes `stream` at all, while the provider writes " +
				"req.Stream on every request including when it is false.",
		},
	}
}

// ReplyDivergences is every events.LLMResponse field a reply parser may
// legitimately populate on one surface and not the other.
func ReplyDivergences() []Divergence {
	return []Divergence{
		{
			Key:  "CostUSD",
			Only: []Surface{SurfaceProvider},
			Why: "Cost is priced from the provider plugin's own pricing table, which " +
				"the coordinator does not carry; a batched line is costed by the " +
				"batch provider adapter from the batch discount instead.",
		},
		{
			Key:  "Metadata[openai_reasoning_items]",
			Only: []Surface{SurfaceProvider},
			Why: "Reasoning Items are continuity state for the NEXT request of a tool " +
				"loop. A batch line is a one-shot request with no next round inside " +
				"the batch to replay them into, so the coordinator counts their " +
				"tokens and steps over the Items themselves.",
		},
		{
			Key:  "Usage.ModalityBreakdown",
			Only: []Surface{SurfaceProvider},
			Why: "The per-modality token split is the provider's audio/text accounting, " +
				"which the coordinator's text-only scope does not carry.",
		},
	}
}

// ReasoningItemsMetadataKey is where a turn's `reasoning` Items are stashed on
// events.LLMResponse.Metadata.
//
// NOTE: this string is duplicated — as an unexported constant in
// plugins/providers/openai (reasoningItemsMetaKey) and as a bare literal in
// pkg/roundtrip's forwarded-key list, because nothing outside a provider may
// import one. Change one spelling and you must change them all; this corpus
// exists partly so that forgetting is a test failure.
const ReasoningItemsMetadataKey = "openai_reasoning_items"

// Invariants names every settled wire rule CheckBody asserts, in the order it
// asserts them. Exported so a test can pin the set itself: an invariant deleted
// to make a run green would otherwise be invisible.
func Invariants() []string {
	out := make([]string, 0, len(invariants))
	for _, inv := range invariants {
		out = append(out, inv.name)
	}
	return out
}

type invariant struct {
	name  string
	check func(v Vector, body map[string]any) []error
}

// invariants are the rules E4, E5 and E6 settled, asserted by name so that a
// change quietly reversing one fails with the rule's own name rather than as a
// diff line in a body dump.
var invariants = []invariant{
	{"store-is-always-false", checkStore},
	{"max_output_tokens-not-max_tokens", checkTokenCap},
	{"input-not-messages", checkInput},
	{"model-is-always-present", checkModel},
	{"text.format-not-response_format", checkText},
	{"function-tools-are-flat-and-explicitly-non-strict", checkTools},
	{"reasoning-is-an-object-not-a-scalar", checkReasoning},
	{"reasoning-strips-the-sampling-parameters-it-rejects", checkReasoningStrip},
}

func checkStore(_ Vector, body map[string]any) []error {
	raw, ok := body["store"]
	if !ok {
		return []error{fmt.Errorf("`store` is missing. Nexus keeps its own conversation history, so server-side state would be a second and divergent source of truth for the same turn — and `store: false` is also what makes reasoning Items come back carrying encrypted_content")}
	}
	if store, ok := raw.(bool); !ok || store {
		return []error{fmt.Errorf("`store` = %#v, want the literal false on every request", raw)}
	}
	return nil
}

func checkTokenCap(v Vector, body map[string]any) []error {
	var errs []error
	if _, ok := body["max_tokens"]; ok {
		errs = append(errs, fmt.Errorf("`max_tokens` is the Chat Completions spelling; the Responses API takes `max_output_tokens`"))
	}
	raw, ok := body["max_output_tokens"]
	if !ok {
		return append(errs, fmt.Errorf("`max_output_tokens` is missing"))
	}
	if n, ok := asNumber(raw); !ok || int(n) != v.MaxTokens {
		errs = append(errs, fmt.Errorf("`max_output_tokens` = %#v, want %d", raw, v.MaxTokens))
	}
	return errs
}

func checkInput(_ Vector, body map[string]any) []error {
	var errs []error
	if _, ok := body["messages"]; ok {
		errs = append(errs, fmt.Errorf("`messages` is the Chat Completions spelling; the Responses API takes an `input` array of Items"))
	}
	raw, ok := body["input"]
	if !ok {
		return append(errs, fmt.Errorf("`input` is missing"))
	}
	items, ok := raw.([]any)
	if !ok {
		return append(errs, fmt.Errorf("`input` is %T, want an array of Items", raw))
	}
	if len(items) == 0 {
		errs = append(errs, fmt.Errorf("`input` is empty; every vector carries at least one message"))
	}
	return errs
}

func checkModel(v Vector, body map[string]any) []error {
	model, ok := body["model"].(string)
	if !ok || model == "" {
		return []error{fmt.Errorf("`model` = %#v, want %q. It is on the body in EVERY mode, Azure included: the Azure Responses route carries the deployment name in the body rather than in the URL path, so the chat path's strip must not be inherited here", body["model"], v.Model)}
	}
	if model != v.Model {
		return []error{fmt.Errorf("`model` = %q, want %q", model, v.Model)}
	}
	return nil
}

func checkText(v Vector, body map[string]any) []error {
	var errs []error
	if _, ok := body["response_format"]; ok {
		errs = append(errs, fmt.Errorf("`response_format` is the Chat Completions spelling; structured output lives under `text.format` on the Responses API"))
	}
	if v.ResponseFormat == nil || v.ResponseFormat.Type == "text" {
		if _, ok := body["text"]; ok {
			errs = append(errs, fmt.Errorf("`text` is present but the vector asks for no structured output; free text is the API's own default and needs no field"))
		}
		return errs
	}
	text, ok := body["text"].(map[string]any)
	if !ok {
		return append(errs, fmt.Errorf("`text` = %#v, want the object carrying `format`", body["text"]))
	}
	format, ok := text["format"].(map[string]any)
	if !ok {
		return append(errs, fmt.Errorf("`text.format` = %#v, want the flattened format object", text["format"]))
	}
	if got, _ := format["type"].(string); got != v.ResponseFormat.Type {
		errs = append(errs, fmt.Errorf("`text.format.type` = %q, want %q", got, v.ResponseFormat.Type))
	}
	if _, nested := format["json_schema"]; nested {
		errs = append(errs, fmt.Errorf("`text.format.json_schema` is the Chat Completions nesting; the Responses API puts name, schema and strict directly on the format"))
	}
	return errs
}

func checkTools(v Vector, body map[string]any) []error {
	if len(v.Tools) == 0 {
		if _, ok := body["tools"]; ok {
			return []error{fmt.Errorf("`tools` is present but the vector declares none")}
		}
		return nil
	}
	raw, ok := body["tools"].([]any)
	if !ok {
		return []error{fmt.Errorf("`tools` = %#v, want an array of %d function tools", body["tools"], len(v.Tools))}
	}
	var errs []error
	if len(raw) != len(v.Tools) {
		errs = append(errs, fmt.Errorf("`tools` has %d entries, want %d", len(raw), len(v.Tools)))
	}
	for i, entry := range raw {
		tool, ok := entry.(map[string]any)
		if !ok {
			errs = append(errs, fmt.Errorf("`tools[%d]` is %T, want an object", i, entry))
			continue
		}
		if got, _ := tool["type"].(string); got != "function" {
			errs = append(errs, fmt.Errorf("`tools[%d].type` = %q, want \"function\"", i, got))
		}
		if name, _ := tool["name"].(string); name == "" {
			errs = append(errs, fmt.Errorf("`tools[%d].name` is missing; the Responses API puts name, description and parameters directly on the tool rather than under a nested `function` object", i))
		}
		if _, nested := tool["function"]; nested {
			errs = append(errs, fmt.Errorf("`tools[%d].function` is the Chat Completions nesting; the Responses tool shape is flat", i))
		}
		strict, present := tool["strict"]
		if !present {
			errs = append(errs, fmt.Errorf("`tools[%d].strict` is MISSING, and on the Responses API an absent `strict` ATTEMPTS strict mode — the reverse of Chat Completions. Nexus tool schemas come from plugins, MCP servers, skills and operator catalogs, and plenty of them are valid JSON Schema that strict mode rejects, so omitting the key turns a working tool catalog into an HTTP 400 purely from changing `api:`", i))
			continue
		}
		if b, ok := strict.(bool); !ok || b {
			errs = append(errs, fmt.Errorf("`tools[%d].strict` = %#v, want the explicit literal false", i, strict))
		}
	}
	return errs
}

func checkReasoning(v Vector, body map[string]any) []error {
	var errs []error
	if _, ok := body["reasoning_effort"]; ok {
		errs = append(errs, fmt.Errorf("`reasoning_effort` is the Chat Completions scalar; the Responses API takes a `reasoning` OBJECT carrying both the depth and the summary verbosity"))
	}
	want := map[string]any{}
	if v.Reasoning.On() {
		if v.Reasoning.Effort != "" {
			want["effort"] = v.Reasoning.Effort
		}
		if v.Reasoning.Summary != "" {
			want["summary"] = v.Reasoning.Summary
		}
	}
	raw, present := body["reasoning"]
	if len(want) == 0 {
		if present {
			errs = append(errs, fmt.Errorf("`reasoning` = %#v, want no key at all: the vector resolves to %q with no depth and no summary, and an empty object says nothing the model's own defaults do not", raw, v.Reasoning.Mode))
		}
		return errs
	}
	got, ok := raw.(map[string]any)
	if !ok {
		return append(errs, fmt.Errorf("`reasoning` = %#v, want the object %v", raw, want))
	}
	for k, wantV := range want {
		if got[k] != wantV {
			errs = append(errs, fmt.Errorf("`reasoning.%s` = %#v, want %#v", k, got[k], wantV))
		}
	}
	return errs
}

// reasoningRejectedFields are the sampling parameters a reasoning model
// rejects. Both surfaces keep their own copy of this list; the corpus keeps a
// third so that a list trimmed on one side fails here rather than in
// production.
var reasoningRejectedFields = []string{
	"temperature", "top_p", "presence_penalty", "frequency_penalty",
	"logprobs", "top_logprobs", "prediction",
}

func checkReasoningStrip(v Vector, body map[string]any) []error {
	if !v.Reasoning.On() {
		return nil
	}
	var errs []error
	for _, f := range reasoningRejectedFields {
		if _, ok := body[f]; ok {
			errs = append(errs, fmt.Errorf("`%s` survived a reasoning-mode request; a reasoning model rejects it, so it must be stripped AFTER the rest of the body is written", f))
		}
	}
	return errs
}

// CheckBody compares one surface's produced body against a vector, and returns
// every disagreement rather than the first.
//
// It is a pure function on purpose, so the corpus's own test can feed it a
// deliberately wrong body and prove the checking is not vacuous.
//
// Order of work, and why: the divergence rules run first so that an allowed key
// is out of the way and a forbidden one is reported as the rule it broke; the
// named invariants run next, so a body that drifted in a way the diff would
// describe badly still fails by name; the whole-body comparison runs last and
// catches everything nobody thought to name — in particular a KEY SET
// divergence in either direction.
func CheckBody(v Vector, surface Surface, body map[string]any) []error {
	if !surface.Known() {
		return []error{fmt.Errorf("%q is not a declared surface (want one of %v)", surface, Surfaces())}
	}
	got, err := normalize(body)
	if err != nil {
		return []error{fmt.Errorf("the produced body is not JSON-serializable, so it could never reach OpenAI: %w", err)}
	}

	var errs []error
	for _, d := range RequestDivergences() {
		_, present := got[d.Key]
		if d.allows(surface) {
			// Allowed here, so it is not part of the shared comparison.
			delete(got, d.Key)
			continue
		}
		if present {
			errs = append(errs, fmt.Errorf("`%s` is declared surface-specific to %v and %s produced it anyway.\n      why the divergence exists: %s\n      If this surface now legitimately carries the key, widen the Only list in RequestDivergences — do not delete the key from the body",
				d.Key, d.Only, surface, d.Why))
			delete(got, d.Key)
		}
	}

	for _, inv := range invariants {
		for _, err := range inv.check(v, got) {
			errs = append(errs, fmt.Errorf("invariant %s: %w", inv.name, err))
		}
	}

	errs = append(errs, diffValue("", v.Body, got)...)
	return errs
}

// normalize round-trips a body through JSON, which is what actually reaches
// OpenAI. It collapses []map[string]any and []any, int and float64, and any
// other shape difference that is invisible on the wire — so the comparison
// reports real divergence rather than Go typing.
func normalize(body map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// diffValue compares an expectation with an observation and names every
// difference by its path through the body.
//
// Missing and unexpected keys are reported separately from value mismatches
// because they are the failure this corpus exists for: one surface gaining a
// field the other lacks passes every value check ever written.
func diffValue(path string, want, got any) []error {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []error{fmt.Errorf("%s: got %s, want an object", at(path), render(got))}
		}
		var errs []error
		for _, k := range sortedKeys(w) {
			gv, present := g[k]
			if !present {
				errs = append(errs, fmt.Errorf("%s: MISSING — the corpus expects %s and this surface wrote no such key", at(join(path, k)), render(w[k])))
				continue
			}
			errs = append(errs, diffValue(join(path, k), w[k], gv)...)
		}
		for _, k := range sortedKeys(g) {
			if _, expected := w[k]; !expected {
				errs = append(errs, fmt.Errorf("%s: UNEXPECTED KEY — this surface wrote %s and the corpus expects no such key. Either the other surface is missing it (fix the other surface and add it to the vector) or it is legitimately surface-specific (add it to RequestDivergences with a rationale)", at(join(path, k)), render(g[k])))
			}
		}
		return errs
	case []any:
		g, ok := got.([]any)
		if !ok {
			return []error{fmt.Errorf("%s: got %s, want an array of %d", at(path), render(got), len(w))}
		}
		if len(w) != len(g) {
			return []error{fmt.Errorf("%s: has %d entries, want %d\n      want: %s\n      got:  %s", at(path), len(g), len(w), render(want), render(got))}
		}
		var errs []error
		for i := range w {
			errs = append(errs, diffValue(fmt.Sprintf("%s[%d]", path, i), w[i], g[i])...)
		}
		return errs
	default:
		if !reflect.DeepEqual(want, got) {
			return []error{fmt.Errorf("%s: got %s, want %s", at(path), render(got), render(want))}
		}
		return nil
	}
}

func at(path string) string {
	if path == "" {
		return "the body"
	}
	return "`" + strings.TrimPrefix(path, ".") + "`"
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func render(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%#v", v)
	}
	return string(raw)
}

func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}
