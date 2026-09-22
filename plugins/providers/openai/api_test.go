package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file covers the `api:` selector — which OpenAI API a deployment speaks —
// as a plugin key, as a `core.models` per-entry key, and as the one input to
// the endpoint choice.
//
// The Responses path itself does not exist yet, so an explicit `api: responses`
// is an Init error rather than a request shaped for one API and posted to
// another. The narrowing that keeps declared-compat endpoints on
// chat_completions is written and tested here so it is already correct when the
// default flips.

// initAPI runs a real Init with the given plugin config and model registry
// against a real bus, capturing every log record Init emits.
func initAPI(t *testing.T, cfg map[string]any, models *engine.ModelRegistry) (*Plugin, []map[string]any, error) {
	t.Helper()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.Level(-16)}))

	if _, ok := cfg["api_key"]; !ok {
		cfg["api_key"] = "sk-not-used"
	}

	p := &Plugin{}
	err := p.Init(engine.PluginContext{
		Config: cfg,
		Bus:    engine.NewEventBus(),
		Logger: logger,
		Models: models,
	})
	if err == nil {
		t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	}

	var records []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if decodeErr := json.Unmarshal([]byte(line), &rec); decodeErr != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		records = append(records, rec)
	}
	return p, records, err
}

// apiModels builds a registry of single-entry roles, one per map key, merged
// over the provider/model skeleton.
func apiModels(entries map[string]map[string]any) *engine.ModelRegistry {
	raw := map[string]any{"default": "balanced"}
	for role, extra := range entries {
		cfg := map[string]any{"provider": pluginID, "model": "gpt-5"}
		for k, v := range extra {
			cfg[k] = v
		}
		raw[role] = cfg
	}
	return engine.NewModelRegistry(raw)
}

func countWarnings(records []map[string]any, substr string) int {
	n := 0
	for _, rec := range records {
		msg, _ := rec["msg"].(string)
		level, _ := rec["level"].(string)
		if level == "WARN" && strings.Contains(msg, substr) {
			n++
		}
	}
	return n
}

// --- the narrowed default ---------------------------------------------------

// A plain api.openai.com deployment gets the unnarrowed default.
func TestDefaultAPI_PlainOpenAI(t *testing.T) {
	a := &authState{mode: authModeOpenAI}
	if got := defaultAPI(a); got != unnarrowedDefaultAPI {
		t.Errorf("defaultAPI = %q, want %q", got, unnarrowedDefaultAPI)
	}
	if a.narrowsToChatCompletions() {
		t.Error("plain api.openai.com must not narrow")
	}
}

// base_url is documented as the override for proxies and OpenAI-compatible
// endpoints, which implement /chat/completions and mostly not /responses.
func TestNarrowsToChatCompletions_BaseURL(t *testing.T) {
	a := &authState{mode: authModeOpenAI, baseURL: "http://localhost:11434/v1/chat/completions"}
	if !a.narrowsToChatCompletions() {
		t.Error("a declared base_url must narrow the default to chat_completions")
	}
	if got := defaultAPI(a); got != apiChatCompletions {
		t.Errorf("defaultAPI = %q, want chat_completions", got)
	}
}

// Azure's Responses surface is a different route shape entirely, so neither
// Azure mode inherits the plain default.
func TestNarrowsToChatCompletions_AzureModes(t *testing.T) {
	for _, mode := range []authMode{authModeAzureKey, authModeAzureAAD} {
		a := &authState{mode: mode}
		if !a.narrowsToChatCompletions() {
			t.Errorf("auth_mode %q must narrow the default to chat_completions", mode)
		}
		if got := defaultAPI(a); got != apiChatCompletions {
			t.Errorf("auth_mode %q: defaultAPI = %q, want chat_completions", mode, got)
		}
	}
}

// --- the plugin key ---------------------------------------------------------

func TestParseAPIConfig_UnsetTakesTheDefault(t *testing.T) {
	a := &authState{mode: authModeOpenAI}
	got, err := parseAPIConfig(map[string]any{}, a)
	if err != nil {
		t.Fatalf("parseAPIConfig: %v", err)
	}
	if got != defaultAPI(a) {
		t.Errorf("api = %q, want the default %q", got, defaultAPI(a))
	}
}

func TestParseAPIConfig_ExplicitChatCompletions(t *testing.T) {
	got, err := parseAPIConfig(map[string]any{"api": "chat_completions"}, &authState{mode: authModeOpenAI})
	if err != nil {
		t.Fatalf("parseAPIConfig: %v", err)
	}
	if got != apiChatCompletions {
		t.Errorf("api = %q, want chat_completions", got)
	}
}

// The honest failure: `api: responses` is recognised, and refused by name,
// until the path that speaks it exists.
func TestParseAPIConfig_ResponsesIsNotImplementedYet(t *testing.T) {
	_, err := parseAPIConfig(map[string]any{"api": "responses"}, &authState{mode: authModeOpenAI})
	if err == nil {
		t.Fatal("expected api: responses to fail Init while the path is unimplemented")
	}
	if !strings.Contains(err.Error(), "not implemented yet") || !strings.Contains(err.Error(), "v0.29.0") {
		t.Errorf("error must name the gap and the release: %v", err)
	}
}

func TestParseAPIConfig_UnknownValueErrors(t *testing.T) {
	_, err := parseAPIConfig(map[string]any{"api": "assistants"}, &authState{mode: authModeOpenAI})
	if err == nil {
		t.Fatal("expected an unknown api to fail Init")
	}
	if !strings.Contains(err.Error(), "chat_completions") {
		t.Errorf("error must name the accepted set: %v", err)
	}
}

// --- the per-entry key ------------------------------------------------------

// A role's `api:` bypasses schema.json entirely, so this sweep is the only
// thing that checks it — and it checks it at boot, naming the role.
func TestValidateRoleAPI_UnknownValueFailsBootNamingTheRole(t *testing.T) {
	models := apiModels(map[string]map[string]any{
		"balanced": {},
		"deep":     {"api": "assistants"},
	})
	err := validateRoleAPI(models)
	if err == nil {
		t.Fatal("expected an unknown role api to fail the boot")
	}
	if !strings.Contains(err.Error(), `"deep"`) {
		t.Errorf("error must name the role: %v", err)
	}
}

func TestValidateRoleAPI_ResponsesFailsBootNamingTheRole(t *testing.T) {
	models := apiModels(map[string]map[string]any{
		"balanced": {},
		"deep":     {"api": "responses"},
	})
	err := validateRoleAPI(models)
	if err == nil {
		t.Fatal("expected a role's api: responses to fail the boot")
	}
	if !strings.Contains(err.Error(), `"deep"`) || !strings.Contains(err.Error(), "not implemented yet") {
		t.Errorf("error must name the role and the gap: %v", err)
	}
}

// An entry naming another provider is that provider's business.
func TestValidateRoleAPI_ForeignEntriesAreSkipped(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": map[string]any{
			"provider": "nexus.llm.anthropic",
			"model":    "claude-opus-4-7",
			"api":      "messages",
		},
	})
	if err := validateRoleAPI(models); err != nil {
		t.Errorf("a foreign entry's api is not this provider's to validate: %v", err)
	}
}

// The whole chain is swept, not just the primary: a non-first entry reaches a
// provider through the fallback or fanout stamp and is just as capable of being
// wrong.
func TestValidateRoleAPI_NonFirstChainEntryIsChecked(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": []any{
			map[string]any{"provider": pluginID, "model": "gpt-5"},
			map[string]any{"provider": pluginID, "model": "gpt-5-mini", "api": "assistants"},
		},
	})
	if err := validateRoleAPI(models); err == nil {
		t.Fatal("expected the second chain entry's api to be checked")
	}
}

func TestValidateRoleAPI_NilRegistryIsFine(t *testing.T) {
	if err := validateRoleAPI(nil); err != nil {
		t.Errorf("a nil registry is not an error: %v", err)
	}
}

// --- per-request resolution -------------------------------------------------

// The per-entry value wins over the plugin key.
func TestResolveAPI_EntryWinsOverPlugin(t *testing.T) {
	p := &Plugin{logger: silentLogger(), api: apiChatCompletions}
	req := events.LLMRequest{Overrides: events.ModelOverrides{API: string(apiResponses)}}
	if got := p.resolveAPI(req); got != apiResponses {
		t.Errorf("resolveAPI = %q, want the entry's responses", got)
	}
}

func TestResolveAPI_UnsetFallsBackToThePlugin(t *testing.T) {
	p := &Plugin{logger: silentLogger(), api: apiChatCompletions}
	if got := p.resolveAPI(events.LLMRequest{}); got != apiChatCompletions {
		t.Errorf("resolveAPI = %q, want the plugin's chat_completions", got)
	}
}

// A value nothing here can honour degrades to the plugin's rather than
// reaching resolveEndpoint as an unknown surface.
func TestResolveAPI_UnrecognisedOverrideDegrades(t *testing.T) {
	p := &Plugin{logger: silentLogger(), api: apiChatCompletions}
	req := events.LLMRequest{Overrides: events.ModelOverrides{API: "assistants"}}
	if got := p.resolveAPI(req); got != apiChatCompletions {
		t.Errorf("resolveAPI = %q, want the plugin's chat_completions", got)
	}
}

// api is a provider-native axis, so a named role that sets none does NOT
// inherit the default role's — the split E1-S3 established.
func TestApplyEntryOverrides_APIDoesNotFallThroughToTheDefaultRole(t *testing.T) {
	models := engine.NewModelRegistry(map[string]any{
		"default": "balanced",
		"balanced": map[string]any{
			"provider": pluginID, "model": "gpt-5", "api": "chat_completions",
		},
		"deep": map[string]any{"provider": pluginID, "model": "gpt-5-pro"},
	})
	p := &Plugin{logger: silentLogger(), models: models, api: apiChatCompletions}

	req := events.LLMRequest{Role: "deep"}
	p.applyEntryOverrides(&req)
	if req.Overrides.API != "" {
		t.Errorf("Overrides.API = %q, want empty — a native axis must not fall through", req.Overrides.API)
	}

	// The default role's own request does pick it up, which is what "the
	// default role" means.
	req = events.LLMRequest{}
	p.applyEntryOverrides(&req)
	if req.Overrides.API != "chat_completions" {
		t.Errorf("Overrides.API = %q, want the default role's chat_completions", req.Overrides.API)
	}
}

// A coordinator's stamp is final: the registry is not consulted again.
func TestApplyEntryOverrides_StampWins(t *testing.T) {
	models := apiModels(map[string]map[string]any{
		"balanced": {"api": "chat_completions"},
	})
	p := &Plugin{logger: silentLogger(), models: models, api: apiChatCompletions}

	req := events.LLMRequest{Role: "balanced"}
	engine.StampModelConfig(&req, engine.ModelConfig{API: "responses"})
	p.applyEntryOverrides(&req)
	if req.Overrides.API != "responses" {
		t.Errorf("Overrides.API = %q, want the stamped responses", req.Overrides.API)
	}
}

// --- the endpoint -----------------------------------------------------------

// Responses has no URL yet on any auth mode; refusing by name beats guessing a
// route shape nothing exercises.
func TestResolveEndpoint_ResponsesIsRefused(t *testing.T) {
	for _, a := range []*authState{
		{mode: authModeOpenAI},
		{mode: authModeAzureKey, resource: "r", deployment: "d", apiVersion: "v"},
	} {
		if _, err := a.resolveEndpoint(apiResponses); err == nil {
			t.Errorf("auth_mode %q: expected resolveEndpoint(responses) to error", a.mode)
		}
	}
}

func TestResolveEndpoint_UnknownSurfaceErrors(t *testing.T) {
	a := &authState{mode: authModeOpenAI}
	if _, err := a.resolveEndpoint(apiSurface("assistants")); err == nil {
		t.Fatal("expected an unknown surface to error")
	}
}

// --- Init -------------------------------------------------------------------

// The tree must be shippable at every commit: an ordinary deployment that names
// no `api:` boots, and lands on the chat surface.
func TestInit_PlainDeploymentBoots(t *testing.T) {
	p, _, err := initAPI(t, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.api != apiChatCompletions {
		t.Errorf("api = %q, want chat_completions", p.api)
	}
}

func TestInit_ExplicitResponsesFailsBoot(t *testing.T) {
	_, _, err := initAPI(t, map[string]any{"api": "responses"}, nil)
	if err == nil {
		t.Fatal("expected Init to refuse api: responses")
	}
}

func TestInit_RoleResponsesFailsBoot(t *testing.T) {
	models := apiModels(map[string]map[string]any{
		"balanced": {},
		"deep":     {"api": "responses"},
	})
	if _, _, err := initAPI(t, map[string]any{}, models); err == nil {
		t.Fatal("expected Init to refuse a role's api: responses")
	}
}

func TestInit_BaseURLNarrowsTheDefault(t *testing.T) {
	p, _, err := initAPI(t, map[string]any{"base_url": "http://localhost:11434/v1/chat/completions"}, nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.api != apiChatCompletions {
		t.Errorf("api = %q, want chat_completions", p.api)
	}
}

// --- the GPT-5.4 warning ----------------------------------------------------

const restrictionPhrase = "refuses tool calling"

// Reasoning configured on the chat surface is reasoning a current model will
// not give you, and Init says so once.
func TestInit_WarnsOnceWhenReasoningMeetsChatCompletions(t *testing.T) {
	_, records, err := initAPI(t, map[string]any{
		"reasoning": map[string]any{"mode": "effort", "effort": "high"},
	}, nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if n := countWarnings(records, restrictionPhrase); n != 1 {
		t.Errorf("warning count = %d, want exactly 1", n)
	}
}

// A role can declare the reasoning the plugin did not, and the warning still
// fires — once, naming the role.
func TestInit_WarnsForARoleThatReasons(t *testing.T) {
	models := apiModels(map[string]map[string]any{
		"balanced": {},
		"deep":     {"reasoning": map[string]any{"mode": "effort", "effort": "high"}},
		"deeper":   {"reasoning": map[string]any{"mode": "effort", "effort": "max"}},
	})
	_, records, err := initAPI(t, map[string]any{}, models)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if n := countWarnings(records, restrictionPhrase); n != 1 {
		t.Errorf("warning count = %d, want exactly 1", n)
	}
}

// No reasoning anywhere, nothing to warn about.
func TestInit_NoWarningWithoutReasoning(t *testing.T) {
	_, records, err := initAPI(t, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if n := countWarnings(records, restrictionPhrase); n != 0 {
		t.Errorf("warning count = %d, want 0", n)
	}
}

// mode: off is a deployment that asked for no reasoning at all.
func TestInit_NoWarningUnderModeOff(t *testing.T) {
	_, records, err := initAPI(t, map[string]any{
		"reasoning": map[string]any{"mode": "off"},
	}, nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if n := countWarnings(records, restrictionPhrase); n != 0 {
		t.Errorf("warning count = %d, want 0", n)
	}
}
