package gemini

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

func warnCaptureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// --- mergeCacheBlock --------------------------------------------------------

// The role wins on every key it names and a plugin key it is silent about
// survives.
func TestMergeCacheBlock_RoleWinsPerKey(t *testing.T) {
	plugin := map[string]any{"enabled": true, "min_tokens": 4096, "ttl": "1h"}
	role := map[string]any{"ttl": "10m"}

	merged := mergeCacheBlock(plugin, role)

	if merged["ttl"] != "10m" {
		t.Errorf("ttl = %v, want the role's 10m", merged["ttl"])
	}
	if merged["min_tokens"] != 4096 {
		t.Errorf("merged = %#v, want the plugin's min_tokens to survive", merged)
	}
	if plugin["ttl"] != "1h" {
		t.Errorf("plugin block was mutated: %#v", plugin)
	}
}

func TestMergeCacheBlock_NilRole(t *testing.T) {
	plugin := map[string]any{"enabled": true}
	if got := mergeCacheBlock(plugin, nil); got["enabled"] != true {
		t.Errorf("merged = %#v, want the plugin block unchanged", got)
	}
	if mergeCacheBlock(nil, nil) != nil {
		t.Error("nil over nil should stay nil")
	}
}

// A set-but-empty role block overrides no key but is still a statement.
func TestParseMergedCache_EmptyRoleBlockIsAStatement(t *testing.T) {
	cs, err := parseMergedCache(map[string]any{"enabled": true, "ttl": "10m"}, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("parseMergedCache: unexpected error: %v", err)
	}
	if !cs.enabled || cs.ttl != 10*time.Minute {
		t.Fatalf("cs = %#v, want the plugin block to stand", cs)
	}

	cs, err = parseMergedCache(nil, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("parseMergedCache: unexpected error: %v", err)
	}
	if cs.enabled {
		t.Fatalf("cs = %#v, want caching off", cs)
	}
}

// --- validateRoleCache ------------------------------------------------------

func TestValidateRoleCache_UnknownKeyFailsInitNamingTheRole(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "cheap",
		"cheap": map[string]any{
			"provider": pluginID,
			"model":    "gemini-3-flash",
			"cache":    map[string]any{"enabled": true, "min_token": 1024},
		},
	})

	err := validateRoleCache(models, nil, quietLogger())
	if err == nil {
		t.Fatal("expected an unknown key on a role block to fail Init")
	}
	if !strings.Contains(err.Error(), `role "cheap"`) || !strings.Contains(err.Error(), "min_token") {
		t.Fatalf("error = %v, want it to name the role and the key", err)
	}
}

func TestValidateRoleCache_WrongTypeFailsInit(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "cheap",
		"cheap": map[string]any{
			"provider": pluginID,
			"model":    "gemini-3-flash",
			"cache":    map[string]any{"enabled": "yes"},
		},
	})

	if err := validateRoleCache(models, nil, quietLogger()); err == nil {
		t.Fatal("expected a wrongly typed key to fail Init")
	}
}

func TestValidateRoleCache_SkipsForeignProviders(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "cheap",
		"cheap": map[string]any{
			"provider": "nexus.llm.anthropic",
			"model":    "claude-haiku-4-5",
			"cache":    map[string]any{"message_prefix": 2},
		},
	})

	if err := validateRoleCache(models, nil, quietLogger()); err != nil {
		t.Fatalf("foreign entry should be skipped, got %v", err)
	}
	if err := validateRoleCache(nil, nil, quietLogger()); err != nil {
		t.Fatalf("nil registry: unexpected error: %v", err)
	}
}

func TestValidateRoleCache_WalksTheWholeChain(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": []any{
			map[string]any{"provider": pluginID, "model": "gemini-3-pro", "cache": map[string]any{"enabled": true}},
			map[string]any{"provider": pluginID, "model": "gemini-3-flash", "cache": map[string]any{"enabled": true, "tll": "1h"}},
		},
	})

	err := validateRoleCache(models, nil, quietLogger())
	if err == nil {
		t.Fatal("expected the second chain entry's block to fail Init")
	}
	if !strings.Contains(err.Error(), `role "balanced"`) {
		t.Fatalf("error = %v, want it to name the role", err)
	}
}

// An unparseable TTL stays soft — defaulted, warned once at Init, naming the
// role — rather than failing the boot.
func TestValidateRoleCache_WarnsOnUnparseableTTL(t *testing.T) {
	var buf bytes.Buffer
	models := engine.NewModelRegistry(map[string]any{
		"default": "cheap",
		"cheap": map[string]any{
			"provider": pluginID,
			"model":    "gemini-3-flash",
			"cache":    map[string]any{"enabled": true, "ttl": "forever"},
		},
	})

	if err := validateRoleCache(models, nil, warnCaptureLogger(&buf)); err != nil {
		t.Fatalf("validateRoleCache: unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "cheap") || !strings.Contains(buf.String(), "forever") {
		t.Fatalf("warning = %q, want it to name the role and the value", buf.String())
	}
}

// --- per-role caching on the wire -------------------------------------------

// newRoleCachePlugin builds a plugin exactly as Init would for the given
// plugin-level block: the shared cache state and the raw block side by side.
func newRoleCachePlugin(t *testing.T, block map[string]any, models *engine.ModelRegistry) *Plugin {
	t.Helper()

	cfg := map[string]any{}
	if block != nil {
		cfg["cache"] = block
	}
	cs, err := newCacheState(cfg, quietLogger())
	if err != nil {
		t.Fatalf("newCacheState: unexpected error: %v", err)
	}
	return &Plugin{
		logger:   quietLogger(),
		models:   models,
		cache:    cs,
		cacheRaw: rawCacheBlock(cfg),
	}
}

// resolveCacheFor runs the same two steps handleRequest does — recover the
// role's block onto the request, then resolve it — and reports the policy the
// body builder would use.
func resolveCacheFor(p *Plugin, req events.LLMRequest) cacheSettings {
	p.applyEntryOverrides(&req)
	return p.resolveCache(req)
}

// The named-role path: a plain single-entry role's `cache:` reaches the
// resolution point with no coordinator involved.
func TestRoleCache_NamedRoleReachesTheRequest(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "cheap",
		"cheap": map[string]any{
			"provider": pluginID,
			"model":    "gemini-3-flash",
			"cache":    map[string]any{"enabled": true, "min_tokens": 2048},
		},
	})
	p := newRoleCachePlugin(t, nil, models)

	cs := resolveCacheFor(p, events.LLMRequest{Role: "cheap"})
	if !cs.enabled || cs.minTokens != 2048 {
		t.Fatalf("cs = %#v, want the role's block", cs)
	}
	if p.cache.enabled {
		t.Errorf("plugin-level state was mutated: %#v", p.cache.cacheSettings)
	}
}

// The role merges over the plugin block rather than replacing it.
func TestRoleCache_MergesOverThePluginBlock(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "cheap",
		"cheap": map[string]any{
			"provider": pluginID,
			"model":    "gemini-3-flash",
			"cache":    map[string]any{"ttl": "10m"},
		},
	})
	p := newRoleCachePlugin(t, map[string]any{"enabled": true, "min_tokens": 4096}, models)

	cs := resolveCacheFor(p, events.LLMRequest{Role: "cheap"})
	if cs.ttl != 10*time.Minute {
		t.Errorf("ttl = %v, want the role's 10m", cs.ttl)
	}
	if !cs.enabled || cs.minTokens != 4096 {
		t.Errorf("cs = %#v, want the plugin's enabled + min_tokens to survive", cs)
	}
}

// A role can opt out of a deployment-wide cache, which is the one axis that
// changes the request path today.
func TestRoleCache_RoleTurnsCachingOff(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "cheap",
		"cheap": map[string]any{
			"provider": pluginID,
			"model":    "gemini-3-flash",
			"cache":    map[string]any{"enabled": false},
		},
	})
	p := newRoleCachePlugin(t, map[string]any{"enabled": true}, models)

	contents := []map[string]any{{"role": "user", "parts": []map[string]any{{"text": "hi"}}}}
	p.cache.populate("gemini-3-flash", "sys", nil, contents, "cachedContents/abc", time.Hour)

	if cs := resolveCacheFor(p, events.LLMRequest{Role: "cheap"}); cs.enabled {
		t.Fatal("expected the role's enabled: false to win")
	}
	if got := p.cache.lookupWith(false, "gemini-3-flash", "sys", nil, contents); got != "" {
		t.Fatalf("lookupWith(false) = %q, want no cache reference", got)
	}
	// The entry map is shared, so a role that DOES cache still sees it.
	if got := p.cache.lookupWith(true, "gemini-3-flash", "sys", nil, contents); got != "cachedContents/abc" {
		t.Fatalf("lookupWith(true) = %q, want the shared entry", got)
	}
}

// The default-role path.
func TestRoleCache_DefaultRolePath(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "cheap",
		"cheap": map[string]any{
			"provider": pluginID,
			"model":    "gemini-3-flash",
			"cache":    map[string]any{"enabled": true},
		},
	})
	p := newRoleCachePlugin(t, nil, models)

	if cs := resolveCacheFor(p, events.LLMRequest{}); !cs.enabled {
		t.Fatal("expected the default role's block to be picked up")
	}
}

// The provider-native axes do not fall through from the default role to a named
// one.
func TestRoleCache_NamedRoleDoesNotInheritTheDefaultRoleBlock(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "premium",
		"premium": map[string]any{
			"provider": pluginID,
			"model":    "gemini-3-pro",
			"cache":    map[string]any{"enabled": true},
		},
		"cheap": map[string]any{"provider": pluginID, "model": "gemini-3-flash"},
	})
	p := newRoleCachePlugin(t, nil, models)

	if cs := resolveCacheFor(p, events.LLMRequest{Role: "cheap"}); cs.enabled {
		t.Fatal("a named role must not inherit the default role's cache block")
	}
}

// The router-rewrite path.
func TestRoleCache_SurvivesARouterRewrite(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "cheap",
		"cheap": map[string]any{
			"provider": pluginID,
			"model":    "gemini-3-flash",
			"cache":    map[string]any{"enabled": true},
		},
	})
	p := newRoleCachePlugin(t, nil, models)

	req := events.LLMRequest{Role: "cheap", Model: "gemini-3-pro"}
	if cs := resolveCacheFor(p, req); !cs.enabled {
		t.Fatal("expected the role's cache block to survive a model rewrite")
	}
}

// The fallback/fanout path: a stamped request short-circuits the registry, so
// the SECOND chain entry's block governs.
func TestRoleCache_StampedChainEntryWins(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": []any{
			map[string]any{"provider": pluginID, "model": "gemini-3-pro", "cache": map[string]any{"enabled": false}},
			map[string]any{"provider": pluginID, "model": "gemini-3-flash", "cache": map[string]any{"enabled": true, "min_tokens": 512}},
		},
	})
	p := newRoleCachePlugin(t, nil, models)

	req := events.LLMRequest{Role: "balanced"}
	second, ok := models.Fallback("balanced", 1)
	if !ok {
		t.Fatal("expected a second chain entry")
	}
	engine.StampModelConfig(&req, second)

	cs := resolveCacheFor(p, req)
	if !cs.enabled || cs.minTokens != 512 {
		t.Fatalf("cs = %#v, want the served entry's block", cs)
	}
}

// A request carrying a block nothing validated degrades to the plugin-level
// configuration, and says so once.
func TestResolveCache_InvalidRequestBlockDegrades(t *testing.T) {
	var buf bytes.Buffer
	cs, err := newCacheState(map[string]any{"cache": map[string]any{"enabled": true}}, quietLogger())
	if err != nil {
		t.Fatalf("newCacheState: unexpected error: %v", err)
	}
	p := &Plugin{
		logger:   warnCaptureLogger(&buf),
		cache:    cs,
		cacheRaw: map[string]any{"enabled": true},
	}

	got := p.resolveCache(events.LLMRequest{
		Role:      "hand-set",
		Overrides: events.ModelOverrides{Cache: map[string]any{"nonsense": true}},
	})

	if !got.enabled {
		t.Fatalf("cs = %#v, want the plugin-level configuration", got)
	}
	if !strings.Contains(buf.String(), "hand-set") {
		t.Fatalf("warning = %q, want it to name the role", buf.String())
	}
}
