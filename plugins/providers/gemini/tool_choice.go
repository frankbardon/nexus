package gemini

import "github.com/frankbardon/nexus/pkg/events"

// applyToolFilter returns a filtered copy of tools based on the filter config.
// Include takes precedence over Exclude. Nil filter returns tools unchanged.
func applyToolFilter(tools []events.ToolDef, filter *events.ToolFilter) []events.ToolDef {
	if filter == nil {
		return tools
	}

	if len(filter.Include) > 0 {
		allowed := make(map[string]bool, len(filter.Include))
		for _, name := range filter.Include {
			allowed[name] = true
		}
		var out []events.ToolDef
		for _, t := range tools {
			if allowed[t.Name] {
				out = append(out, t)
			}
		}
		return out
	}

	if len(filter.Exclude) > 0 {
		blocked := make(map[string]bool, len(filter.Exclude))
		for _, name := range filter.Exclude {
			blocked[name] = true
		}
		var out []events.ToolDef
		for _, t := range tools {
			if !blocked[t.Name] {
				out = append(out, t)
			}
		}
		return out
	}

	return tools
}

// resolveToolChoice maps an events.ToolChoice to the Gemini API
// tool_config.function_calling_config block. Returns nil when no tool_config
// should be sent (default AUTO behavior).
func resolveToolChoice(tc *events.ToolChoice, tools []events.ToolDef) map[string]any {
	if tc == nil {
		return nil
	}

	switch tc.Mode {
	case "auto":
		return map[string]any{
			"function_calling_config": map[string]any{"mode": "AUTO"},
		}
	case "required":
		return map[string]any{
			"function_calling_config": map[string]any{"mode": "ANY"},
		}
	case "none":
		// Gemini supports NONE natively; tools are still sent but unusable.
		return map[string]any{
			"function_calling_config": map[string]any{"mode": "NONE"},
		}
	case "tool":
		if tc.Name == "" {
			return map[string]any{
				"function_calling_config": map[string]any{"mode": "ANY"},
			}
		}
		// Validate the named tool exists in the filtered set.
		found := false
		for _, t := range tools {
			if t.Name == tc.Name {
				found = true
				break
			}
		}
		if !found {
			return map[string]any{
				"function_calling_config": map[string]any{"mode": "ANY"},
			}
		}
		return map[string]any{
			"function_calling_config": map[string]any{
				"mode":                   "ANY",
				"allowed_function_names": []string{tc.Name},
			},
		}
	default:
		return nil
	}
}
