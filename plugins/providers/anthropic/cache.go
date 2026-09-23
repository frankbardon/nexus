package anthropic

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
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

// cacheBlockKeys is the complete set of keys a `cache:` block may carry. It
// duplicates schema.json's property list on purpose: a block written on a
// `core.models` role never passes through schema.json — core stores those maps
// without looking inside them — so this is the only thing that ever rejects a
// typo there.
var cacheBlockKeys = map[string]struct{}{
	"enabled":        {},
	"system":         {},
	"tools":          {},
	"message_prefix": {},
	"ttl":            {},
}

// validateCacheBlock rejects a `cache:` block that schema.json would have
// rejected — an unknown key, or a key of the wrong type. Everything else stays
// lenient, which is this parser's long-standing contract: an unsupported `ttl`
// is warned about and defaulted rather than refused.
//
// At plugin level schema.json has already caught both classes, so this is
// belt-and-braces there. On a merged role block it is the only guard.
func validateCacheBlock(raw map[string]any) error {
	var unknown []string
	for k := range raw {
		if _, ok := cacheBlockKeys[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("anthropic: cache block has unknown key(s) %s; accepted keys are enabled, system, tools, message_prefix, ttl",
			strings.Join(unknown, ", "))
	}

	for _, key := range []string{"enabled", "system", "tools"} {
		if v, present := raw[key]; present {
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("anthropic: cache.%s must be a bool, got %s", key, typeName(v))
			}
		}
	}
	if v, present := raw["message_prefix"]; present {
		// YAML decoders surface integers as int or float64 depending on path.
		switch n := v.(type) {
		case int:
			if n < 0 {
				return fmt.Errorf("anthropic: cache.message_prefix must not be negative, got %d", n)
			}
		case float64:
			if n < 0 {
				return fmt.Errorf("anthropic: cache.message_prefix must not be negative, got %v", n)
			}
		default:
			return fmt.Errorf("anthropic: cache.message_prefix must be an int, got %s", typeName(v))
		}
	}
	if v, present := raw["ttl"]; present {
		if _, ok := v.(string); !ok {
			return fmt.Errorf("anthropic: cache.ttl must be a string, got %s", typeName(v))
		}
	}
	return nil
}

// parseCacheConfig pulls a cacheConfig out of the plugin's raw config map.
//
// When the `cache` block is absent or `enabled` is false, returns a zero-value
// config that suppresses every mutation in applyCacheControl.
//
// The error is reserved for the two things schema.json would have caught and a
// `core.models` role block bypasses: an unknown key and a key of the wrong
// type. Value-level surprises stay soft, matching the rest of the plugin's
// parsing style — an unsupported TTL is defaulted to "5m" with a warning, not
// refused.
//
// logger may be nil, which suppresses those warnings. That is what the
// per-request path wants: validateRoleCache has already run every role's merged
// block through here once at Init with the real logger attached.
func parseCacheConfig(cfg map[string]any, logger *slog.Logger) (cacheConfig, error) {
	cc := cacheConfig{}

	raw, ok := cfg["cache"].(map[string]any)
	if !ok {
		return cc, nil
	}

	if err := validateCacheBlock(raw); err != nil {
		return cacheConfig{}, err
	}

	enabled, _ := raw["enabled"].(bool)
	if !enabled {
		return cc, nil
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
		default:
			if logger != nil {
				logger.Warn("anthropic: cache.ttl is not one of 5m, 1h; using 5m",
					"ttl", v,
				)
			}
		}
	}

	if !cc.System && !cc.Tools && cc.MessagePrefix == 0 && logger != nil {
		// Reachable at plugin level, but far more likely on a merged role block
		// that turned off the one breakpoint the plugin block had marked.
		logger.Warn("anthropic: cache.enabled is true but no breakpoint is marked; the request carries no cache_control at all")
	}

	return cc, nil
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

// --- per-role caching -------------------------------------------------------

// rawCacheBlock lifts the plugin-level `cache:` block off the plugin config so
// it can be merged with a `core.models` role's block later. nil means the
// plugin set no block at all, which parseCacheConfig reads as caching off.
//
// The map is not copied: everything reachable from it is treated as read-only,
// and mergeCacheBlock always builds a fresh map rather than writing into either
// input.
func rawCacheBlock(cfg map[string]any) map[string]any {
	block, ok := cfg["cache"].(map[string]any)
	if !ok {
		return nil
	}
	return block
}

// mergeCacheBlock merges a `core.models` role's `cache:` block over the
// plugin-level one, key by key. The role wins on every key it mentions, and a
// plugin-level key the role is silent about survives — so a role that only
// wants a longer `ttl` does not have to restate which breakpoints to mark.
//
// A nil role block means the role said nothing, and the plugin block stands
// unchanged. A set-but-empty role block (`cache: {}`) is a statement rather
// than a gap: it overrides no individual key, but the merged block is non-nil,
// so on a deployment whose plugin block is absent entirely it still resolves to
// caching off (parseCacheConfig requires `enabled: true`) rather than to
// something inherited.
//
// Neither input is mutated and the result aliases neither: the plugin config
// map and the registry's block are both shared with the loaded configuration.
func mergeCacheBlock(plugin, role map[string]any) map[string]any {
	if role == nil {
		return plugin
	}
	merged := make(map[string]any, len(plugin)+len(role))
	for k, v := range plugin {
		merged[k] = v
	}
	for k, v := range role {
		merged[k] = v
	}
	return merged
}

// parseMergedCache runs a merged block back through parseCacheConfig — the one
// parser — by handing it the block wrapped in the shape it expects. A merged
// block is an ordinary `cache:` block and gets exactly the same validation,
// defaulting and warning treatment the plugin-level one does.
//
// A nil logger suppresses the warnings, which is what the per-request path
// wants: validateRoleCache has already run every role's merged block through
// here once at Init with the real logger attached.
func parseMergedCache(plugin, role map[string]any, logger *slog.Logger) (cacheConfig, error) {
	merged := mergeCacheBlock(plugin, role)
	if merged == nil {
		return cacheConfig{}, nil
	}
	return parseCacheConfig(map[string]any{"cache": merged}, logger)
}

// resolveCache picks the prompt-caching configuration for one request.
//
// This is the single lookup point for the cache block, deliberately separate
// from its two use sites (applyCacheControl in the body builder, and the 1h
// beta flag in betaFlags). Precedence is the unified rule, most specific first:
//
//  1. req.Overrides.Cache — the block of the chain entry actually being served.
//     It arrives either stamped by the fallback or fanout coordinator (which
//     alone knows a non-first entry is in play) or recovered from the registry
//     by engine.ResolveModelConfig on the paths no coordinator touches. Either
//     way it is merged over, not substituted for, the plugin block.
//  2. the plugin-level `cache:` block, which is what a request carrying no
//     override of its own gets, unchanged.
//
// There is no `effort` analogue here: caching has no shared cross-provider axis
// on a `core.models` entry, only the native block.
//
// An unparseable merged block is unreachable through Init, which sweeps every
// role this provider could serve and refuses to boot on one. The degradation
// here exists for a request whose Overrides.Cache was hand-set by something
// other than the registry: fall back to the plugin-level configuration rather
// than silently caching to a policy nobody wrote.
func (p *Plugin) resolveCache(req events.LLMRequest) cacheConfig {
	if req.Overrides.Cache == nil {
		return p.cache
	}
	// Nil logger: the warnings are boot-time facts, already said once by
	// validateRoleCache. Repeating them per request would flood the log of a
	// busy role.
	cc, err := parseMergedCache(p.cacheRaw, req.Overrides.Cache, nil)
	if err != nil {
		logger := p.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("anthropic: ignoring an invalid per-request cache block; using the plugin-level configuration",
			"role", req.Role,
			"error", err,
		)
		return p.cache
	}
	return cc
}

// validateRoleCache checks every `core.models` role this provider could serve,
// at Init, by merging its `cache:` block over the plugin-level one and parsing
// the result.
//
// It exists because a block on a `core.models` entry bypasses the plugin's
// schema.json entirely — core stores these maps without looking inside them, so
// nothing else ever checks them. Without this sweep a role whose merged block
// carries a typo (`ttl_hours: 1`, say) would boot clean and cache nothing,
// invisibly, for as long as the deployment ran.
//
// The error names the role, which is the useful half of the answer. The sweep
// itself — sorted roles, whole chain, foreign entries skipped — is
// engine.WalkRoleEntries; see there for why each of those matters.
func validateRoleCache(models *engine.ModelRegistry, plugin map[string]any, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	return engine.WalkRoleEntries(models, pluginID, func(role string, cfg engine.ModelConfig) error {
		if cfg.Cache == nil {
			return nil
		}
		// The role's logger, so a warning raised by the merged block says which
		// role raised it. Only entries that actually set a block get here, so
		// the plugin block's own warnings — already said once by Init — are not
		// repeated per role.
		if _, err := parseMergedCache(plugin, cfg.Cache, logger.With("role", role)); err != nil {
			return fmt.Errorf("core.models role %q: %w", role, err)
		}
		return nil
	})
}
