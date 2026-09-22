package openai

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file covers the step between a `core.models` entry's reasoning depth —
// its `effort:` and its native `reasoning:` block — and the request body.
//
// Until this story `nexus.llm.openai` read neither: reasoning depth was a
// plugin-level key and nothing else, so a role's `effort:` parsed, validated,
// travelled and was read by nobody on this provider. It is now the third
// consumer, and the easy one: OpenAI's vocabulary is a superset of the union of
// what nexus.llm.anthropic and nexus.llm.gemini accept, so there is no clamp and
// no clamp warning — every word an operator can write for either of those
// passes through verbatim.

// reasoningModels builds a registry of single-entry roles, one per map key. The
// entry map is merged over the provider/model/max_tokens skeleton, so a test
// writes only the axes it cares about.
func reasoningModels(entries map[string]map[string]any) *engine.ModelRegistry {
	raw := map[string]any{"default": "balanced"}
	for role, extra := range entries {
		cfg := map[string]any{
			"provider":   pluginID,
			"model":      "gpt-5",
			"max_tokens": 2048,
		}
		for k, v := range extra {
			cfg[k] = v
		}
		raw[role] = cfg
	}
	return engine.NewModelRegistry(raw)
}

// reasoningPlugin builds the minimum Plugin buildRequestBody runs against, with
// the given plugin-level `reasoning:` block already parsed the way Init parses
// it. A nil block is the deployment that configured no reasoning at all.
func reasoningPlugin(t *testing.T, models *engine.ModelRegistry, block map[string]any) *Plugin {
	t.Helper()

	cfg := map[string]any{}
	if block != nil {
		cfg["reasoning"] = block
	}
	rc, err := parseReasoningConfig(cfg, silentLogger())
	if err != nil {
		t.Fatalf("parseReasoningConfig(%#v): %v", block, err)
	}
	return &Plugin{
		logger:       silentLogger(),
		models:       models,
		reasoning:    rc,
		reasoningRaw: rawReasoningBlock(cfg),
	}
}

// wireEffort runs the step handleRequest does before building the body and
// reports what `reasoning_effort` ended up as. A false second return means no
// key was emitted.
//
// It calls the very method handleRequest calls, so a regression that unwires
// applyEntryOverrides from the body builder cannot hide behind the helper.
func wireEffort(t *testing.T, p *Plugin, req events.LLMRequest) (string, bool) {
	t.Helper()

	p.applyEntryOverrides(&req)

	model := req.Model
	if model == "" {
		model = "gpt-5"
	}
	body := p.buildRequestBody(model, 1024, req)
	effort, ok := body["reasoning_effort"].(string)
	return effort, ok
}

// --- mergeReasoningBlock ----------------------------------------------------

// The role wins on every key it names and a plugin key it is silent about
// survives, so a role that only wants a deeper effort keeps the plugin's mode.
func TestMergeReasoningBlock_RoleWinsPerKey(t *testing.T) {
	plugin := map[string]any{"mode": "effort", "effort": "low", "summary": "concise"}
	role := map[string]any{"effort": "high"}

	merged := mergeReasoningBlock(plugin, role)

	if merged["effort"] != "high" {
		t.Errorf("effort = %v, want the role's high", merged["effort"])
	}
	if merged["mode"] != "effort" || merged["summary"] != "concise" {
		t.Errorf("merged = %#v, want the plugin's mode and summary to survive", merged)
	}
	if plugin["effort"] != "low" {
		t.Errorf("plugin block was mutated: %#v", plugin)
	}
}

// A nil role block leaves the plugin block exactly as it was; nil over nil
// stays nil, because "said nothing" must not become "set an empty block".
func TestMergeReasoningBlock_NilRole(t *testing.T) {
	plugin := map[string]any{"effort": "medium"}
	if got := mergeReasoningBlock(plugin, nil); got["effort"] != "medium" {
		t.Errorf("merged = %#v, want the plugin block unchanged", got)
	}
	if mergeReasoningBlock(nil, nil) != nil {
		t.Error("nil over nil should stay nil")
	}
}

// A set-but-empty role block is a statement, not a gap: it overrides no key,
// but the merged block is non-nil and goes through the parser — so on a
// deployment with no plugin block at all it still declares a reasoning mode.
func TestParseMergedReasoning_EmptyRoleBlockIsAStatement(t *testing.T) {
	rc, err := parseMergedReasoning(nil, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("parseMergedReasoning: %v", err)
	}
	if rc.Mode != reasoningModeEffort {
		t.Errorf("mode = %q, want %q — an empty role block still declares one", rc.Mode, reasoningModeEffort)
	}
}

// applyReasoning's gate reads Mode and treats the empty string as off, so every
// path out of the merge has to produce one.
func TestParseMergedReasoning_AlwaysSetsAMode(t *testing.T) {
	cases := []struct {
		name         string
		plugin, role map[string]any
		want         reasoningMode
	}{
		{"nothing at all", nil, nil, reasoningModeOff},
		{"plugin only", map[string]any{"effort": "low"}, nil, reasoningModeEffort},
		{"role only", nil, map[string]any{"effort": "low"}, reasoningModeEffort},
		{"role turns it off", map[string]any{"mode": "effort"}, map[string]any{"mode": "off"}, reasoningModeOff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc, err := parseMergedReasoning(tc.plugin, tc.role, nil)
			if err != nil {
				t.Fatalf("parseMergedReasoning: %v", err)
			}
			if rc.Mode != tc.want {
				t.Errorf("mode = %q, want %q", rc.Mode, tc.want)
			}
		})
	}
}

// Only an effort the role itself named is marked as the role's; one inherited
// from the plugin block stays the least specific layer.
func TestParseMergedReasoning_EffortSource(t *testing.T) {
	rc, err := parseMergedReasoning(map[string]any{"mode": "effort", "effort": "low"}, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("parseMergedReasoning: %v", err)
	}
	if rc.EffortSource != effortFromPlugin {
		t.Errorf("inherited effort: source = %v, want effortFromPlugin", rc.EffortSource)
	}

	rc, err = parseMergedReasoning(map[string]any{"mode": "effort", "effort": "low"}, map[string]any{"effort": "high"}, nil)
	if err != nil {
		t.Fatalf("parseMergedReasoning: %v", err)
	}
	if rc.EffortSource != effortFromRole {
		t.Errorf("role-named effort: source = %v, want effortFromRole", rc.EffortSource)
	}
}

// --- effortFor --------------------------------------------------------------

// The unified precedence, in one table: a role's own `reasoning.effort` beats
// its `effort:`, which beats the plugin-level key.
func TestEffortFor_Precedence(t *testing.T) {
	cases := []struct {
		name   string
		rc     reasoningConfig
		effort string
		want   string
	}{
		{
			name: "plugin only",
			rc:   reasoningConfig{Mode: reasoningModeEffort, Effort: "low"},
			want: "low",
		},
		{
			name:   "role effort beats the plugin key",
			rc:     reasoningConfig{Mode: reasoningModeEffort, Effort: "low"},
			effort: "high",
			want:   "high",
		},
		{
			name:   "a role-named block effort beats the role's effort",
			rc:     reasoningConfig{Mode: reasoningModeEffort, Effort: "max", EffortSource: effortFromRole},
			effort: "high",
			want:   "max",
		},
		{
			name:   "an unrecognised effort falls through rather than reaching the wire",
			rc:     reasoningConfig{Mode: reasoningModeEffort, Effort: "low"},
			effort: "thorough",
			want:   "low",
		},
		{
			name: "nothing anywhere means no key",
			rc:   reasoningConfig{Mode: reasoningModeEffort},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effortFor(tc.rc, tc.effort); got != tc.want {
				t.Errorf("effortFor = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- the wire ---------------------------------------------------------------

// The headline: a plain single-entry role's `effort:` reaches the wire, with no
// coordinator involved.
func TestRoleReasoning_SingleEntryRoleEffortReachesTheWire(t *testing.T) {
	p := reasoningPlugin(t, reasoningModels(map[string]map[string]any{
		"balanced": {},
		"deep":     {"effort": "high"},
	}), map[string]any{"mode": "effort"})

	got, ok := wireEffort(t, p, events.LLMRequest{Role: "deep"})
	if !ok || got != "high" {
		t.Fatalf("reasoning_effort = %q (present=%v), want the role's high", got, ok)
	}
}

// Every word in the Anthropic ∪ Gemini union — plus OpenAI's own two extras —
// passes through unchanged. No clamp, no warning: that is the whole reason this
// provider needed neither.
func TestRoleReasoning_WholeVocabularyPassesThroughUnclamped(t *testing.T) {
	for _, effort := range reasoningEfforts {
		t.Run(effort, func(t *testing.T) {
			p := reasoningPlugin(t, reasoningModels(map[string]map[string]any{
				"balanced": {"effort": effort},
			}), map[string]any{"mode": "effort"})

			got, ok := wireEffort(t, p, events.LLMRequest{Role: "balanced"})
			if !ok || got != effort {
				t.Fatalf("reasoning_effort = %q (present=%v), want %q verbatim", got, ok, effort)
			}
		})
	}
}

// `mode` is the declaration, and nothing else is. A role's `effort:` on a
// deployment where no layer declares a reasoning mode sends nothing at all:
// `reasoning_effort` on a model that is not a reasoning model is an HTTP 400,
// and this provider holds no model-capability table to tell the difference.
func TestRoleReasoning_BareEffortWithNoDeclaredModeSendsNothing(t *testing.T) {
	p := reasoningPlugin(t, reasoningModels(map[string]map[string]any{
		"balanced": {"effort": "high"},
	}), nil)

	if got, ok := wireEffort(t, p, events.LLMRequest{Role: "balanced"}); ok {
		t.Fatalf("reasoning_effort = %q on the wire, want no key with no reasoning mode declared anywhere", got)
	}
}

// The role's own block is the cheapest way to declare that mode, and it does so
// on a deployment whose plugin block is absent entirely.
func TestRoleReasoning_RoleBlockDeclaresTheModeOnItsOwn(t *testing.T) {
	p := reasoningPlugin(t, reasoningModels(map[string]map[string]any{
		"balanced": {},
		"deep":     {"effort": "xhigh", "reasoning": map[string]any{"mode": "effort"}},
	}), nil)

	got, ok := wireEffort(t, p, events.LLMRequest{Role: "deep"})
	if !ok || got != "xhigh" {
		t.Fatalf("reasoning_effort = %q (present=%v), want the role's xhigh", got, ok)
	}
}

// A role's `reasoning:` block merges over the plugin's key by key: the role's
// effort wins, the plugin's mode survives.
func TestRoleReasoning_RoleBlockMergesOverThePluginBlock(t *testing.T) {
	p := reasoningPlugin(t, reasoningModels(map[string]map[string]any{
		"balanced": {},
		"deep":     {"reasoning": map[string]any{"effort": "max"}},
	}), map[string]any{"mode": "effort", "effort": "low"})

	got, ok := wireEffort(t, p, events.LLMRequest{Role: "deep"})
	if !ok || got != "max" {
		t.Fatalf("reasoning_effort = %q (present=%v), want the role's max", got, ok)
	}

	// The role that named nothing still gets the plugin's low.
	got, ok = wireEffort(t, p, events.LLMRequest{Role: "balanced"})
	if !ok || got != "low" {
		t.Fatalf("reasoning_effort = %q (present=%v), want the plugin's low", got, ok)
	}
}

// A role's own block effort outranks that same role's `effort:` — the more
// specific statement, in this provider's own vocabulary.
func TestRoleReasoning_RoleBlockEffortBeatsRoleEffort(t *testing.T) {
	p := reasoningPlugin(t, reasoningModels(map[string]map[string]any{
		"balanced": {"effort": "low", "reasoning": map[string]any{"effort": "max"}},
	}), map[string]any{"mode": "effort"})

	got, ok := wireEffort(t, p, events.LLMRequest{Role: "balanced"})
	if !ok || got != "max" {
		t.Fatalf("reasoning_effort = %q (present=%v), want the block's max", got, ok)
	}
}

// A role can turn reasoning off outright, whatever the plugin declared.
func TestRoleReasoning_RoleBlockCanTurnItOff(t *testing.T) {
	p := reasoningPlugin(t, reasoningModels(map[string]map[string]any{
		"balanced": {},
		"plain":    {"effort": "high", "reasoning": map[string]any{"mode": "off"}},
	}), map[string]any{"mode": "effort", "effort": "medium"})

	if got, ok := wireEffort(t, p, events.LLMRequest{Role: "plain"}); ok {
		t.Fatalf("reasoning_effort = %q on the wire, want nothing under the role's mode: off", got)
	}
}

// An effort already on the request wins outright — that is how the fallback and
// fanout coordinators hand a non-first chain entry's depth to the provider
// serving it.
func TestRoleReasoning_RequestBeatsTheRole(t *testing.T) {
	p := reasoningPlugin(t, reasoningModels(map[string]map[string]any{
		"balanced": {"effort": "low"},
	}), map[string]any{"mode": "effort"})

	got, ok := wireEffort(t, p, events.LLMRequest{Role: "balanced", Effort: "max"})
	if !ok || got != "max" {
		t.Fatalf("reasoning_effort = %q (present=%v), want the request's max", got, ok)
	}
}

// Effort is a shared axis, so a named role that sets none falls through to the
// default role's entry — the same rule max_tokens and temperature follow.
func TestRoleReasoning_EffortFallsThroughToTheDefaultRole(t *testing.T) {
	p := reasoningPlugin(t, reasoningModels(map[string]map[string]any{
		"balanced": {"effort": "medium"},
		"plain":    {},
	}), map[string]any{"mode": "effort"})

	got, ok := wireEffort(t, p, events.LLMRequest{Role: "plain"})
	if !ok || got != "medium" {
		t.Fatalf("reasoning_effort = %q (present=%v), want the default role's medium", got, ok)
	}
}

// The `reasoning:` block is provider-native and does NOT fall through: a named
// role owns it outright, so the default role's block never reaches a role that
// was never given one.
func TestRoleReasoning_BlockDoesNotFallThroughToTheDefaultRole(t *testing.T) {
	p := reasoningPlugin(t, reasoningModels(map[string]map[string]any{
		"balanced": {"reasoning": map[string]any{"mode": "effort", "effort": "max"}},
		"plain":    {},
	}), nil)

	if got, ok := wireEffort(t, p, events.LLMRequest{Role: "plain"}); ok {
		t.Fatalf("reasoning_effort = %q on the wire, want the default role's block not to fall through", got)
	}
}

// A request that names no role at all does get the default role's block: that
// is what "the default role" means.
func TestRoleReasoning_NoRoleGetsTheDefaultRoleBlock(t *testing.T) {
	p := reasoningPlugin(t, reasoningModels(map[string]map[string]any{
		"balanced": {"reasoning": map[string]any{"mode": "effort", "effort": "max"}},
	}), nil)

	got, ok := wireEffort(t, p, events.LLMRequest{})
	if !ok || got != "max" {
		t.Fatalf("reasoning_effort = %q (present=%v), want the default role's max", got, ok)
	}
}

// A stamped request is final: the coordinator knows which chain entry is being
// served, and the registry would answer with the role's first entry.
func TestRoleReasoning_StampedEntryWinsOverTheRole(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": []any{
			map[string]any{"provider": pluginID, "model": "gpt-5", "effort": "low"},
			map[string]any{"provider": pluginID, "model": "gpt-5-mini", "effort": "max",
				"reasoning": map[string]any{"mode": "effort"}},
		},
	})
	p := reasoningPlugin(t, models, nil)

	second, ok := models.Fallback("balanced", 1)
	if !ok {
		t.Fatal("expected a second chain entry")
	}
	req := events.LLMRequest{Role: "balanced", Model: "gpt-5-mini"}
	engine.StampModelConfig(&req, second)

	got, present := wireEffort(t, p, req)
	if !present || got != "max" {
		t.Fatalf("reasoning_effort = %q (present=%v), want the stamped entry's max", got, present)
	}
}

// The late-recovery path: a router rewrote `model` and left `role` alone, so
// handleRequest's model-resolution branches never run. The role's depth must
// still be recovered.
func TestRoleReasoning_RecoveredAfterModelRewrite(t *testing.T) {
	p := reasoningPlugin(t, reasoningModels(map[string]map[string]any{
		"balanced": {},
		"deep":     {"effort": "xhigh"},
	}), map[string]any{"mode": "effort"})

	got, ok := wireEffort(t, p, events.LLMRequest{Role: "deep", Model: "gpt-5-pro"})
	if !ok || got != "xhigh" {
		t.Fatalf("reasoning_effort = %q (present=%v), want the role's xhigh", got, ok)
	}
}

// A role that declares a reasoning mode also gets the sampling parameters such
// a model rejects stripped — the same behaviour the plugin-level block has.
func TestRoleReasoning_RoleDeclaredModeStripsTemperature(t *testing.T) {
	p := reasoningPlugin(t, reasoningModels(map[string]map[string]any{
		"balanced": {"temperature": 0.2, "reasoning": map[string]any{"mode": "effort", "effort": "low"}},
	}), nil)

	req := events.LLMRequest{Role: "balanced"}
	p.applyEntryOverrides(&req)
	body := p.buildRequestBody("gpt-5", 1024, req)
	if temp, ok := body["temperature"]; ok {
		t.Fatalf("temperature = %v on the wire, want it stripped under the role's declared mode", temp)
	}
}

// A hand-set unparseable block degrades to the plugin-level configuration
// rather than putting a shape OpenAI will 400 on onto the wire. Init makes this
// unreachable through config; the guard is for everything else.
func TestRoleReasoning_InvalidRequestBlockDegradesToThePlugin(t *testing.T) {
	p := reasoningPlugin(t, nil, map[string]any{"mode": "effort", "effort": "low"})

	rc := p.resolveReasoning(events.LLMRequest{
		Role:      "balanced",
		Overrides: events.ModelOverrides{Reasoning: map[string]any{"mode": "thorough"}},
	})
	if rc.Mode != reasoningModeEffort || rc.Effort != "low" {
		t.Fatalf("resolved = %#v, want the plugin-level configuration", rc)
	}
}

// --- validateRoleReasoning --------------------------------------------------

// An invalid merged block fails the boot and names the role.
func TestValidateRoleReasoning_InvalidMergedBlockNamesTheRole(t *testing.T) {
	models := reasoningModels(map[string]map[string]any{
		"balanced": {},
		"broken":   {"reasoning": map[string]any{"mode": "thorough"}},
	})

	err := validateRoleReasoning(models, nil, reasoningConfig{Mode: reasoningModeOff}, silentLogger())
	if err == nil {
		t.Fatal("expected an error for an invalid merged block")
	}
	if !strings.Contains(err.Error(), `role "broken"`) {
		t.Errorf("error = %v, want it to name the role", err)
	}
}

// An unrecognised `effort:` on a role whose depth would actually reach the wire
// is a typo, and fails the boot naming the role. There is no clamp on this
// provider, so there is nothing else it could be.
func TestValidateRoleReasoning_UnknownEffortNamesTheRole(t *testing.T) {
	models := reasoningModels(map[string]map[string]any{
		"balanced": {},
		"broken":   {"effort": "thorough"},
	})

	err := validateRoleReasoning(models, map[string]any{"mode": "effort"},
		reasoningConfig{Mode: reasoningModeEffort}, silentLogger())
	if err == nil {
		t.Fatal("expected an error for an unrecognised effort")
	}
	if !strings.Contains(err.Error(), `role "broken"`) || !strings.Contains(err.Error(), "thorough") {
		t.Errorf("error = %v, want it to name the role and the value", err)
	}
}

// Where the effort cannot reach the wire its vocabulary is not this provider's
// business — the ordinary shape of a chain whose other entries are served by
// somebody else.
func TestValidateRoleReasoning_UnknownEffortIsInertWithNoMode(t *testing.T) {
	models := reasoningModels(map[string]map[string]any{
		"balanced": {"effort": "thorough"},
	})

	if err := validateRoleReasoning(models, nil, reasoningConfig{Mode: reasoningModeOff}, silentLogger()); err != nil {
		t.Fatalf("validateRoleReasoning: %v, want an inert effort to be ignored", err)
	}
}

// An effort the role's own block outranks is likewise never read, so it is
// never judged.
func TestValidateRoleReasoning_UnknownEffortBehindARoleBlockEffort(t *testing.T) {
	models := reasoningModels(map[string]map[string]any{
		"balanced": {"effort": "thorough", "reasoning": map[string]any{"mode": "effort", "effort": "high"}},
	})

	if err := validateRoleReasoning(models, nil, reasoningConfig{Mode: reasoningModeOff}, silentLogger()); err != nil {
		t.Fatalf("validateRoleReasoning: %v, want the shadowed effort to be ignored", err)
	}
}

// Every word in the union is accepted by the sweep — the point of having no
// clamp.
func TestValidateRoleReasoning_WholeVocabularyAccepted(t *testing.T) {
	entries := map[string]map[string]any{"balanced": {}}
	for _, effort := range reasoningEfforts {
		entries["role_"+effort] = map[string]any{"effort": effort}
	}
	models := reasoningModels(entries)

	if err := validateRoleReasoning(models, map[string]any{"mode": "effort"},
		reasoningConfig{Mode: reasoningModeEffort}, silentLogger()); err != nil {
		t.Fatalf("validateRoleReasoning: %v, want every word accepted", err)
	}
}

// An entry naming another provider is that provider's business.
func TestValidateRoleReasoning_SkipsForeignEntries(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default":  "balanced",
		"balanced": map[string]any{"provider": pluginID, "model": "gpt-5"},
		"claude":   map[string]any{"provider": "nexus.llm.anthropic", "model": "claude", "reasoning": map[string]any{"mode": "thorough"}},
	})

	if err := validateRoleReasoning(models, nil, reasoningConfig{Mode: reasoningModeOff}, silentLogger()); err != nil {
		t.Fatalf("validateRoleReasoning: %v, want a foreign entry skipped", err)
	}
}

// A nil registry is the embedder who configured no roles at all.
func TestValidateRoleReasoning_NilRegistry(t *testing.T) {
	if err := validateRoleReasoning(nil, nil, reasoningConfig{Mode: reasoningModeOff}, silentLogger()); err != nil {
		t.Fatalf("validateRoleReasoning: %v, want nil with no registry", err)
	}
}
