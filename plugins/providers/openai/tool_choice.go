package openai

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

// resolveToolChoice maps an events.ToolChoice to the OpenAI API tool_choice format.
// Returns nil when no tool_choice should be sent (default auto behavior).
func resolveToolChoice(tc *events.ToolChoice, tools []events.ToolDef) any {
	if tc == nil {
		return nil
	}

	switch tc.Mode {
	case "auto":
		return "auto"
	case "required":
		return "required"
	case "none":
		return "none"
	case "tool":
		if tc.Name == "" {
			return "required"
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
			return "required"
		}
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": tc.Name,
			},
		}
	default:
		return nil
	}
}

// resolveResponsesToolChoice is resolveToolChoice for the Responses API, where
// the named-tool form is flattened the same way the tool definition itself is:
// `{"type":"function","name":"x"}` rather than a nested `function` object. The
// three keywords ("auto", "required", "none") are spelled identically on both
// surfaces.
//
// The decision — including the fallbacks to "required" for a nameless or
// unknown tool — is deliberately not restated here: it is one policy, and this
// reshapes its result rather than re-deriving it.
func resolveResponsesToolChoice(tc *events.ToolChoice, tools []events.ToolDef) any {
	chosen := resolveToolChoice(tc, tools)
	nested, ok := chosen.(map[string]any)
	if !ok {
		return chosen
	}
	fn, _ := nested["function"].(map[string]any)
	name, _ := fn["name"].(string)
	return map[string]any{
		"type": "function",
		"name": name,
	}
}
