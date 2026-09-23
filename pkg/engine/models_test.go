package engine

import (
	"testing"
)

func TestNewModelRegistry_SingleMap(t *testing.T) {
	raw := map[string]any{
		"default": "balanced",
		"balanced": map[string]any{
			"provider":   "nexus.llm.anthropic",
			"model":      "claude-sonnet-4-20250514",
			"max_tokens": 8192,
		},
	}

	r := NewModelRegistry(raw)

	cfg, ok := r.Resolve("balanced")
	if !ok {
		t.Fatal("expected balanced role to exist")
	}
	if cfg.Provider != "nexus.llm.anthropic" {
		t.Fatalf("expected provider nexus.llm.anthropic, got %s", cfg.Provider)
	}
	if cfg.Model != "claude-sonnet-4-20250514" {
		t.Fatalf("expected model claude-sonnet-4-20250514, got %s", cfg.Model)
	}
	if cfg.MaxTokens != 8192 {
		t.Fatalf("expected max_tokens 8192, got %d", cfg.MaxTokens)
	}
}

func TestNewModelRegistry_FallbackChain(t *testing.T) {
	raw := map[string]any{
		"default": "balanced",
		"balanced": []any{
			map[string]any{
				"provider":   "nexus.llm.anthropic",
				"model":      "claude-sonnet-4-20250514",
				"max_tokens": 8192,
			},
			map[string]any{
				"provider":   "nexus.llm.openai",
				"model":      "gpt-4o",
				"max_tokens": 8192,
			},
		},
		"quick": map[string]any{
			"provider": "nexus.llm.anthropic",
			"model":    "claude-haiku-4-5-20251001",
		},
	}

	r := NewModelRegistry(raw)

	// Resolve returns primary (index 0).
	cfg, ok := r.Resolve("balanced")
	if !ok {
		t.Fatal("expected balanced role to exist")
	}
	if cfg.Provider != "nexus.llm.anthropic" {
		t.Fatalf("expected primary provider nexus.llm.anthropic, got %s", cfg.Provider)
	}

	// Fallback(0) == Resolve primary.
	cfg0, ok := r.Fallback("balanced", 0)
	if !ok || cfg0.Provider != "nexus.llm.anthropic" {
		t.Fatalf("expected Fallback(0) = primary, got %+v", cfg0)
	}

	// Fallback(1) = OpenAI.
	cfg1, ok := r.Fallback("balanced", 1)
	if !ok {
		t.Fatal("expected fallback at index 1")
	}
	if cfg1.Provider != "nexus.llm.openai" || cfg1.Model != "gpt-4o" {
		t.Fatalf("expected openai/gpt-4o, got %s/%s", cfg1.Provider, cfg1.Model)
	}

	// Fallback(2) = exhausted.
	_, ok = r.Fallback("balanced", 2)
	if ok {
		t.Fatal("expected chain exhausted at index 2")
	}

	// ChainLen.
	if r.ChainLen("balanced") != 2 {
		t.Fatalf("expected chain length 2, got %d", r.ChainLen("balanced"))
	}

	// Single-entry role has chain of 1.
	if r.ChainLen("quick") != 1 {
		t.Fatalf("expected chain length 1 for quick, got %d", r.ChainLen("quick"))
	}
}

func TestNewModelRegistry_NilConfig(t *testing.T) {
	r := NewModelRegistry(nil)

	cfg := r.Default()
	if cfg.Model != "" {
		t.Fatalf("expected empty default model, got %s", cfg.Model)
	}
	if r.ChainLen("balanced") != 0 {
		t.Fatalf("expected chain length 0, got %d", r.ChainLen("balanced"))
	}
}

func TestModelRegistry_Fallback_NonexistentRole(t *testing.T) {
	r := NewModelRegistry(map[string]any{
		"balanced": map[string]any{
			"provider": "nexus.llm.anthropic",
			"model":    "claude-sonnet-4-20250514",
		},
	})

	_, ok := r.Fallback("nonexistent", 0)
	if ok {
		t.Fatal("expected false for nonexistent role")
	}
}

func TestModelRegistry_Fallback_NegativeIndex(t *testing.T) {
	r := NewModelRegistry(map[string]any{
		"balanced": []any{
			map[string]any{
				"provider": "nexus.llm.anthropic",
				"model":    "claude-sonnet-4-20250514",
			},
		},
	})

	_, ok := r.Fallback("balanced", -1)
	if ok {
		t.Fatal("expected false for negative index")
	}
}

func TestModelRegistry_BackwardCompatRawModel(t *testing.T) {
	r := NewModelRegistry(map[string]any{})

	cfg, ok := r.Resolve("claude-sonnet-4-20250514")
	if !ok {
		t.Fatal("expected raw model ID to resolve")
	}
	if cfg.Model != "claude-sonnet-4-20250514" {
		t.Fatalf("expected model passthrough, got %s", cfg.Model)
	}

	// Raw model IDs don't have fallback chains.
	if r.ChainLen("claude-sonnet-4-20250514") != 0 {
		t.Fatal("expected no chain for raw model ID")
	}
}

func TestModelRegistry_FloatMaxTokens(t *testing.T) {
	// YAML parsers sometimes decode integers as float64.
	r := NewModelRegistry(map[string]any{
		"balanced": map[string]any{
			"provider":   "nexus.llm.anthropic",
			"model":      "claude-sonnet-4-20250514",
			"max_tokens": float64(8192),
		},
	})

	cfg, _ := r.Resolve("balanced")
	if cfg.MaxTokens != 8192 {
		t.Fatalf("expected 8192 from float64, got %d", cfg.MaxTokens)
	}
}

func TestModelRegistry_EmptyChain(t *testing.T) {
	// Empty array should not create a role.
	r := NewModelRegistry(map[string]any{
		"balanced": []any{},
	})

	_, ok := r.Resolve("balanced")
	if ok {
		t.Fatal("expected empty chain to not create a role")
	}
}

func TestModelRegistry_FanoutRole(t *testing.T) {
	r := NewModelRegistry(map[string]any{
		"compare": map[string]any{
			"fanout": true,
			"providers": []any{
				map[string]any{
					"provider":   "nexus.llm.anthropic",
					"model":      "claude-sonnet-4-20250514",
					"max_tokens": 4096,
				},
				map[string]any{
					"provider":   "nexus.llm.openai",
					"model":      "gpt-4o",
					"max_tokens": 4096,
				},
			},
		},
		"balanced": map[string]any{
			"provider": "nexus.llm.anthropic",
			"model":    "claude-sonnet-4-20250514",
		},
	})

	// Fanout role detected.
	if !r.IsFanout("compare") {
		t.Fatal("expected compare to be fanout role")
	}

	// Non-fanout role.
	if r.IsFanout("balanced") {
		t.Fatal("expected balanced to not be fanout role")
	}

	// FanoutProviders returns all providers.
	providers := r.FanoutProviders("compare")
	if len(providers) != 2 {
		t.Fatalf("expected 2 fanout providers, got %d", len(providers))
	}
	if providers[0].Provider != "nexus.llm.anthropic" {
		t.Fatalf("expected first provider nexus.llm.anthropic, got %s", providers[0].Provider)
	}
	if providers[1].Provider != "nexus.llm.openai" {
		t.Fatalf("expected second provider nexus.llm.openai, got %s", providers[1].Provider)
	}

	// FanoutProviders returns nil for non-fanout roles.
	if r.FanoutProviders("balanced") != nil {
		t.Fatal("expected nil for non-fanout role")
	}

	// Resolve still works (returns first provider).
	cfg, ok := r.Resolve("compare")
	if !ok {
		t.Fatal("expected compare role to resolve")
	}
	if cfg.Provider != "nexus.llm.anthropic" {
		t.Fatalf("expected primary provider nexus.llm.anthropic, got %s", cfg.Provider)
	}
}

func TestModelRegistry_FanoutEmptyProviders(t *testing.T) {
	r := NewModelRegistry(map[string]any{
		"compare": map[string]any{
			"fanout":    true,
			"providers": []any{},
		},
	})

	if r.IsFanout("compare") {
		t.Fatal("expected empty fanout to not register")
	}
}

func TestModelRegistry_Effort(t *testing.T) {
	raw := map[string]any{
		"default": "reasoning",
		"reasoning": map[string]any{
			"provider": "nexus.llm.anthropic",
			"model":    "claude-opus-4-7",
			"effort":   "xhigh",
		},
	}

	r := NewModelRegistry(raw)

	cfg, ok := r.Resolve("reasoning")
	if !ok {
		t.Fatal("expected reasoning role to exist")
	}
	if cfg.Effort != "xhigh" {
		t.Fatalf("expected effort xhigh, got %q", cfg.Effort)
	}
}

func TestModelRegistry_EffortAbsent(t *testing.T) {
	raw := map[string]any{
		"balanced": map[string]any{
			"provider":   "nexus.llm.anthropic",
			"model":      "claude-sonnet-4-6",
			"max_tokens": 8192,
		},
	}

	r := NewModelRegistry(raw)

	cfg, ok := r.Resolve("balanced")
	if !ok {
		t.Fatal("expected balanced role to exist")
	}
	if cfg.Effort != "" {
		t.Fatalf("expected empty effort when the key is absent, got %q", cfg.Effort)
	}
}

// Core must not validate the effort string: the vocabularies differ per
// provider, so an unrecognised value parses fine and reaches the provider
// untouched.
func TestModelRegistry_EffortUnrecognisedValuePassesThrough(t *testing.T) {
	raw := map[string]any{
		"weird": map[string]any{
			"provider": "nexus.llm.anthropic",
			"model":    "claude-sonnet-4-6",
			"effort":   "ludicrous",
		},
	}

	r := NewModelRegistry(raw)

	cfg, ok := r.Resolve("weird")
	if !ok {
		t.Fatal("expected weird role to exist")
	}
	if cfg.Effort != "ludicrous" {
		t.Fatalf("expected effort passed through verbatim, got %q", cfg.Effort)
	}
}

func TestModelRegistry_EffortNonStringIgnored(t *testing.T) {
	raw := map[string]any{
		"odd": map[string]any{
			"provider": "nexus.llm.anthropic",
			"model":    "claude-sonnet-4-6",
			"effort":   42,
		},
	}

	r := NewModelRegistry(raw)

	cfg, ok := r.Resolve("odd")
	if !ok {
		t.Fatal("expected odd role to exist")
	}
	if cfg.Effort != "" {
		t.Fatalf("expected non-string effort to be ignored, got %q", cfg.Effort)
	}
}

func TestModelRegistry_EffortInFallbackChain(t *testing.T) {
	raw := map[string]any{
		"balanced": []any{
			map[string]any{
				"provider": "nexus.llm.anthropic",
				"model":    "claude-sonnet-4-6",
				"effort":   "high",
			},
			map[string]any{
				"provider": "nexus.llm.gemini",
				"model":    "gemini-2.5-pro",
				"effort":   "minimal",
			},
		},
	}

	r := NewModelRegistry(raw)

	primary, ok := r.Fallback("balanced", 0)
	if !ok || primary.Effort != "high" {
		t.Fatalf("expected primary effort high, got %+v", primary)
	}

	secondary, ok := r.Fallback("balanced", 1)
	if !ok || secondary.Effort != "minimal" {
		t.Fatalf("expected fallback effort minimal, got %+v", secondary)
	}
}

func TestModelRegistry_EffortOnFanoutRole(t *testing.T) {
	raw := map[string]any{
		"panel": map[string]any{
			"fanout": true,
			"providers": []any{
				map[string]any{
					"provider": "nexus.llm.anthropic",
					"model":    "claude-sonnet-4-6",
					"effort":   "medium",
				},
				map[string]any{
					"provider": "nexus.llm.gemini",
					"model":    "gemini-2.5-pro",
					"effort":   "low",
				},
				map[string]any{
					"provider": "nexus.llm.openai",
					"model":    "gpt-4o",
				},
			},
		},
	}

	r := NewModelRegistry(raw)

	providers := r.FanoutProviders("panel")
	if len(providers) != 3 {
		t.Fatalf("expected 3 fanout providers, got %d", len(providers))
	}
	if providers[0].Effort != "medium" {
		t.Fatalf("expected first fanout effort medium, got %q", providers[0].Effort)
	}
	if providers[1].Effort != "low" {
		t.Fatalf("expected second fanout effort low, got %q", providers[1].Effort)
	}
	if providers[2].Effort != "" {
		t.Fatalf("expected third fanout effort empty, got %q", providers[2].Effort)
	}
}

// --- Native provider blocks on a role entry (E1-S1) ---------------------------

func TestModelRegistry_NativeBlocks_SingleRole(t *testing.T) {
	raw := map[string]any{
		"reasoning": map[string]any{
			"provider":    "nexus.llm.anthropic",
			"model":       "claude-opus-4-7",
			"api":         "messages",
			"temperature": 0.7,
			"thinking": map[string]any{
				"mode":          "budget",
				"budget_tokens": 8192,
			},
			"reasoning": map[string]any{
				"effort": "high",
			},
			"cache": map[string]any{
				"enabled": true,
			},
			"retry": map[string]any{
				"max_attempts": 2,
			},
		},
	}

	r := NewModelRegistry(raw)

	cfg, ok := r.Resolve("reasoning")
	if !ok {
		t.Fatal("expected reasoning role to exist")
	}
	if cfg.API != "messages" {
		t.Fatalf("expected api messages, got %q", cfg.API)
	}
	if cfg.Temperature == nil || *cfg.Temperature != 0.7 {
		t.Fatalf("expected temperature 0.7, got %v", cfg.Temperature)
	}
	if cfg.Thinking["mode"] != "budget" {
		t.Fatalf("expected thinking.mode budget, got %v", cfg.Thinking["mode"])
	}
	if cfg.Thinking["budget_tokens"] != 8192 {
		t.Fatalf("expected thinking.budget_tokens 8192 (int), got %v", cfg.Thinking["budget_tokens"])
	}
	if cfg.Reasoning["effort"] != "high" {
		t.Fatalf("expected reasoning.effort high, got %v", cfg.Reasoning["effort"])
	}
	if cfg.Cache["enabled"] != true {
		t.Fatalf("expected cache.enabled true, got %v", cfg.Cache["enabled"])
	}
	if cfg.Retry["max_attempts"] != 2 {
		t.Fatalf("expected retry.max_attempts 2, got %v", cfg.Retry["max_attempts"])
	}
}

func TestModelRegistry_NativeBlocks_AbsentLeavesNil(t *testing.T) {
	r := NewModelRegistry(map[string]any{
		"balanced": map[string]any{
			"provider": "nexus.llm.anthropic",
			"model":    "claude-sonnet-4-6",
		},
	})

	cfg, ok := r.Resolve("balanced")
	if !ok {
		t.Fatal("expected balanced role to exist")
	}
	if cfg.Thinking != nil {
		t.Fatalf("expected nil thinking when the key is absent, got %v", cfg.Thinking)
	}
	if cfg.Reasoning != nil {
		t.Fatalf("expected nil reasoning when the key is absent, got %v", cfg.Reasoning)
	}
	if cfg.Cache != nil {
		t.Fatalf("expected nil cache when the key is absent, got %v", cfg.Cache)
	}
	if cfg.Retry != nil {
		t.Fatalf("expected nil retry when the key is absent, got %v", cfg.Retry)
	}
	if cfg.Temperature != nil {
		t.Fatalf("expected nil temperature when the key is absent, got %v", *cfg.Temperature)
	}
	if cfg.API != "" {
		t.Fatalf("expected empty api when the key is absent, got %q", cfg.API)
	}
}

// An explicitly empty block is "set, empty" — a later merge must be able to tell
// it apart from an absent one.
func TestModelRegistry_NativeBlocks_EmptyBlockIsNotNil(t *testing.T) {
	r := NewModelRegistry(map[string]any{
		"balanced": map[string]any{
			"provider": "nexus.llm.anthropic",
			"model":    "claude-sonnet-4-6",
			"thinking": map[string]any{},
		},
	})

	cfg, _ := r.Resolve("balanced")
	if cfg.Thinking == nil {
		t.Fatal("expected an explicitly empty thinking block to parse as non-nil")
	}
	if len(cfg.Thinking) != 0 {
		t.Fatalf("expected an empty thinking block, got %v", cfg.Thinking)
	}
}

// `thinking:` with no value at all decodes to a nil any, which means "not set".
func TestModelRegistry_NativeBlocks_NullBlockIsNil(t *testing.T) {
	r := NewModelRegistry(map[string]any{
		"balanced": map[string]any{
			"provider": "nexus.llm.anthropic",
			"model":    "claude-sonnet-4-6",
			"thinking": nil,
		},
	})

	cfg, _ := r.Resolve("balanced")
	if cfg.Thinking != nil {
		t.Fatalf("expected a null thinking block to parse as nil, got %v", cfg.Thinking)
	}
}

func TestModelRegistry_NativeBlocks_NonMapIgnored(t *testing.T) {
	r := NewModelRegistry(map[string]any{
		"balanced": map[string]any{
			"provider": "nexus.llm.anthropic",
			"model":    "claude-sonnet-4-6",
			"thinking": "yes",
			"retry":    42,
		},
	})

	cfg, _ := r.Resolve("balanced")
	if cfg.Thinking != nil {
		t.Fatalf("expected a scalar thinking value to be ignored, got %v", cfg.Thinking)
	}
	if cfg.Retry != nil {
		t.Fatalf("expected a scalar retry value to be ignored, got %v", cfg.Retry)
	}
}

// Core validates nothing inside a block: an inner key no provider has ever
// heard of parses and is forwarded untouched.
func TestModelRegistry_NativeBlocks_UnknownInnerKeyPassesThrough(t *testing.T) {
	r := NewModelRegistry(map[string]any{
		"weird": map[string]any{
			"provider": "nexus.llm.anthropic",
			"model":    "claude-sonnet-4-6",
			"thinking": map[string]any{
				"ludicrous_speed": true,
				"mode":            "not-a-mode",
			},
		},
	})

	cfg, _ := r.Resolve("weird")
	if cfg.Thinking["ludicrous_speed"] != true {
		t.Fatalf("expected unknown inner key forwarded, got %v", cfg.Thinking["ludicrous_speed"])
	}
	if cfg.Thinking["mode"] != "not-a-mode" {
		t.Fatalf("expected unvalidated inner value forwarded, got %v", cfg.Thinking["mode"])
	}
}

// A parsed block must not alias the loaded config map: writing to the entry's
// copy leaves the source config untouched.
func TestModelRegistry_NativeBlocks_BlockIsCopied(t *testing.T) {
	source := map[string]any{"mode": "budget"}
	r := NewModelRegistry(map[string]any{
		"balanced": map[string]any{
			"provider": "nexus.llm.anthropic",
			"model":    "claude-sonnet-4-6",
			"thinking": source,
		},
	})

	cfg, _ := r.Resolve("balanced")
	cfg.Thinking["mode"] = "off"
	if source["mode"] != "budget" {
		t.Fatalf("expected the source config untouched, got %v", source["mode"])
	}
}

func TestModelRegistry_Temperature_ZeroIsDistinctFromUnset(t *testing.T) {
	r := NewModelRegistry(map[string]any{
		"deterministic": map[string]any{
			"provider":    "nexus.llm.anthropic",
			"model":       "claude-sonnet-4-6",
			"temperature": 0,
		},
		"unset": map[string]any{
			"provider": "nexus.llm.anthropic",
			"model":    "claude-sonnet-4-6",
		},
	})

	zero, _ := r.Resolve("deterministic")
	if zero.Temperature == nil {
		t.Fatal("expected an explicit temperature: 0 to parse as set")
	}
	if *zero.Temperature != 0 {
		t.Fatalf("expected temperature 0, got %v", *zero.Temperature)
	}

	unset, _ := r.Resolve("unset")
	if unset.Temperature != nil {
		t.Fatalf("expected unset temperature to stay nil, got %v", *unset.Temperature)
	}
}

// YAML surfaces a whole number as int and a fractional one as float64 depending
// on the path; both must produce the same typed value.
func TestModelRegistry_Temperature_IntAndFloatDuality(t *testing.T) {
	cases := map[string]struct {
		raw  any
		want float64
	}{
		"int":        {raw: 1, want: 1},
		"float64":    {raw: float64(1), want: 1},
		"fractional": {raw: 0.25, want: 0.25},
		"int zero":   {raw: 0, want: 0},
		"float zero": {raw: float64(0), want: 0},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := NewModelRegistry(map[string]any{
				"balanced": map[string]any{
					"provider":    "nexus.llm.anthropic",
					"model":       "claude-sonnet-4-6",
					"temperature": tc.raw,
				},
			})
			cfg, _ := r.Resolve("balanced")
			if cfg.Temperature == nil {
				t.Fatalf("expected temperature set from %T", tc.raw)
			}
			if *cfg.Temperature != tc.want {
				t.Fatalf("expected temperature %v from %T, got %v", tc.want, tc.raw, *cfg.Temperature)
			}
		})
	}
}

func TestModelRegistry_Temperature_NonNumberIgnored(t *testing.T) {
	r := NewModelRegistry(map[string]any{
		"balanced": map[string]any{
			"provider":    "nexus.llm.anthropic",
			"model":       "claude-sonnet-4-6",
			"temperature": "hot",
		},
	})

	cfg, _ := r.Resolve("balanced")
	if cfg.Temperature != nil {
		t.Fatalf("expected a non-numeric temperature to be ignored, got %v", *cfg.Temperature)
	}
}

// A nested number inside an opaque block is forwarded verbatim in whichever Go
// type YAML produced — core neither normalises nor inspects it.
func TestModelRegistry_NativeBlocks_NestedNumberDuality(t *testing.T) {
	r := NewModelRegistry(map[string]any{
		"as_int": map[string]any{
			"provider": "nexus.llm.anthropic",
			"model":    "claude-sonnet-4-6",
			"thinking": map[string]any{"budget_tokens": 8192},
		},
		"as_float": map[string]any{
			"provider": "nexus.llm.anthropic",
			"model":    "claude-sonnet-4-6",
			"thinking": map[string]any{"budget_tokens": float64(8192)},
		},
	})

	asInt, _ := r.Resolve("as_int")
	if got, ok := asInt.Thinking["budget_tokens"].(int); !ok || got != 8192 {
		t.Fatalf("expected int 8192 forwarded verbatim, got %#v", asInt.Thinking["budget_tokens"])
	}

	asFloat, _ := r.Resolve("as_float")
	if got, ok := asFloat.Thinking["budget_tokens"].(float64); !ok || got != 8192 {
		t.Fatalf("expected float64 8192 forwarded verbatim, got %#v", asFloat.Thinking["budget_tokens"])
	}
}

// The same parse must apply to a fallback chain entry, not just a single role.
func TestModelRegistry_NativeBlocks_FallbackChainEntry(t *testing.T) {
	r := NewModelRegistry(map[string]any{
		"balanced": []any{
			map[string]any{
				"provider":    "nexus.llm.anthropic",
				"model":       "claude-sonnet-4-6",
				"api":         "messages",
				"temperature": 0.2,
				"thinking":    map[string]any{"mode": "adaptive"},
				"retry":       map[string]any{"max_attempts": 1},
			},
			map[string]any{
				"provider":  "nexus.llm.openai",
				"model":     "gpt-5",
				"api":       "responses",
				"reasoning": map[string]any{"effort": "medium"},
				"cache":     map[string]any{"enabled": false},
			},
		},
	})

	primary, ok := r.Fallback("balanced", 0)
	if !ok {
		t.Fatal("expected a primary chain entry")
	}
	if primary.API != "messages" {
		t.Fatalf("expected primary api messages, got %q", primary.API)
	}
	if primary.Temperature == nil || *primary.Temperature != 0.2 {
		t.Fatalf("expected primary temperature 0.2, got %v", primary.Temperature)
	}
	if primary.Thinking["mode"] != "adaptive" {
		t.Fatalf("expected primary thinking.mode adaptive, got %v", primary.Thinking["mode"])
	}
	if primary.Retry["max_attempts"] != 1 {
		t.Fatalf("expected primary retry.max_attempts 1, got %v", primary.Retry["max_attempts"])
	}
	if primary.Reasoning != nil {
		t.Fatalf("expected primary reasoning nil, got %v", primary.Reasoning)
	}

	secondary, ok := r.Fallback("balanced", 1)
	if !ok {
		t.Fatal("expected a fallback chain entry")
	}
	if secondary.API != "responses" {
		t.Fatalf("expected fallback api responses, got %q", secondary.API)
	}
	if secondary.Reasoning["effort"] != "medium" {
		t.Fatalf("expected fallback reasoning.effort medium, got %v", secondary.Reasoning["effort"])
	}
	if secondary.Cache["enabled"] != false {
		t.Fatalf("expected fallback cache.enabled false, got %v", secondary.Cache["enabled"])
	}
	if secondary.Temperature != nil {
		t.Fatalf("expected fallback temperature nil, got %v", *secondary.Temperature)
	}
	if secondary.Thinking != nil {
		t.Fatalf("expected fallback thinking nil, got %v", secondary.Thinking)
	}
}

// ... and to a fanout providers: leg.
func TestModelRegistry_NativeBlocks_FanoutLeg(t *testing.T) {
	r := NewModelRegistry(map[string]any{
		"panel": map[string]any{
			"fanout": true,
			"providers": []any{
				map[string]any{
					"provider":    "nexus.llm.anthropic",
					"model":       "claude-opus-4-7",
					"temperature": 1,
					"thinking":    map[string]any{"mode": "budget", "budget_tokens": 4096},
				},
				map[string]any{
					"provider":  "nexus.llm.gemini",
					"model":     "gemini-3.1-pro",
					"api":       "vertex",
					"thinking":  map[string]any{"mode": "level", "level": "high"},
					"cache":     map[string]any{"ttl": "5m"},
					"reasoning": map[string]any{},
				},
				map[string]any{
					"provider": "nexus.llm.openai",
					"model":    "gpt-5",
				},
			},
		},
	})

	legs := r.FanoutProviders("panel")
	if len(legs) != 3 {
		t.Fatalf("expected 3 fanout legs, got %d", len(legs))
	}

	if legs[0].Temperature == nil || *legs[0].Temperature != 1 {
		t.Fatalf("expected first leg temperature 1 from int, got %v", legs[0].Temperature)
	}
	if legs[0].Thinking["budget_tokens"] != 4096 {
		t.Fatalf("expected first leg thinking.budget_tokens 4096, got %v", legs[0].Thinking["budget_tokens"])
	}

	if legs[1].API != "vertex" {
		t.Fatalf("expected second leg api vertex, got %q", legs[1].API)
	}
	if legs[1].Thinking["level"] != "high" {
		t.Fatalf("expected second leg thinking.level high, got %v", legs[1].Thinking["level"])
	}
	if legs[1].Cache["ttl"] != "5m" {
		t.Fatalf("expected second leg cache.ttl 5m, got %v", legs[1].Cache["ttl"])
	}
	if legs[1].Reasoning == nil || len(legs[1].Reasoning) != 0 {
		t.Fatalf("expected second leg reasoning to be an empty non-nil block, got %v", legs[1].Reasoning)
	}

	if legs[2].Thinking != nil || legs[2].Reasoning != nil || legs[2].Cache != nil || legs[2].Retry != nil {
		t.Fatalf("expected third leg to carry no blocks, got %+v", legs[2])
	}
	if legs[2].API != "" {
		t.Fatalf("expected third leg api empty, got %q", legs[2].API)
	}
	if legs[2].Temperature != nil {
		t.Fatalf("expected third leg temperature nil, got %v", *legs[2].Temperature)
	}
}
