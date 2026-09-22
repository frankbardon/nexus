package gemini

import (
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file covers the step between a `core.models` entry's `retry:` block and
// the retry loop.
//
// `retry` is the one per-entry axis that never reaches the request body: it is
// call-time behaviour, not a request field, so the only way a role's block can
// mean anything is for doWithRetry to be driven by the resolved configuration
// rather than the plugin's. That is what these tests pin — the value arriving
// on the request is necessary but not sufficient.

// --- mergeRetryBlock --------------------------------------------------------

// The role wins on every key it names and a plugin key it is silent about
// survives, so a role that only wants fewer attempts keeps the plugin's backoff
// shape and status list.
func TestMergeRetryBlock_RoleWinsPerKey(t *testing.T) {
	plugin := map[string]any{"max_retries": 5, "backoff": "constant", "initial_delay": "2s"}
	role := map[string]any{"max_retries": 1}

	merged := mergeRetryBlock(plugin, role)

	if merged["max_retries"] != 1 {
		t.Errorf("max_retries = %v, want the role's 1", merged["max_retries"])
	}
	if merged["backoff"] != "constant" || merged["initial_delay"] != "2s" {
		t.Errorf("merged = %#v, want the plugin's backoff shape to survive", merged)
	}
	if plugin["max_retries"] != 5 {
		t.Errorf("plugin block was mutated: %#v", plugin)
	}
}

// A nil role block leaves the plugin block exactly as it was; nil over nil
// stays nil, because "said nothing" must not become "set an empty block".
func TestMergeRetryBlock_NilRole(t *testing.T) {
	plugin := map[string]any{"max_retries": 2}
	if got := mergeRetryBlock(plugin, nil); got["max_retries"] != 2 {
		t.Errorf("merged = %#v, want the plugin block unchanged", got)
	}
	if mergeRetryBlock(nil, nil) != nil {
		t.Error("nil over nil should stay nil")
	}
}

// A set-but-empty role block is a statement, not a gap: it overrides no key,
// but the merged block is non-nil and goes through the parser — so on a
// deployment with no plugin block at all it still turns retrying on.
func TestParseMergedRetry_EmptyRoleBlockIsAStatement(t *testing.T) {
	rc, err := parseMergedRetry(map[string]any{"max_retries": 7}, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("parseMergedRetry: unexpected error: %v", err)
	}
	if !rc.Enabled || rc.MaxRetries != 7 {
		t.Fatalf("rc = %#v, want the plugin block to stand", rc)
	}

	rc, err = parseMergedRetry(nil, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("parseMergedRetry: unexpected error: %v", err)
	}
	if !rc.Enabled || rc.MaxRetries != defaultRetryConfig().MaxRetries {
		t.Fatalf("rc = %#v, want retrying on with the built-in defaults", rc)
	}
}

// A role can turn retrying on for itself on a deployment whose plugin block is
// absent entirely.
func TestParseMergedRetry_RoleTurnsRetryingOn(t *testing.T) {
	rc, err := parseMergedRetry(nil, map[string]any{"max_retries": 1, "backoff": "constant"}, nil)
	if err != nil {
		t.Fatalf("parseMergedRetry: unexpected error: %v", err)
	}
	if !rc.Enabled || rc.MaxRetries != 1 || rc.Backoff != BackoffConstant {
		t.Fatalf("rc = %#v, want retrying on with the role's shape", rc)
	}
}

// YAML surfaces integers as int or float64 depending on path, and a multiplier
// written without a decimal point used to be dropped in silence.
func TestParseMergedRetry_AcceptsIntAndFloatNumbers(t *testing.T) {
	rc, err := parseMergedRetry(nil, map[string]any{
		"max_retries": float64(4),
		"multiplier":  3,
		"statuses":    []any{float64(418), 503},
	}, nil)
	if err != nil {
		t.Fatalf("parseMergedRetry: unexpected error: %v", err)
	}
	if rc.MaxRetries != 4 || rc.Multiplier != 3 {
		t.Fatalf("rc = %#v, want both number shapes honoured", rc)
	}
	if !rc.RetryableStatuses[418] || !rc.RetryableStatuses[503] {
		t.Fatalf("statuses = %v, want both entries", rc.RetryableStatuses)
	}
}

// --- validateRoleRetry ------------------------------------------------------

// A typo on a role block has nothing else to catch it — core stores these maps
// without looking inside them and schema.json never sees them — so Init refuses
// the boot, naming the role.
func TestValidateRoleRetry_UnknownKeyFailsInitNamingTheRole(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": map[string]any{
			"provider": pluginID,
			"model":    "gemini-2.5-pro",
			"retry":    map[string]any{"max_retry": 1},
		},
	})

	err := validateRoleRetry(models, nil, quietLogger())
	if err == nil {
		t.Fatal("expected an unknown key on a role block to fail Init")
	}
	if !strings.Contains(err.Error(), `role "balanced"`) || !strings.Contains(err.Error(), "max_retry") {
		t.Fatalf("error = %v, want it to name the role and the key", err)
	}
}

// A wrongly typed key is the other class schema.json would have caught.
func TestValidateRoleRetry_WrongTypeFailsInit(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": map[string]any{
			"provider": pluginID,
			"model":    "gemini-2.5-pro",
			"retry":    map[string]any{"max_retries": "lots"},
		},
	})

	if err := validateRoleRetry(models, nil, quietLogger()); err == nil {
		t.Fatal("expected a wrongly typed key on a role block to fail Init")
	}
}

// An entry naming another provider is that provider's business, and a nil
// registry is the embedder who configured no roles at all.
func TestValidateRoleRetry_ForeignEntriesAndNilRegistry(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": map[string]any{
			"provider": "nexus.llm.somebody-else",
			"model":    "whatever",
			"retry":    map[string]any{"max_retry": 1},
		},
	})

	if err := validateRoleRetry(models, nil, quietLogger()); err != nil {
		t.Fatalf("a foreign entry should be skipped, got %v", err)
	}
	if err := validateRoleRetry(nil, nil, quietLogger()); err != nil {
		t.Fatalf("a nil registry should be fine, got %v", err)
	}
}

// The whole chain is swept, not just the primary: a fallback entry's block
// reaches this provider through the coordinator's stamp.
func TestValidateRoleRetry_WalksTheWholeChain(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": []any{
			map[string]any{"provider": pluginID, "model": "gemini-2.5-pro"},
			map[string]any{"provider": pluginID, "model": "gemini-2.5-flash", "retry": map[string]any{"backoff": 7}},
		},
	})

	err := validateRoleRetry(models, nil, quietLogger())
	if err == nil {
		t.Fatal("expected a broken non-primary entry to fail Init")
	}
	if !strings.Contains(err.Error(), `role "balanced"`) {
		t.Fatalf("error = %v, want it to name the role", err)
	}
}

// --- resolveRetry -----------------------------------------------------------

// retryModels builds a registry of single-entry roles, one per map key. A nil
// block omits the `retry` key entirely, which is how a role that says nothing
// about retrying is configured.
func retryModels(entries map[string]map[string]any) *engine.ModelRegistry {
	raw := map[string]any{"default": "balanced"}
	for role, block := range entries {
		cfg := map[string]any{"provider": pluginID, "model": "gemini-2.5-pro", "max_tokens": 2048}
		if block != nil {
			cfg["retry"] = block
		}
		raw[role] = cfg
	}
	return engine.NewModelRegistry(raw)
}

// retryPlugin builds the minimum Plugin resolveRetry runs against, with the
// plugin-level block parsed exactly as Init parses it.
func retryPlugin(t *testing.T, models *engine.ModelRegistry, block map[string]any) *Plugin {
	t.Helper()

	cfg := map[string]any{}
	if block != nil {
		cfg["retry"] = block
	}
	rc, err := parseRetryConfig(cfg, nil)
	if err != nil {
		t.Fatalf("parseRetryConfig: unexpected error: %v", err)
	}
	return &Plugin{logger: quietLogger(), models: models, retry: rc, retryRaw: rawRetryBlock(cfg)}
}

// resolveRetryFor runs the same two steps handleRequest does — recover the
// serving entry onto the request, then resolve the block — so a regression that
// unwires applyEntryOverrides from the retry loop cannot hide behind the
// helper.
func resolveRetryFor(p *Plugin, req events.LLMRequest) retryConfig {
	p.applyEntryOverrides(&req)
	return p.resolveRetry(req)
}

// Path 1: the role the request names.
func TestRoleRetry_NamedRoleReachesTheLoop(t *testing.T) {
	p := retryPlugin(t, retryModels(map[string]map[string]any{
		"balanced": nil,
		"quick":    {"max_retries": 1},
	}), map[string]any{"max_retries": 5, "backoff": "constant", "initial_delay": "3s"})

	rc := resolveRetryFor(p, events.LLMRequest{Role: "quick"})
	if rc.MaxRetries != 1 {
		t.Fatalf("max_retries = %d, want the role's 1", rc.MaxRetries)
	}
	if rc.Backoff != BackoffConstant || rc.InitialDelay != 3*time.Second {
		t.Fatalf("rc = %#v, want the plugin's backoff shape to survive the merge", rc)
	}
}

// Path 2: no role at all, which resolves the default role's entry.
func TestRoleRetry_DefaultRoleReachesTheLoop(t *testing.T) {
	p := retryPlugin(t, retryModels(map[string]map[string]any{
		"balanced": {"max_retries": 2},
	}), map[string]any{"max_retries": 5})

	if rc := resolveRetryFor(p, events.LLMRequest{}); rc.MaxRetries != 2 {
		t.Fatalf("max_retries = %d, want the default role's 2", rc.MaxRetries)
	}
}

// Path 3: late recovery — a router rewrote `model` and left `role` alone.
func TestRoleRetry_RecoveredAfterModelRewrite(t *testing.T) {
	p := retryPlugin(t, retryModels(map[string]map[string]any{
		"balanced": nil,
		"quick":    {"max_retries": 1},
	}), map[string]any{"max_retries": 5})

	rc := resolveRetryFor(p, events.LLMRequest{Role: "quick", Model: "gemini-2.5-flash"})
	if rc.MaxRetries != 1 {
		t.Fatalf("max_retries = %d, want the role's 1", rc.MaxRetries)
	}
}

// Path 4: the fallback coordinator's stamp, which names the chain entry
// actually being served — the registry would answer with the first one.
func TestRoleRetry_FallbackStampWins(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": []any{
			map[string]any{"provider": pluginID, "model": "gemini-2.5-pro", "retry": map[string]any{"max_retries": 1}},
			map[string]any{"provider": pluginID, "model": "gemini-2.5-flash", "retry": map[string]any{"max_retries": 9}},
		},
	})
	p := retryPlugin(t, models, map[string]any{"max_retries": 5})

	second, ok := models.Fallback("balanced", 1)
	if !ok {
		t.Fatal("expected a second chain entry")
	}
	req := events.LLMRequest{Role: "balanced", Model: "gemini-2.5-flash"}
	engine.StampModelConfig(&req, second)

	if rc := resolveRetryFor(p, req); rc.MaxRetries != 9 {
		t.Fatalf("max_retries = %d, want the stamped entry's 9", rc.MaxRetries)
	}
}

// Path 5: a fanout leg, stamped the same way.
func TestRoleRetry_FanoutLegStampWins(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": map[string]any{
			"fanout": true,
			"providers": []any{
				map[string]any{"provider": pluginID, "model": "gemini-2.5-pro"},
				map[string]any{"provider": pluginID, "model": "gemini-2.5-flash", "retry": map[string]any{"max_retries": 0}},
			},
		},
	})
	p := retryPlugin(t, models, map[string]any{"max_retries": 5})

	legs := models.FanoutProviders("balanced")
	if len(legs) != 2 {
		t.Fatalf("expected 2 fanout legs, got %d", len(legs))
	}
	req := events.LLMRequest{Role: "balanced", Model: "gemini-2.5-flash"}
	engine.StampModelConfig(&req, legs[1])

	if rc := resolveRetryFor(p, req); rc.MaxRetries != 0 {
		t.Fatalf("max_retries = %d, want the stamped leg's 0", rc.MaxRetries)
	}
}

// A named role that says nothing about retrying gets the plugin block
// unchanged. Native blocks never fall through from a named role to the default
// role's entry — a different role may well be a different provider.
func TestRoleRetry_SilentRoleKeepsThePluginConfig(t *testing.T) {
	p := retryPlugin(t, retryModels(map[string]map[string]any{
		"balanced": {"max_retries": 1},
		"plain":    nil,
	}), map[string]any{"max_retries": 5})

	if rc := resolveRetryFor(p, events.LLMRequest{Role: "plain"}); rc.MaxRetries != 5 {
		t.Fatalf("max_retries = %d, want the plugin's 5", rc.MaxRetries)
	}
}

// Init sweeps every role, so an unparseable merged block is unreachable through
// configuration. A block hand-set by something other than the registry degrades
// to the plugin configuration rather than to a policy nobody wrote.
func TestRoleRetry_InvalidHandSetBlockFallsBackToThePlugin(t *testing.T) {
	p := retryPlugin(t, nil, map[string]any{"max_retries": 5})

	req := events.LLMRequest{Role: "balanced"}
	req.Overrides.Retry = map[string]any{"max_retry": 1}

	if rc := p.resolveRetry(req); rc.MaxRetries != 5 {
		t.Fatalf("max_retries = %d, want the plugin's 5", rc.MaxRetries)
	}
}
