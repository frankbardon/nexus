package gemini

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file covers the step between a `core.models` entry's `temperature:` and
// generationConfig.temperature. buildRequestBody has always copied
// req.Temperature onto the wire; nothing proved a plain single-entry role's
// `temperature:` ever became that value.
//
// It did not. Only an agent posture and the approval_policy gate ever set
// LLMRequest.Temperature, plus the fallback/fanout stamp for a coordinated
// entry — so an ordinary role's `temperature:` parsed, validated, travelled and
// was read by nobody.

// tempModels builds a registry of single-entry roles, one per map key. A nil
// value omits the `temperature` key entirely, which is how a role that never
// sets one is configured.
func tempModels(entries map[string]*float64) *engine.ModelRegistry {
	raw := map[string]any{"default": "balanced"}
	for role, temp := range entries {
		cfg := map[string]any{
			"provider":   pluginID,
			"model":      "gemini-3-flash",
			"max_tokens": 2048,
		}
		if temp != nil {
			cfg["temperature"] = *temp
		}
		raw[role] = cfg
	}
	return engine.NewModelRegistry(raw)
}

func tempPtr(f float64) *float64 { return &f }

// tempPlugin builds the minimum Plugin buildRequestBody runs against.
func tempPlugin(models *engine.ModelRegistry) *Plugin {
	return &Plugin{
		logger:   quietLogger(),
		models:   models,
		auth:     &authState{},
		cache:    &cacheState{},
		thinking: thinkingConfig{Mode: thinkingModeOff},
	}
}

// wireTemperature runs the same two steps handleRequest does — resolve the
// entry onto the request, then build the body — and reports what
// generationConfig.temperature ended up as. A false second return means no
// temperature key was emitted.
//
// It calls the very method handleRequest calls, so a regression that unwires
// applyEntryOverrides from the body builder cannot hide behind the helper.
func wireTemperature(t *testing.T, p *Plugin, req events.LLMRequest) (float64, bool) {
	t.Helper()

	target := p.resolveTarget(req)
	if target.skip {
		t.Fatalf("resolveTarget skipped the request for role %q", req.Role)
	}
	p.applyEntryOverrides(&req)

	body, err := p.buildRequestBody(target.model, target.maxTokens, target.effort, req)
	if err != nil {
		t.Fatalf("buildRequestBody: unexpected error: %v", err)
	}
	gen, ok := body["generationConfig"].(map[string]any)
	if !ok {
		return 0, false
	}
	temp, ok := gen["temperature"].(float64)
	return temp, ok
}

// The headline: a plain single-entry role's temperature reaches the wire.
func TestRoleTemperature_SingleEntryRoleReachesTheWire(t *testing.T) {
	p := tempPlugin(tempModels(map[string]*float64{
		"balanced": nil,
		"creative": tempPtr(1),
		"quick":    tempPtr(0.2),
	}))

	got, ok := wireTemperature(t, p, events.LLMRequest{Role: "creative"})
	if !ok || got != 1 {
		t.Fatalf("creative: temperature = %v (present=%v), want 1", got, ok)
	}
	got, ok = wireTemperature(t, p, events.LLMRequest{Role: "quick"})
	if !ok || got != 0.2 {
		t.Fatalf("quick: temperature = %v (present=%v), want 0.2", got, ok)
	}
}

// 0 is a real value, not "unset" — the whole reason the axis is a *float64.
func TestRoleTemperature_ZeroIsAValue(t *testing.T) {
	p := tempPlugin(tempModels(map[string]*float64{"balanced": tempPtr(0)}))

	got, ok := wireTemperature(t, p, events.LLMRequest{Role: "balanced"})
	if !ok {
		t.Fatal("temperature: no key on the wire, want an explicit 0")
	}
	if got != 0 {
		t.Fatalf("temperature = %v, want 0", got)
	}
}

// Unset on the role leaves the request — and so the body — untouched.
func TestRoleTemperature_UnsetLeavesTheRequestAlone(t *testing.T) {
	p := tempPlugin(tempModels(map[string]*float64{"balanced": nil}))

	if got, ok := wireTemperature(t, p, events.LLMRequest{Role: "balanced"}); ok {
		t.Fatalf("temperature = %v on the wire, want no key at all", got)
	}
}

// A value already on the request wins. This inverts what "role config" usually
// implies and is deliberate: an agent posture (pkg/delegate) and the
// approval_policy gate both set LLMRequest.Temperature, and a per-turn decision
// must outrank the deployment-wide role.
func TestRoleTemperature_RequestBeatsTheRole(t *testing.T) {
	p := tempPlugin(tempModels(map[string]*float64{"balanced": tempPtr(1)}))

	got, ok := wireTemperature(t, p, events.LLMRequest{Role: "balanced", Temperature: tempPtr(0.3)})
	if !ok || got != 0.3 {
		t.Fatalf("temperature = %v (present=%v), want the request's 0.3", got, ok)
	}
}

// The posture-wins rule has to survive a posture asking for 0, which is exactly
// the value a naive "is it set?" check would misread as a gap.
func TestRoleTemperature_RequestZeroBeatsTheRole(t *testing.T) {
	p := tempPlugin(tempModels(map[string]*float64{"balanced": tempPtr(1)}))

	got, ok := wireTemperature(t, p, events.LLMRequest{Role: "balanced", Temperature: tempPtr(0)})
	if !ok || got != 0 {
		t.Fatalf("temperature = %v (present=%v), want the request's explicit 0", got, ok)
	}
}

// Temperature is a shared axis, so a named role that sets none falls through to
// the default role's entry — the same rule max_tokens and effort follow.
func TestRoleTemperature_FallsThroughToTheDefaultRole(t *testing.T) {
	p := tempPlugin(tempModels(map[string]*float64{
		"balanced": tempPtr(0.4),
		"plain":    nil,
	}))

	got, ok := wireTemperature(t, p, events.LLMRequest{Role: "plain"})
	if !ok || got != 0.4 {
		t.Fatalf("temperature = %v (present=%v), want the default role's 0.4", got, ok)
	}
}

// A stamped request is final: the coordinator knows which chain entry is being
// served, and the registry would answer with the role's first entry.
func TestRoleTemperature_StampedEntryWinsOverTheRole(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": []any{
			map[string]any{"provider": pluginID, "model": "gemini-3-pro", "temperature": 1},
			map[string]any{"provider": pluginID, "model": "gemini-3-flash", "temperature": 0.1},
		},
	})
	p := tempPlugin(models)

	second, ok := models.Fallback("balanced", 1)
	if !ok {
		t.Fatal("expected a second chain entry")
	}
	req := events.LLMRequest{Role: "balanced", Model: "gemini-3-flash"}
	engine.StampModelConfig(&req, second)

	p.applyEntryOverrides(&req)
	if req.Temperature == nil || *req.Temperature != 0.1 {
		t.Fatalf("temperature = %v, want the stamped entry's 0.1", req.Temperature)
	}
}

// The late-recovery path: a router rewrote `model` and left `role` alone, so
// resolveTarget's model branches never run. The role's temperature must still
// be recovered.
func TestRoleTemperature_RecoveredAfterModelRewrite(t *testing.T) {
	p := tempPlugin(tempModels(map[string]*float64{
		"balanced": nil,
		"quick":    tempPtr(0.2),
	}))

	got, ok := wireTemperature(t, p, events.LLMRequest{Role: "quick", Model: "gemini-3-pro"})
	if !ok || got != 0.2 {
		t.Fatalf("temperature = %v (present=%v), want the role's 0.2", got, ok)
	}
}

// A nil registry is the embedder who configured no roles at all.
func TestRoleTemperature_NilRegistry(t *testing.T) {
	p := tempPlugin(nil)

	req := events.LLMRequest{Role: "balanced"}
	p.applyEntryOverrides(&req)
	if req.Temperature != nil {
		t.Fatalf("temperature = %v, want nil with no registry", *req.Temperature)
	}
}
