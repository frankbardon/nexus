package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// authMode determines how requests are authenticated and routed.
type authMode string

const (
	// authModeOpenAI uses the public api.openai.com endpoint (or a custom
	// base_url override) with an "Authorization: Bearer <api_key>" header.
	authModeOpenAI authMode = "openai"
	// authModeAzureKey routes requests to an Azure OpenAI resource, using
	// the Azure-specific "api-key: <key>" header (NOT Authorization).
	authModeAzureKey authMode = "azure_key"
	// authModeAzureAAD routes requests to an Azure OpenAI resource using a
	// short-lived Entra ID (AAD) bearer token obtained via the OAuth2 client
	// credentials flow.
	authModeAzureAAD authMode = "azure_aad"
)

// aadTokenEndpointTemplate is the OAuth2 v2.0 token endpoint format. Tests
// override authState.aadTokenURL to redirect to an httptest server.
const aadTokenEndpointTemplate = "https://login.microsoftonline.com/%s/oauth2/v2.0/token"

// aadCognitiveServicesScope is the resource scope for Azure Cognitive
// Services (which Azure OpenAI is part of).
const aadCognitiveServicesScope = "https://cognitiveservices.azure.com/.default"

// authState holds resolved auth configuration plus any cached AAD tokens.
//
// Constructed once at Init and reused for every request. The token cache is
// guarded by mu; the openai-direct and azure_key paths don't lock.
type authState struct {
	mode authMode

	// OpenAI-direct + Azure key share apiKey. The Files API also reads from
	// this field — see plugin.go's note about Files API behavior in Azure
	// modes (it stays on the public OpenAI endpoint; supply a separate
	// api_key alongside Azure config to use both).
	apiKey string

	// baseURL overrides the default https://api.openai.com/v1/chat/completions
	// for openai-mode (e.g. local proxies or OpenAI-compatible endpoints). On
	// `api: responses` the sibling route beneath it is used instead — see
	// responsesEndpointUnder. Ignored in Azure modes, where resolveEndpoint
	// constructs both URLs from the resource/deployment/api-version triple.
	// Setting it also narrows the default `api:` to chat_completions — see
	// narrowsToChatCompletions.
	baseURL string

	// Azure (used by both azure_key and azure_aad).
	resource   string // e.g. "my-resource" → https://my-resource.openai.azure.com
	deployment string // deployment name (replaces model in URL)
	apiVersion string // e.g. "2024-10-21"

	// Azure AAD service-principal credentials.
	tenantID     string
	clientID     string
	clientSecret string

	// Cached AAD bearer token. Refreshed proactively (60s before expiry).
	mu        sync.Mutex
	token     string
	tokExpiry time.Time

	// aadTokenURL redirects the OAuth2 token exchange to a test server.
	// Production code leaves this empty; the public Microsoft endpoint is
	// derived from tenantID via aadTokenEndpointTemplate.
	aadTokenURL string
}

// parseAuthConfig builds an authState from the raw plugin config map.
//
// Backwards compatible: if auth_mode is missing, the legacy api_key /
// api_key_env / base_url top-level keys are used. The new "azure" config
// block is only consulted in azure_key / azure_aad modes.
func parseAuthConfig(cfg map[string]any) (*authState, error) {
	mode := authModeOpenAI
	if v, ok := cfg["auth_mode"].(string); ok && v != "" {
		switch authMode(v) {
		case authModeOpenAI, authModeAzureKey, authModeAzureAAD:
			mode = authMode(v)
		default:
			return nil, fmt.Errorf("openai: unknown auth_mode %q (expected openai, azure_key, or azure_aad)", v)
		}
	}

	a := &authState{mode: mode}

	// Top-level api_key / api_key_env / base_url are honored regardless of
	// mode. In Azure modes they're optional fall-through values used only by
	// the Files API (which stays on the public OpenAI endpoint).
	if key, ok := cfg["api_key"].(string); ok && key != "" {
		a.apiKey = key
	} else {
		envVar, _ := cfg["api_key_env"].(string)
		if envVar == "" {
			envVar = "OPENAI_API_KEY"
		}
		if v := os.Getenv(envVar); v != "" {
			a.apiKey = v
		}
	}

	if base, ok := cfg["base_url"].(string); ok && base != "" {
		a.baseURL = strings.TrimRight(base, "/")
	}

	switch mode {
	case authModeOpenAI:
		if a.apiKey == "" {
			return nil, fmt.Errorf("openai: no API key configured (set api_key in config or OPENAI_API_KEY env var)")
		}

	case authModeAzureKey:
		if err := populateAzureCommon(a, cfg); err != nil {
			return nil, err
		}
		raw, _ := cfg["azure"].(map[string]any)
		if raw == nil {
			raw = map[string]any{}
		}
		// Azure key auth needs the api-key. Read from the azure block first
		// (api_key / api_key_env), then fall back to the top-level apiKey
		// already populated above.
		if k := readAzureField(raw, "api_key", "api_key_env", ""); k != "" {
			a.apiKey = k
		}
		if a.apiKey == "" {
			return nil, fmt.Errorf("openai: azure_key auth requires azure.api_key (or azure.api_key_env, or top-level api_key/api_key_env)")
		}

	case authModeAzureAAD:
		if err := populateAzureCommon(a, cfg); err != nil {
			return nil, err
		}
		raw, _ := cfg["azure"].(map[string]any)
		if raw == nil {
			raw = map[string]any{}
		}
		a.tenantID = readAzureField(raw, "tenant_id", "tenant_id_env", "AZURE_TENANT_ID")
		if a.tenantID == "" {
			return nil, fmt.Errorf("openai: azure_aad auth requires azure.tenant_id (or AZURE_TENANT_ID env var)")
		}
		a.clientID = readAzureField(raw, "client_id", "client_id_env", "AZURE_CLIENT_ID")
		if a.clientID == "" {
			return nil, fmt.Errorf("openai: azure_aad auth requires azure.client_id (or AZURE_CLIENT_ID env var)")
		}
		a.clientSecret = readAzureField(raw, "client_secret", "client_secret_env", "AZURE_CLIENT_SECRET")
		if a.clientSecret == "" {
			return nil, fmt.Errorf("openai: azure_aad auth requires azure.client_secret (or AZURE_CLIENT_SECRET env var)")
		}
	}

	return a, nil
}

// populateAzureCommon validates and assigns the resource/deployment/
// api_version triple shared by both Azure modes.
func populateAzureCommon(a *authState, cfg map[string]any) error {
	raw, _ := cfg["azure"].(map[string]any)
	if raw == nil {
		return fmt.Errorf("openai: azure auth requires an `azure` config block")
	}

	resource, _ := raw["resource"].(string)
	if resource == "" {
		return fmt.Errorf("openai: azure auth requires azure.resource")
	}
	a.resource = resource

	deployment, _ := raw["deployment"].(string)
	if deployment == "" {
		return fmt.Errorf("openai: azure auth requires azure.deployment")
	}
	a.deployment = deployment

	apiVersion, _ := raw["api_version"].(string)
	if apiVersion == "" {
		return fmt.Errorf("openai: azure auth requires azure.api_version")
	}
	a.apiVersion = apiVersion

	return nil
}

// readAzureField reads a config field that may be supplied either inline
// (literalKey) or by env-var indirection (envKey); returns "" if neither is
// set. fallbackEnv is consulted last when the config block omits both keys.
func readAzureField(raw map[string]any, literalKey, envKey, fallbackEnv string) string {
	if v, ok := raw[literalKey].(string); ok && v != "" {
		return v
	}
	if v, ok := raw[envKey].(string); ok && v != "" {
		if val := os.Getenv(v); val != "" {
			return val
		}
	}
	if fallbackEnv != "" {
		return os.Getenv(fallbackEnv)
	}
	return ""
}

// resolveEndpoint returns the full request URL for one request, for the API
// surface that request resolved to.
//
// It is the single place an endpoint is chosen: the surface arrives already
// decided by Plugin.resolveAPI and nothing else here influences the result.
//
// chat_completions:
//
//	openai:    <baseURL or default>   (base_url is the whole endpoint)
//	azure_key: https://<resource>.openai.azure.com/openai/deployments/<deployment>/chat/completions?api-version=<v>
//	azure_aad: same as azure_key
//
// responses:
//
//	openai:    https://api.openai.com/v1/responses, or the Responses route
//	           beneath a configured base_url — see responsesEndpointUnder
//	azure_key: https://<resource>.openai.azure.com/openai/v1/responses
//	azure_aad: same as azure_key
//
// The Azure pair is the reason this takes an api at all rather than swapping a
// suffix. Azure's Responses surface is the versionless `/openai/v1/` route: the
// deployment is **not** in the path — it travels in the body's `model` field,
// which is why buildResponsesBody writes `model` even in Azure modes — and the
// route is implicitly versioned, so no `api-version` query is sent. (Preview
// features want `api-version=preview`; Nexus sends none and stays on GA.) The
// configured `azure.api_version` is therefore unused on this surface, and still
// required, because the chat surface on the same instance needs it.
func (a *authState) resolveEndpoint(api apiSurface) (string, error) {
	switch api {
	case apiChatCompletions:
		switch a.mode {
		case authModeAzureKey, authModeAzureAAD:
			return fmt.Sprintf(
				"https://%s.openai.azure.com/openai/deployments/%s/chat/completions?api-version=%s",
				a.resource,
				url.PathEscape(a.deployment),
				url.QueryEscape(a.apiVersion),
			), nil
		default:
			// openai mode: honor base_url override.
			if a.baseURL != "" {
				return a.baseURL, nil
			}
			return apiURL, nil
		}
	case apiResponses:
		switch a.mode {
		case authModeAzureKey, authModeAzureAAD:
			return fmt.Sprintf(
				"https://%s.openai.azure.com/openai/v1/responses",
				a.resource,
			), nil
		default:
			if a.baseURL != "" {
				return responsesEndpointUnder(a.baseURL), nil
			}
			return responsesURL, nil
		}
	default:
		return "", fmt.Errorf("openai: unknown api surface %q", string(api))
	}
}

// responsesEndpointUnder turns a configured base_url into the Responses URL
// beneath it.
//
// base_url is documented as the whole chat endpoint (the default it overrides
// is `https://api.openai.com/v1/chat/completions`), so the Responses route is
// its sibling rather than its child: the `/chat/completions` tail comes off and
// `/responses` goes on. A base_url written as the bare API root works too, and
// one already pointing at `/responses` is left alone — an operator who declared
// `api: responses` and wrote the matching URL means it.
//
// Whether the endpoint on the other end actually implements `/responses` is the
// operator's to know: that is exactly what declaring `api:` asserts, and it is
// why a base_url narrows the default away from this surface.
func responsesEndpointUnder(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/responses") {
		return base
	}
	base = strings.TrimSuffix(base, "/chat/completions")
	return base + "/responses"
}

// stripModelFromBody reports whether the model field should be omitted from
// the JSON request body. Azure's *chat* route encodes the deployment in the URL
// path (and rejects bodies whose "model" disagrees with it), so the cleanest
// path is to omit the field entirely.
//
// This is a fact about one surface, and buildRequestBody is its only caller.
// The Responses path deliberately never consults it: Azure's
// `/openai/v1/responses` route carries no deployment in the path, so
// buildResponsesBody writes `model` unconditionally — stripping it there would
// send a request naming no model at all.
func (a *authState) stripModelFromBody() bool {
	return a.mode == authModeAzureKey || a.mode == authModeAzureAAD
}

// applyAuth attaches the right auth credentials onto the outgoing request.
// For azure_aad it fetches/refreshes the cached bearer token first.
func (a *authState) applyAuth(ctx context.Context, req *http.Request, client *http.Client) error {
	switch a.mode {
	case authModeOpenAI:
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
		return nil

	case authModeAzureKey:
		// Azure uses the "api-key" header (NOT Authorization).
		req.Header.Set("api-key", a.apiKey)
		return nil

	case authModeAzureAAD:
		token, err := a.aadAccessToken(ctx, client)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
	return fmt.Errorf("openai: unknown auth mode %q", a.mode)
}

// aadAccessToken returns a valid OAuth2 access token, minting a new one
// when the cached token is empty or within 60s of expiry. Concurrent
// callers serialize on a.mu so only one HTTP exchange runs at a time.
func (a *authState) aadAccessToken(ctx context.Context, client *http.Client) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.token != "" && time.Now().Before(a.tokExpiry) {
		return a.token, nil
	}

	endpoint := a.aadTokenURL
	if endpoint == "" {
		endpoint = fmt.Sprintf(aadTokenEndpointTemplate, url.PathEscape(a.tenantID))
	}

	form := url.Values{}
	form.Set("client_id", a.clientID)
	form.Set("client_secret", a.clientSecret)
	form.Set("scope", aadCognitiveServicesScope)
	form.Set("grant_type", "client_credentials")

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("openai: build AAD token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("openai: AAD token HTTP error: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openai: AAD token exchange failed (%d): %s", resp.StatusCode, string(body))
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("openai: parse AAD token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("openai: empty access_token in AAD response: %s", string(body))
	}

	a.token = tr.AccessToken
	expiresIn := time.Duration(tr.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 1 * time.Hour
	}
	a.tokExpiry = time.Now().Add(expiresIn - 60*time.Second)
	return a.token, nil
}
