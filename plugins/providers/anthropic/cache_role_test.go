package anthropic

import (
	"bytes"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// --- mergeCacheBlock --------------------------------------------------------

// The role wins on every key it names and a plugin key it is silent about
// survives, so a role that only wants a longer TTL keeps the plugin's
// breakpoint choices.
func TestMergeCacheBlock_RoleWinsPerKey(t *testing.T) {
	plugin := map[string]any{"enabled": true, "system": true, "tools": false, "ttl": "5m"}
	role := map[string]any{"ttl": "1h"}

	merged := mergeCacheBlock(plugin, role)

	if merged["ttl"] != "1h" {
		t.Errorf("ttl = %v, want the role's 1h", merged["ttl"])
	}
	if merged["system"] != true || merged["tools"] != false {
		t.Errorf("merged = %#v, want the plugin's breakpoint choices to survive", merged)
	}
	if plugin["ttl"] != "5m" {
		t.Errorf("plugin block was mutated: %#v", plugin)
	}
}

// A nil role block leaves the plugin block exactly as it was; nil over nil
// stays nil, because "said nothing" must not become "set an empty block".
func TestMergeCacheBlock_NilRole(t *testing.T) {
	plugin := map[string]any{"enabled": true}
	if got := mergeCacheBlock(plugin, nil); got["enabled"] != true {
		t.Errorf("merged = %#v, want the plugin block unchanged", got)
	}
	if mergeCacheBlock(nil, nil) != nil {
		t.Error("nil over nil should stay nil")
	}
}

// A set-but-empty role block is a statement, not a gap: it overrides no key,
// but the merged block is non-nil and goes through the parser.
func TestParseMergedCache_EmptyRoleBlockIsAStatement(t *testing.T) {
	cc, err := parseMergedCache(map[string]any{"enabled": true, "ttl": "1h"}, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("parseMergedCache: unexpected error: %v", err)
	}
	if !cc.Enabled || cc.TTL != "1h" {
		t.Fatalf("cc = %#v, want the plugin block to stand", cc)
	}

	// With no plugin block at all, an empty role block resolves to caching off
	// rather than to anything inherited.
	cc, err = parseMergedCache(nil, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("parseMergedCache: unexpected error: %v", err)
	}
	if cc.Enabled {
		t.Fatalf("cc = %#v, want caching off", cc)
	}
}

// A role can turn caching on for itself on a deployment whose plugin block is
// absent entirely.
func TestParseMergedCache_RoleTurnsCachingOn(t *testing.T) {
	cc, err := parseMergedCache(nil, map[string]any{"enabled": true, "message_prefix": 2}, nil)
	if err != nil {
		t.Fatalf("parseMergedCache: unexpected error: %v", err)
	}
	if !cc.Enabled || !cc.System || !cc.Tools || cc.MessagePrefix != 2 {
		t.Fatalf("cc = %#v, want caching on with the enabled-defaults plus the role's prefix", cc)
	}
}

// --- validateRoleCache ------------------------------------------------------

// A typo on a role block has nothing else to catch it — core stores these maps
// without looking inside them and schema.json never sees them — so Init refuses
// the boot, naming the role.
func TestValidateRoleCache_UnknownKeyFailsInitNamingTheRole(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "cheap",
		"cheap": map[string]any{
			"provider": pluginID,
			"model":    "claude-haiku-4-5",
			"cache":    map[string]any{"enabled": true, "ttl_hours": 1},
		},
	})

	err := validateRoleCache(models, nil, silentTestLogger())
	if err == nil {
		t.Fatal("expected an unknown key on a role block to fail Init")
	}
	if !strings.Contains(err.Error(), `role "cheap"`) || !strings.Contains(err.Error(), "ttl_hours") {
		t.Fatalf("error = %v, want it to name the role and the key", err)
	}
}

// A wrongly typed key is the other class schema.json would have caught.
func TestValidateRoleCache_WrongTypeFailsInit(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "cheap",
		"cheap": map[string]any{
			"provider": pluginID,
			"model":    "claude-haiku-4-5",
			"cache":    map[string]any{"message_prefix": "two"},
		},
	})

	if err := validateRoleCache(models, nil, silentTestLogger()); err == nil {
		t.Fatal("expected a wrongly typed key to fail Init")
	}
}

// Entries naming another provider are that provider's business.
func TestValidateRoleCache_SkipsForeignProviders(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "cheap",
		"cheap": map[string]any{
			"provider": "nexus.llm.gemini",
			"model":    "gemini-3-flash",
			"cache":    map[string]any{"min_tokens": 1024},
		},
	})

	if err := validateRoleCache(models, nil, silentTestLogger()); err != nil {
		t.Fatalf("foreign entry should be skipped, got %v", err)
	}
	if err := validateRoleCache(nil, nil, silentTestLogger()); err != nil {
		t.Fatalf("nil registry: unexpected error: %v", err)
	}
}

// The whole chain is walked, not just the primary: a fallback entry's block
// reaches this provider through the coordinator's stamp.
func TestValidateRoleCache_WalksTheWholeChain(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": []any{
			map[string]any{"provider": pluginID, "model": "claude-opus-4-7", "cache": map[string]any{"enabled": true}},
			map[string]any{"provider": pluginID, "model": "claude-haiku-4-5", "cache": map[string]any{"enabld": true}},
		},
	})

	err := validateRoleCache(models, nil, silentTestLogger())
	if err == nil {
		t.Fatal("expected the second chain entry's block to fail Init")
	}
	if !strings.Contains(err.Error(), `role "balanced"`) {
		t.Fatalf("error = %v, want it to name the role", err)
	}
}

// A merged block that turns every breakpoint off is legal but pointless; Init
// says so once, naming the role.
func TestValidateRoleCache_WarnsWhenNoBreakpointRemains(t *testing.T) {
	var buf bytes.Buffer
	models := engine.NewModelRegistry(map[string]any{
		"default": "cheap",
		"cheap": map[string]any{
			"provider": pluginID,
			"model":    "claude-haiku-4-5",
			"cache":    map[string]any{"system": false, "tools": false},
		},
	})

	if err := validateRoleCache(models, map[string]any{"enabled": true}, warnCaptureLogger(&buf)); err != nil {
		t.Fatalf("validateRoleCache: unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "no breakpoint") || !strings.Contains(buf.String(), "cheap") {
		t.Fatalf("warning = %q, want it to name the role and the empty policy", buf.String())
	}
}

// --- per-role caching on the wire -------------------------------------------

// newRoleCachePlugin builds a plugin exactly as Init would for the given
// plugin-level block: parsed config and the raw block kept side by side.
func newRoleCachePlugin(t *testing.T, block map[string]any, models *engine.ModelRegistry) *Plugin {
	t.Helper()

	cfg := map[string]any{}
	if block != nil {
		cfg["cache"] = block
	}
	cc, err := parseCacheConfig(cfg, silentTestLogger())
	if err != nil {
		t.Fatalf("parseCacheConfig: unexpected error: %v", err)
	}
	return &Plugin{
		logger:   silentTestLogger(),
		models:   models,
		cache:    cc,
		cacheRaw: rawCacheBlock(cfg),
	}
}

// wireCache runs the same two steps handleRequest does — resolve the role's
// block onto the request, then build the body — and reports the system block's
// cache_control marker. A false second return means no marker was placed.
func wireCache(t *testing.T, p *Plugin, req events.LLMRequest) (map[string]any, bool) {
	t.Helper()

	target := p.resolveTarget(req)
	if target.skip {
		t.Fatal("resolveTarget skipped the request")
	}
	req.Effort = target.effort
	p.applyEntryOverrides(&req)

	body := p.buildRequestBody(target.model, target.maxTokens, req)
	sys, ok := body["system"].([]map[string]any)
	if !ok || len(sys) == 0 {
		return nil, false
	}
	marker, ok := sys[len(sys)-1]["cache_control"].(map[string]any)
	return marker, ok
}

func cacheReq(role string) events.LLMRequest {
	return events.LLMRequest{
		Role: role,
		Messages: []events.Message{
			{Role: "system", Content: "you are a helpful assistant"},
			{Role: "user", Content: "hi"},
		},
	}
}

// The named-role path: a plain single-entry role's `cache:` reaches the wire
// with no coordinator involved.
func TestRoleCache_NamedRoleReachesTheWire(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "cheap",
		"cheap": map[string]any{
			"provider": pluginID,
			"model":    "claude-haiku-4-5",
			"cache":    map[string]any{"enabled": true, "ttl": "1h"},
		},
	})
	p := newRoleCachePlugin(t, nil, models)

	marker, ok := wireCache(t, p, cacheReq("cheap"))
	if !ok {
		t.Fatal("expected the role to turn caching on")
	}
	if marker["ttl"] != "1h" {
		t.Fatalf("cache_control = %#v, want the role's 1h TTL", marker)
	}
}

// The role merges over the plugin block rather than replacing it: `ttl` comes
// from the role, `message_prefix` survives from the plugin.
func TestRoleCache_MergesOverThePluginBlock(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "cheap",
		"cheap": map[string]any{
			"provider": pluginID,
			"model":    "claude-haiku-4-5",
			"cache":    map[string]any{"ttl": "1h"},
		},
	})
	p := newRoleCachePlugin(t, map[string]any{"enabled": true, "message_prefix": 2}, models)

	cc := p.resolveCache(mustResolved(t, p, cacheReq("cheap")))
	if cc.TTL != "1h" {
		t.Errorf("ttl = %q, want the role's 1h", cc.TTL)
	}
	if cc.MessagePrefix != 2 {
		t.Errorf("message_prefix = %d, want the plugin's 2 to survive", cc.MessagePrefix)
	}
	if p.cache.TTL != "5m" {
		t.Errorf("plugin-level config was mutated: %#v", p.cache)
	}
}

// A role can opt out of a deployment-wide cache entirely.
func TestRoleCache_RoleTurnsCachingOff(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "cheap",
		"cheap": map[string]any{
			"provider": pluginID,
			"model":    "claude-haiku-4-5",
			"cache":    map[string]any{"enabled": false},
		},
	})
	p := newRoleCachePlugin(t, map[string]any{"enabled": true}, models)

	if _, ok := wireCache(t, p, cacheReq("cheap")); ok {
		t.Fatal("expected the role's enabled: false to suppress every marker")
	}
}

// The default-role path: a request naming no role picks up the default role's
// block.
func TestRoleCache_DefaultRolePath(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "cheap",
		"cheap": map[string]any{
			"provider": pluginID,
			"model":    "claude-haiku-4-5",
			"cache":    map[string]any{"enabled": true},
		},
	})
	p := newRoleCachePlugin(t, nil, models)

	if _, ok := wireCache(t, p, cacheReq("")); !ok {
		t.Fatal("expected the default role's block to reach the wire")
	}
}

// The provider-native axes do NOT fall through from the default role to a named
// role: a named Anthropic role with no `cache:` of its own gets the plugin
// block, not the default role's.
func TestRoleCache_NamedRoleDoesNotInheritTheDefaultRoleBlock(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "premium",
		"premium": map[string]any{
			"provider": pluginID,
			"model":    "claude-opus-4-7",
			"cache":    map[string]any{"enabled": true},
		},
		"cheap": map[string]any{"provider": pluginID, "model": "claude-haiku-4-5"},
	})
	p := newRoleCachePlugin(t, nil, models)

	if _, ok := wireCache(t, p, cacheReq("cheap")); ok {
		t.Fatal("a named role must not inherit the default role's cache block")
	}
}

// The router-rewrite path: a router that set `model` and left `role` alone must
// not silently drop the role's cache block.
func TestRoleCache_SurvivesARouterRewrite(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "cheap",
		"cheap": map[string]any{
			"provider": pluginID,
			"model":    "claude-haiku-4-5",
			"cache":    map[string]any{"enabled": true},
		},
	})
	p := newRoleCachePlugin(t, nil, models)

	req := cacheReq("cheap")
	req.Model = "claude-opus-4-7" // the router's rewrite
	if _, ok := wireCache(t, p, req); !ok {
		t.Fatal("expected the role's cache block to survive a model rewrite")
	}
}

// The fallback/fanout path: the coordinator stamps the entry actually being
// served, and a stamped request short-circuits the registry lookup, so the
// SECOND chain entry's block governs.
func TestRoleCache_StampedChainEntryWins(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": []any{
			map[string]any{"provider": pluginID, "model": "claude-opus-4-7", "cache": map[string]any{"enabled": false}},
			map[string]any{"provider": pluginID, "model": "claude-haiku-4-5", "cache": map[string]any{"enabled": true, "ttl": "1h"}},
		},
	})
	p := newRoleCachePlugin(t, nil, models)

	req := cacheReq("balanced")
	second, ok := models.Fallback("balanced", 1)
	if !ok {
		t.Fatal("expected a second chain entry")
	}
	engine.StampModelConfig(&req, second)

	marker, ok := wireCache(t, p, req)
	if !ok || marker["ttl"] != "1h" {
		t.Fatalf("cache_control = %#v (present=%v), want the served entry's 1h TTL", marker, ok)
	}
}

// The 1h beta gate follows the markers actually placed, so a role that lifts a
// 5m plugin default to 1h gets the header its own request needs.
func TestRoleCache_BetaHeaderFollowsTheResolvedTTL(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "cheap",
		"cheap": map[string]any{
			"provider": pluginID,
			"model":    "claude-haiku-4-5",
			"cache":    map[string]any{"ttl": "1h"},
		},
	})
	p := newRoleCachePlugin(t, map[string]any{"enabled": true}, models)

	if got := p.betaFlags(p.cache, nil); strings.Contains(got, "extended-cache-ttl") {
		t.Fatalf("plugin-level flags = %q, want no extended-cache-ttl on a 5m default", got)
	}

	req := mustResolved(t, p, cacheReq("cheap"))
	got := p.betaFlags(p.resolveCache(req), nil)
	if !strings.Contains(got, "extended-cache-ttl-2025-04-11") {
		t.Fatalf("resolved flags = %q, want the extended-cache-ttl beta gate", got)
	}
}

// A request carrying a block nothing validated must not put a policy nobody
// wrote onto the wire: degrade to the plugin-level configuration, and say so
// once.
func TestResolveCache_InvalidRequestBlockDegrades(t *testing.T) {
	var buf bytes.Buffer
	p := &Plugin{
		logger:   warnCaptureLogger(&buf),
		cache:    cacheConfig{Enabled: true, System: true, Tools: true, TTL: "5m"},
		cacheRaw: map[string]any{"enabled": true},
	}

	cc := p.resolveCache(events.LLMRequest{
		Role:      "hand-set",
		Overrides: events.ModelOverrides{Cache: map[string]any{"nonsense": true}},
	})

	if !cc.Enabled || cc.TTL != "5m" {
		t.Fatalf("cc = %#v, want the plugin-level configuration", cc)
	}
	if !strings.Contains(buf.String(), "hand-set") {
		t.Fatalf("warning = %q, want it to name the role", buf.String())
	}
}

// mustResolved runs applyEntryOverrides the way handleRequest does and hands
// back the resulting request.
func mustResolved(t *testing.T, p *Plugin, req events.LLMRequest) events.LLMRequest {
	t.Helper()
	p.applyEntryOverrides(&req)
	return req
}
