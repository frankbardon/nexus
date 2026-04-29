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
	// for openai-mode (e.g. local proxies or OpenAI-compatible endpoints).
	// Ignored in Azure modes — buildURL constructs Azure URLs from the
	// resource/deployment/api-version triple.
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

// buildURL returns the full request URL for chat completions.
//
//	openai:    <baseURL or default>/chat/completions
//	azure_key: https://<resource>.openai.azure.com/openai/deployments/<deployment>/chat/completions?api-version=<v>
//	azure_aad: same as azure_key
func (a *authState) buildURL() string {
	switch a.mode {
	case authModeAzureKey, authModeAzureAAD:
		return fmt.Sprintf(
			"https://%s.openai.azure.com/openai/deployments/%s/chat/completions?api-version=%s",
			a.resource,
			url.PathEscape(a.deployment),
			url.QueryEscape(a.apiVersion),
		)
	default:
		// openai mode: honor base_url override.
		if a.baseURL != "" {
			return a.baseURL
		}
		return apiURL
	}
}

// stripModelFromBody reports whether the model field should be omitted from
// the JSON request body. Azure encodes the deployment in the URL path (and
// rejects bodies that carry "model" — or rather, the deployment name there
// must match), so the cleanest path is to omit the field entirely.
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
