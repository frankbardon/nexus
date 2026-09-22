package engine

import (
	"errors"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

func f64(v float64) *float64 { return &v }

// overridesRegistry has a role whose first entry carries a full set of
// per-entry configuration and a second entry that carries a different one, so a
// test can tell "the entry being served" apart from "the role's first entry".
func overridesRegistry() *ModelRegistry {
	return NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": []any{
			map[string]any{
				"provider":    "nexus.llm.anthropic",
				"model":       "claude-opus-4-7",
				"max_tokens":  8192,
				"effort":      "high",
				"api":         "messages",
				"temperature": 0,
				"thinking": map[string]any{
					"mode":          "budget",
					"budget_tokens": 8192,
				},
				"cache": map[string]any{"enabled": true},
			},
			map[string]any{
				"provider": "nexus.llm.gemini",
				"model":    "gemini-2.5-pro",
				"effort":   "minimal",
				"thinking": map[string]any{"mode": "level", "level": "low"},
			},
		},
		"quick": map[string]any{
			"provider": "nexus.llm.anthropic",
			"model":    "claude-haiku-4-5",
			"thinking": map[string]any{"mode": "off"},
		},
	})
}

func TestStampModelConfig_FillsEveryAxis(t *testing.T) {
	cfg, ok := overridesRegistry().Fallback("balanced", 0)
	if !ok {
		t.Fatal("expected the primary entry to resolve")
	}

	req := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Role: "balanced"}
	StampModelConfig(&req, cfg)

	if !req.Overrides.Stamped {
		t.Fatal("expected the request to be marked stamped")
	}
	if req.MaxTokens != 8192 {
		t.Fatalf("max_tokens = %d, want 8192", req.MaxTokens)
	}
	if req.Effort != "high" {
		t.Fatalf("effort = %q, want %q", req.Effort, "high")
	}
	if req.Temperature == nil || *req.Temperature != 0 {
		t.Fatalf("temperature = %v, want an explicit 0", req.Temperature)
	}
	if req.Overrides.API != "messages" {
		t.Fatalf("api = %q, want %q", req.Overrides.API, "messages")
	}
	if got := req.Overrides.Thinking["mode"]; got != "budget" {
		t.Fatalf("thinking.mode = %v, want %q", got, "budget")
	}
	if got := req.Overrides.Cache["enabled"]; got != true {
		t.Fatalf("cache.enabled = %v, want true", got)
	}
	if req.Overrides.Reasoning != nil || req.Overrides.Retry != nil {
		t.Fatalf("unset blocks must stay nil; reasoning=%v retry=%v",
			req.Overrides.Reasoning, req.Overrides.Retry)
	}
}

func TestStampModelConfig_RequestWins(t *testing.T) {
	cfg, _ := overridesRegistry().Fallback("balanced", 0)

	req := events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Role:          "balanced",
		MaxTokens:     100,
		Effort:        "caller-set",
		Temperature:   f64(0.7),
		Overrides: events.ModelOverrides{
			API:      "caller-api",
			Thinking: map[string]any{"mode": "caller"},
		},
	}
	StampModelConfig(&req, cfg)

	if req.MaxTokens != 100 {
		t.Fatalf("max_tokens = %d, want the request's 100", req.MaxTokens)
	}
	if req.Effort != "caller-set" {
		t.Fatalf("effort = %q, want the request's value", req.Effort)
	}
	if *req.Temperature != 0.7 {
		t.Fatalf("temperature = %v, want the request's 0.7", *req.Temperature)
	}
	if req.Overrides.API != "caller-api" {
		t.Fatalf("api = %q, want the request's value", req.Overrides.API)
	}
	if got := req.Overrides.Thinking["mode"]; got != "caller" {
		t.Fatalf("thinking.mode = %v, want the request's block untouched", got)
	}
	// An axis the request left alone is still filled.
	if got := req.Overrides.Cache["enabled"]; got != true {
		t.Fatalf("cache.enabled = %v, want the entry to fill the gap", got)
	}
}

// An explicitly empty block is a statement, not a silence: it must block the
// fill rather than read as "unset".
func TestStampModelConfig_EmptyBlockIsNotAGap(t *testing.T) {
	cfg, _ := overridesRegistry().Fallback("balanced", 0)

	req := events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Overrides:     events.ModelOverrides{Thinking: map[string]any{}},
	}
	StampModelConfig(&req, cfg)

	if len(req.Overrides.Thinking) != 0 {
		t.Fatalf("thinking = %v, want the request's empty block preserved", req.Overrides.Thinking)
	}
}

// A stamped request must never alias the registry's map: it travels the bus and
// outlives the call, and every request resolving the same entry shares that map.
func TestStampModelConfig_DoesNotAliasRegistry(t *testing.T) {
	models := overridesRegistry()
	cfg, _ := models.Fallback("balanced", 0)

	req := events.LLMRequest{SchemaVersion: events.LLMRequestVersion}
	StampModelConfig(&req, cfg)

	req.Overrides.Thinking["mode"] = "mutated"
	req.Overrides.Cache["enabled"] = false
	*req.Temperature = 1

	fresh, _ := models.Fallback("balanced", 0)
	if got := fresh.Thinking["mode"]; got != "budget" {
		t.Fatalf("registry thinking.mode = %v, want the stamp not to have reached it", got)
	}
	if got := fresh.Cache["enabled"]; got != true {
		t.Fatalf("registry cache.enabled = %v, want the stamp not to have reached it", got)
	}
	if *fresh.Temperature != 0 {
		t.Fatalf("registry temperature = %v, want the stamp not to have reached it", *fresh.Temperature)
	}
}

// Path 1 of the three no-coordinator paths: the role the request names.
func TestResolveModelConfig_NamedRole(t *testing.T) {
	req := ResolveModelConfig(overridesRegistry(), events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Role:          "quick",
	})

	if got := req.Overrides.Thinking["mode"]; got != "off" {
		t.Fatalf("thinking.mode = %v, want the named role's %q", got, "off")
	}
	if req.Overrides.Stamped {
		t.Fatal("resolution must not claim a coordinator stamped the request")
	}
}

// Path 2: no role named at all, so the default role's entry applies.
func TestResolveModelConfig_DefaultRole(t *testing.T) {
	req := ResolveModelConfig(overridesRegistry(), events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
	})

	if got := req.Overrides.Thinking["mode"]; got != "budget" {
		t.Fatalf("thinking.mode = %v, want the default role's %q", got, "budget")
	}
	if req.Effort != "high" {
		t.Fatalf("effort = %q, want the default role's %q", req.Effort, "high")
	}
	if req.Overrides.API != "messages" {
		t.Fatalf("api = %q, want the default role's %q", req.Overrides.API, "messages")
	}
}

// Path 3: a router rewrote Model to a concrete id and left the rest alone. The
// model-resolution branches a provider would take are skipped, so the role's
// configuration has to be recovered anyway.
func TestResolveModelConfig_RecoversAfterRouterRewroteModel(t *testing.T) {
	req := ResolveModelConfig(overridesRegistry(), events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Role:          "quick",
		Model:         "claude-haiku-4-5-20251001",
	})

	if got := req.Overrides.Thinking["mode"]; got != "off" {
		t.Fatalf("thinking.mode = %v, want the role's block recovered despite the rewritten model", got)
	}
	if req.Model != "claude-haiku-4-5-20251001" {
		t.Fatalf("model = %q, want the router's rewrite left alone", req.Model)
	}
}

// A coordinator has already applied the entry actually being served. Consulting
// the registry again would answer with the chain's first entry, which is exactly
// the silent wrongness this carrier exists to prevent.
func TestResolveModelConfig_StampedRequestIsLeftAlone(t *testing.T) {
	models := overridesRegistry()
	second, ok := models.Fallback("balanced", 1)
	if !ok {
		t.Fatal("expected a second chain entry")
	}

	req := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Role: "balanced"}
	StampModelConfig(&req, second)
	got := ResolveModelConfig(models, req)

	if lvl := got.Overrides.Thinking["level"]; lvl != "low" {
		t.Fatalf("thinking.level = %v, want the served entry's block, not the chain's first", lvl)
	}
	if _, leaked := got.Overrides.Thinking["budget_tokens"]; leaked {
		t.Fatal("the chain's first entry leaked into the served entry's block")
	}
	if got.Effort != "minimal" {
		t.Fatalf("effort = %q, want the served entry's %q", got.Effort, "minimal")
	}
	if got.Overrides.API != "" {
		t.Fatalf("api = %q, want empty — the served entry sets none", got.Overrides.API)
	}
	if got.Overrides.Cache != nil {
		t.Fatalf("cache = %v, want nil — the served entry sets none", got.Overrides.Cache)
	}
}

// A *shared* axis the named role leaves unset falls through to the default
// role, which is exactly how the Effort recovery this generalises already
// behaves. max_tokens and temperature ride along for the same reason: one
// vocabulary, understood by every provider.
func TestResolveModelConfig_NamedRoleFallsThroughToDefault(t *testing.T) {
	req := ResolveModelConfig(overridesRegistry(), events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Role:          "quick",
	})

	if req.Effort != "high" {
		t.Fatalf("effort = %q, want the default role's %q after the named role set none",
			req.Effort, "high")
	}
	if req.MaxTokens != 8192 {
		t.Fatalf("max_tokens = %d, want the default role's 8192 after the named role set none",
			req.MaxTokens)
	}
	if req.Temperature == nil || *req.Temperature != 0 {
		t.Fatalf("temperature = %v, want the default role's explicit 0", req.Temperature)
	}
}

// crossProviderRegistry is the failure this split exists to stop: a default
// role served by Anthropic carrying a `thinking:` block, and a named role served
// by Gemini carrying none of its own.
func crossProviderRegistry() *ModelRegistry {
	return NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": map[string]any{
			"provider":   "nexus.llm.anthropic",
			"model":      "claude-opus-4-7",
			"max_tokens": 8192,
			"effort":     "high",
			"api":        "messages",
			"thinking":   map[string]any{"mode": "adaptive"},
			"cache":      map[string]any{"enabled": true},
			"retry":      map[string]any{"max_attempts": 4},
			"reasoning":  map[string]any{"effort": "high"},
		},
		"quick": map[string]any{
			"provider": "nexus.llm.gemini",
			"model":    "gemini-3-flash",
		},
	})
}

// A named role must never inherit the default role's provider-native blocks:
// they are written in one provider's vocabulary and the named role may well be
// served by another. The shared axes still fall through.
func TestResolveModelConfig_NativeBlocksDoNotBleedFromDefaultRole(t *testing.T) {
	req := ResolveModelConfig(crossProviderRegistry(), events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Role:          "quick",
	})

	if req.Overrides.Thinking != nil {
		t.Fatalf("thinking = %v, want nil — the Gemini role was never given an Anthropic-shaped block",
			req.Overrides.Thinking)
	}
	if req.Overrides.Cache != nil {
		t.Fatalf("cache = %v, want nil", req.Overrides.Cache)
	}
	if req.Overrides.Retry != nil {
		t.Fatalf("retry = %v, want nil", req.Overrides.Retry)
	}
	if req.Overrides.Reasoning != nil {
		t.Fatalf("reasoning = %v, want nil", req.Overrides.Reasoning)
	}
	if req.Overrides.API != "" {
		t.Fatalf("api = %q, want empty — %q is Anthropic's surface name", req.Overrides.API, "messages")
	}

	// The shared axes are unaffected: one vocabulary, every provider reads it.
	if req.Effort != "high" {
		t.Fatalf("effort = %q, want the default role's %q", req.Effort, "high")
	}
	if req.MaxTokens != 8192 {
		t.Fatalf("max_tokens = %d, want the default role's 8192", req.MaxTokens)
	}
}

// "The default role" means the entry that answers a request naming no role at
// all — and there its native blocks apply in full.
func TestResolveModelConfig_NativeBlocksApplyWhenNoRoleNamed(t *testing.T) {
	req := ResolveModelConfig(crossProviderRegistry(), events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
	})

	if got := req.Overrides.Thinking["mode"]; got != "adaptive" {
		t.Fatalf("thinking.mode = %v, want the default role's %q", got, "adaptive")
	}
	if req.Overrides.API != "messages" {
		t.Fatalf("api = %q, want the default role's %q", req.Overrides.API, "messages")
	}
}

func TestResolveModelConfig_NilRegistryAndUnknownRole(t *testing.T) {
	in := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Role: "quick"}
	if got := ResolveModelConfig(nil, in); got.Overrides.Thinking != nil {
		t.Fatalf("thinking = %v, want a nil registry to change nothing", got.Overrides.Thinking)
	}

	got := ResolveModelConfig(overridesRegistry(), events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Role:          "no-such-role",
	})
	// Falls through to the default role rather than inventing anything.
	if got.Effort != "high" {
		t.Fatalf("effort = %q, want the default role's %q", got.Effort, "high")
	}
}

func TestStampModelConfig_NilRequestIsANoOp(t *testing.T) {
	cfg, _ := overridesRegistry().Fallback("balanced", 0)
	StampModelConfig(nil, cfg) // must not panic
}

// --- WalkRoleEntries --------------------------------------------------------

// The sweep every provider's Init-time validator is built from: sorted roles,
// the whole chain rather than just the primary, and an entry naming another
// provider left to that provider.
func TestWalkRoleEntries_SortedWholeChainAndProviderSkip(t *testing.T) {
	models := NewModelRegistry(map[string]any{
		"default": "balanced",
		"zebra":   map[string]any{"provider": "mine", "model": "z"},
		"balanced": []any{
			map[string]any{"provider": "mine", "model": "b0"},
			map[string]any{"provider": "theirs", "model": "b1"},
			map[string]any{"model": "b2"}, // names no provider: could be anyone's
		},
	})

	var seen []string
	err := WalkRoleEntries(models, "mine", func(role string, cfg ModelConfig) error {
		seen = append(seen, role+"/"+cfg.Model)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkRoleEntries: unexpected error: %v", err)
	}

	want := []string{"balanced/b0", "balanced/b2", "zebra/z"}
	if len(seen) != len(want) {
		t.Fatalf("visited %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("visited %v, want %v", seen, want)
		}
	}
}

// The first error stops the walk, so a config with several broken roles fails
// on the same one every boot.
func TestWalkRoleEntries_StopsAtTheFirstError(t *testing.T) {
	models := NewModelRegistry(map[string]any{
		"default": "alpha",
		"alpha":   map[string]any{"provider": "mine", "model": "a"},
		"beta":    map[string]any{"provider": "mine", "model": "b"},
	})

	var visited int
	err := WalkRoleEntries(models, "mine", func(role string, _ ModelConfig) error {
		visited++
		return errors.New("broken " + role)
	})
	if err == nil || err.Error() != "broken alpha" {
		t.Fatalf("err = %v, want the alphabetically first role's error", err)
	}
	if visited != 1 {
		t.Fatalf("visited %d entries, want the walk to stop at the first error", visited)
	}
}

// A nil registry is the embedder who configured no roles at all, and a nil
// callback is a caller with nothing to do — neither is an error.
func TestWalkRoleEntries_NilInputs(t *testing.T) {
	if err := WalkRoleEntries(nil, "mine", func(string, ModelConfig) error { return errors.New("never") }); err != nil {
		t.Fatalf("nil registry: %v", err)
	}
	if err := WalkRoleEntries(NewModelRegistry(nil), "mine", nil); err != nil {
		t.Fatalf("nil callback: %v", err)
	}
}
