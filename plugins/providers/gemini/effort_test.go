package gemini

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file covers the step between `core.models` and applyThinking: how a
// request's reasoning-depth hint is resolved. thinking_test.go already pins
// what applyThinking does with a hint once it has one; nothing there proves a
// plain single-entry role's `effort:` ever becomes that hint.
//
// It used not to. The provider read events.LLMRequest.Effort and nothing else,
// and only the fallback and fanout coordinators ever stamp that field — so an
// ordinary role's `effort:` was validated at Init and then silently dropped.

// effortModels builds a registry of single-entry roles, one per map key, each
// carrying the given effort (empty means the key is omitted entirely).
func effortModels(entries map[string]string) *engine.ModelRegistry {
	raw := map[string]any{"default": "balanced"}
	for role, effort := range entries {
		cfg := map[string]any{
			"provider":   pluginID,
			"model":      "gemini-3-flash",
			"max_tokens": 2048,
		}
		if effort != "" {
			cfg["effort"] = effort
		}
		raw[role] = cfg
	}
	return engine.NewModelRegistry(raw)
}

// effortPlugin builds the minimum Plugin buildRequestBody can run against: no
// bus, no network, just the collaborators it dereferences unconditionally.
func effortPlugin(tc thinkingConfig, models *engine.ModelRegistry) *Plugin {
	return &Plugin{
		logger:   quietLogger(),
		models:   models,
		auth:     &authState{},
		cache:    &cacheState{},
		thinking: tc,
	}
}

// wireThinkingLevel runs the same two steps handleRequest does — resolve the
// role, then build the body with the resolved effort — and reports what
// generationConfig.thinkingConfig.thinkingLevel ended up as. A false second
// return means no thinkingLevel was emitted at all.
func wireThinkingLevel(t *testing.T, p *Plugin, req events.LLMRequest) (string, bool) {
	t.Helper()

	target := p.resolveTarget(req)
	if target.skip {
		t.Fatalf("resolveTarget skipped the request for role %q", req.Role)
	}
	body, err := p.buildRequestBody(target.model, target.maxTokens, target.effort, req)
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	gen, ok := body["generationConfig"].(map[string]any)
	if !ok {
		return "", false
	}
	tc, ok := gen["thinkingConfig"].(map[string]any)
	if !ok {
		return "", false
	}
	level, ok := tc["thinkingLevel"].(string)
	return level, ok
}

func userMessages() []events.Message {
	return []events.Message{{Role: "user", Content: "hi"}}
}

// TestEffort_SingleEntryRoleReachesTheWire is the defect this file exists for.
// Nothing stamps req.Effort for a plain single-entry role, so the registry has
// to be consulted or the role's `effort:` never leaves the process.
func TestEffort_SingleEntryRoleReachesTheWire(t *testing.T) {
	p := effortPlugin(thinkingConfig{Mode: thinkingModeLevel}, effortModels(map[string]string{
		"balanced": "",
		"worker":   "low",
	}))

	got, ok := wireThinkingLevel(t, p, events.LLMRequest{
		Role:     "worker",
		Messages: userMessages(),
	})
	if !ok || got != "low" {
		t.Fatalf("thinkingLevel = %q (present=%v), want %q", got, ok, "low")
	}
}

// TestEffort_RequestBeatsRegistry pins the precedence that keeps E3-S5's fix in
// place: a fallback entry or a non-first fanout leg stamps the chain entry it
// is actually serving onto the request, whereas a Resolve() here always reads
// chain[0]. The registry is the last resort, never an override.
func TestEffort_RequestBeatsRegistry(t *testing.T) {
	p := effortPlugin(thinkingConfig{Mode: thinkingModeLevel}, effortModels(map[string]string{
		"balanced": "",
		"worker":   "minimal",
	}))

	got, ok := wireThinkingLevel(t, p, events.LLMRequest{
		Role:     "worker",
		Effort:   "high",
		Messages: userMessages(),
	})
	if !ok || got != "high" {
		t.Fatalf("thinkingLevel = %q (present=%v), want the request's %q", got, ok, "high")
	}
}

// TestEffort_DefaultRoleFallback covers the default-role branch beside the
// max_tokens resolution: a request naming no role at all resolves through
// `core.models`' default.
func TestEffort_DefaultRoleFallback(t *testing.T) {
	p := effortPlugin(thinkingConfig{Mode: thinkingModeLevel}, effortModels(map[string]string{
		"balanced": "medium",
	}))

	got, ok := wireThinkingLevel(t, p, events.LLMRequest{Messages: userMessages()})
	if !ok || got != "medium" {
		t.Fatalf("thinkingLevel = %q (present=%v), want %q", got, ok, "medium")
	}
}

// TestEffort_RegistryValueIsClamped verifies the xhigh/max → high clamp applies
// to a registry-sourced value exactly as it does to a stamped one. Anthropic's
// top two levels have no Gemini analogue, and a shared fanout role must stay
// configurable.
func TestEffort_RegistryValueIsClamped(t *testing.T) {
	for _, effort := range []string{"xhigh", "max"} {
		p := effortPlugin(thinkingConfig{Mode: thinkingModeLevel}, effortModels(map[string]string{
			"balanced": "",
			"worker":   effort,
		}))

		got, ok := wireThinkingLevel(t, p, events.LLMRequest{
			Role:     "worker",
			Messages: userMessages(),
		})
		if !ok || got != "high" {
			t.Fatalf("effort %q: thinkingLevel = %q (present=%v), want %q", effort, got, ok, "high")
		}
	}
}

// TestEffort_PluginLevelBeatsRegistryEffort pins that this story did not touch
// the Gemini precedence inversion: `thinking.level` is Gemini's own vocabulary
// and still wins over a role's translated `effort:`.
func TestEffort_PluginLevelBeatsRegistryEffort(t *testing.T) {
	p := effortPlugin(thinkingConfig{Mode: thinkingModeLevel, Level: "minimal"}, effortModels(map[string]string{
		"balanced": "",
		"worker":   "high",
	}))

	got, ok := wireThinkingLevel(t, p, events.LLMRequest{
		Role:     "worker",
		Messages: userMessages(),
	})
	if !ok || got != "minimal" {
		t.Fatalf("thinkingLevel = %q (present=%v), want the plugin-level %q", got, ok, "minimal")
	}
}

// TestEffort_RegistryEffortIgnoredUnderBudgetMode verifies role effort stays a
// silent no-op under mode: budget now that it is actually delivered.
func TestEffort_RegistryEffortIgnoredUnderBudgetMode(t *testing.T) {
	p := effortPlugin(thinkingConfig{Mode: thinkingModeBudget, BudgetTokens: 4096}, effortModels(map[string]string{
		"balanced": "",
		"worker":   "high",
	}))

	req := events.LLMRequest{Role: "worker", Messages: userMessages()}
	if got, ok := wireThinkingLevel(t, p, req); ok {
		t.Fatalf("thinkingLevel = %q, want none under mode: budget", got)
	}

	target := p.resolveTarget(req)
	body, err := p.buildRequestBody(target.model, target.maxTokens, target.effort, req)
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	gen := body["generationConfig"].(map[string]any)
	tc := gen["thinkingConfig"].(map[string]any)
	if tc["thinkingBudget"] != 4096 {
		t.Fatalf("expected the budget to survive untouched, got %#v", tc)
	}
}

// TestEffort_RegistryEffortIgnoredUnderOffMode verifies mode: off still emits
// no thinkingConfig whatever the role says.
func TestEffort_RegistryEffortIgnoredUnderOffMode(t *testing.T) {
	p := effortPlugin(thinkingConfig{Mode: thinkingModeOff}, effortModels(map[string]string{
		"balanced": "",
		"worker":   "high",
	}))

	req := events.LLMRequest{Role: "worker", Messages: userMessages()}
	target := p.resolveTarget(req)
	body, err := p.buildRequestBody(target.model, target.maxTokens, target.effort, req)
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	gen, ok := body["generationConfig"].(map[string]any)
	if ok {
		if _, present := gen["thinkingConfig"]; present {
			t.Fatalf("mode off must emit no thinkingConfig, got %#v", gen)
		}
	}
}

// TestEffort_NoEffortAnywhereEmitsNoLevel verifies an existing deployment's
// request is unchanged: with no effort configured and no plugin-level level,
// no thinkingLevel goes out and the model's own default stands.
func TestEffort_NoEffortAnywhereEmitsNoLevel(t *testing.T) {
	p := effortPlugin(thinkingConfig{Mode: thinkingModeLevel}, effortModels(map[string]string{
		"balanced": "",
		"worker":   "",
	}))

	if got, ok := wireThinkingLevel(t, p, events.LLMRequest{
		Role:     "worker",
		Messages: userMessages(),
	}); ok {
		t.Fatalf("thinkingLevel = %q, want none emitted", got)
	}
}

// --- resolveTarget ladder ---------------------------------------------------

// TestResolveTarget_LateRecoveryAfterModelRewrite is the router case the
// max_tokens recovery branches exist for: something rewrote req.Model to a
// concrete id without touching the rest, so the role-resolution branches are
// skipped entirely. The role's effort must still be found.
func TestResolveTarget_LateRecoveryAfterModelRewrite(t *testing.T) {
	p := effortPlugin(thinkingConfig{Mode: thinkingModeLevel}, effortModels(map[string]string{
		"balanced": "",
		"worker":   "high",
	}))

	target := p.resolveTarget(events.LLMRequest{
		Role:  "worker",
		Model: "gemini-3-flash-rewritten-by-a-router",
	})
	if target.effort != "high" {
		t.Fatalf("effort = %q, want %q after a model rewrite skipped the role branch", target.effort, "high")
	}
	if target.model != "gemini-3-flash-rewritten-by-a-router" {
		t.Fatalf("model = %q, want the rewritten id untouched", target.model)
	}
}

// TestResolveTarget_LateRecoveryFromDefaultRole covers the last recovery
// branch: an explicit model plus an unknown role, so only the default role is
// left to answer.
func TestResolveTarget_LateRecoveryFromDefaultRole(t *testing.T) {
	p := effortPlugin(thinkingConfig{Mode: thinkingModeLevel}, effortModels(map[string]string{
		"balanced": "medium",
	}))

	target := p.resolveTarget(events.LLMRequest{
		Role:  "nosuchrole",
		Model: "gemini-3-flash",
	})
	if target.effort != "medium" {
		t.Fatalf("effort = %q, want %q from the default role", target.effort, "medium")
	}
}

// TestResolveTarget_ForeignProviderRoleSkips pins that the skip flag preserves
// the early return the inline resolution used to do: a role naming another
// provider is not this plugin's request, whatever effort it carries.
func TestResolveTarget_ForeignProviderRoleSkips(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default":  "balanced",
		"balanced": map[string]any{"provider": pluginID, "model": "gemini-3-flash"},
		"worker":   map[string]any{"provider": "nexus.llm.anthropic", "model": "claude-opus-4-5", "effort": "max"},
	})
	p := effortPlugin(thinkingConfig{Mode: thinkingModeLevel}, models)

	target := p.resolveTarget(events.LLMRequest{Role: "worker"})
	if !target.skip {
		t.Fatalf("expected a foreign-provider role to skip, got %+v", target)
	}
}

// TestResolveTarget_NilRegistry verifies the ladder is a no-op without a
// registry rather than a panic — a request that carries its own model and
// effort must still resolve.
func TestResolveTarget_NilRegistry(t *testing.T) {
	p := effortPlugin(thinkingConfig{Mode: thinkingModeLevel}, nil)

	target := p.resolveTarget(events.LLMRequest{
		Model:  "gemini-3-flash",
		Effort: "low",
	})
	if target.skip {
		t.Fatal("a nil registry must not skip the request")
	}
	if target.effort != "low" {
		t.Fatalf("effort = %q, want the request's %q", target.effort, "low")
	}
	if target.maxTokens != defaultMaxTokens {
		t.Fatalf("maxTokens = %d, want the %d floor", target.maxTokens, defaultMaxTokens)
	}
}
