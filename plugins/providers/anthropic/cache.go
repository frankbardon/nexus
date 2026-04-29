package anthropic

import (
	"log/slog"
)

// maxCacheBreakpoints is Anthropic's hard limit on cache_control markers per
// request. We enforce it locally so misconfiguration is obvious and we never
// generate API errors purely from over-budget marker counts.
const maxCacheBreakpoints = 4

// cacheConfig describes the explicit prompt-caching policy for a single
// outgoing Anthropic request. Mirrors the `cache:` block under the plugin's
// YAML config.
//
// All fields default to zero values when caching is disabled. When enabled,
// System and Tools default to true (the high-leverage breakpoints) while
// MessagePrefix stays at 0 because marking message turns is only safe when
// the caller can guarantee a stable leading prefix.
type cacheConfig struct {
	Enabled       bool
	System        bool
	Tools         bool
	MessagePrefix int
	TTL           string // "5m" or "1h"
}

// parseCacheConfig pulls a cacheConfig out of the plugin's raw config map.
//
// When the `cache` block is absent or `enabled` is false, returns a zero-value
// config that suppresses every mutation in applyCacheControl.
//
// Invalid TTL values are logged-by-omission (defaulted to "5m") rather than
// erroring, matching the rest of the plugin's "soft fallback" parsing style.
func parseCacheConfig(cfg map[string]any) cacheConfig {
	cc := cacheConfig{}

	raw, ok := cfg["cache"].(map[string]any)
	if !ok {
		return cc
	}

	enabled, _ := raw["enabled"].(bool)
	if !enabled {
		return cc
	}

	cc.Enabled = true
	cc.System = true
	cc.Tools = true
	cc.TTL = "5m"

	if v, ok := raw["system"].(bool); ok {
		cc.System = v
	}
	if v, ok := raw["tools"].(bool); ok {
		cc.Tools = v
	}

	// YAML decoders may surface integers as int or float64 depending on path.
	if v, ok := raw["message_prefix"].(int); ok && v > 0 {
		cc.MessagePrefix = v
	} else if v, ok := raw["message_prefix"].(float64); ok && v > 0 {
		cc.MessagePrefix = int(v)
	}

	if v, ok := raw["ttl"].(string); ok {
		switch v {
		case "5m", "1h":
			cc.TTL = v
		}
	}

	return cc
}

// applyCacheControl mutates an in-flight Anthropic request body to add
// `cache_control` markers per cfg. No-op when caching is disabled.
//
// Mutations performed (in order):
//
//  1. body["system"] is converted from a bare string into the array-of-blocks
//     form Anthropic requires for cacheable system prompts, with one
//     cache_control marker on the single text block.
//  2. The LAST entry in body["tools"] gets a cache_control field. Marking only
//     the tail covers the entire tools prefix at one breakpoint cost — there's
//     no benefit to marking individual tools.
//  3. Up to cfg.MessagePrefix leading user messages get cache_control on the
//     last content block of each. String content is upgraded to the
//     array-of-blocks form first. Assistant + tool-result turns are skipped.
//
// 4-breakpoint cap: Anthropic accepts at most 4 cache_control markers per
// request. We tally up our budget (system=1, tools=1, plus per-message) and
// drop the OLDEST message markers first if the user requested more than the
// remaining budget. A debug log reports the cap event.
func applyCacheControl(body map[string]any, cfg cacheConfig, logger *slog.Logger) {
	if !cfg.Enabled {
		return
	}

	marker := buildCacheControl(cfg.TTL)

	used := 0

	if cfg.System {
		if applySystemCacheControl(body, marker) {
			used++
		}
	}
	if cfg.Tools {
		if applyToolsCacheControl(body, marker) {
			used++
		}
	}

	if cfg.MessagePrefix <= 0 {
		return
	}

	// Budget remaining for message markers.
	budget := maxCacheBreakpoints - used
	wanted := cfg.MessagePrefix
	allowed := wanted
	if allowed > budget {
		allowed = budget
		if allowed < 0 {
			allowed = 0
		}
		if logger != nil {
			logger.Debug("cache_control message markers capped at 4-breakpoint limit",
				"requested", wanted,
				"granted", allowed,
				"system_marker", cfg.System,
				"tools_marker", cfg.Tools,
			)
		}
	}

	if allowed > 0 {
		applyMessagesCacheControl(body, marker, wanted, allowed)
	}
}

// buildCacheControl returns the cache_control value for the given TTL.
// 5m TTL is the implicit Anthropic default, expressed as a bare ephemeral
// block. 1h TTL must spell out the ttl field and requires the
// extended-cache-ttl-2025-04-11 beta header (set by the request builder).
func buildCacheControl(ttl string) map[string]any {
	if ttl == "1h" {
		return map[string]any{"type": "ephemeral", "ttl": "1h"}
	}
	return map[string]any{"type": "ephemeral"}
}

// applySystemCacheControl rewrites body["system"] from string-form to the
// array-of-blocks form required for cacheable system prompts. Returns true if
// a marker was placed.
func applySystemCacheControl(body map[string]any, marker map[string]any) bool {
	sys, ok := body["system"]
	if !ok {
		return false
	}
	switch v := sys.(type) {
	case string:
		if v == "" {
			return false
		}
		body["system"] = []map[string]any{
			{
				"type":          "text",
				"text":          v,
				"cache_control": marker,
			},
		}
		return true
	case []map[string]any:
		// Already array-form (unexpected at this stage); attach to the last
		// block so we don't double-mark.
		if len(v) == 0 {
			return false
		}
		v[len(v)-1]["cache_control"] = marker
		body["system"] = v
		return true
	default:
		return false
	}
}

// applyToolsCacheControl appends cache_control to the LAST tool definition in
// body["tools"]. Returns true if a marker was placed. Marking only the tail
// caches the full tools prefix at a single breakpoint.
func applyToolsCacheControl(body map[string]any, marker map[string]any) bool {
	tools, ok := body["tools"].([]map[string]any)
	if !ok || len(tools) == 0 {
		return false
	}
	tools[len(tools)-1]["cache_control"] = marker
	body["tools"] = tools
	return true
}

// applyMessagesCacheControl marks the leading run of user messages with
// cache_control. The caller passes `wanted` (what the user requested) and
// `allowed` (the post-cap budget). When allowed < wanted, the OLDEST markers
// are dropped first — i.e. we keep the markers nearest the end of the
// requested run because each marker caches everything UP TO and INCLUDING
// its position, so later markers cover strictly larger prefixes.
//
// Assistant + tool-result turns terminate the leading run (they break the
// stable-prefix invariant). String-form content is upgraded to the
// array-of-blocks form so the marker has somewhere to live; the marker is
// attached to the LAST content block of each selected message.
func applyMessagesCacheControl(body map[string]any, marker map[string]any, wanted, allowed int) {
	msgs, ok := body["messages"].([]map[string]any)
	if !ok || len(msgs) == 0 || allowed <= 0 || wanted <= 0 {
		return
	}

	// Walk the leading user run and collect candidate indices, stopping at
	// the first non-user / tool-result turn.
	var candidates []int
	for i := range msgs {
		role, _ := msgs[i]["role"].(string)
		if role != "user" {
			break
		}
		if isToolResultMessage(msgs[i]) {
			break
		}
		candidates = append(candidates, i)
		if len(candidates) >= wanted {
			break
		}
	}

	if len(candidates) == 0 {
		return
	}

	// If we have fewer candidates than allowed, mark them all. Otherwise
	// drop the oldest (lowest-index) entries until we fit the budget.
	if len(candidates) > allowed {
		candidates = candidates[len(candidates)-allowed:]
	}

	for _, idx := range candidates {
		markLastContentBlock(msgs[idx], marker)
	}

	body["messages"] = msgs
}

// isToolResultMessage reports whether a user-role message is actually a
// tool-result envelope (Anthropic packs tool results into role=user with a
// tool_result content block).
func isToolResultMessage(msg map[string]any) bool {
	content, ok := msg["content"].([]map[string]any)
	if !ok {
		return false
	}
	for _, block := range content {
		if t, _ := block["type"].(string); t == "tool_result" {
			return true
		}
	}
	return false
}

// markLastContentBlock attaches cache_control to the final content block of a
// message, upgrading string-form content to array-of-blocks form first.
// Returns true on success.
func markLastContentBlock(msg map[string]any, marker map[string]any) bool {
	content, ok := msg["content"]
	if !ok {
		return false
	}
	switch v := content.(type) {
	case string:
		if v == "" {
			return false
		}
		msg["content"] = []map[string]any{
			{
				"type":          "text",
				"text":          v,
				"cache_control": marker,
			},
		}
		return true
	case []map[string]any:
		if len(v) == 0 {
			return false
		}
		v[len(v)-1]["cache_control"] = marker
		msg["content"] = v
		return true
	default:
		return false
	}
}
