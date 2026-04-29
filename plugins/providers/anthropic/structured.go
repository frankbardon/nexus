package anthropic

// structuredOutputsConfig controls how json_schema response formats map onto
// the Anthropic Messages API.
//
// Two modes are supported:
//
//   - "tool" (default): inject a synthetic `_structured_output` tool whose
//     input_schema mirrors the requested schema and force tool_choice on it.
//     This works on every Claude model that supports tool use, but conflicts
//     with the agent's real tools (tool_choice is overridden).
//   - "native": send the schema via the top-level `response_format` field
//     Anthropic added once `output_format` shipped. The model returns the JSON
//     value as a single text block; tool_choice is left untouched. Anthropic's
//     exact field naming and beta gating was in flux at the time of writing,
//     so this stays opt-in until operators confirm their target model accepts
//     it. The optional BetaHeader lets operators opt into the beta string when
//     Anthropic publishes one without us shipping a stale guess.
type structuredOutputsConfig struct {
	Mode       string // "tool" (default) | "native"
	BetaHeader string // optional; appended to anthropic-beta when Mode == "native"
}

// parseStructuredOutputsConfig reads the optional `structured_outputs:` block
// from the plugin config. Unknown or invalid `mode` values fall back to the
// safe "tool" default — operators have to explicitly opt into "native".
func parseStructuredOutputsConfig(cfg map[string]any) structuredOutputsConfig {
	out := structuredOutputsConfig{Mode: "tool"}
	raw, ok := cfg["structured_outputs"].(map[string]any)
	if !ok {
		return out
	}
	if v, ok := raw["mode"].(string); ok && (v == "tool" || v == "native") {
		out.Mode = v
	}
	if v, ok := raw["beta_header"].(string); ok {
		out.BetaHeader = v
	}
	return out
}
