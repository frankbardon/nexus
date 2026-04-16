package react

import "github.com/frankbardon/nexus/pkg/events"

// toolChoiceConfig holds the parsed tool_choice configuration for the ReAct agent.
type toolChoiceConfig struct {
	// Default mode applied when no sequence or override is active.
	Default *events.ToolChoice

	// Sequence of tool choices applied per-iteration. The last entry repeats
	// for all subsequent iterations.
	Sequence []events.ToolChoice
}

// toolChoiceOverride is a dynamic override set by an agent.tool_choice event.
type toolChoiceOverride struct {
	Choice   events.ToolChoice
	Duration string // "once" | "sticky"
}

// parseToolChoiceConfig extracts tool_choice settings from plugin config.
func parseToolChoiceConfig(cfg map[string]any) toolChoiceConfig {
	raw, ok := cfg["tool_choice"]
	if !ok {
		return toolChoiceConfig{}
	}

	switch v := raw.(type) {
	case map[string]any:
		// Check for sequence first.
		if seqRaw, ok := v["sequence"]; ok {
			return toolChoiceConfig{
				Sequence: parseToolChoiceSequence(seqRaw),
			}
		}
		// Otherwise it's a single default.
		tc := parseOneToolChoice(v)
		if tc != nil {
			return toolChoiceConfig{Default: tc}
		}

	case string:
		// Shorthand: tool_choice: "required"
		return toolChoiceConfig{
			Default: &events.ToolChoice{Mode: v},
		}
	}

	return toolChoiceConfig{}
}

// parseToolChoiceSequence parses a sequence list from config.
func parseToolChoiceSequence(raw any) []events.ToolChoice {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}

	var seq []events.ToolChoice
	for _, item := range list {
		switch v := item.(type) {
		case map[string]any:
			if tc := parseOneToolChoice(v); tc != nil {
				seq = append(seq, *tc)
			}
		case string:
			seq = append(seq, events.ToolChoice{Mode: v})
		}
	}
	return seq
}

// parseOneToolChoice parses a single tool_choice map entry.
func parseOneToolChoice(m map[string]any) *events.ToolChoice {
	mode, _ := m["mode"].(string)
	if mode == "" {
		return nil
	}
	tc := &events.ToolChoice{Mode: mode}
	if name, ok := m["name"].(string); ok {
		tc.Name = name
	}
	return tc
}

// resolveToolChoice determines the effective ToolChoice for a given iteration,
// consuming any pending override.
func resolveToolChoice(cfg toolChoiceConfig, iteration int, override **toolChoiceOverride) *events.ToolChoice {
	// Dynamic override takes priority.
	if override != nil && *override != nil {
		ov := *override
		tc := ov.Choice
		if ov.Duration == "once" {
			*override = nil
		}
		return &tc
	}

	// Sequence: index into list, last entry is sticky.
	if len(cfg.Sequence) > 0 {
		idx := iteration
		if idx >= len(cfg.Sequence) {
			idx = len(cfg.Sequence) - 1
		}
		tc := cfg.Sequence[idx]
		return &tc
	}

	// Static default.
	return cfg.Default
}
