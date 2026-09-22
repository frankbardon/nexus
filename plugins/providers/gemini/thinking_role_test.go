package gemini

import (
	"bytes"
	"log/slog"
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
		"mode":             "level",
		"level":            "low",
		"include_thoughts": true,
	}
	role := map[string]any{"level": "high"}

	merged := mergeThinkingBlock(plugin, role)

	if merged["level"] != "high" {
		t.Fatalf("level = %v, want the role's %q", merged["level"], "high")
	}
	if merged["mode"] != "level" {
		t.Fatalf("mode = %v, want the plugin's %q to survive", merged["mode"], "level")
	}
	if merged["include_thoughts"] != true {
		t.Fatalf("include_thoughts = %v, want the plugin's true to survive", merged["include_thoughts"])
	}
	// Neither input may be touched: both are shared with loaded configuration.
	if plugin["level"] != "low" {
		t.Fatalf("plugin block mutated: %v", plugin)
	}
	if len(role) != 1 {
		t.Fatalf("role block mutated: %v", role)
	}
}

// A nil role block is a silence: the plugin block stands as-is.
func TestMergeThinkingBlock_NilRoleKeepsPluginBlock(t *testing.T) {
	plugin := map[string]any{"mode": "level"}
	if got := mergeThinkingBlock(plugin, nil); got["mode"] != "level" {
		t.Fatalf("merged = %v, want the plugin block", got)
	}
	if mergeThinkingBlock(nil, nil) != nil {
		t.Fatal("nil + nil must stay nil — the entry said nothing")
	}
}

// The Gemini-specific half of the merge: level and budget_tokens are mutually
// exclusive on the wire, so a role naming one displaces an inherited other
// rather than landing beside it. Without this the commonest per-role
// configuration there is — a 2.5 plugin block and a 3.x role — would merge into
// a block no operator could have written.
func TestMergeThinkingBlock_RoleLevelDisplacesInheritedBudget(t *testing.T) {
	plugin := map[string]any{"mode": "budget", "budget_tokens": 8192}
	role := map[string]any{"mode": "level", "level": "high"}

	tc, err := parseMergedThinking(plugin, role, quietLogger())
	if err != nil {
		t.Fatalf("parseMergedThinking: unexpected error: %v", err)
	}
	if tc.Mode != thinkingModeLevel || tc.Level != "high" {
		t.Fatalf("mode/level = %q/%q, want level/high", tc.Mode, tc.Level)
	}

	// And the other direction.
	tc, err = parseMergedThinking(
		map[string]any{"mode": "level", "level": "high"},
		map[string]any{"mode": "budget", "budget_tokens": 4096},
		quietLogger(),
	)
	if err != nil {
		t.Fatalf("parseMergedThinking: unexpected error: %v", err)
	}
	if tc.Mode != thinkingModeBudget || tc.BudgetTokens != 4096 {
		t.Fatalf("mode/budget = %q/%d, want budget/4096", tc.Mode, tc.BudgetTokens)
	}
}

// A role naming both keys itself is still the config error it always was — that
// one the operator did write.
func TestParseMergedThinking_RoleNamingBothKeysIsAnError(t *testing.T) {
	_, err := parseMergedThinking(nil, map[string]any{
		"level":         "high",
		"budget_tokens": 4096,
	}, quietLogger())
	if err == nil {
		t.Fatal("expected level + budget_tokens on one role to fail")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error = %v, want it to name the exclusivity rule", err)
	}
}

// `thinking: {}` on a role is a statement, not a gap. It overrides no key, but
// it is present — so on a deployment with no plugin-level block at all it turns
// thinking on, exactly as a bare `thinking: {}` in the plugin config does.
func TestParseMergedThinking_EmptyRoleBlockIsAStatement(t *testing.T) {
	tc, err := parseMergedThinking(nil, map[string]any{}, quietLogger())
	if err != nil {
		t.Fatalf("parseMergedThinking: unexpected error: %v", err)
	}
	if tc.Mode != thinkingModeLevel {
		t.Fatalf("mode = %q, want %q — a present block with no mode is level",
			tc.Mode, thinkingModeLevel)
	}

	// And over a plugin block it overrides nothing.
	tc, err = parseMergedThinking(map[string]any{"mode": "off"}, map[string]any{}, quietLogger())
	if err != nil {
		t.Fatalf("parseMergedThinking: unexpected error: %v", err)
	}
	if tc.Mode != thinkingModeOff {
		t.Fatalf("mode = %q, want the plugin's %q untouched", tc.Mode, thinkingModeOff)
	}
}

// The merged block goes through the one parser, so it inherits every check the
// plugin-level block gets — including the mode/budget rule.
func TestParseMergedThinking_UsesTheOneParser(t *testing.T) {
	if _, err := parseMergedThinking(
		map[string]any{"mode": "level"},
		map[string]any{"mode": "budget"},
		quietLogger(),
	); err == nil {
		t.Fatal("expected mode: budget with no budget_tokens to fail")
	}

	// A plugin-level budget the role did not displace satisfies the same rule.
	tc, err := parseMergedThinking(
		map[string]any{"budget_tokens": 4096},
		map[string]any{"mode": "budget"},
		quietLogger(),
	)
	if err != nil {
		t.Fatalf("parseMergedThinking: unexpected error: %v", err)
	}
	if tc.Mode != thinkingModeBudget || tc.BudgetTokens != 4096 {
		t.Fatalf("mode/budget = %q/%d, want budget/4096", tc.Mode, tc.BudgetTokens)
	}
}

// Only a level the role itself named outranks that role's effort; one inherited
// from the plugin block does not.
func TestParseMergedThinking_RecordsWhichLayerNamedTheLevel(t *testing.T) {
	tc, err := parseMergedThinking(map[string]any{"mode": "level", "level": "low"}, map[string]any{}, quietLogger())
	if err != nil {
		t.Fatalf("parseMergedThinking: unexpected error: %v", err)
	}
	if tc.LevelSource != levelFromPlugin {
		t.Fatal("an inherited level must stay marked as the plugin's")
	}

	tc, err = parseMergedThinking(map[string]any{"mode": "level"}, map[string]any{"level": "low"}, quietLogger())
	if err != nil {
		t.Fatalf("parseMergedThinking: unexpected error: %v", err)
	}
	if tc.LevelSource != levelFromRole {
		t.Fatal("a level the role named must be marked as the role's")
	}
}

// --- validateRoleThinking: the block half -----------------------------------

// roleThinkingModels builds a `core.models` registry whose default role is
// "balanced". A nil block omits the `thinking:` key entirely, which is how a
// role that never sets one is configured.
func roleThinkingModels(provider string, entries map[string]map[string]any) *engine.ModelRegistry {
	raw := map[string]any{"default": "balanced"}
	for role, block := range entries {
		cfg := map[string]any{
			"provider":   provider,
			"model":      "gemini-3.1-pro",
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
		"balanced": {"mode": "level"},
		"legacy":   {"mode": "budget"},
	})

	err := validateRoleThinking(models, nil, thinkingConfig{Mode: thinkingModeLevel}, quietLogger())
	if err == nil {
		t.Fatal("expected mode: budget with no budget_tokens to fail Init")
	}
	if !strings.Contains(err.Error(), `role "legacy"`) {
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
		"balanced": {"mode": "level"},
		"legacy":   {"mode": "budget"},
	})

	plugin := map[string]any{"budget_tokens": 8192}
	if err := validateRoleThinking(models, plugin, thinkingConfig{Mode: thinkingModeBudget, BudgetTokens: 8192}, quietLogger()); err != nil {
		t.Fatalf("validateRoleThinking: unexpected error: %v", err)
	}
}

// A role served by another provider is that provider's business.
func TestValidateRoleThinking_SkipsForeignRoles(t *testing.T) {
	models := roleThinkingModels("nexus.llm.anthropic", map[string]map[string]any{
		"balanced": {"mode": "adaptive"},
	})
	if err := validateRoleThinking(models, nil, thinkingConfig{Mode: thinkingModeLevel}, quietLogger()); err != nil {
		t.Fatalf("an Anthropic role must not be parsed as a Gemini block: %v", err)
	}
}

// The whole chain is walked, not just the primary: a fallback entry's block
// reaches this provider through the coordinator's stamp.
func TestValidateRoleThinking_WalksTheWholeChain(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": []any{
			map[string]any{"provider": pluginID, "model": "gemini-3.1-pro", "thinking": map[string]any{"mode": "level"}},
			map[string]any{"provider": pluginID, "model": "gemini-2.5-flash", "thinking": map[string]any{"mode": "budget"}},
		},
	})

	err := validateRoleThinking(models, nil, thinkingConfig{Mode: thinkingModeLevel}, quietLogger())
	if err == nil {
		t.Fatal("expected the second chain entry's block to fail Init")
	}
	if !strings.Contains(err.Error(), `role "balanced"`) {
		t.Fatalf("error = %v, want it to name the role", err)
	}
}

// A role's own mode decides whether that role's effort matters, so a role on
// mode: off is silent even where the plugin block is on mode: level.
func TestValidateRoleThinking_RoleBlockDecidesWhetherEffortMatters(t *testing.T) {
	var buf bytes.Buffer
	models := engine.NewModelRegistry(map[string]any{
		"default": "quiet",
		"quiet": map[string]any{
			"provider": pluginID,
			"model":    "gemini-3.1-pro",
			"effort":   "ludicrous",
			"thinking": map[string]any{"mode": "off"},
		},
	})

	if err := validateRoleThinking(models, nil, thinkingConfig{Mode: thinkingModeLevel}, slog.New(slog.NewTextHandler(&buf, nil))); err != nil {
		t.Fatalf("a role on mode: off must ignore its own effort entirely: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no warning, got %s", buf.String())
	}
}

// --- per-role thinking on the wire ------------------------------------------

// newRoleThinkingPlugin builds a plugin exactly as Init would for the given
// plugin-level block: parsed config and the raw block kept side by side.
func newRoleThinkingPlugin(t *testing.T, block map[string]any, models *engine.ModelRegistry) *Plugin {
	t.Helper()

	cfg := map[string]any{}
	if block != nil {
		cfg["thinking"] = block
	}
	tc, err := parseThinkingConfig(cfg, quietLogger())
	if err != nil {
		t.Fatalf("parseThinkingConfig: unexpected error: %v", err)
	}
	return &Plugin{
		logger:      quietLogger(),
		models:      models,
		thinking:    tc,
		thinkingRaw: rawThinkingBlock(cfg),
	}
}

// wireThinkingFor runs the same three steps handleRequest and buildRequestBody
// do — resolve the target, recover the role's block onto the request, then apply
// the thinking configuration — and reports the thinkingConfig object that ended
// up in generationConfig. A false second return means none was emitted at all.
func wireThinkingFor(t *testing.T, p *Plugin, req events.LLMRequest) (map[string]any, bool) {
	t.Helper()

	gen, err := genThinkingFor(p, req)
	if err != nil {
		t.Fatalf("applyThinking: unexpected error: %v", err)
	}
	cfg, ok := gen["thinkingConfig"].(map[string]any)
	return cfg, ok
}

func genThinkingFor(p *Plugin, req events.LLMRequest) (map[string]any, error) {
	target := p.resolveTarget(req)
	req.Overrides.Thinking = engine.ResolveModelConfig(p.models, req).Overrides.Thinking

	gen := map[string]any{}
	if err := applyThinking(gen, p.resolveThinking(req), target.effort); err != nil {
		return nil, err
	}
	return gen, nil
}

// The headline: two roles on one plugin instance put different thinking objects
// on the wire, which is impossible without per-role blocks.
func TestRoleThinking_TwoRolesDifferentWireShapes(t *testing.T) {
	models := roleThinkingModels(pluginID, map[string]map[string]any{
		"balanced": {"mode": "level", "level": "high"},
		"legacy":   {"mode": "budget", "budget_tokens": 4096},
		"plain":    {"mode": "off"},
	})
	p := newRoleThinkingPlugin(t, map[string]any{"mode": "level", "level": "minimal"}, models)

	level, ok := wireThinkingFor(t, p, events.LLMRequest{Role: "balanced"})
	if !ok || level["thinkingLevel"] != "high" {
		t.Fatalf("balanced: thinkingConfig = %#v (present=%v), want thinkingLevel high", level, ok)
	}
	if _, present := level["thinkingBudget"]; present {
		t.Fatalf("balanced: exactly one of the two may reach the wire: %#v", level)
	}

	budget, ok := wireThinkingFor(t, p, events.LLMRequest{Role: "legacy"})
	if !ok || budget["thinkingBudget"] != 4096 {
		t.Fatalf("legacy: thinkingConfig = %#v (present=%v), want thinkingBudget 4096", budget, ok)
	}
	if _, present := budget["thinkingLevel"]; present {
		t.Fatalf("legacy: a plugin-level level must not ride alongside the budget: %#v", budget)
	}

	if _, ok := wireThinkingFor(t, p, events.LLMRequest{Role: "plain"}); ok {
		t.Fatal("plain: mode off must emit no thinkingConfig at all")
	}
}

// A role that sets no block of its own gets the plugin-level configuration,
// unchanged — the pre-existing behaviour every deployment relies on.
func TestRoleThinking_RoleWithoutBlockKeepsPluginConfig(t *testing.T) {
	models := roleThinkingModels(pluginID, map[string]map[string]any{
		"balanced": {"mode": "level", "level": "high"},
		"quick":    nil,
	})
	p := newRoleThinkingPlugin(t, map[string]any{"mode": "budget", "budget_tokens": 1024}, models)

	got, ok := wireThinkingFor(t, p, events.LLMRequest{Role: "quick"})
	if !ok || got["thinkingBudget"] != 1024 {
		t.Fatalf("thinkingConfig = %#v (present=%v), want the plugin-level budget", got, ok)
	}
}

// Merge, on the wire: a role restating only `include_thoughts` keeps the
// plugin's mode and level.
func TestRoleThinking_PluginKeysSurviveTheRoleBlock(t *testing.T) {
	models := roleThinkingModels(pluginID, map[string]map[string]any{
		"balanced": {"include_thoughts": true},
	})
	p := newRoleThinkingPlugin(t, map[string]any{"mode": "level", "level": "medium"}, models)

	got, ok := wireThinkingFor(t, p, events.LLMRequest{Role: "balanced"})
	if !ok {
		t.Fatal("no thinkingConfig emitted")
	}
	if got["thinkingLevel"] != "medium" {
		t.Fatalf("thinkingLevel = %v, want the plugin's medium to survive", got["thinkingLevel"])
	}
	if got["includeThoughts"] != true {
		t.Fatalf("includeThoughts = %v, want the role's true", got["includeThoughts"])
	}
}

// Precedence, top of the rule: a block already on the request — what the
// fallback and fanout coordinators stamp for the chain entry they are actually
// serving — wins over the role's own, because a registry lookup would answer
// with the chain's first entry.
func TestRoleThinking_StampedRequestWinsOverTheRole(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": []any{
			map[string]any{"provider": pluginID, "model": "gemini-3.1-pro", "thinking": map[string]any{"mode": "level", "level": "high"}},
			map[string]any{"provider": pluginID, "model": "gemini-2.5-flash", "thinking": map[string]any{"mode": "budget", "budget_tokens": 2048}},
		},
	})
	p := newRoleThinkingPlugin(t, nil, models)

	req := events.LLMRequest{Role: "balanced"}
	second, ok := models.Fallback("balanced", 1)
	if !ok {
		t.Fatal("expected a second chain entry")
	}
	engine.StampModelConfig(&req, second)

	got, present := wireThinkingFor(t, p, req)
	if !present || got["thinkingBudget"] != 2048 {
		t.Fatalf("thinkingConfig = %#v (present=%v), want the served entry's budget", got, present)
	}
	if _, has := got["thinkingLevel"]; has {
		t.Fatalf("the first entry's level must not ride along: %#v", got)
	}
}

// Precedence, the whole unified rule on one registry: a role's own `thinking:`
// level beats its `effort`, and that `effort` beats the plugin-level `level`.
// The second half is the direction this story reversed.
func TestRoleThinking_UnifiedPrecedence(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "deep",
		"deep": map[string]any{
			"provider": pluginID,
			"model":    "gemini-3.1-pro",
			"effort":   "max",
			"thinking": map[string]any{"level": "minimal"},
		},
		"shallow": map[string]any{
			"provider": pluginID,
			"model":    "gemini-3.1-pro",
			"effort":   "low",
		},
	})
	p := newRoleThinkingPlugin(t, map[string]any{"mode": "level", "level": "high"}, models)

	// Role block > role effort.
	got, ok := wireThinkingFor(t, p, events.LLMRequest{Role: "deep"})
	if !ok || got["thinkingLevel"] != "minimal" {
		t.Fatalf("deep: thinkingConfig = %#v (present=%v), want the role block's minimal", got, ok)
	}

	// Role effort > plugin block. Before this story `shallow` sent `high`.
	got, ok = wireThinkingFor(t, p, events.LLMRequest{Role: "shallow"})
	if !ok || got["thinkingLevel"] != "low" {
		t.Fatalf("shallow: thinkingConfig = %#v (present=%v), want the role effort's low — the plugin level no longer wins", got, ok)
	}
}

// A request carrying a block nothing validated must not put a shape Gemini will
// 400 on onto the wire: degrade to the plugin-level configuration, and say so
// once.
func TestResolveThinking_InvalidRequestBlockDegrades(t *testing.T) {
	var buf bytes.Buffer
	p := &Plugin{
		logger:      slog.New(slog.NewTextHandler(&buf, nil)),
		thinking:    thinkingConfig{Mode: thinkingModeLevel, Level: "medium"},
		thinkingRaw: map[string]any{"mode": "level", "level": "medium"},
	}

	tc := p.resolveThinking(events.LLMRequest{
		Role:      "hand-set",
		Overrides: events.ModelOverrides{Thinking: map[string]any{"mode": "nonsense"}},
	})

	if tc.Mode != thinkingModeLevel || tc.Level != "medium" {
		t.Fatalf("resolved = %+v, want the plugin-level configuration", tc)
	}
	if !strings.Contains(buf.String(), "hand-set") {
		t.Fatalf("warning = %q, want it to name the role", buf.String())
	}
}
