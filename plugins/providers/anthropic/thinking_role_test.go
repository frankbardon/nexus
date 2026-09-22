package anthropic

import (
	"bytes"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// --- mergeThinkingBlock -----------------------------------------------------

// A role's block wins per key and leaves the plugin keys it does not mention
// alone — the whole point of a merge rather than a substitution.
func TestMergeThinkingBlock_RoleWinsPerKey(t *testing.T) {
	plugin := map[string]any{
		"mode":             "adaptive",
		"display":          "summarized",
		"include_thoughts": true,
	}
	role := map[string]any{"display": "omitted"}

	merged := mergeThinkingBlock(plugin, role)

	if merged["display"] != "omitted" {
		t.Fatalf("display = %v, want the role's %q", merged["display"], "omitted")
	}
	if merged["mode"] != "adaptive" {
		t.Fatalf("mode = %v, want the plugin's %q to survive", merged["mode"], "adaptive")
	}
	if merged["include_thoughts"] != true {
		t.Fatalf("include_thoughts = %v, want the plugin's true to survive", merged["include_thoughts"])
	}
	// Neither input may be touched: both are shared with loaded configuration.
	if plugin["display"] != "summarized" {
		t.Fatalf("plugin block mutated: %v", plugin)
	}
	if len(role) != 1 {
		t.Fatalf("role block mutated: %v", role)
	}
}

// A nil role block is a silence: the plugin block stands as-is.
func TestMergeThinkingBlock_NilRoleKeepsPluginBlock(t *testing.T) {
	plugin := map[string]any{"mode": "adaptive"}
	if got := mergeThinkingBlock(plugin, nil); got["mode"] != "adaptive" {
		t.Fatalf("merged = %v, want the plugin block", got)
	}
	if mergeThinkingBlock(nil, nil) != nil {
		t.Fatal("nil + nil must stay nil — the entry said nothing")
	}
}

// `thinking: {}` on a role is a statement, not a gap. It overrides no key, but
// it is present — so on a deployment with no plugin-level block at all it turns
// thinking on, exactly as a bare `thinking: {}` in the plugin config does.
func TestParseMergedThinking_EmptyRoleBlockIsAStatement(t *testing.T) {
	tc, err := parseMergedThinking(nil, map[string]any{}, silentTestLogger())
	if err != nil {
		t.Fatalf("parseMergedThinking: unexpected error: %v", err)
	}
	if tc.Mode != thinkingModeAdaptive {
		t.Fatalf("mode = %q, want %q — a present block with no mode is adaptive",
			tc.Mode, thinkingModeAdaptive)
	}

	// And over a plugin block it overrides nothing.
	tc, err = parseMergedThinking(map[string]any{"mode": "disabled"}, map[string]any{}, silentTestLogger())
	if err != nil {
		t.Fatalf("parseMergedThinking: unexpected error: %v", err)
	}
	if tc.Mode != thinkingModeDisabled {
		t.Fatalf("mode = %q, want the plugin's %q untouched", tc.Mode, thinkingModeDisabled)
	}
}

// The merged block goes through the one parser, so it inherits every check and
// inference the plugin-level block gets — including the mode/budget rule.
func TestParseMergedThinking_UsesTheOneParser(t *testing.T) {
	// A role switching to budget without supplying one is an error, even
	// though the plugin block is perfectly valid on its own.
	if _, err := parseMergedThinking(
		map[string]any{"mode": "adaptive"},
		map[string]any{"mode": "budget"},
		silentTestLogger(),
	); err == nil {
		t.Fatal("expected mode: budget with no budget_tokens to fail")
	}

	// The documented sharp edge, pinned: a plugin-level budget_tokens survives
	// a role's switch to budget and satisfies the same rule.
	tc, err := parseMergedThinking(
		map[string]any{"mode": "adaptive", "budget_tokens": 4096},
		map[string]any{"mode": "budget"},
		silentTestLogger(),
	)
	if err != nil {
		t.Fatalf("parseMergedThinking: unexpected error: %v", err)
	}
	if tc.Mode != thinkingModeBudget || tc.BudgetTokens != 4096 {
		t.Fatalf("mode/budget = %q/%d, want budget/4096", tc.Mode, tc.BudgetTokens)
	}
}

// --- validateRoleThinking ---------------------------------------------------

// roleThinkingModels builds a `core.models` registry whose default role is
// "balanced". A nil block omits the `thinking:` key entirely, which is how a
// role that never sets one is configured.
func roleThinkingModels(provider string, entries map[string]map[string]any) *engine.ModelRegistry {
	raw := map[string]any{"default": "balanced"}
	for role, block := range entries {
		cfg := map[string]any{
			"provider":   provider,
			"model":      "claude-opus-4-7",
			"max_tokens": 2048,
		}
		if block != nil {
			cfg["thinking"] = block
		}
		raw[role] = cfg
	}
	return engine.NewModelRegistry(raw)
}

// A role block that is invalid once merged must fail the boot, naming the role
// — these blocks never pass through schema.json, so nothing else checks them.
func TestValidateRoleThinking_FailsBootNamingTheRole(t *testing.T) {
	models := roleThinkingModels(pluginID, map[string]map[string]any{
		"balanced": {"mode": "adaptive"},
		"deep":     {"mode": "budget"},
	})

	err := validateRoleThinking(models, nil, silentTestLogger())
	if err == nil {
		t.Fatal("expected mode: budget with no budget_tokens to fail Init")
	}
	if !strings.Contains(err.Error(), `role "deep"`) {
		t.Fatalf("error = %v, want it to name the offending role", err)
	}
	if !strings.Contains(err.Error(), "budget_tokens") {
		t.Fatalf("error = %v, want it to name the missing key", err)
	}
}

// A plugin-level key the role inherits can satisfy the rule, so the same role
// block boots clean under a different plugin block.
func TestValidateRoleThinking_PluginKeySatisfiesTheRoleBlock(t *testing.T) {
	models := roleThinkingModels(pluginID, map[string]map[string]any{
		"balanced": {"mode": "adaptive"},
		"deep":     {"mode": "budget"},
	})

	if err := validateRoleThinking(models, map[string]any{"budget_tokens": 8192}, silentTestLogger()); err != nil {
		t.Fatalf("validateRoleThinking: unexpected error: %v", err)
	}
}

// A role served by another provider is that provider's business, and a nil
// registry is the embedder who configured no roles at all.
func TestValidateRoleThinking_SkipsForeignRolesAndNilRegistry(t *testing.T) {
	models := roleThinkingModels("nexus.llm.gemini", map[string]map[string]any{
		"balanced": {"mode": "level", "level": "high"},
	})
	if err := validateRoleThinking(models, nil, silentTestLogger()); err != nil {
		t.Fatalf("a Gemini role must not be parsed as an Anthropic block: %v", err)
	}
	if err := validateRoleThinking(nil, nil, silentTestLogger()); err != nil {
		t.Fatalf("nil registry: unexpected error: %v", err)
	}
}

// The whole chain is walked, not just the primary: a fallback entry's block
// reaches this provider through the coordinator's stamp.
func TestValidateRoleThinking_WalksTheWholeChain(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": []any{
			map[string]any{"provider": pluginID, "model": "claude-opus-4-7", "thinking": map[string]any{"mode": "adaptive"}},
			map[string]any{"provider": pluginID, "model": "claude-haiku-4-5", "thinking": map[string]any{"mode": "budget"}},
		},
	})

	err := validateRoleThinking(models, nil, silentTestLogger())
	if err == nil {
		t.Fatal("expected the second chain entry's block to fail Init")
	}
	if !strings.Contains(err.Error(), `role "balanced"`) {
		t.Fatalf("error = %v, want it to name the role", err)
	}
}

// --- per-role thinking on the wire ------------------------------------------

// wireThinking runs the same two steps handleRequest does — resolve the role's
// block onto the request, then build the body — and reports the `thinking`
// object that ended up on the wire. A false second return means no `thinking`
// field was emitted at all.
func wireThinking(t *testing.T, p *Plugin, req events.LLMRequest) (map[string]any, bool) {
	t.Helper()

	target := p.resolveTarget(req)
	if target.skip {
		t.Fatal("resolveTarget skipped the request")
	}
	req.Effort = target.effort
	req.Overrides.Thinking = engine.ResolveModelConfig(p.models, req).Overrides.Thinking

	body := p.buildRequestBody(target.model, target.maxTokens, req)
	thinking, ok := body["thinking"].(map[string]any)
	return thinking, ok
}

// newRoleThinkingPlugin builds a plugin exactly as Init would for the given
// plugin-level block: parsed config and the raw block kept side by side.
func newRoleThinkingPlugin(t *testing.T, block map[string]any, models *engine.ModelRegistry) *Plugin {
	t.Helper()

	cfg := map[string]any{}
	if block != nil {
		cfg["thinking"] = block
	}
	tc, err := parseThinkingConfig(cfg, silentTestLogger())
	if err != nil {
		t.Fatalf("parseThinkingConfig: unexpected error: %v", err)
	}
	return &Plugin{
		logger:      silentTestLogger(),
		models:      models,
		thinking:    tc,
		thinkingRaw: rawThinkingBlock(cfg),
	}
}

// The headline: two roles on one plugin instance put different thinking objects
// on the wire, which is impossible without per-role blocks.
func TestRoleThinking_TwoRolesDifferentWireShapes(t *testing.T) {
	models := roleThinkingModels(pluginID, map[string]map[string]any{
		"balanced": {"mode": "adaptive"},
		"legacy":   {"mode": "budget", "budget_tokens": 4096},
		"plain":    {"mode": "off"},
	})
	p := newRoleThinkingPlugin(t, map[string]any{"mode": "disabled"}, models)

	adaptive, ok := wireThinking(t, p, events.LLMRequest{Role: "balanced"})
	if !ok || adaptive["type"] != "adaptive" {
		t.Fatalf("balanced: thinking = %#v (present=%v), want type adaptive", adaptive, ok)
	}

	budget, ok := wireThinking(t, p, events.LLMRequest{Role: "legacy"})
	if !ok || budget["type"] != "enabled" || budget["budget_tokens"] != 4096 {
		t.Fatalf("legacy: thinking = %#v (present=%v), want the fixed-budget shape", budget, ok)
	}

	if _, ok := wireThinking(t, p, events.LLMRequest{Role: "plain"}); ok {
		t.Fatal("plain: mode off must emit no thinking field at all")
	}
}

// A role that sets no block of its own gets the plugin-level configuration,
// unchanged — the pre-existing behaviour every deployment relies on.
func TestRoleThinking_RoleWithoutBlockKeepsPluginConfig(t *testing.T) {
	models := roleThinkingModels(pluginID, map[string]map[string]any{
		"balanced": {"mode": "adaptive"},
		"quick":    nil,
	})
	p := newRoleThinkingPlugin(t, map[string]any{"mode": "disabled"}, models)

	got, ok := wireThinking(t, p, events.LLMRequest{Role: "quick"})
	if !ok || got["type"] != "disabled" {
		t.Fatalf("thinking = %#v (present=%v), want the plugin-level disabled", got, ok)
	}
}

// Merge, on the wire: a role restating only `display` keeps the plugin's mode.
func TestRoleThinking_PluginKeysSurviveTheRoleBlock(t *testing.T) {
	models := roleThinkingModels(pluginID, map[string]map[string]any{
		"balanced": {"display": "omitted"},
	})
	p := newRoleThinkingPlugin(t, map[string]any{"mode": "adaptive", "display": "summarized"}, models)

	got, ok := wireThinking(t, p, events.LLMRequest{Role: "balanced"})
	if !ok {
		t.Fatal("no thinking field emitted")
	}
	if got["type"] != "adaptive" {
		t.Fatalf("type = %v, want the plugin's adaptive to survive", got["type"])
	}
	if got["display"] != "omitted" {
		t.Fatalf("display = %v, want the role's %q", got["display"], "omitted")
	}
}

// Precedence: a block already on the request — what the fallback and fanout
// coordinators stamp for the chain entry they are actually serving — wins over
// the role's own, because a registry lookup would answer with the chain's first
// entry.
func TestRoleThinking_StampedRequestWinsOverTheRole(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": []any{
			map[string]any{"provider": pluginID, "model": "claude-opus-4-7", "thinking": map[string]any{"mode": "adaptive"}},
			map[string]any{"provider": pluginID, "model": "claude-haiku-4-5", "thinking": map[string]any{"mode": "budget", "budget_tokens": 2048}},
		},
	})
	p := newRoleThinkingPlugin(t, nil, models)

	req := events.LLMRequest{Role: "balanced"}
	second, ok := models.Fallback("balanced", 1)
	if !ok {
		t.Fatal("expected a second chain entry")
	}
	engine.StampModelConfig(&req, second)

	got, present := wireThinking(t, p, req)
	if !present || got["type"] != "enabled" || got["budget_tokens"] != 2048 {
		t.Fatalf("thinking = %#v (present=%v), want the served entry's fixed-budget shape", got, present)
	}
}

// The role's `thinking:` and its `effort:` are separate wire fields here, so a
// role can set both and neither displaces the other.
func TestRoleThinking_CoexistsWithRoleEffort(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": map[string]any{
			"provider": pluginID,
			"model":    "claude-opus-4-7",
			"effort":   "max",
			"thinking": map[string]any{"mode": "adaptive"},
		},
	})
	p := newRoleThinkingPlugin(t, map[string]any{"mode": "disabled"}, models)
	p.outputConfig = outputConfig{Effort: effortLow}

	req := events.LLMRequest{Role: "balanced"}
	target := p.resolveTarget(req)
	req.Effort = target.effort
	req.Overrides.Thinking = engine.ResolveModelConfig(p.models, req).Overrides.Thinking
	body := p.buildRequestBody(target.model, target.maxTokens, req)

	thinking, ok := body["thinking"].(map[string]any)
	if !ok || thinking["type"] != "adaptive" {
		t.Fatalf("thinking = %#v, want the role's adaptive over the plugin's disabled", body["thinking"])
	}
	oc, ok := body["output_config"].(map[string]any)
	if !ok || oc["effort"] != "max" {
		t.Fatalf("output_config = %#v, want the role's effort max", body["output_config"])
	}
}

// A request carrying a block nothing validated must not put a shape Anthropic
// will 400 on onto the wire: degrade to the plugin-level configuration, and say
// so once.
func TestResolveThinking_InvalidRequestBlockDegrades(t *testing.T) {
	var buf bytes.Buffer
	p := &Plugin{
		logger:      warnCaptureLogger(&buf),
		thinking:    thinkingConfig{Mode: thinkingModeAdaptive},
		thinkingRaw: map[string]any{"mode": "adaptive"},
	}

	tc := p.resolveThinking(events.LLMRequest{
		Role:      "hand-set",
		Overrides: events.ModelOverrides{Thinking: map[string]any{"mode": "nonsense"}},
	})

	if tc.Mode != thinkingModeAdaptive {
		t.Fatalf("mode = %q, want the plugin-level %q", tc.Mode, thinkingModeAdaptive)
	}
	if !strings.Contains(buf.String(), "hand-set") {
		t.Fatalf("warning = %q, want it to name the role", buf.String())
	}
}
