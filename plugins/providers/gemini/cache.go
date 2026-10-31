package gemini

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// cacheSettings is the value half of the `cache:` block — the policy, with no
// state attached. It is split out from cacheState because a `core.models` role
// may carry a `cache:` block of its own that merges over the plugin-level one
// per request, while the entry map underneath has to stay shared across every
// role the provider serves.
//
//	cache:
//	  enabled: true
//	  min_tokens: 32768
//	  ttl: "1h"
//	  max_entries: 64
type cacheSettings struct {
	enabled    bool
	minTokens  int
	ttl        time.Duration
	maxEntries int
}

// cacheBlockKeys is the complete set of keys a `cache:` block may carry. It
// duplicates schema.json's property list on purpose: a block written on a
// `core.models` role never passes through schema.json — core stores those maps
// without looking inside them — so this is the only thing that ever rejects a
// typo there.
var cacheBlockKeys = map[string]struct{}{
	"enabled":     {},
	"min_tokens":  {},
	"ttl":         {},
	"max_entries": {},
}

// defaultCacheSettings is the policy a deployment with no `cache:` block at all
// resolves to: caching off, and the values the other keys fall back to should
// it ever be turned on.
func defaultCacheSettings() cacheSettings {
	return cacheSettings{
		minTokens:  32768,
		ttl:        time.Hour,
		maxEntries: 64,
	}
}

// validateCacheBlock rejects a `cache:` block that schema.json would have
// rejected — an unknown key, or a key of the wrong type. Value-level surprises
// stay lenient, matching this provider's parsing style: an unparseable `ttl` is
// warned about and defaulted rather than refused.
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
		return fmt.Errorf("gemini: cache block has unknown key(s) %s; accepted keys are enabled, min_tokens, ttl, max_entries",
			strings.Join(unknown, ", "))
	}

	if v, present := raw["enabled"]; present {
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("gemini: cache.enabled must be a bool, got %T", v)
		}
	}
	for _, key := range []string{"min_tokens", "max_entries"} {
		if v, present := raw[key]; present {
			// YAML decoders surface integers as int or float64 depending on path.
			switch n := v.(type) {
			case int:
				if n < 0 {
					return fmt.Errorf("gemini: cache.%s must not be negative, got %d", key, n)
				}
			case float64:
				if n < 0 {
					return fmt.Errorf("gemini: cache.%s must not be negative, got %v", key, n)
				}
			default:
				return fmt.Errorf("gemini: cache.%s must be an int, got %T", key, v)
			}
		}
	}
	if v, present := raw["ttl"]; present {
		if _, ok := v.(string); !ok {
			return fmt.Errorf("gemini: cache.ttl must be a string, got %T", v)
		}
	}
	return nil
}

// parseCacheSettings pulls a cacheSettings out of a config map carrying a
// `cache:` block. An absent block resolves to the defaults with caching off.
//
// The error is reserved for the two things schema.json would have caught and a
// `core.models` role block bypasses: an unknown key and a key of the wrong
// type. logger may be nil, which suppresses the value-level warnings — that is
// what the per-request path wants, since validateRoleCache has already run
// every role's merged block through here once at Init.
func parseCacheSettings(cfg map[string]any, logger *slog.Logger) (cacheSettings, error) {
	cs := defaultCacheSettings()

	raw, ok := cfg["cache"].(map[string]any)
	if !ok {
		return cs, nil
	}

	if err := validateCacheBlock(raw); err != nil {
		return defaultCacheSettings(), err
	}

	if v, ok := raw["enabled"].(bool); ok {
		cs.enabled = v
	}
	if v, ok := intValue(raw["min_tokens"]); ok && v > 0 {
		cs.minTokens = v
	}
	if v, ok := intValue(raw["max_entries"]); ok && v > 0 {
		cs.maxEntries = v
	}
	if v, ok := raw["ttl"].(string); ok {
		if d, err := time.ParseDuration(v); err == nil {
			cs.ttl = d
		} else if logger != nil {
			logger.Warn("gemini: cache.ttl is not a Go duration; using the default",
				"ttl", v,
				"default", cs.ttl,
			)
		}
	}

	return cs, nil
}

// intValue accepts the int and float64 shapes a YAML decoder may produce for
// the same integer.
func intValue(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}

// cacheState holds prompt-cache policy and the in-memory key map.
//
// Caching is conservative: only the stable prefix (system instruction + tool
// declarations + the leading run of contents up to the first functionResponse
// turn, see cachePrefix) is hashed and cached. On a hit buildRequestBody sends
// only what the entry does not cover: the contents after that prefix.
//
// Nothing populates the map automatically. The read path (lookupWith, called
// from buildRequestBody) is live, but an entry exists only after
// createCachedContent has been called, and no code path in Nexus calls it
// today — there is no token-count probe or auto-create. With no entries every
// lookup misses and requests go out uncached.
//
// The embedded cacheSettings is the *plugin-level* policy. A request served
// under a `core.models` role that carries its own `cache:` block resolves a
// different cacheSettings per request (see Plugin.resolveCache) and hands it to
// lookupWith; the entry map is deliberately shared regardless, because a cache
// entry lives at Google and is addressed by prefix hash alone.
type cacheState struct {
	cacheSettings
	logger *slog.Logger

	mu      sync.Mutex
	entries map[string]*cacheEntry // prefix-hash -> entry
	order   []string               // LRU eviction order, oldest first
}

type cacheEntry struct {
	name    string // resource name returned by cachedContents.create
	expires time.Time
	// covered is how many leading request contents the cached resource
	// holds — len(cachePrefix(contents)) at populate time. A hit strips
	// exactly this many from the request; the systemInstruction and tools
	// are always covered, because createCachedContentAt always sends them.
	covered int
}

// newCacheState builds the shared cache state from the plugin-level `cache:`
// block. The error is parseCacheSettings's — an unknown key or a wrongly typed
// one — and fails Init rather than the first request.
func newCacheState(cfg map[string]any, logger *slog.Logger) (*cacheState, error) {
	settings, err := parseCacheSettings(cfg, logger)
	if err != nil {
		return nil, err
	}
	return &cacheState{
		cacheSettings: settings,
		logger:        logger,
		entries:       make(map[string]*cacheEntry),
	}, nil
}

// lookup returns a cached_content resource name when the request prefix
// matches a live cache entry under the plugin-level policy. Returns "" when no
// cache should be applied. It is a pure read: entries are only ever added by
// populate, via createCachedContent.
func (cs *cacheState) lookup(model, system string, tools []events.ToolDef, contents []map[string]any) string {
	if cs == nil {
		return ""
	}
	name, _ := cs.lookupWith(cs.enabled, model, system, tools, contents)
	return name
}

// lookupWith is lookup against a per-request policy rather than the
// plugin-level one, so a `core.models` role whose merged `cache:` block turns
// caching off (or on) governs its own requests. Only `enabled` can differ per
// request today: the entry map is addressed by prefix hash, and `min_tokens`,
// `ttl` and `max_entries` all belong to the write path, which is not
// role-scoped. See Plugin.resolveCache.
//
// covered is the number of leading contents the cached resource already holds
// (see cacheEntry.covered); the caller must not resend them.
func (cs *cacheState) lookupWith(enabled bool, model, system string, tools []events.ToolDef, contents []map[string]any) (name string, covered int) {
	if cs == nil || !enabled {
		return "", 0
	}

	hash := cs.prefixHash(model, system, tools, contents)

	cs.mu.Lock()
	defer cs.mu.Unlock()

	entry, ok := cs.entries[hash]
	if !ok {
		return "", 0
	}
	if time.Now().After(entry.expires) {
		delete(cs.entries, hash)
		cs.removeOrder(hash)
		return "", 0
	}
	return entry.name, entry.covered
}

// populate stores a cachedContents resource name against a prefix hash, and
// records how many leading contents the resource covers. Its only production
// caller is createCachedContentAt, after a successful cachedContents.create.
func (cs *cacheState) populate(model, system string, tools []events.ToolDef, contents []map[string]any, name string, ttl time.Duration) {
	if cs == nil || !cs.enabled {
		return
	}
	hash := cs.prefixHash(model, system, tools, contents)
	if ttl == 0 {
		ttl = cs.ttl
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	cs.entries[hash] = &cacheEntry{name: name, expires: time.Now().Add(ttl), covered: len(cachePrefix(contents))}
	cs.order = append(cs.order, hash)
	for len(cs.order) > cs.maxEntries {
		oldest := cs.order[0]
		cs.order = cs.order[1:]
		delete(cs.entries, oldest)
	}
}

// invalidate removes a cache entry by name (used when a 404 indicates Google-
// side expiry).
func (cs *cacheState) invalidate(name string) {
	if cs == nil {
		return
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for hash, entry := range cs.entries {
		if entry.name == name {
			delete(cs.entries, hash)
			cs.removeOrder(hash)
			return
		}
	}
}

func (cs *cacheState) removeOrder(hash string) {
	for i, h := range cs.order {
		if h == hash {
			cs.order = append(cs.order[:i], cs.order[i+1:]...)
			return
		}
	}
}

// prefixHash hashes the request prefix that would be cached. Order: model,
// system, tool declarations (sorted by name), and the leading run of contents
// up to (but not including) any functionResponse turn (those represent the
// dynamic tail).
func (cs *cacheState) prefixHash(model, system string, tools []events.ToolDef, contents []map[string]any) string {
	type toolKey struct {
		Name        string
		Description string
		Params      map[string]any
	}

	keys := make([]toolKey, 0, len(tools))
	for _, t := range tools {
		keys = append(keys, toolKey{Name: t.Name, Description: t.Description, Params: t.Parameters})
	}
	// Stable order: same input set produces the same hash even if tool slice
	// order varies between calls.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1].Name > keys[j].Name; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}

	prefixContents := cachePrefix(contents)

	payload := struct {
		Model    string
		System   string
		Tools    []toolKey
		Contents []map[string]any
	}{
		Model:    model,
		System:   system,
		Tools:    keys,
		Contents: prefixContents,
	}

	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// cachePrefix is the leading run of contents up to (not including) the first
// functionResponse turn — the part of a conversation that is both hashed and
// stored in a cachedContents resource. prefixHash and createCachedContentAt
// share it so the hash never describes more or less than the resource holds.
func cachePrefix(contents []map[string]any) []map[string]any {
	for i, c := range contents {
		if hasFunctionResponsePart(c) {
			return contents[:i]
		}
	}
	return contents
}

func hasFunctionResponsePart(c map[string]any) bool {
	parts, ok := c["parts"].([]map[string]any)
	if !ok {
		// Json round-trip might normalize to []any.
		if alt, ok := c["parts"].([]any); ok {
			for _, p := range alt {
				if pm, ok := p.(map[string]any); ok {
					if _, ok := pm["functionResponse"]; ok {
						return true
					}
				}
			}
			return false
		}
		return false
	}
	for _, p := range parts {
		if _, ok := p["functionResponse"]; ok {
			return true
		}
	}
	return false
}

// createCachedContent calls the cachedContents API to create a new cache
// entry and records it in the entry map. It is the only way an entry is ever
// created, and nothing in Nexus calls it today: it is kept so the populate
// path stays tested and ready for an explicit caller, while the read path in
// buildRequestBody honours whatever it records.
func (p *Plugin) createCachedContent(ctx context.Context, model, system string, tools []events.ToolDef, contents []map[string]any, ttl time.Duration) (string, error) {
	if !p.cache.enabled {
		return "", fmt.Errorf("cache disabled")
	}
	return p.createCachedContentAt(ctx, p.auth.cachedContentsURL(), model, system, tools, contents, ttl)
}

// createCachedContentAt is the test-friendly variant: it posts to an explicit
// URL rather than the one resolved from authState.
func (p *Plugin) createCachedContentAt(ctx context.Context, url, model, system string, tools []events.ToolDef, contents []map[string]any, ttl time.Duration) (string, error) {
	body := map[string]any{
		"model": fmt.Sprintf("models/%s", model),
	}
	if system != "" {
		body["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": system}},
		}
	}
	if len(tools) > 0 {
		decls := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			decls = append(decls, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  p.sanitizeToolSchema(t.Name, t.Parameters),
			})
		}
		body["tools"] = []map[string]any{{"functionDeclarations": decls}}
	}
	// A request that hits this entry may not resend tools, so the
	// plugin-level codeExecution tool has to live in the cache too.
	if p.codeExecution {
		tools, _ := body["tools"].([]map[string]any)
		body["tools"] = append(tools, map[string]any{"codeExecution": map[string]any{}})
	}
	// Only the stable prefix is cached — exactly the run prefixHash covers —
	// so a hit can strip a known number of leading contents.
	if prefix := cachePrefix(contents); len(prefix) > 0 {
		body["contents"] = prefix
	}
	if ttl > 0 {
		body["ttl"] = fmt.Sprintf("%ds", int(ttl.Seconds()))
	}

	bodyJSON, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyJSON))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := p.auth.applyAuth(ctx, req); err != nil {
		return "", err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cachedContents.create failed (%d): %s", resp.StatusCode, string(respBody))
	}

	var out struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("parse cachedContents response: %w", err)
	}
	if out.Name == "" {
		return "", fmt.Errorf("empty cache name in response: %s", string(respBody))
	}

	p.cache.populate(model, system, tools, contents, out.Name, ttl)
	return out.Name, nil
}

// --- per-role caching -------------------------------------------------------

// rawCacheBlock lifts the plugin-level `cache:` block off the plugin config so
// it can be merged with a `core.models` role's block later. nil means the
// plugin set no block at all.
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
// wants caching off does not have to restate the ttl.
//
// A nil role block means the role said nothing, and the plugin block stands
// unchanged. A set-but-empty role block (`cache: {}`) is a statement rather
// than a gap: it overrides no individual key, but the merged block is non-nil.
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

// parseMergedCache runs a merged block back through parseCacheSettings — the
// one parser — by handing it the block wrapped in the shape it expects. A
// merged block is an ordinary `cache:` block and gets exactly the same
// validation, defaulting and warning treatment the plugin-level one does.
func parseMergedCache(plugin, role map[string]any, logger *slog.Logger) (cacheSettings, error) {
	merged := mergeCacheBlock(plugin, role)
	if merged == nil {
		return defaultCacheSettings(), nil
	}
	return parseCacheSettings(map[string]any{"cache": merged}, logger)
}

// resolveCache picks the prompt-caching policy for one request.
//
// Precedence is the unified rule, most specific first:
//
//  1. req.Overrides.Cache — the block of the chain entry actually being served,
//     stamped by the fallback or fanout coordinator or recovered from the
//     registry by engine.ResolveModelConfig. Either way it is merged over, not
//     substituted for, the plugin block.
//  2. the plugin-level `cache:` block, which is what a request carrying no
//     override of its own gets, unchanged.
//
// There is no `effort` analogue here: caching has no shared cross-provider axis
// on a `core.models` entry, only the native block.
//
// An unparseable merged block is unreachable through Init, which sweeps every
// role this provider could serve and refuses to boot on one. The degradation
// here exists for a request whose Overrides.Cache was hand-set by something
// other than the registry.
func (p *Plugin) resolveCache(req events.LLMRequest) cacheSettings {
	plugin := cacheSettings{}
	if p.cache != nil {
		plugin = p.cache.cacheSettings
	}
	if req.Overrides.Cache == nil {
		return plugin
	}
	// Nil logger: the warnings are boot-time facts, already said once by
	// validateRoleCache. Repeating them per request would flood the log of a
	// busy role.
	cs, err := parseMergedCache(p.cacheRaw, req.Overrides.Cache, nil)
	if err != nil {
		p.log().Warn("gemini: ignoring an invalid per-request cache block; using the plugin-level configuration",
			"role", req.Role,
			"error", err,
		)
		return plugin
	}
	return cs
}

// validateRoleCache checks every `core.models` role this provider could serve,
// at Init, by merging its `cache:` block over the plugin-level one and parsing
// the result.
//
// It exists because a block on a `core.models` entry bypasses the plugin's
// schema.json entirely — core stores these maps without looking inside them, so
// nothing else ever checks them. Without this sweep a role whose merged block
// carries a typo would boot clean and cache nothing, invisibly.
//
// The error names the role. The sweep itself — sorted roles, whole chain,
// foreign entries skipped — is engine.WalkRoleEntries; see there for why each
// of those matters.
func validateRoleCache(models *engine.ModelRegistry, plugin map[string]any, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	return engine.WalkRoleEntries(models, pluginID, func(role string, cfg engine.ModelConfig) error {
		if cfg.Cache == nil {
			return nil
		}
		if _, err := parseMergedCache(plugin, cfg.Cache, logger.With("role", role)); err != nil {
			return fmt.Errorf("core.models role %q: %w", role, err)
		}
		return nil
	})
}
