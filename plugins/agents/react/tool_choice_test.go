package react

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

func TestParseToolChoiceConfig_Empty(t *testing.T) {
	cfg := parseToolChoiceConfig(map[string]any{})
	if cfg.Default != nil {
		t.Error("expected nil default for empty config")
	}
	if len(cfg.Sequence) != 0 {
		t.Error("expected empty sequence for empty config")
	}
}

func TestParseToolChoiceConfig_Shorthand(t *testing.T) {
	cfg := parseToolChoiceConfig(map[string]any{
		"tool_choice": "required",
	})
	if cfg.Default == nil || cfg.Default.Mode != "required" {
		t.Errorf("expected default mode=required, got %+v", cfg.Default)
	}
}

func TestParseToolChoiceConfig_StaticDefault(t *testing.T) {
	cfg := parseToolChoiceConfig(map[string]any{
		"tool_choice": map[string]any{
			"mode": "none",
		},
	})
	if cfg.Default == nil || cfg.Default.Mode != "none" {
		t.Errorf("expected default mode=none, got %+v", cfg.Default)
	}
}

func TestParseToolChoiceConfig_ToolMode(t *testing.T) {
	cfg := parseToolChoiceConfig(map[string]any{
		"tool_choice": map[string]any{
			"mode": "tool",
			"name": "shell",
		},
	})
	if cfg.Default == nil || cfg.Default.Mode != "tool" || cfg.Default.Name != "shell" {
		t.Errorf("expected mode=tool name=shell, got %+v", cfg.Default)
	}
}

func TestParseToolChoiceConfig_Sequence(t *testing.T) {
	cfg := parseToolChoiceConfig(map[string]any{
		"tool_choice": map[string]any{
			"sequence": []any{
				map[string]any{"mode": "required"},
				map[string]any{"mode": "tool", "name": "shell"},
				"auto",
			},
		},
	})
	if len(cfg.Sequence) != 3 {
		t.Fatalf("expected 3 sequence entries, got %d", len(cfg.Sequence))
	}
	if cfg.Sequence[0].Mode != "required" {
		t.Errorf("seq[0] mode=%s, want required", cfg.Sequence[0].Mode)
	}
	if cfg.Sequence[1].Mode != "tool" || cfg.Sequence[1].Name != "shell" {
		t.Errorf("seq[1] = %+v, want mode=tool name=shell", cfg.Sequence[1])
	}
	if cfg.Sequence[2].Mode != "auto" {
		t.Errorf("seq[2] mode=%s, want auto", cfg.Sequence[2].Mode)
	}
}

func TestResolveToolChoice_NilDefault(t *testing.T) {
	cfg := toolChoiceConfig{}
	var override *toolChoiceOverride
	tc := resolveToolChoice(cfg, 0, &override)
	if tc != nil {
		t.Errorf("expected nil, got %+v", tc)
	}
}

func TestResolveToolChoice_StaticDefault(t *testing.T) {
	cfg := toolChoiceConfig{
		Default: &events.ToolChoice{Mode: "required"},
	}
	var override *toolChoiceOverride
	tc := resolveToolChoice(cfg, 0, &override)
	if tc == nil || tc.Mode != "required" {
		t.Errorf("expected mode=required, got %+v", tc)
	}
}

func TestResolveToolChoice_Sequence(t *testing.T) {
	cfg := toolChoiceConfig{
		Sequence: []events.ToolChoice{
			{Mode: "required"},
			{Mode: "tool", Name: "shell"},
			{Mode: "auto"},
		},
	}
	var override *toolChoiceOverride

	// Iteration 0 → first entry.
	tc := resolveToolChoice(cfg, 0, &override)
	if tc.Mode != "required" {
		t.Errorf("iter 0: mode=%s, want required", tc.Mode)
	}

	// Iteration 1 → second entry.
	tc = resolveToolChoice(cfg, 1, &override)
	if tc.Mode != "tool" || tc.Name != "shell" {
		t.Errorf("iter 1: %+v, want mode=tool name=shell", tc)
	}

	// Iteration 5 → last entry (sticky).
	tc = resolveToolChoice(cfg, 5, &override)
	if tc.Mode != "auto" {
		t.Errorf("iter 5: mode=%s, want auto", tc.Mode)
	}
}

func TestResolveToolChoice_OverrideOnce(t *testing.T) {
	cfg := toolChoiceConfig{
		Default: &events.ToolChoice{Mode: "auto"},
	}
	override := &toolChoiceOverride{
		Choice:   events.ToolChoice{Mode: "required"},
		Duration: "once",
	}

	// First call consumes the override.
	tc := resolveToolChoice(cfg, 0, &override)
	if tc.Mode != "required" {
		t.Errorf("first call: mode=%s, want required", tc.Mode)
	}
	if override != nil {
		t.Error("expected override to be consumed after once")
	}

	// Second call falls back to default.
	tc = resolveToolChoice(cfg, 1, &override)
	if tc.Mode != "auto" {
		t.Errorf("second call: mode=%s, want auto", tc.Mode)
	}
}

func TestResolveToolChoice_OverrideSticky(t *testing.T) {
	cfg := toolChoiceConfig{
		Default: &events.ToolChoice{Mode: "auto"},
	}
	override := &toolChoiceOverride{
		Choice:   events.ToolChoice{Mode: "none"},
		Duration: "sticky",
	}

	// Should persist across calls.
	for i := 0; i < 3; i++ {
		tc := resolveToolChoice(cfg, i, &override)
		if tc.Mode != "none" {
			t.Errorf("iter %d: mode=%s, want none", i, tc.Mode)
		}
	}
	if override == nil {
		t.Error("sticky override should not be consumed")
	}
}
