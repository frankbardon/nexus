package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file covers the `api:` selector — which OpenAI API a deployment speaks —
// as a plugin key, as a `core.models` per-entry key, and as the one input to
// the endpoint choice.
//
// Both surfaces are reachable: an explicit `api: responses` resolves an endpoint
// and posts a Responses request to it. What has NOT flipped is the default — a
// deployment that declares nothing still speaks chat_completions, because
// encrypted-reasoning-item replay is not wired yet. The narrowing that keeps
// declared-compat endpoints on chat_completions is written and tested here so it
// is already correct when the default does flip.

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

// The half-flip: an explicit `api: responses` is now honoured as written. The
// *default* is unchanged — see TestDefaultAPI_StaysOnChatCompletions.
func TestParseAPIConfig_ExplicitResponsesIsHonoured(t *testing.T) {
	got, err := parseAPIConfig(map[string]any{"api": "responses"}, &authState{mode: authModeOpenAI})
	if err != nil {
		t.Fatalf("parseAPIConfig: %v", err)
	}
	if got != apiResponses {
		t.Errorf("api = %q, want responses", got)
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

// A role may declare `api: responses` and is taken at its word — the same
// half-flip as the plugin key.
func TestValidateRoleAPI_ResponsesIsAccepted(t *testing.T) {
	models := apiModels(map[string]map[string]any{
		"balanced": {},
		"deep":     {"api": "responses"},
	})
	if err := validateRoleAPI(models); err != nil {
		t.Errorf("a role's api: responses is a supported surface: %v", err)
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

// Every (auth_mode × api) pair, which is the whole of what resolveEndpoint
// decides. The Azure rows are the reason the function takes an api at all:
// Azure's Responses route is the versionless `/openai/v1/responses`, with no
// deployment in the path and no `api-version` query, which is a different URL
// shape from the deployment-scoped chat one rather than a suffix swap.
func TestResolveEndpoint_EveryAuthModeAndSurface(t *testing.T) {
	azure := func(mode authMode) *authState {
		return &authState{
			mode:       mode,
			resource:   "my-resource",
			deployment: "gpt-5-deploy",
			apiVersion: "2024-10-21",
		}
	}
	const azureChat = "https://my-resource.openai.azure.com/openai/deployments/gpt-5-deploy/chat/completions?api-version=2024-10-21"
	const azureResponses = "https://my-resource.openai.azure.com/openai/v1/responses"

	cases := []struct {
		name string
		auth *authState
		api  apiSurface
		want string
	}{
		{"openai/chat", &authState{mode: authModeOpenAI}, apiChatCompletions, apiURL},
		{"openai/responses", &authState{mode: authModeOpenAI}, apiResponses, "https://api.openai.com/v1/responses"},

		// base_url is the whole chat endpoint, so the Responses route is its
		// sibling: the /chat/completions tail comes off, /responses goes on.
		{
			"base_url chat endpoint/chat",
			&authState{mode: authModeOpenAI, baseURL: "https://proxy.example.com/v1/chat/completions"},
			apiChatCompletions, "https://proxy.example.com/v1/chat/completions",
		},
		{
			"base_url chat endpoint/responses",
			&authState{mode: authModeOpenAI, baseURL: "https://proxy.example.com/v1/chat/completions"},
			apiResponses, "https://proxy.example.com/v1/responses",
		},
		{
			"base_url API root/responses",
			&authState{mode: authModeOpenAI, baseURL: "https://proxy.example.com/v1"},
			apiResponses, "https://proxy.example.com/v1/responses",
		},
		{
			"base_url trailing slash/responses",
			&authState{mode: authModeOpenAI, baseURL: "https://proxy.example.com/v1/"},
			apiResponses, "https://proxy.example.com/v1/responses",
		},
		{
			"base_url already the responses route",
			&authState{mode: authModeOpenAI, baseURL: "https://proxy.example.com/v1/responses"},
			apiResponses, "https://proxy.example.com/v1/responses",
		},

		{"azure_key/chat", azure(authModeAzureKey), apiChatCompletions, azureChat},
		{"azure_key/responses", azure(authModeAzureKey), apiResponses, azureResponses},
		{"azure_aad/chat", azure(authModeAzureAAD), apiChatCompletions, azureChat},
		{"azure_aad/responses", azure(authModeAzureAAD), apiResponses, azureResponses},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.auth.resolveEndpoint(tc.api)
			if err != nil {
				t.Fatalf("resolveEndpoint(%q): %v", tc.api, err)
			}
			if got != tc.want {
				t.Errorf("resolveEndpoint(%q) = %q, want %q", tc.api, got, tc.want)
			}
		})
	}
}

// The two things about the Azure Responses route that are easy to get wrong and
// silent when you do: the deployment must NOT be in the path (it rides the
// body's `model`), and the GA /openai/v1/ route is implicitly versioned, so no
// api-version query is sent even though azure.api_version is configured.
func TestResolveEndpoint_AzureResponsesCarriesNeitherDeploymentNorVersion(t *testing.T) {
	for _, mode := range []authMode{authModeAzureKey, authModeAzureAAD} {
		a := &authState{mode: mode, resource: "r", deployment: "my-deployment", apiVersion: "2024-10-21"}
		got := mustEndpoint(t, a, apiResponses)
		if strings.Contains(got, "my-deployment") || strings.Contains(got, "/deployments/") {
			t.Errorf("auth_mode %q: deployment must travel in the body, not the URL: %q", mode, got)
		}
		if strings.Contains(got, "api-version") {
			t.Errorf("auth_mode %q: the GA /openai/v1/ route takes no api-version: %q", mode, got)
		}
	}
}

// Azure AAD/MSI auth is orthogonal to the surface: the bearer token is attached
// to a Responses request exactly as it is to a chat one.
func TestResolveEndpoint_AzureAADAuthAppliesOnTheResponsesPath(t *testing.T) {
	srv := newAADTokenServer(t, "responses-token", 3600)
	defer srv.Close()

	a := &authState{
		mode:         authModeAzureAAD,
		resource:     "r",
		deployment:   "d",
		apiVersion:   "2024-10-21",
		tenantID:     "t",
		clientID:     "c",
		clientSecret: "s",
		aadTokenURL:  srv.URL,
	}
	endpoint := mustEndpoint(t, a, apiResponses)

	httpReq, err := http.NewRequest("POST", endpoint, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if err := a.applyAuth(context.Background(), httpReq, srv.Client()); err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	if got := httpReq.Header.Get("Authorization"); got != "Bearer responses-token" {
		t.Errorf("Authorization = %q, want the AAD bearer token", got)
	}
}

// api_key auth keeps its Azure spelling on the Responses route too.
func TestResolveEndpoint_AzureKeyAuthAppliesOnTheResponsesPath(t *testing.T) {
	a := &authState{mode: authModeAzureKey, resource: "r", deployment: "d", apiVersion: "v", apiKey: "k"}
	httpReq, err := http.NewRequest("POST", mustEndpoint(t, a, apiResponses), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if err := a.applyAuth(context.Background(), httpReq, http.DefaultClient); err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	if got := httpReq.Header.Get("api-key"); got != "k" {
		t.Errorf("api-key = %q, want k", got)
	}
	if got := httpReq.Header.Get("Authorization"); got != "" {
		t.Errorf("Azure key mode must not set Authorization, got %q", got)
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
// no `api:` boots, and lands on the Responses surface.
func TestInit_PlainDeploymentBoots(t *testing.T) {
	p, _, err := initAPI(t, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.api != apiResponses {
		t.Errorf("api = %q, want responses", p.api)
	}
}

func TestInit_ExplicitResponsesBoots(t *testing.T) {
	p, _, err := initAPI(t, map[string]any{"api": "responses"}, nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.api != apiResponses {
		t.Errorf("api = %q, want responses", p.api)
	}
}

func TestInit_RoleResponsesBoots(t *testing.T) {
	models := apiModels(map[string]map[string]any{
		"balanced": {},
		"deep":     {"api": "responses"},
	})
	if _, _, err := initAPI(t, map[string]any{}, models); err != nil {
		t.Fatalf("a role's api: responses must boot: %v", err)
	}
}

// The flip, pinned so nobody reverses it by accident: a deployment that
// declares nothing narrowing gets the Responses surface, because on current
// OpenAI models the chat surface cannot reason while tools are on the turn —
// which is every Nexus turn. Chat Completions remains one `api:` away.
//
// The two narrowed cases are the compensating half and are pinned separately
// (TestInit_BaseURLNarrowsTheDefault and the Azure ones): a declared-compat
// endpoint or an Azure auth mode does not move.
func TestDefaultAPI_IsResponses(t *testing.T) {
	if unnarrowedDefaultAPI != apiResponses {
		t.Fatalf("unnarrowedDefaultAPI = %q — a plain deployment must default to the surface that can reason with tools", unnarrowedDefaultAPI)
	}
	p, _, err := initAPI(t, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.api != apiResponses {
		t.Errorf("a deployment that declares no api gets %q, want responses", p.api)
	}
}

// And an operator who wants the older surface still gets it by saying so.
func TestDefaultAPI_ChatCompletionsIsOneKeyAway(t *testing.T) {
	p, _, err := initAPI(t, map[string]any{"api": "chat_completions"}, nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.api != apiChatCompletions {
		t.Errorf("api = %q, want chat_completions", p.api)
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

// --- the whole path, end to end ---------------------------------------------

// The story this file has been pinning open since E3-S4: a deployment that
// declares `api: responses` posts a Responses request to the Responses route
// and gets an llm.response back. Every earlier piece — serializer, reply parser,
// SSE reader, multimodal shapes — was reachable only from its own unit test
// until the endpoint existed.
//
// base_url points at the test server, which also exercises the rule that an
// explicit `api:` lifts the narrowing a base_url imposes.
func TestResponsesPath_ReachableEndToEnd(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "id": "resp_1",
		  "model": "gpt-5.1",
		  "status": "completed",
		  "output": [
		    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"pong"}]}
		  ],
		  "usage": {"input_tokens": 3, "output_tokens": 1}
		}`))
	}))
	defer srv.Close()

	bus := engine.NewEventBus()
	p := &Plugin{}
	if err := p.Init(engine.PluginContext{
		Config: map[string]any{
			"api_key":  "sk-not-used",
			"api":      "responses",
			"base_url": srv.URL + "/v1/chat/completions",
		},
		Bus:    bus,
		Logger: silentLogger(),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	var got events.LLMResponse
	unsub := bus.Subscribe("llm.response", func(ev engine.Event[any]) {
		got, _ = ev.Payload.(events.LLMResponse)
	})
	defer unsub()

	if err := bus.Emit("llm.request", events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Model:         "gpt-5.1",
		Messages:      []events.Message{{Role: "user", Content: "ping"}},
	}); err != nil {
		t.Fatalf("emit llm.request: %v", err)
	}

	if gotPath != "/v1/responses" {
		t.Errorf("posted to %q, want /v1/responses", gotPath)
	}
	// The Responses serializer, not the chat one: `input`, not `messages`.
	if !strings.Contains(gotBody, `"input"`) || strings.Contains(gotBody, `"messages"`) {
		t.Errorf("body is not a Responses request: %s", gotBody)
	}
	if got.Content != "pong" {
		t.Errorf("llm.response Content = %q, want pong", got.Content)
	}
}

// --- the GPT-5.4 warning ----------------------------------------------------

const restrictionPhrase = "refuses tool calling"

// Reasoning configured on the chat surface is reasoning a current model will
// not give you, and Init says so once. The surface has to be declared now that
// it is no longer the default — which is the point of the warning: reaching
// this configuration takes a deliberate `api: chat_completions`.
func TestInit_WarnsOnceWhenReasoningMeetsChatCompletions(t *testing.T) {
	_, records, err := initAPI(t, map[string]any{
		"api":       "chat_completions",
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
	_, records, err := initAPI(t, map[string]any{"api": "chat_completions"}, models)
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
