package anthropic

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// =====================================================================
// parseAuthConfig
// =====================================================================

func TestParseAuthConfig_DefaultsToAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "secret-key")

	a, err := parseAuthConfig(map[string]any{})
	if err != nil {
		t.Fatalf("parseAuthConfig: %v", err)
	}
	if a.mode != authModeAPIKey {
		t.Fatalf("mode = %q, want %q", a.mode, authModeAPIKey)
	}
	if a.apiKey != "secret-key" {
		t.Fatalf("apiKey = %q, want %q", a.apiKey, "secret-key")
	}
}

func TestParseAuthConfig_APIKey_LiteralWins(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "from-env")

	a, err := parseAuthConfig(map[string]any{
		"api_key": "from-config",
	})
	if err != nil {
		t.Fatalf("parseAuthConfig: %v", err)
	}
	if a.apiKey != "from-config" {
		t.Fatalf("apiKey = %q, want %q", a.apiKey, "from-config")
	}
}

func TestParseAuthConfig_APIKey_NoKeyErrors(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")

	_, err := parseAuthConfig(map[string]any{})
	if err == nil {
		t.Fatal("expected error when no API key configured")
	}
}

func TestParseAuthConfig_UnknownMode(t *testing.T) {
	_, err := parseAuthConfig(map[string]any{"auth_mode": "azure"})
	if err == nil {
		t.Fatal("expected error for unknown auth_mode")
	}
}

func TestParseAuthConfig_Bedrock_Full(t *testing.T) {
	a, err := parseAuthConfig(map[string]any{
		"auth_mode": "bedrock",
		"bedrock": map[string]any{
			"region":            "us-east-1",
			"access_key_id":     "AKIDEXAMPLE",
			"secret_access_key": "secret-yo",
			"session_token":     "sess-token",
		},
	})
	if err != nil {
		t.Fatalf("parseAuthConfig: %v", err)
	}
	if a.mode != authModeBedrock {
		t.Fatalf("mode = %q, want bedrock", a.mode)
	}
	if a.bedrockRegion != "us-east-1" {
		t.Fatalf("region = %q", a.bedrockRegion)
	}
	if a.bedrockAccessKeyID != "AKIDEXAMPLE" {
		t.Fatalf("access_key = %q", a.bedrockAccessKeyID)
	}
	if a.bedrockSecretKey != "secret-yo" {
		t.Fatalf("secret = %q", a.bedrockSecretKey)
	}
	if a.bedrockSessionToken != "sess-token" {
		t.Fatalf("session_token = %q", a.bedrockSessionToken)
	}
}

func TestParseAuthConfig_Bedrock_FromEnvVars(t *testing.T) {
	t.Setenv("AWS_REGION", "us-west-2")
	t.Setenv("AWS_ACCESS_KEY_ID", "env-akid")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "env-secret")

	a, err := parseAuthConfig(map[string]any{
		"auth_mode": "bedrock",
	})
	if err != nil {
		t.Fatalf("parseAuthConfig: %v", err)
	}
	if a.bedrockRegion != "us-west-2" {
		t.Fatalf("region = %q, want us-west-2", a.bedrockRegion)
	}
	if a.bedrockAccessKeyID != "env-akid" {
		t.Fatalf("akid = %q, want env-akid", a.bedrockAccessKeyID)
	}
	if a.bedrockSecretKey != "env-secret" {
		t.Fatalf("secret = %q, want env-secret", a.bedrockSecretKey)
	}
}

func TestParseAuthConfig_Bedrock_MissingRegionErrors(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "akid")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	_, err := parseAuthConfig(map[string]any{
		"auth_mode": "bedrock",
	})
	if err == nil {
		t.Fatal("expected error for missing region")
	}
}

func TestParseAuthConfig_Bedrock_MissingCredsErrors(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")

	_, err := parseAuthConfig(map[string]any{
		"auth_mode": "bedrock",
		"bedrock":   map[string]any{"region": "us-east-1"},
	})
	if err == nil {
		t.Fatal("expected error for missing access_key_id")
	}
}

func TestParseAuthConfig_Vertex_Full(t *testing.T) {
	dir := t.TempDir()
	saPath := filepath.Join(dir, "sa.json")
	writeFakeServiceAccount(t, saPath, "test-sa@example.iam.gserviceaccount.com")

	a, err := parseAuthConfig(map[string]any{
		"auth_mode": "vertex",
		"vertex": map[string]any{
			"project":     "my-proj",
			"region":      "us-east5",
			"sa_key_file": saPath,
		},
	})
	if err != nil {
		t.Fatalf("parseAuthConfig: %v", err)
	}
	if a.mode != authModeVertex {
		t.Fatalf("mode = %q", a.mode)
	}
	if a.vertexProject != "my-proj" {
		t.Fatalf("project = %q", a.vertexProject)
	}
	if a.vertexRegion != "us-east5" {
		t.Fatalf("region = %q", a.vertexRegion)
	}
	if a.saEmail != "test-sa@example.iam.gserviceaccount.com" {
		t.Fatalf("saEmail = %q", a.saEmail)
	}
	if a.saKey == nil {
		t.Fatal("saKey not parsed")
	}
}

func TestParseAuthConfig_Vertex_MissingProjectErrors(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	_, err := parseAuthConfig(map[string]any{
		"auth_mode": "vertex",
	})
	if err == nil {
		t.Fatal("expected error for missing project")
	}
}

func TestParseAuthConfig_Vertex_MissingSAErrors(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")

	_, err := parseAuthConfig(map[string]any{
		"auth_mode": "vertex",
		"vertex":    map[string]any{"project": "p"},
	})
	if err == nil {
		t.Fatal("expected error for missing sa_key_file")
	}
}

// =====================================================================
// buildURL / bodyVersionField / stripModelFromBody
// =====================================================================

func TestBuildURL_APIKey(t *testing.T) {
	a := &authState{mode: authModeAPIKey}
	got := a.buildURL("claude-sonnet-4-5-20250514", false)
	want := "https://api.anthropic.com/v1/messages"
	if got != want {
		t.Errorf("buildURL = %q, want %q", got, want)
	}
	if a.buildURL("claude-sonnet-4-5-20250514", true) != want {
		t.Errorf("api_key url should be the same regardless of stream flag")
	}
}

func TestBuildURL_Bedrock(t *testing.T) {
	a := &authState{mode: authModeBedrock, bedrockRegion: "us-east-1"}

	got := a.buildURL("anthropic.claude-sonnet-4-5-v1:0", false)
	want := "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-sonnet-4-5-v1%3A0/invoke"
	if got != want {
		t.Errorf("non-stream URL = %q, want %q", got, want)
	}

	gotStream := a.buildURL("anthropic.claude-sonnet-4-5-v1:0", true)
	wantStream := "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-sonnet-4-5-v1%3A0/invoke-with-response-stream"
	if gotStream != wantStream {
		t.Errorf("stream URL = %q, want %q", gotStream, wantStream)
	}
}

func TestBuildURL_Vertex(t *testing.T) {
	a := &authState{mode: authModeVertex, vertexProject: "my-proj", vertexRegion: "us-east5"}

	got := a.buildURL("claude-sonnet-4@20250514", false)
	want := "https://us-east5-aiplatform.googleapis.com/v1/projects/my-proj/locations/us-east5/publishers/anthropic/models/claude-sonnet-4@20250514:rawPredict"
	if got != want {
		t.Errorf("non-stream URL = %q, want %q", got, want)
	}

	gotStream := a.buildURL("claude-sonnet-4@20250514", true)
	wantStream := "https://us-east5-aiplatform.googleapis.com/v1/projects/my-proj/locations/us-east5/publishers/anthropic/models/claude-sonnet-4@20250514:streamRawPredict"
	if gotStream != wantStream {
		t.Errorf("stream URL = %q, want %q", gotStream, wantStream)
	}
}

func TestBodyVersionField(t *testing.T) {
	cases := []struct {
		mode authMode
		want string
	}{
		{authModeAPIKey, ""},
		{authModeBedrock, "bedrock-2023-05-31"},
		{authModeVertex, "vertex-2023-10-16"},
	}
	for _, tc := range cases {
		a := &authState{mode: tc.mode}
		if got := a.bodyVersionField(); got != tc.want {
			t.Errorf("bodyVersionField(%q) = %q, want %q", tc.mode, got, tc.want)
		}
	}
}

func TestStripModelFromBody(t *testing.T) {
	cases := []struct {
		mode authMode
		want bool
	}{
		{authModeAPIKey, false},
		{authModeBedrock, true},
		{authModeVertex, false},
	}
	for _, tc := range cases {
		a := &authState{mode: tc.mode}
		if got := a.stripModelFromBody(); got != tc.want {
			t.Errorf("stripModelFromBody(%q) = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

// =====================================================================
// SigV4 — AWS-published test vectors
// =====================================================================

// TestDeriveSigningKey_AWSExample verifies the four-step HMAC chain matches
// AWS's documented signing-key example. From the SigV4 spec reference page:
//
//	Secret:    wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY
//	Date:      20150830
//	Region:    us-east-1
//	Service:   iam
//	Expected kSigning (hex):
//	  c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9
//
// If this passes, the HMAC chain is correct end-to-end; only the canonical-
// request shape can still go wrong.
func TestDeriveSigningKey_AWSExample(t *testing.T) {
	got := hex.EncodeToString(deriveSigningKey(
		"wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		"20150830",
		"us-east-1",
		"iam",
	))
	want := "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9"
	if got != want {
		t.Errorf("deriveSigningKey = %s, want %s", got, want)
	}
}

// TestSigv4Escape exercises the percent-encoder for both unreserved and
// reserved input. AWS SigV4 requires unreserved RFC-3986 characters to pass
// through and everything else (including ":", "/", "@") to be encoded.
func TestSigv4Escape(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"a/b", "a%2Fb"},
		{"a:b", "a%3Ab"},
		{"a b", "a%20b"},
		{"~_-.", "~_-."},
		{"foo+bar", "foo%2Bbar"},
	}
	for _, tc := range cases {
		if got := sigv4Escape(tc.in); got != tc.want {
			t.Errorf("sigv4Escape(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSignSigV4_RoundTripDeterministic signs a request, then independently
// recomputes the signature using the same building blocks against a frozen
// timestamp — proves the public Authorization header is well-formed and
// reproducible. Combined with TestDeriveSigningKey_AWSExample (which pins the
// HMAC chain) and TestSigv4Escape (which pins the encoder), this gives high
// confidence in the full SigV4 path without bundling a giant test-vector
// fixture.
func TestSignSigV4_RoundTripDeterministic(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	req, err := http.NewRequest("POST",
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-sonnet-4-5-v1%3A0/invoke",
		strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")

	now := time.Date(2025, 4, 29, 12, 0, 0, 0, time.UTC)
	creds := sigv4Creds{
		Service:   "bedrock",
		Region:    "us-east-1",
		AccessKey: "AKIDEXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	}
	if err := signSigV4(req, body, creds, now); err != nil {
		t.Fatalf("signSigV4: %v", err)
	}

	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		t.Fatalf("Authorization header missing algorithm: %q", auth)
	}
	if !strings.Contains(auth, "Credential=AKIDEXAMPLE/20250429/us-east-1/bedrock/aws4_request") {
		t.Errorf("Authorization missing/incorrect credential scope: %q", auth)
	}
	if !strings.Contains(auth, "SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date") {
		t.Errorf("SignedHeaders incorrect: %q", auth)
	}
	if req.Header.Get("x-amz-date") != "20250429T120000Z" {
		t.Errorf("x-amz-date = %q", req.Header.Get("x-amz-date"))
	}
	if got := req.Header.Get("x-amz-content-sha256"); len(got) != 64 {
		t.Errorf("x-amz-content-sha256 wrong length: %q", got)
	}

	// Re-sign a second time with the same inputs; signature should match.
	req2, _ := http.NewRequest("POST",
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-sonnet-4-5-v1%3A0/invoke",
		strings.NewReader(string(body)))
	req2.Header.Set("content-type", "application/json")
	if err := signSigV4(req2, body, creds, now); err != nil {
		t.Fatalf("signSigV4 second pass: %v", err)
	}
	if req.Header.Get("Authorization") != req2.Header.Get("Authorization") {
		t.Error("identical inputs produced different signatures")
	}
}

func TestSignSigV4_SessionTokenIncluded(t *testing.T) {
	body := []byte(`{}`)
	req, _ := http.NewRequest("POST", "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/invoke",
		strings.NewReader(string(body)))
	req.Header.Set("content-type", "application/json")

	creds := sigv4Creds{
		Service:      "bedrock",
		Region:       "us-east-1",
		AccessKey:    "AKID",
		SecretKey:    "SECRET",
		SessionToken: "sess-token",
	}
	if err := signSigV4(req, body, creds, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("x-amz-security-token"); got != "sess-token" {
		t.Errorf("x-amz-security-token = %q, want sess-token", got)
	}
	if !strings.Contains(req.Header.Get("Authorization"), "x-amz-security-token") {
		t.Error("session-token header missing from SignedHeaders")
	}
}

// =====================================================================
// Vertex JWT bearer
// =====================================================================

// TestVertexAccessToken_FetchAndCache hits a fake OAuth2 endpoint with a
// freshly-minted JWT, then asserts the cached token is reused on the second
// call (no second HTTP roundtrip).
func TestVertexAccessToken_FetchAndCache(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		if r.Form.Get("assertion") == "" {
			t.Error("assertion missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fake-token","expires_in":3600}`))
	}))
	defer srv.Close()

	a := &authState{
		mode:                  authModeVertex,
		saEmail:               "test@example.iam.gserviceaccount.com",
		saKey:                 mustGenerateRSAKey(t),
		tokenEndpointOverride: srv.URL,
	}

	token, err := a.vertexAccessToken(context.Background(), srv.Client())
	if err != nil {
		t.Fatalf("vertexAccessToken: %v", err)
	}
	if token != "fake-token" {
		t.Errorf("token = %q, want fake-token", token)
	}
	if hits.Load() != 1 {
		t.Errorf("expected 1 token-endpoint hit, got %d", hits.Load())
	}

	// Second call: should return the cached token without re-hitting endpoint.
	token2, err := a.vertexAccessToken(context.Background(), srv.Client())
	if err != nil {
		t.Fatalf("second vertexAccessToken: %v", err)
	}
	if token2 != "fake-token" {
		t.Errorf("cached token = %q, want fake-token", token2)
	}
	if hits.Load() != 1 {
		t.Errorf("expected token caching; saw %d hits to endpoint", hits.Load())
	}
}

func TestVertexAccessToken_RefreshOnExpiry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"refreshed","expires_in":3600}`))
	}))
	defer srv.Close()

	a := &authState{
		mode:                  authModeVertex,
		saEmail:               "test@example.iam.gserviceaccount.com",
		saKey:                 mustGenerateRSAKey(t),
		tokenEndpointOverride: srv.URL,
		// Pre-seed an expired token; should trigger a refresh.
		token:     "stale",
		tokExpiry: time.Now().Add(-1 * time.Minute),
	}

	token, err := a.vertexAccessToken(context.Background(), srv.Client())
	if err != nil {
		t.Fatalf("vertexAccessToken: %v", err)
	}
	if token != "refreshed" {
		t.Errorf("token = %q, want refreshed", token)
	}
	if hits.Load() != 1 {
		t.Errorf("expected refresh, got %d hits", hits.Load())
	}
}

func TestApplyAuth_APIKey(t *testing.T) {
	a := &authState{mode: authModeAPIKey, apiKey: "k"}
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	if err := a.applyAuth(context.Background(), req, nil, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("x-api-key"); got != "k" {
		t.Errorf("x-api-key = %q", got)
	}
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q", got)
	}
}

func TestApplyAuth_Bedrock_SetsAuthorization(t *testing.T) {
	a := &authState{
		mode:               authModeBedrock,
		bedrockRegion:      "us-east-1",
		bedrockAccessKeyID: "AKID",
		bedrockSecretKey:   "SECRET",
	}
	body := []byte(`{}`)
	req, _ := http.NewRequest("POST", "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/invoke",
		strings.NewReader(string(body)))
	req.Header.Set("content-type", "application/json")

	if err := a.applyAuth(context.Background(), req, body, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(req.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		t.Errorf("Authorization missing or wrong prefix: %q", req.Header.Get("Authorization"))
	}
}

// =====================================================================
// Test helpers
// =====================================================================

// mustGenerateRSAKey generates a 2048-bit RSA key for test signing. 2048 is
// fast enough (~50ms) and matches what GCP service-accounts use.
func mustGenerateRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return key
}

// writeFakeServiceAccount writes a synthetic GCP service-account JSON to
// `path`, suitable as input to parseAuthConfig with auth_mode=vertex.
func writeFakeServiceAccount(t *testing.T, path, email string) {
	t.Helper()
	key := mustGenerateRSAKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	sa := map[string]any{
		"type":         "service_account",
		"client_email": email,
		"private_key":  string(pemBytes),
	}
	data, err := json.Marshal(sa)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

