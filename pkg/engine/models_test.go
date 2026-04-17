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
