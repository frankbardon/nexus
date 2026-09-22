package anthropic

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// --- parseOutputConfig ------------------------------------------------------

// TestParseOutputConfig_Absent verifies that omitting the `output_config` block
// leaves the effort unset, so nothing is emitted and the API default applies.
func TestParseOutputConfig_Absent(t *testing.T) {
	oc, err := parseOutputConfig(map[string]any{})
	if err != nil {
		t.Fatalf("parseOutputConfig: unexpected error: %v", err)
	}
	if oc.Effort != effortUnset {
		t.Fatalf("effort = %q, want unset", oc.Effort)
	}
}

// TestParseOutputConfig_BlockWithoutEffort verifies that a present block with
// no `effort` key is the same as no block at all.
func TestParseOutputConfig_BlockWithoutEffort(t *testing.T) {
	oc, err := parseOutputConfig(map[string]any{
		"output_config": map[string]any{},
	})
	if err != nil {
		t.Fatalf("parseOutputConfig: unexpected error: %v", err)
	}
	if oc.Effort != effortUnset {
		t.Fatalf("effort = %q, want unset", oc.Effort)
	}
}

// TestParseOutputConfig_AcceptedLevels pins the closed vocabulary: exactly the
// five levels Anthropic documents, no more.
func TestParseOutputConfig_AcceptedLevels(t *testing.T) {
	for _, want := range []effortLevel{effortLow, effortMedium, effortHigh, effortXHigh, effortMax} {
		oc, err := parseOutputConfig(map[string]any{
			"output_config": map[string]any{"effort": string(want)},
		})
		if err != nil {
			t.Fatalf("effort %q: unexpected error: %v", want, err)
		}
		if oc.Effort != want {
			t.Fatalf("effort = %q, want %q", oc.Effort, want)
		}
	}
}

// TestParseOutputConfig_UnknownLevel verifies an invalid value fails at parse
// (hence Init) time with an error naming the accepted values, rather than
// reaching the wire as an Anthropic 400.
func TestParseOutputConfig_UnknownLevel(t *testing.T) {
	_, err := parseOutputConfig(map[string]any{
		"output_config": map[string]any{"effort": "extreme"},
	})
	if err == nil {
		t.Fatal("expected an error for an unknown effort level")
	}
	for _, want := range []string{"extreme", "low", "medium", "high", "xhigh", "max"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// TestParseOutputConfig_NonStringLevel verifies a wrongly-typed value is a
// config error rather than a silent drop.
func TestParseOutputConfig_NonStringLevel(t *testing.T) {
	_, err := parseOutputConfig(map[string]any{
		"output_config": map[string]any{"effort": 3},
	})
	if err == nil {
		t.Fatal("expected an error for a non-string effort level")
	}
	if !strings.Contains(err.Error(), "output_config.effort") {
		t.Fatalf("error %q does not name the key", err)
	}
}

// --- applyEffort ------------------------------------------------------------

// TestApplyEffort_Unset verifies that an unset effort adds no `output_config`
// key at all, leaving the API default (`high`) in force.
func TestApplyEffort_Unset(t *testing.T) {
	body := map[string]any{"model": "claude-opus-4-5"}
	applyEffort(body, effortUnset)
	if _, ok := body["output_config"]; ok {
		t.Fatalf("output_config present for an unset effort: %#v", body["output_config"])
	}
}

// TestApplyEffort_Nesting pins the wire shape: effort is nested inside
// `output_config`, never a top-level request field.
func TestApplyEffort_Nesting(t *testing.T) {
	body := map[string]any{}
	applyEffort(body, effortXHigh)

	if _, ok := body["effort"]; ok {
		t.Fatal("effort written as a top-level body field; it must nest in output_config")
	}
	oc, ok := body["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config = %#v, want map[string]any", body["output_config"])
	}
	if oc["effort"] != "xhigh" {
		t.Fatalf("output_config.effort = %#v, want %q", oc["effort"], "xhigh")
	}
}

// TestApplyEffort_MergesIntoExisting verifies the emission merges into an
// `output_config` object that is already on the body rather than replacing it.
// Nothing else writes that object today — the native structured-output path
// still emits a stale top-level `response_format` — but when that is corrected
// `format` and `effort` must coexist.
func TestApplyEffort_MergesIntoExisting(t *testing.T) {
	body := map[string]any{
		"output_config": map[string]any{
			"format": map[string]any{"type": "json_schema"},
		},
	}
	applyEffort(body, effortMax)

	oc, ok := body["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config = %#v, want map[string]any", body["output_config"])
	}
	if oc["effort"] != "max" {
		t.Fatalf("output_config.effort = %#v, want %q", oc["effort"], "max")
	}
	if _, ok := oc["format"]; !ok {
		t.Fatalf("applyEffort clobbered a pre-existing output_config: %#v", oc)
	}
}

// --- buildRequestBody -------------------------------------------------------

// TestBuildRequestBody_EffortOnWire verifies the provider-level key reaches the
// request body through the normal build path.
func TestBuildRequestBody_EffortOnWire(t *testing.T) {
	p := &Plugin{logger: silentTestLogger()}
	p.outputConfig = outputConfig{Effort: effortLow}

	body := p.buildRequestBody("claude-opus-4-5", 1024, events.LLMRequest{
		Messages: []events.Message{{Role: "user", Content: "hi"}},
	})

	oc, ok := body["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config = %#v, want map[string]any", body["output_config"])
	}
	if oc["effort"] != "low" {
		t.Fatalf("output_config.effort = %#v, want %q", oc["effort"], "low")
	}
}

// TestBuildRequestBody_NoEffortNoOutputConfig verifies the unset default adds
// nothing, so an existing deployment's request body is byte-identical.
func TestBuildRequestBody_NoEffortNoOutputConfig(t *testing.T) {
	p := &Plugin{logger: silentTestLogger()}

	body := p.buildRequestBody("claude-opus-4-5", 1024, events.LLMRequest{
		Messages: []events.Message{{Role: "user", Content: "hi"}},
	})

	if _, ok := body["output_config"]; ok {
		t.Fatalf("output_config present with no effort configured: %#v", body["output_config"])
	}
}

// TestBuildRequestBody_EffortIndependentOfThinkingMode verifies effort is
// emitted under every thinking mode, including `off`. The two are separate
// controls: one sets how hard the model works, the other whether thinking
// blocks come back.
func TestBuildRequestBody_EffortIndependentOfThinkingMode(t *testing.T) {
	modes := []thinkingMode{thinkingModeOff, thinkingModeAdaptive, thinkingModeDisabled, thinkingModeBudget}

	for _, mode := range modes {
		p := &Plugin{logger: silentTestLogger()}
		p.outputConfig = outputConfig{Effort: effortHigh}
		p.thinking = thinkingConfig{Mode: mode, BudgetTokens: 2048}

		body := p.buildRequestBody("claude-opus-4-5", 4096, events.LLMRequest{
			Messages: []events.Message{{Role: "user", Content: "hi"}},
		})

		oc, ok := body["output_config"].(map[string]any)
		if !ok {
			t.Fatalf("mode %q: output_config = %#v, want map[string]any", mode, body["output_config"])
		}
		if oc["effort"] != "high" {
			t.Fatalf("mode %q: output_config.effort = %#v, want %q", mode, oc["effort"], "high")
		}
	}
}

// TestBuildRequestBody_EffortSurvivesNativeStructuredOutput is the regression
// guard for the deferred `response_format` → `output_config.format` fix: the
// two emissions must not interfere in either direction.
func TestBuildRequestBody_EffortSurvivesNativeStructuredOutput(t *testing.T) {
	p := &Plugin{logger: silentTestLogger()}
	p.outputConfig = outputConfig{Effort: effortMedium}
	p.structuredOutputs = structuredOutputsConfig{Mode: "native"}

	body := p.buildRequestBody("claude-opus-4-5", 1024, events.LLMRequest{
		Messages: []events.Message{{Role: "user", Content: "hi"}},
		ResponseFormat: &events.ResponseFormat{
			Type:   "json_schema",
			Schema: map[string]any{"type": "object"},
		},
	})

	oc, ok := body["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config = %#v, want map[string]any", body["output_config"])
	}
	if oc["effort"] != "medium" {
		t.Fatalf("output_config.effort = %#v, want %q", oc["effort"], "medium")
	}
	if _, ok := body["response_format"]; !ok {
		t.Fatal("native structured output was lost")
	}
}

// --- per-role effort --------------------------------------------------------

// effortModels builds a `core.models` registry whose default role is
// "balanced". Each entry is a (role, effort) pair; an empty effort omits the
// key entirely, which is how an operator who never sets one is configured.
func effortModels(entries map[string]string) *engine.ModelRegistry {
	raw := map[string]any{"default": "balanced"}
	for role, effort := range entries {
		cfg := map[string]any{
			"provider":   pluginID,
			"model":      "claude-opus-4-5",
			"max_tokens": 2048,
		}
		if effort != "" {
			cfg["effort"] = effort
		}
		raw[role] = cfg
	}
	return engine.NewModelRegistry(raw)
}

// wireEffort runs the same two steps handleRequest does — resolve the role,
// then build the body with the resolved effort on the request — and reports
// what `output_config.effort` ended up as. A false second return means no
// `output_config` was emitted at all.
func wireEffort(t *testing.T, p *Plugin, req events.LLMRequest) (string, bool) {
	t.Helper()

	target := p.resolveTarget(req)
	if target.skip {
		t.Fatal("resolveTarget skipped the request")
	}
	req.Effort = target.effort

	body := p.buildRequestBody(target.model, target.maxTokens, req)
	oc, ok := body["output_config"].(map[string]any)
	if !ok {
		return "", false
	}
	v, ok := oc["effort"].(string)
	return v, ok
}

// TestEffort_RoleOnly verifies a `core.models` role's effort reaches the wire
// when the provider block sets none.
func TestEffort_RoleOnly(t *testing.T) {
	p := &Plugin{logger: silentTestLogger()}
	p.models = effortModels(map[string]string{"balanced": "", "deep": "max"})

	got, ok := wireEffort(t, p, events.LLMRequest{
		Role:     "deep",
		Messages: []events.Message{{Role: "user", Content: "hi"}},
	})
	if !ok || got != "max" {
		t.Fatalf("output_config.effort = %q (present=%v), want %q", got, ok, "max")
	}
}

// TestEffort_ProviderOnly verifies the E1-S3 provider-level key still applies
// when the resolved role carries no effort of its own.
func TestEffort_ProviderOnly(t *testing.T) {
	p := &Plugin{logger: silentTestLogger()}
	p.outputConfig = outputConfig{Effort: effortLow}
	p.models = effortModels(map[string]string{"balanced": "", "deep": ""})

	got, ok := wireEffort(t, p, events.LLMRequest{
		Role:     "deep",
		Messages: []events.Message{{Role: "user", Content: "hi"}},
	})
	if !ok || got != "low" {
		t.Fatalf("output_config.effort = %q (present=%v), want %q", got, ok, "low")
	}
}

// TestEffort_RoleWinsOverProvider pins the precedence: the role's value beats
// the provider-level key. This is deliberately the inverse of Gemini, where
// the plugin-level `thinking.level` beats a role's effort — there `level` is
// the native vocabulary and effort the translated one, whereas both speak the
// same words here.
func TestEffort_RoleWinsOverProvider(t *testing.T) {
	p := &Plugin{logger: silentTestLogger()}
	p.outputConfig = outputConfig{Effort: effortLow}
	p.models = effortModels(map[string]string{"balanced": "", "deep": "xhigh"})

	got, ok := wireEffort(t, p, events.LLMRequest{
		Role:     "deep",
		Messages: []events.Message{{Role: "user", Content: "hi"}},
	})
	if !ok || got != "xhigh" {
		t.Fatalf("output_config.effort = %q (present=%v), want %q", got, ok, "xhigh")
	}
}

// TestEffort_NeitherSet verifies that with no effort anywhere the body carries
// no `output_config` at all, so the API default applies and an existing
// deployment's request is byte-identical.
func TestEffort_NeitherSet(t *testing.T) {
	p := &Plugin{logger: silentTestLogger()}
	p.models = effortModels(map[string]string{"balanced": "", "deep": ""})

	got, ok := wireEffort(t, p, events.LLMRequest{
		Role:     "deep",
		Messages: []events.Message{{Role: "user", Content: "hi"}},
	})
	if ok {
		t.Fatalf("output_config.effort = %q, want no output_config emitted", got)
	}
}

// TestEffort_RequestBeatsRegistry verifies an effort already on the request
// wins over the registry. This is what makes a fallback entry or a non-first
// fanout entry correct: those coordinators stamp the chain entry they are
// actually serving onto the request, whereas a Resolve() here always reads
// chain[0].
func TestEffort_RequestBeatsRegistry(t *testing.T) {
	p := &Plugin{logger: silentTestLogger()}
	p.outputConfig = outputConfig{Effort: effortLow}
	p.models = effortModels(map[string]string{"balanced": "", "deep": "max"})

	got, ok := wireEffort(t, p, events.LLMRequest{
		Role:     "deep",
		Effort:   "medium",
		Messages: []events.Message{{Role: "user", Content: "hi"}},
	})
	if !ok || got != "medium" {
		t.Fatalf("output_config.effort = %q (present=%v), want %q", got, ok, "medium")
	}
}

// TestEffort_DefaultRoleFallback verifies the default-role branch picks up
// effort, mirroring the max_tokens resolution beside it: a request that names
// no role at all resolves through `core.models`' default.
func TestEffort_DefaultRoleFallback(t *testing.T) {
	p := &Plugin{logger: silentTestLogger()}
	p.outputConfig = outputConfig{Effort: effortLow}
	p.models = effortModels(map[string]string{"balanced": "high"})

	got, ok := wireEffort(t, p, events.LLMRequest{
		Messages: []events.Message{{Role: "user", Content: "hi"}},
	})
	if !ok || got != "high" {
		t.Fatalf("output_config.effort = %q (present=%v), want %q", got, ok, "high")
	}
}

// TestEffort_LateRecoveryAfterModelRewrite is the router case the max_tokens
// recovery branches exist for: something rewrote req.Model to a concrete id
// without touching the rest, so the role-resolution branches are skipped
// entirely. The role's effort must still be found.
func TestEffort_LateRecoveryAfterModelRewrite(t *testing.T) {
	p := &Plugin{logger: silentTestLogger()}
	p.models = effortModels(map[string]string{"balanced": "", "deep": "max"})

	target := p.resolveTarget(events.LLMRequest{
		Role:  "deep",
		Model: "claude-opus-4-5-rewritten-by-a-router",
	})
	if target.effort != "max" {
		t.Fatalf("effort = %q, want %q after a model rewrite skipped the role branch", target.effort, "max")
	}
	if target.model != "claude-opus-4-5-rewritten-by-a-router" {
		t.Fatalf("model = %q, want the rewritten id untouched", target.model)
	}
}

// TestEffort_LateRecoveryFromDefaultRole covers the last recovery branch: an
// explicit model plus an unknown role, so only the default role is left to
// answer. It mirrors the max_tokens branch immediately above it.
func TestEffort_LateRecoveryFromDefaultRole(t *testing.T) {
	p := &Plugin{logger: silentTestLogger()}
	p.models = effortModels(map[string]string{"balanced": "medium"})

	target := p.resolveTarget(events.LLMRequest{
		Role:  "no-such-role",
		Model: "claude-opus-4-5",
	})
	if target.effort != "medium" {
		t.Fatalf("effort = %q, want %q from the default role", target.effort, "medium")
	}
	if target.effortSource != defaultRoleEffortSource {
		t.Fatalf("effortSource = %q, want %q", target.effortSource, defaultRoleEffortSource)
	}
}

// TestEffort_InvalidRoleValueFailsTheRequest verifies an unusable role effort
// is reported rather than dropped. Core deliberately does not validate the key
// — the vocabularies differ per provider — so this provider must, and it can
// only do so per request because a role's value does not exist at Init.
func TestEffort_InvalidRoleValueFailsTheRequest(t *testing.T) {
	rec := newBusRecorder()
	p := &Plugin{logger: silentTestLogger(), bus: rec.bus}
	p.models = effortModels(map[string]string{"balanced": "", "deep": "ludicrous"})

	p.handleRequest(events.LLMRequest{
		Role:     "deep",
		Messages: []events.Message{{Role: "user", Content: "hi"}},
	})

	errs := rec.byType("core.error")
	if len(errs) != 1 {
		t.Fatalf("core.error count = %d, want 1", len(errs))
	}
	info, ok := errs[0].Payload.(events.ErrorInfo)
	if !ok {
		t.Fatalf("core.error payload = %#v, want events.ErrorInfo", errs[0].Payload)
	}
	msg := info.Err.Error()
	// The error must name the role, the offending value, and the vocabulary.
	for _, want := range []string{`core.models role "deep"`, `"ludicrous"`, effortValues} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not mention %q", msg, want)
		}
	}
	if info.Retryable {
		t.Fatal("a misconfigured effort is not retryable")
	}
}

// TestEffort_InvalidRequestValueFailsTheRequest covers the same rejection for
// a value that arrived on the request rather than off a role — what a fanout
// leg carrying another provider's vocabulary looks like from here.
func TestEffort_InvalidRequestValueFailsTheRequest(t *testing.T) {
	rec := newBusRecorder()
	p := &Plugin{logger: silentTestLogger(), bus: rec.bus}
	p.models = effortModels(map[string]string{"balanced": ""})

	p.handleRequest(events.LLMRequest{
		Effort:   "minimal", // Gemini's vocabulary, not Anthropic's.
		Messages: []events.Message{{Role: "user", Content: "hi"}},
	})

	errs := rec.byType("core.error")
	if len(errs) != 1 {
		t.Fatalf("core.error count = %d, want 1", len(errs))
	}
	info := errs[0].Payload.(events.ErrorInfo)
	if !strings.Contains(info.Err.Error(), "the request") {
		t.Fatalf("error %q does not name the request as the source", info.Err.Error())
	}
}

// TestResolveEffort_IgnoresAnUnvalidatedBadValue pins the defensive half of
// the seam: handleRequest never lets a bad value reach here, but a direct
// buildRequestBody caller must degrade to the configured default rather than
// put a guaranteed 400 on the wire.
func TestResolveEffort_IgnoresAnUnvalidatedBadValue(t *testing.T) {
	p := &Plugin{logger: silentTestLogger()}
	p.outputConfig = outputConfig{Effort: effortHigh}

	if got := p.resolveEffort(events.LLMRequest{Effort: "ludicrous"}); got != effortHigh {
		t.Fatalf("resolveEffort = %q, want the provider-level default %q", got, effortHigh)
	}
}
