package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =====================================================================
// parseAuthConfig
// =====================================================================

func TestParseAuthConfig_DefaultsToOpenAI(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "secret-key")

	a, err := parseAuthConfig(map[string]any{})
	if err != nil {
		t.Fatalf("parseAuthConfig: %v", err)
	}
	if a.mode != authModeOpenAI {
		t.Fatalf("mode = %q, want %q", a.mode, authModeOpenAI)
	}
	if a.apiKey != "secret-key" {
		t.Fatalf("apiKey = %q, want secret-key", a.apiKey)
	}
}

func TestParseAuthConfig_OpenAI_LiteralWins(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "from-env")

	a, err := parseAuthConfig(map[string]any{
		"api_key": "from-config",
	})
	if err != nil {
		t.Fatalf("parseAuthConfig: %v", err)
	}
	if a.apiKey != "from-config" {
		t.Fatalf("apiKey = %q, want from-config", a.apiKey)
	}
}

func TestParseAuthConfig_OpenAI_NoKeyErrors(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")

	_, err := parseAuthConfig(map[string]any{})
	if err == nil {
		t.Fatal("expected error when no API key configured")
	}
}

func TestParseAuthConfig_OpenAI_BaseURLOverride(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "k")

	a, err := parseAuthConfig(map[string]any{
		"base_url": "https://my-proxy.example.com/v1/chat/completions/",
	})
	if err != nil {
		t.Fatalf("parseAuthConfig: %v", err)
	}
	// Trailing slash should be trimmed.
	if a.baseURL != "https://my-proxy.example.com/v1/chat/completions" {
		t.Fatalf("baseURL = %q", a.baseURL)
	}
}

func TestParseAuthConfig_UnknownMode(t *testing.T) {
	_, err := parseAuthConfig(map[string]any{"auth_mode": "vertex"})
	if err == nil {
		t.Fatal("expected error for unknown auth_mode")
	}
}

func TestParseAuthConfig_AzureKey_Full(t *testing.T) {
	a, err := parseAuthConfig(map[string]any{
		"auth_mode": "azure_key",
		"azure": map[string]any{
			"resource":    "my-resource",
			"deployment":  "gpt-4o",
			"api_version": "2024-10-21",
			"api_key":     "azure-secret",
		},
	})
	if err != nil {
		t.Fatalf("parseAuthConfig: %v", err)
	}
	if a.mode != authModeAzureKey {
		t.Fatalf("mode = %q, want azure_key", a.mode)
	}
	if a.resource != "my-resource" {
		t.Fatalf("resource = %q", a.resource)
	}
	if a.deployment != "gpt-4o" {
		t.Fatalf("deployment = %q", a.deployment)
	}
	if a.apiVersion != "2024-10-21" {
		t.Fatalf("apiVersion = %q", a.apiVersion)
	}
	if a.apiKey != "azure-secret" {
		t.Fatalf("apiKey = %q", a.apiKey)
	}
}

func TestParseAuthConfig_AzureKey_FromEnvVar(t *testing.T) {
	t.Setenv("AZURE_OPENAI_KEY", "env-azure-key")

	a, err := parseAuthConfig(map[string]any{
		"auth_mode": "azure_key",
		"azure": map[string]any{
			"resource":    "r",
			"deployment":  "d",
			"api_version": "v",
			"api_key_env": "AZURE_OPENAI_KEY",
		},
	})
	if err != nil {
		t.Fatalf("parseAuthConfig: %v", err)
	}
	if a.apiKey != "env-azure-key" {
		t.Fatalf("apiKey = %q", a.apiKey)
	}
}

func TestParseAuthConfig_AzureKey_MissingResourceErrors(t *testing.T) {
	_, err := parseAuthConfig(map[string]any{
		"auth_mode": "azure_key",
		"azure": map[string]any{
			"deployment":  "d",
			"api_version": "v",
			"api_key":     "k",
		},
	})
	if err == nil {
		t.Fatal("expected error for missing resource")
	}
}

func TestParseAuthConfig_AzureKey_MissingDeploymentErrors(t *testing.T) {
	_, err := parseAuthConfig(map[string]any{
		"auth_mode": "azure_key",
		"azure": map[string]any{
			"resource":    "r",
			"api_version": "v",
			"api_key":     "k",
		},
	})
	if err == nil {
		t.Fatal("expected error for missing deployment")
	}
}

func TestParseAuthConfig_AzureKey_MissingAPIVersionErrors(t *testing.T) {
	_, err := parseAuthConfig(map[string]any{
		"auth_mode": "azure_key",
		"azure": map[string]any{
			"resource":   "r",
			"deployment": "d",
			"api_key":    "k",
		},
	})
	if err == nil {
		t.Fatal("expected error for missing api_version")
	}
}

func TestParseAuthConfig_AzureKey_MissingKeyErrors(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")

	_, err := parseAuthConfig(map[string]any{
		"auth_mode": "azure_key",
		"azure": map[string]any{
			"resource":    "r",
			"deployment":  "d",
			"api_version": "v",
		},
	})
	if err == nil {
		t.Fatal("expected error for missing api_key in azure_key mode")
	}
}

func TestParseAuthConfig_AzureAAD_Full(t *testing.T) {
	a, err := parseAuthConfig(map[string]any{
		"auth_mode": "azure_aad",
		"azure": map[string]any{
			"resource":      "my-resource",
			"deployment":    "gpt-4o",
			"api_version":   "2024-10-21",
			"tenant_id":     "tenant-uuid",
			"client_id":     "client-uuid",
			"client_secret": "shh",
		},
	})
	if err != nil {
		t.Fatalf("parseAuthConfig: %v", err)
	}
	if a.mode != authModeAzureAAD {
		t.Fatalf("mode = %q, want azure_aad", a.mode)
	}
	if a.tenantID != "tenant-uuid" {
		t.Fatalf("tenantID = %q", a.tenantID)
	}
	if a.clientID != "client-uuid" {
		t.Fatalf("clientID = %q", a.clientID)
	}
	if a.clientSecret != "shh" {
		t.Fatalf("clientSecret = %q", a.clientSecret)
	}
}

func TestParseAuthConfig_AzureAAD_FromEnvVars(t *testing.T) {
	t.Setenv("AZURE_TENANT_ID", "env-tenant")
	t.Setenv("AZURE_CLIENT_ID", "env-client")
	t.Setenv("AZURE_CLIENT_SECRET", "env-secret")

	a, err := parseAuthConfig(map[string]any{
		"auth_mode": "azure_aad",
		"azure": map[string]any{
			"resource":    "r",
			"deployment":  "d",
			"api_version": "v",
		},
	})
	if err != nil {
		t.Fatalf("parseAuthConfig: %v", err)
	}
	if a.tenantID != "env-tenant" {
		t.Fatalf("tenantID = %q", a.tenantID)
	}
	if a.clientID != "env-client" {
		t.Fatalf("clientID = %q", a.clientID)
	}
	if a.clientSecret != "env-secret" {
		t.Fatalf("clientSecret = %q", a.clientSecret)
	}
}

func TestParseAuthConfig_AzureAAD_MissingTenantErrors(t *testing.T) {
	t.Setenv("AZURE_TENANT_ID", "")

	_, err := parseAuthConfig(map[string]any{
		"auth_mode": "azure_aad",
		"azure": map[string]any{
			"resource":      "r",
			"deployment":    "d",
			"api_version":   "v",
			"client_id":     "c",
			"client_secret": "s",
		},
	})
	if err == nil {
		t.Fatal("expected error for missing tenant_id")
	}
}

func TestParseAuthConfig_AzureAAD_MissingClientIDErrors(t *testing.T) {
	t.Setenv("AZURE_CLIENT_ID", "")

	_, err := parseAuthConfig(map[string]any{
		"auth_mode": "azure_aad",
		"azure": map[string]any{
			"resource":      "r",
			"deployment":    "d",
			"api_version":   "v",
			"tenant_id":     "t",
			"client_secret": "s",
		},
	})
	if err == nil {
		t.Fatal("expected error for missing client_id")
	}
}

func TestParseAuthConfig_AzureAAD_MissingClientSecretErrors(t *testing.T) {
	t.Setenv("AZURE_CLIENT_SECRET", "")

	_, err := parseAuthConfig(map[string]any{
		"auth_mode": "azure_aad",
		"azure": map[string]any{
			"resource":    "r",
			"deployment":  "d",
			"api_version": "v",
			"tenant_id":   "t",
			"client_id":   "c",
		},
	})
	if err == nil {
		t.Fatal("expected error for missing client_secret")
	}
}

// =====================================================================
// buildURL / stripModelFromBody
// =====================================================================

func TestBuildURL_OpenAI_Default(t *testing.T) {
	a := &authState{mode: authModeOpenAI}
	got := a.buildURL()
	want := "https://api.openai.com/v1/chat/completions"
	if got != want {
		t.Errorf("buildURL = %q, want %q", got, want)
	}
}

func TestBuildURL_OpenAI_BaseURLOverride(t *testing.T) {
	a := &authState{
		mode:    authModeOpenAI,
		baseURL: "https://my-proxy.example.com/v1/chat/completions",
	}
	got := a.buildURL()
	if got != "https://my-proxy.example.com/v1/chat/completions" {
		t.Errorf("buildURL = %q", got)
	}
}

func TestBuildURL_AzureKey(t *testing.T) {
	a := &authState{
		mode:       authModeAzureKey,
		resource:   "my-resource",
		deployment: "gpt-4o",
		apiVersion: "2024-10-21",
	}
	got := a.buildURL()
	// All three pieces must appear in the URL.
	if !strings.Contains(got, "my-resource.openai.azure.com") {
		t.Errorf("URL missing resource: %q", got)
	}
	if !strings.Contains(got, "/deployments/gpt-4o/") {
		t.Errorf("URL missing deployment: %q", got)
	}
	if !strings.Contains(got, "api-version=2024-10-21") {
		t.Errorf("URL missing api-version: %q", got)
	}
}

func TestBuildURL_AzureAAD_SameAsAzureKey(t *testing.T) {
	a := &authState{
		mode:       authModeAzureAAD,
		resource:   "r",
		deployment: "d",
		apiVersion: "v",
	}
	got := a.buildURL()
	want := "https://r.openai.azure.com/openai/deployments/d/chat/completions?api-version=v"
	if got != want {
		t.Errorf("buildURL = %q, want %q", got, want)
	}
}

func TestStripModelFromBody(t *testing.T) {
	cases := []struct {
		mode authMode
		want bool
	}{
		{authModeOpenAI, false},
		{authModeAzureKey, true},
		{authModeAzureAAD, true},
	}
	for _, tc := range cases {
		a := &authState{mode: tc.mode}
		if got := a.stripModelFromBody(); got != tc.want {
			t.Errorf("stripModelFromBody(%q) = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

// =====================================================================
// applyAuth
// =====================================================================

func TestApplyAuth_OpenAI_SetsBearerHeader(t *testing.T) {
	a := &authState{mode: authModeOpenAI, apiKey: "k"}
	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", nil)

	if err := a.applyAuth(context.Background(), req, http.DefaultClient); err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer k" {
		t.Errorf("Authorization = %q, want Bearer k", got)
	}
	if got := req.Header.Get("api-key"); got != "" {
		t.Errorf("api-key should not be set in openai mode, got %q", got)
	}
}

func TestApplyAuth_AzureKey_SetsAPIKeyHeader(t *testing.T) {
	a := &authState{mode: authModeAzureKey, apiKey: "azure-k"}
	req, _ := http.NewRequest("POST", "https://r.openai.azure.com/...", nil)

	if err := a.applyAuth(context.Background(), req, http.DefaultClient); err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	if got := req.Header.Get("api-key"); got != "azure-k" {
		t.Errorf("api-key = %q, want azure-k", got)
	}
	// Critically: Authorization MUST NOT be set (Azure rejects requests with
	// both api-key and Authorization headers).
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization should be empty in azure_key mode, got %q", got)
	}
}

func TestApplyAuth_AzureAAD_FetchesBearerToken(t *testing.T) {
	srv := newAADTokenServer(t, "fake-aad-token", 3600)
	defer srv.Close()

	a := &authState{
		mode:         authModeAzureAAD,
		tenantID:     "tenant",
		clientID:     "client",
		clientSecret: "secret",
		aadTokenURL:  srv.URL,
	}
	req, _ := http.NewRequest("POST", "https://r.openai.azure.com/...", nil)

	if err := a.applyAuth(context.Background(), req, srv.Client()); err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer fake-aad-token" {
		t.Errorf("Authorization = %q, want Bearer fake-aad-token", got)
	}
	if got := req.Header.Get("api-key"); got != "" {
		t.Errorf("api-key should not be set in azure_aad mode, got %q", got)
	}
}

// =====================================================================
// AAD token cache + refresh + concurrency
// =====================================================================

func TestAADToken_FetchAndCache(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		// Verify the form body shape per the plan spec.
		if r.Form.Get("grant_type") != "client_credentials" {
			t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		if r.Form.Get("client_id") != "client" {
			t.Errorf("client_id = %q", r.Form.Get("client_id"))
		}
		if r.Form.Get("client_secret") != "secret" {
			t.Errorf("client_secret = %q", r.Form.Get("client_secret"))
		}
		if r.Form.Get("scope") != "https://cognitiveservices.azure.com/.default" {
			t.Errorf("scope = %q", r.Form.Get("scope"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer srv.Close()

	a := &authState{
		mode:         authModeAzureAAD,
		tenantID:     "t",
		clientID:     "client",
		clientSecret: "secret",
		aadTokenURL:  srv.URL,
	}

	tok, err := a.aadAccessToken(context.Background(), srv.Client())
	if err != nil {
		t.Fatalf("aadAccessToken: %v", err)
	}
	if tok != "tok" {
		t.Errorf("token = %q, want tok", tok)
	}
	if hits.Load() != 1 {
		t.Errorf("expected 1 token-endpoint hit, got %d", hits.Load())
	}

	// Second call should be served from cache (no new HTTP roundtrip).
	tok2, err := a.aadAccessToken(context.Background(), srv.Client())
	if err != nil {
		t.Fatalf("second aadAccessToken: %v", err)
	}
	if tok2 != "tok" {
		t.Errorf("cached token = %q, want tok", tok2)
	}
	if hits.Load() != 1 {
		t.Errorf("expected token caching; saw %d hits", hits.Load())
	}
}

func TestAADToken_RefreshOnExpiry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"refreshed","expires_in":3600}`))
	}))
	defer srv.Close()

	a := &authState{
		mode:         authModeAzureAAD,
		tenantID:     "t",
		clientID:     "c",
		clientSecret: "s",
		aadTokenURL:  srv.URL,
		// Pre-seed an expired token; should trigger a refresh.
		token:     "stale",
		tokExpiry: time.Now().Add(-1 * time.Minute),
	}

	tok, err := a.aadAccessToken(context.Background(), srv.Client())
	if err != nil {
		t.Fatalf("aadAccessToken: %v", err)
	}
	if tok != "refreshed" {
		t.Errorf("token = %q, want refreshed", tok)
	}
	if hits.Load() != 1 {
		t.Errorf("expected refresh, got %d hits", hits.Load())
	}
}

func TestAADToken_EndpointFailureWrapsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"bad creds"}`))
	}))
	defer srv.Close()

	a := &authState{
		mode:         authModeAzureAAD,
		tenantID:     "t",
		clientID:     "c",
		clientSecret: "s",
		aadTokenURL:  srv.URL,
	}

	_, err := a.aadAccessToken(context.Background(), srv.Client())
	if err == nil {
		t.Fatal("expected error from failing token endpoint")
	}
	msg := err.Error()
	if !strings.Contains(msg, "401") {
		t.Errorf("error missing status code: %q", msg)
	}
	if !strings.Contains(msg, "invalid_client") {
		t.Errorf("error missing body content: %q", msg)
	}
}

// TestAADToken_ConcurrentRequestsSerialize spins up N goroutines that all
// call applyAuth with an empty cache; only one should reach the token
// endpoint. Run with `go test -race` to also check the mutex pairing.
func TestAADToken_ConcurrentRequestsSerialize(t *testing.T) {
	var hits atomic.Int32
	// Add a tiny delay to widen the race window if the mutex is missing.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"only-one","expires_in":3600}`))
	}))
	defer srv.Close()

	a := &authState{
		mode:         authModeAzureAAD,
		tenantID:     "t",
		clientID:     "c",
		clientSecret: "s",
		aadTokenURL:  srv.URL,
	}

	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", "https://example.invalid/", nil)
			if err := a.applyAuth(context.Background(), req, srv.Client()); err != nil {
				errs <- err
				return
			}
			if got := req.Header.Get("Authorization"); got != "Bearer only-one" {
				errs <- fmt.Errorf("bad header: %q", got)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if hits.Load() != 1 {
		t.Errorf("expected exactly 1 token-endpoint hit under concurrency, got %d", hits.Load())
	}
}

// =====================================================================
// Test helpers
// =====================================================================

// newAADTokenServer returns a fake AAD token endpoint that always issues
// the given token with the given expires_in.
func newAADTokenServer(t *testing.T, token string, expiresIn int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"expires_in":%d,"token_type":"Bearer"}`, token, expiresIn)
	}))
}
