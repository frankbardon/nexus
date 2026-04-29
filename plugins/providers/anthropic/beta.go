package anthropic

import "strings"

// appendExtraTools appends entries from req.Metadata["_anthropic_extra_tools"]
// onto body["tools"]. Values are passed through verbatim — they're already in
// Anthropic's wire format (e.g., {"type":"bash_20250124","name":"bash"}).
//
// Accepts both []map[string]any (in-process) and []any (post-JSON unmarshal).
// Non-map entries in the []any case are skipped with a warn log so a single
// malformed element doesn't drop the whole batch.
func (p *Plugin) appendExtraTools(body map[string]any, reqMeta map[string]any) {
	if reqMeta == nil {
		return
	}
	raw, ok := reqMeta["_anthropic_extra_tools"]
	if !ok || raw == nil {
		return
	}

	var extras []map[string]any
	switch v := raw.(type) {
	case []map[string]any:
		extras = v
	case []any:
		for i, elem := range v {
			m, ok := elem.(map[string]any)
			if !ok {
				if p.logger != nil {
					p.logger.Warn("anthropic: skipping non-map entry in _anthropic_extra_tools",
						"index", i, "type", typeName(elem))
				}
				continue
			}
			extras = append(extras, m)
		}
	default:
		if p.logger != nil {
			p.logger.Warn("anthropic: ignoring _anthropic_extra_tools with unsupported type",
				"type", typeName(raw))
		}
		return
	}

	if len(extras) == 0 {
		return
	}

	if existing, ok := body["tools"].([]map[string]any); ok {
		body["tools"] = append(existing, extras...)
	} else {
		body["tools"] = extras
	}
}

// typeName returns a short descriptor for an interface value's dynamic type.
// Used in warn logs so operators can see what shape arrived.
func typeName(v any) string {
	if v == nil {
		return "nil"
	}
	switch v.(type) {
	case string:
		return "string"
	case int, int32, int64:
		return "int"
	case float32, float64:
		return "float"
	case bool:
		return "bool"
	case map[string]any:
		return "map[string]any"
	case []any:
		return "[]any"
	case []map[string]any:
		return "[]map[string]any"
	default:
		return "unknown"
	}
}

// betaFlags returns the comma-joined value for the anthropic-beta header,
// merging the plugin's standing flags with any per-request additions from
// req.Metadata["_anthropic_beta_headers"]. Duplicates are removed and order
// is preserved (standing flags first, then per-request additions in
// insertion order).
//
// Returns an empty string when no flags are active so callers can omit the
// header entirely.
func (p *Plugin) betaFlags(reqMeta map[string]any) string {
	seen := map[string]struct{}{}
	var flags []string
	add := func(f string) {
		if f == "" {
			return
		}
		if _, ok := seen[f]; ok {
			return
		}
		seen[f] = struct{}{}
		flags = append(flags, f)
	}

	// Standing plugin flags.
	if p.cache.Enabled && p.cache.TTL == "1h" {
		add("extended-cache-ttl-2025-04-11")
	}
	if p.files.Enabled {
		add(filesAPIBetaHeader)
	}
	if p.multimodal.PDFBeta {
		add("pdfs-2024-09-25")
	}

	// Per-request additions from metadata. Server-tool plugins set this on
	// the outgoing llm.request to opt into the right beta gate
	// (e.g. "computer-use-2025-01-24", "code-execution-2025-05-22").
	if reqMeta != nil {
		switch raw := reqMeta["_anthropic_beta_headers"].(type) {
		case []string:
			for _, f := range raw {
				add(f)
			}
		case []any:
			for _, v := range raw {
				if s, ok := v.(string); ok {
					add(s)
				}
			}
		}
	}

	return strings.Join(flags, ",")
}
