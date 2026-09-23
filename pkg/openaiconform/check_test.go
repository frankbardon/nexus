package openaiconform

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

// This file tests the ORACLE, not either surface. CheckBody and CheckReply are
// pure functions precisely so that a deliberately-wrong observation can be fed
// to them here: a conformance harness nobody has watched fail is not evidence.

// conformingBody returns the body a vector expects, plus whatever the surface
// is allowed to add — the shape a passing surface produces.
func conformingBody(v Vector, surface Surface) map[string]any {
	body := map[string]any{}
	for k, val := range v.Body {
		body[k] = val
	}
	for _, d := range RequestDivergences() {
		if d.allows(surface) && d.Key == "stream" {
			body["stream"] = false
		}
	}
	return body
}

// The corpus must actually load, and every document in it must survive strict
// decoding and validation.
func TestCorpusLoads(t *testing.T) {
	if len(Vectors()) < 8 {
		t.Fatalf("the request corpus holds %d vectors; it is meant to cover the whole shared surface", len(Vectors()))
	}
	if len(ReplyVectors()) < 6 {
		t.Fatalf("the reply corpus holds %d vectors", len(ReplyVectors()))
	}
}

// A conforming body passes on both surfaces. If this fails, every conformance
// run below is measuring the oracle rather than the surfaces.
func TestCheckBody_AcceptsAConformingBody(t *testing.T) {
	for _, surface := range Surfaces() {
		for _, v := range Vectors() {
			if errs := CheckBody(v, surface, conformingBody(v, surface)); len(errs) > 0 {
				t.Errorf("%s / %s: a body built from the vector itself failed: %v", surface, v.ID, errs)
			}
		}
	}
}

// mutate applies a named corruption and asserts the checker reports it, naming
// what the message must contain so a future rewording that loses the signal
// fails here.
func assertRejects(t *testing.T, name string, body map[string]any, v Vector, surface Surface, want string) {
	t.Helper()
	errs := CheckBody(v, surface, body)
	if len(errs) == 0 {
		t.Fatalf("%s: CheckBody accepted a body it must reject", name)
	}
	var joined strings.Builder
	for _, err := range errs {
		joined.WriteString(err.Error())
		joined.WriteString("\n")
	}
	if !strings.Contains(joined.String(), want) {
		t.Errorf("%s: the failure never mentions %q.\ngot:\n%s", name, want, joined.String())
	}
}

// THE failure this corpus exists for: one surface grows a key the other does
// not have. No value check anywhere would catch it.
func TestCheckBody_CatchesAnAddedKey(t *testing.T) {
	v := Vectors()[0]
	body := conformingBody(v, SurfaceBatch)
	body["previous_response_id"] = "resp_earlier"
	assertRejects(t, "added key", body, v, SurfaceBatch, "UNEXPECTED KEY")
}

// Its mirror image: one surface stops writing a key the other still writes.
func TestCheckBody_CatchesARemovedKey(t *testing.T) {
	v := Vectors()[0]
	body := conformingBody(v, SurfaceBatch)
	delete(body, "input")
	assertRejects(t, "removed key", body, v, SurfaceBatch, "MISSING")
}

// A key added inside a nested object — the tool array in particular — is the
// same failure one level down, and a top-level key-set check would miss it.
func TestCheckBody_CatchesANestedAddedKey(t *testing.T) {
	var v Vector
	for _, c := range Vectors() {
		if len(c.Tools) > 0 {
			v = c
			break
		}
	}
	if v.ID == "" {
		t.Fatal("no vector carries tools")
	}
	body := conformingBody(v, SurfaceBatch)
	tools := append([]any(nil), body["tools"].([]any)...)
	first := map[string]any{}
	for k, val := range tools[0].(map[string]any) {
		first[k] = val
	}
	first["cache_control"] = "ephemeral"
	tools[0] = first
	body["tools"] = tools
	assertRejects(t, "nested added key", body, v, SurfaceBatch, "tools[0].cache_control")
}

// The settled invariants must fail by NAME, so that a reviewer reading a red
// run sees the rule rather than a diff line.
func TestCheckBody_NamedInvariants(t *testing.T) {
	base := Vectors()[0]
	var withTools, withSchema, withReasoning Vector
	for _, v := range Vectors() {
		if len(v.Tools) > 0 && withTools.ID == "" {
			withTools = v
		}
		if v.ResponseFormat != nil && v.ResponseFormat.Type == "json_schema" && withSchema.ID == "" {
			withSchema = v
		}
		if v.Reasoning.On() && v.Temperature != nil && withReasoning.ID == "" {
			withReasoning = v
		}
	}

	cases := []struct {
		name    string
		vector  Vector
		corrupt func(map[string]any)
		want    string
	}{
		{"store flipped to true", base, func(b map[string]any) { b["store"] = true }, "invariant store-is-always-false"},
		{"store dropped", base, func(b map[string]any) { delete(b, "store") }, "invariant store-is-always-false"},
		{"chat token cap", base, func(b map[string]any) {
			delete(b, "max_output_tokens")
			b["max_tokens"] = base.MaxTokens
		}, "invariant max_output_tokens-not-max_tokens"},
		{"chat message array", base, func(b map[string]any) {
			b["messages"] = b["input"]
			delete(b, "input")
		}, "invariant input-not-messages"},
		{"model stripped the way the chat path strips it in Azure mode", base, func(b map[string]any) {
			delete(b, "model")
		}, "invariant model-is-always-present"},
		{"chat structured-output spelling", withSchema, func(b map[string]any) {
			b["response_format"] = b["text"]
			delete(b, "text")
		}, "invariant text.format-not-response_format"},
		{"strict omitted on a function tool", withTools, func(b map[string]any) {
			tools := b["tools"].([]any)
			first := map[string]any{}
			for k, val := range tools[0].(map[string]any) {
				if k != "strict" {
					first[k] = val
				}
			}
			b["tools"] = append([]any{first}, tools[1:]...)
		}, "invariant function-tools-are-flat-and-explicitly-non-strict"},
		{"strict flipped to true", withTools, func(b map[string]any) {
			tools := b["tools"].([]any)
			first := map[string]any{}
			for k, val := range tools[0].(map[string]any) {
				first[k] = val
			}
			first["strict"] = true
			b["tools"] = append([]any{first}, tools[1:]...)
		}, "invariant function-tools-are-flat-and-explicitly-non-strict"},
		{"chat reasoning scalar", withReasoning, func(b map[string]any) {
			b["reasoning_effort"] = withReasoning.Reasoning.Effort
			delete(b, "reasoning")
		}, "invariant reasoning-is-an-object-not-a-scalar"},
		{"sampling parameter survives a reasoning turn", withReasoning, func(b map[string]any) {
			b["temperature"] = 0.7
		}, "invariant reasoning-strips-the-sampling-parameters-it-rejects"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.vector.ID == "" {
				t.Fatal("no vector in the corpus can express this case")
			}
			body := conformingBody(tc.vector, SurfaceBatch)
			tc.corrupt(body)
			assertRejects(t, tc.name, body, tc.vector, SurfaceBatch, tc.want)
		})
	}
}

// Every named invariant must appear in at least one rejection case above.
// Otherwise an invariant could be deleted and nothing would notice.
func TestInvariants_AreAllExercised(t *testing.T) {
	want := map[string]bool{
		"store-is-always-false":                               true,
		"max_output_tokens-not-max_tokens":                    true,
		"input-not-messages":                                  true,
		"model-is-always-present":                             true,
		"text.format-not-response_format":                     true,
		"function-tools-are-flat-and-explicitly-non-strict":   true,
		"reasoning-is-an-object-not-a-scalar":                 true,
		"reasoning-strips-the-sampling-parameters-it-rejects": true,
	}
	got := Invariants()
	if len(got) != len(want) {
		t.Fatalf("Invariants() = %v; this list is the settled wire contract — adding or removing one is a deliberate act that must be reflected in TestCheckBody_NamedInvariants", got)
	}
	for _, name := range got {
		if !want[name] {
			t.Errorf("invariant %q is not covered by TestCheckBody_NamedInvariants", name)
		}
	}
}

// The divergence allowlist bites in BOTH directions: the surface it names may
// produce the key, and every other surface may not.
func TestRequestDivergences_AreEnforcedBothWays(t *testing.T) {
	v := Vectors()[0]

	// The provider is allowed to write stream.
	if errs := CheckBody(v, SurfaceProvider, conformingBody(v, SurfaceProvider)); len(errs) > 0 {
		t.Errorf("the provider surface was refused a key the allowlist grants it: %v", errs)
	}
	// The batch surface is not.
	strayed := conformingBody(v, SurfaceBatch)
	strayed["stream"] = true
	assertRejects(t, "batch grew a stream field", strayed, v, SurfaceBatch, "declared surface-specific")
}

// A vector may not hide a divergence by leaving the key out of its expected
// body: that is the implicit omission the explicit allowlist replaces.
func TestVectors_DoNotSmuggleDivergentKeys(t *testing.T) {
	for _, v := range Vectors() {
		for _, d := range RequestDivergences() {
			if _, ok := v.Body[d.Key]; ok {
				t.Errorf("vector %s puts the divergent key %q in its shared body", v.ID, d.Key)
			}
		}
	}
}

// --- the reply oracle -------------------------------------------------------

// conformingReply builds the LLMResponse a vector expects on a given surface.
func conformingReply(rv ReplyVector, surface Surface) *events.LLMResponse {
	resp := &events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Model:         rv.Expect.Model,
		Content:       rv.Expect.Content,
		FinishReason:  rv.Expect.FinishReason,
		Usage: events.Usage{
			PromptTokens:     rv.Expect.Usage.PromptTokens,
			CompletionTokens: rv.Expect.Usage.CompletionTokens,
			TotalTokens:      rv.Expect.Usage.TotalTokens,
			CachedTokens:     rv.Expect.Usage.CachedTokens,
			ReasoningTokens:  rv.Expect.Usage.ReasoningTokens,
		},
	}
	for _, tc := range rv.Expect.ToolCalls {
		resp.ToolCalls = append(resp.ToolCalls, events.ToolCallRequest{
			ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
		})
	}
	if surface == SurfaceProvider && rv.Expect.ReasoningItems > 0 {
		items := make([]map[string]any, rv.Expect.ReasoningItems)
		for i := range items {
			items[i] = map[string]any{"type": "reasoning"}
		}
		resp.Metadata = map[string]any{ReasoningItemsMetadataKey: items}
	}
	return resp
}

func TestCheckReply_AcceptsAConformingResponse(t *testing.T) {
	for _, surface := range Surfaces() {
		for _, rv := range ReplyVectors() {
			if rv.Error {
				if errs := CheckReply(rv, surface, nil, errTest); len(errs) > 0 {
					t.Errorf("%s / %s: a reported failure was refused: %v", surface, rv.ID, errs)
				}
				continue
			}
			if errs := CheckReply(rv, surface, conformingReply(rv, surface), nil); len(errs) > 0 {
				t.Errorf("%s / %s: a response built from the vector itself failed: %v", surface, rv.ID, errs)
			}
		}
	}
}

type testError struct{}

func (testError) Error() string { return "the run failed" }

var errTest = testError{}

func TestCheckReply_CatchesDrift(t *testing.T) {
	var withCalls, withReasoning, withError, withCached ReplyVector
	for _, rv := range ReplyVectors() {
		if len(rv.Expect.ToolCalls) > 0 && withCalls.ID == "" {
			withCalls = rv
		}
		if rv.Expect.Usage.CachedTokens > 0 && withCached.ID == "" {
			withCached = rv
		}
		if rv.Expect.ReasoningItems > 0 && withReasoning.ID == "" {
			withReasoning = rv
		}
		if rv.Error && withError.ID == "" {
			withError = rv
		}
	}
	base := ReplyVectors()[0]

	cases := []struct {
		name    string
		vector  ReplyVector
		surface Surface
		build   func(ReplyVector, Surface) (*events.LLMResponse, error)
		want    string
	}{
		{"finish reason left untranslated", base, SurfaceBatch, func(rv ReplyVector, s Surface) (*events.LLMResponse, error) {
			r := conformingReply(rv, s)
			r.FinishReason = "completed"
			return r, nil
		}, "`FinishReason`"},
		{"usage counter dropped", withCached, SurfaceBatch, func(rv ReplyVector, s Surface) (*events.LLMResponse, error) {
			r := conformingReply(rv, s)
			r.Usage.CachedTokens = 0
			return r, nil
		}, "`Usage.CachedTokens`"},
		{"tool call keyed by the Item id instead of call_id", withCalls, SurfaceBatch, func(rv ReplyVector, s Surface) (*events.LLMResponse, error) {
			r := conformingReply(rv, s)
			r.ToolCalls[0].ID = "fc_1"
			return r, nil
		}, "`ToolCalls[0].ID`"},
		{"reasoning Items dropped on the provider", withReasoning, SurfaceProvider, func(rv ReplyVector, s Surface) (*events.LLMResponse, error) {
			r := conformingReply(rv, s)
			r.Metadata = nil
			return r, nil
		}, "carries 0 Items"},
		{"reasoning Items captured where the allowlist forbids them", withReasoning, SurfaceBatch, func(rv ReplyVector, s Surface) (*events.LLMResponse, error) {
			r := conformingReply(rv, s)
			r.Metadata = map[string]any{ReasoningItemsMetadataKey: []map[string]any{{"type": "reasoning"}}}
			return r, nil
		}, "declared surface-specific"},
		{"cost priced where the allowlist forbids it", base, SurfaceBatch, func(rv ReplyVector, s Surface) (*events.LLMResponse, error) {
			r := conformingReply(rv, s)
			r.CostUSD = 0.004
			return r, nil
		}, "CostUSD is declared surface-specific"},
		{"a failed run reported as an empty success", withError, SurfaceBatch, func(rv ReplyVector, s Surface) (*events.LLMResponse, error) {
			return &events.LLMResponse{SchemaVersion: events.LLMResponseVersion}, nil
		}, "reported success"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.vector.ID == "" {
				t.Fatal("no reply vector in the corpus can express this case")
			}
			resp, err := tc.build(tc.vector, tc.surface)
			errs := CheckReply(tc.vector, tc.surface, resp, err)
			if len(errs) == 0 {
				t.Fatalf("%s: CheckReply accepted a response it must reject", tc.name)
			}
			var joined strings.Builder
			for _, e := range errs {
				joined.WriteString(e.Error())
				joined.WriteString("\n")
			}
			if !strings.Contains(joined.String(), tc.want) {
				t.Errorf("the failure never mentions %q.\ngot:\n%s", tc.want, joined.String())
			}
		})
	}
}
