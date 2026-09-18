package anthropic

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/nexuscreds"

	// Side-effect import: registers the "google-adc" credential source the
	// Vertex tests below open by name. It belongs in a TEST file and only in a
	// test file — non-test anthropic code deliberately does not import
	// googleadc, because this plugin is in pkg/engine/allplugins and would
	// otherwise register a credential source into every binary carrying it.
	_ "github.com/frankbardon/nexus/pkg/nexuscreds/googleadc"
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
	pinNotOnGCE(t)
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
	if a.vertexRegionOrigin != regionFromConfig {
		t.Fatalf("region origin = %q, want config", a.vertexRegionOrigin)
	}
	if a.vertexCreds == nil {
		t.Fatal("vertex mode resolved without a credential source")
	}
	if a.vertexCredsName != defaultVertexCredentialSource {
		t.Fatalf("credential source = %q, want %q", a.vertexCredsName, defaultVertexCredentialSource)
	}
}

func TestParseAuthConfig_Vertex_MissingProjectErrors(t *testing.T) {
	pinNotOnGCE(t)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	_, err := parseAuthConfig(map[string]any{
		"auth_mode": "vertex",
	})
	if err == nil {
		t.Fatal("expected error for missing project")
	}
	if !strings.Contains(err.Error(), "vertex.project") {
		t.Fatalf("error should name vertex.project, got %v", err)
	}
}

// TestParseAuthConfig_Vertex_BootsFromMetadataAlone is the pod-identity case:
// a config saying nothing GCP-specific beyond the auth mode must boot, with
// the project and the region both coming off the metadata server and no key
// file anywhere. Before the nexuscreds port this configuration was a hard
// error — "vertex auth requires sa_key_file" — which is exactly what made
// Anthropic-on-Vertex unusable under GKE Workload Identity.
func TestParseAuthConfig_Vertex_BootsFromMetadataAlone(t *testing.T) {
	newFakeMetadata(t)
	isolateADC(t)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	a, err := parseAuthConfig(map[string]any{"auth_mode": "vertex"})
	if err != nil {
		t.Fatalf("auth_mode: vertex alone should boot on GCE, got %v", err)
	}
	if a.vertexProject != testMetadataProjectID {
		t.Fatalf("project = %q, want %q from the metadata server", a.vertexProject, testMetadataProjectID)
	}
	if a.vertexRegion != testMetadataRegion {
		t.Fatalf("region = %q, want %q derived from zone %q", a.vertexRegion, testMetadataRegion, testMetadataZone)
	}
	if a.vertexRegionOrigin != regionFromMetadata {
		t.Fatalf("region origin = %q, want metadata", a.vertexRegionOrigin)
	}
	if a.vertexCreds == nil {
		t.Fatal("vertex mode resolved without a credential source")
	}
}

// =====================================================================
// Vertex project / region / credential chains
// =====================================================================

func TestResolveVertexProject_PrefersConfigOverEnvAndMetadata(t *testing.T) {
	pinMetadataProjectID(t, "metadata-project", nil)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "env-project")

	got, err := resolveVertexProject(map[string]any{"project": "config-project"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "config-project" {
		t.Fatalf("project = %q, want the configured one", got)
	}
}

// project_id is the older spelling and must keep working.
func TestResolveVertexProject_AcceptsTheProjectIDSpelling(t *testing.T) {
	pinNotOnGCE(t)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	got, err := resolveVertexProject(map[string]any{"project_id": "legacy-spelling"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "legacy-spelling" {
		t.Fatalf("project = %q, want legacy-spelling", got)
	}
}

func TestResolveVertexProject_FallsBackToTheEnvVar(t *testing.T) {
	pinMetadataProjectID(t, "metadata-project", nil)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "env-project")

	got, err := resolveVertexProject(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "env-project" {
		t.Fatalf("project = %q, want the env var", got)
	}
}

func TestResolveVertexProject_FallsBackToTheMetadataServer(t *testing.T) {
	pinMetadataProjectID(t, "metadata-project", nil)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	got, err := resolveVertexProject(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "metadata-project" {
		t.Fatalf("project = %q, want the metadata server's", got)
	}
}

func TestResolveVertexProject_SurfacesAMetadataServerError(t *testing.T) {
	pinMetadataProjectID(t, "", errors.New("metadata server timed out"))
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	_, err := resolveVertexProject(map[string]any{})
	if err == nil {
		t.Fatal("expected an error")
	}
	// A wedged metadata server and a laptop are different problems; the
	// message has to distinguish them.
	if !strings.Contains(err.Error(), "metadata server timed out") {
		t.Fatalf("error should carry the metadata failure, got %v", err)
	}
}

func TestResolveVertexRegion_PrefersConfigOverMetadata(t *testing.T) {
	pinMetadataRegion(t, "europe-west4", nil)

	region, origin := resolveVertexRegion(map[string]any{"region": "us-east5"})
	if region != "us-east5" || origin != regionFromConfig {
		t.Fatalf("region = %q (%s), want us-east5 (config)", region, origin)
	}
}

// location is the older spelling and must keep working.
func TestResolveVertexRegion_AcceptsTheLocationSpelling(t *testing.T) {
	pinMetadataRegion(t, "europe-west4", nil)

	region, origin := resolveVertexRegion(map[string]any{"location": "us-central1"})
	if region != "us-central1" || origin != regionFromConfig {
		t.Fatalf("region = %q (%s), want us-central1 (config)", region, origin)
	}
}

func TestResolveVertexRegion_DerivesTheRegionFromTheMetadataServer(t *testing.T) {
	pinMetadataRegion(t, "europe-west4", nil)

	region, origin := resolveVertexRegion(map[string]any{})
	if region != "europe-west4" || origin != regionFromMetadata {
		t.Fatalf("region = %q (%s), want europe-west4 (metadata)", region, origin)
	}
}

func TestResolveVertexRegion_FallsBackToTheDefaultWhenNotOnGCE(t *testing.T) {
	pinNotOnGCE(t)

	region, origin := resolveVertexRegion(map[string]any{})
	if region != defaultVertexRegion || origin != regionFromDefault {
		t.Fatalf("region = %q (%s), want %s (default)", region, origin, defaultVertexRegion)
	}
}

func TestParseAuthConfig_Vertex_UnknownCredentialSource(t *testing.T) {
	pinNotOnGCE(t)

	_, err := parseAuthConfig(map[string]any{
		"auth_mode": "vertex",
		"vertex": map[string]any{
			"project":     "my-proj",
			"credentials": "no-such-source",
		},
	})
	if err == nil {
		t.Fatal("expected an error for an unregistered credential source")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no-such-source") {
		t.Fatalf("error should name the source, got %v", err)
	}
	// The actionable half of nexuscreds.Open's message must survive wrapping:
	// the overwhelmingly likely cause is a missing blank import, not a typo.
	if !strings.Contains(msg, "blank-import") {
		t.Fatalf("error should explain the missing blank import, got %v", err)
	}
}

func TestParseAuthConfig_Vertex_CredentialsMustBeAString(t *testing.T) {
	pinNotOnGCE(t)

	_, err := parseAuthConfig(map[string]any{
		"auth_mode": "vertex",
		"vertex": map[string]any{
			"project":     "my-proj",
			"credentials": 42,
		},
	})
	if err == nil {
		t.Fatal("expected an error for a non-string credentials key")
	}
	if !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("error should name the credentials key, got %v", err)
	}
}

// TestVertexCredentialSourceConfig_NormalisesEveryKeyFileSpelling pins the
// backwards-compatibility contract: four accepted spellings collapse onto the
// two names the credential source understands, and a key that was never
// configured stays absent rather than being forwarded empty — google-adc
// distinguishes "not configured" (run the whole ADC chain, the keyless-pod
// case) from "configured to something empty".
func TestVertexCredentialSourceConfig_NormalisesEveryKeyFileSpelling(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   map[string]any
		want map[string]any
	}{
		{"nothing configured", map[string]any{"project": "p"}, map[string]any{}},
		{"sa_key_file", map[string]any{"sa_key_file": "/k.json"}, map[string]any{"service_account_json": "/k.json"}},
		{"service_account_json", map[string]any{"service_account_json": "/k.json"}, map[string]any{"service_account_json": "/k.json"}},
		{"sa_key_file_env", map[string]any{"sa_key_file_env": "KEY"}, map[string]any{"service_account_json_env": "KEY"}},
		{"service_account_json_env", map[string]any{"service_account_json_env": "KEY"}, map[string]any{"service_account_json_env": "KEY"}},
		{"sa_key_file wins over the alias", map[string]any{"sa_key_file": "/a.json", "service_account_json": "/b.json"}, map[string]any{"service_account_json": "/a.json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := vertexCredentialSourceConfig(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("forwarded %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("forwarded[%q] = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

// A bad key-file path must fail boot, not the first LLM call.
func TestParseAuthConfig_Vertex_MissingKeyFileFailsAtInit(t *testing.T) {
	pinNotOnGCE(t)

	_, err := parseAuthConfig(map[string]any{
		"auth_mode": "vertex",
		"vertex": map[string]any{
			"project":     "my-proj",
			"sa_key_file": filepath.Join(t.TempDir(), "absent.json"),
		},
	})
	if err == nil {
		t.Fatal("expected an error for a missing key file")
	}
	if !strings.Contains(err.Error(), "service_account_json") {
		t.Fatalf("error should name the forwarded config key, got %v", err)
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
// Vertex bearer token
// =====================================================================

// TestApplyAuth_VertexSetsBearerFromSource asserts the whole of what applyAuth
// now does in Vertex mode: ask the source, attach the answer. There is no
// cache to test here any more — minting, caching and refreshing all belong to
// the nexuscreds.Source, and for google-adc that is an oauth2.TokenSource
// which has its own coverage.
func TestApplyAuth_VertexSetsBearerFromSource(t *testing.T) {
	a := &authState{mode: authModeVertex, vertexCreds: stubSource{token: "tok-123"}}
	req, _ := http.NewRequest("POST", "https://us-east5-aiplatform.googleapis.com/v1/x", nil)

	if err := a.applyAuth(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want Bearer tok-123", got)
	}
}

func TestApplyAuth_VertexPropagatesSourceError(t *testing.T) {
	a := &authState{mode: authModeVertex, vertexCreds: stubSource{err: errors.New("the pod is not bound to a service account")}}
	req, _ := http.NewRequest("POST", "https://us-east5-aiplatform.googleapis.com/v1/x", nil)

	err := a.applyAuth(context.Background(), req, nil)
	if err == nil {
		t.Fatal("expected the source error to reach the caller")
	}
	if !strings.Contains(err.Error(), "not bound to a service account") {
		t.Fatalf("error should carry the source's message, got %v", err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("a failed mint must not leave an Authorization header behind")
	}
}

func TestApplyAuth_VertexWithoutASourceIsAnError(t *testing.T) {
	a := &authState{mode: authModeVertex}
	req, _ := http.NewRequest("POST", "https://us-east5-aiplatform.googleapis.com/v1/x", nil)

	if err := a.applyAuth(context.Background(), req, nil); err == nil {
		t.Fatal("expected an error when no credential source was resolved")
	}
}

// TestApplyAuth_VertexMintsFromTheMetadataServer is the end-to-end keyless
// path: no key material anywhere, ADC resolving through the fake metadata
// server, and the token it mints arriving on the request.
func TestApplyAuth_VertexMintsFromTheMetadataServer(t *testing.T) {
	fake := newFakeMetadata(t)
	isolateADC(t)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	a, err := parseAuthConfig(map[string]any{"auth_mode": "vertex"})
	if err != nil {
		t.Fatalf("parseAuthConfig: %v", err)
	}

	req, _ := http.NewRequest("POST", "https://europe-west4-aiplatform.googleapis.com/v1/x", nil)
	if err := a.applyAuth(context.Background(), req, nil); err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+testMetadataToken {
		t.Errorf("Authorization = %q, want the metadata server's token", got)
	}
	if fake.hits() == 0 {
		t.Error("the metadata token endpoint was never asked for a token")
	}
}

// =====================================================================
// confirmCredentials
// =====================================================================

// A pod whose Workload Identity binding is broken resolves cleanly and then
// cannot mint. That has to fail the boot, not the first user message.
func TestConfirmCredentials_FailsWhenTheSourceCannotMint(t *testing.T) {
	a := &authState{
		mode:            authModeVertex,
		vertexCredsName: "google-adc",
		vertexCreds:     stubSource{err: errors.New("403 Forbidden")},
	}

	err := a.confirmCredentials(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("expected the boot probe to fail")
	}
	if !strings.Contains(err.Error(), "google-adc") {
		t.Fatalf("error should name the credential source, got %v", err)
	}
}

// The one INFO line must say where auth came from and nothing about who it
// belongs to — no token, no key material, no principal.
func TestConfirmCredentials_LogsOneLineCarryingNoTokenMaterial(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	a := &authState{
		mode:               authModeVertex,
		vertexProject:      "my-proj",
		vertexRegion:       "us-east5",
		vertexRegionOrigin: regionFromConfig,
		vertexCredsName:    "google-adc",
		vertexCreds:        stubSource{token: "ya29.super-secret"},
	}

	if err := a.confirmCredentials(context.Background(), logger); err != nil {
		t.Fatalf("confirmCredentials: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"vertex credentials confirmed", "google-adc", "my-proj", "us-east5", "config"} {
		if !strings.Contains(out, want) {
			t.Errorf("log line should mention %q, got %s", want, out)
		}
	}
	if strings.Contains(out, "ya29.super-secret") {
		t.Errorf("the token must never be logged, got %s", out)
	}
}

// TestConfirmCredentials_FailsWhenTheMetadataServerRefusesAToken is the broken
// Workload Identity binding as a pod actually experiences it: resolution
// succeeds, and the metadata server then refuses to mint. Nothing before the
// probe can catch this, which is why the probe exists.
func TestConfirmCredentials_FailsWhenTheMetadataServerRefusesAToken(t *testing.T) {
	fake := newFakeMetadata(t)
	isolateADC(t)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	a, err := parseAuthConfig(map[string]any{"auth_mode": "vertex"})
	if err != nil {
		t.Fatalf("parseAuthConfig should still succeed — construction does no I/O: %v", err)
	}

	fake.failToken(http.StatusForbidden)

	if err := a.confirmCredentials(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("a pod that cannot mint a token must fail the boot")
	}
}

// Nothing to probe outside Vertex mode, and nothing to say about it.
func TestConfirmCredentials_IsANoOpOutsideVertex(t *testing.T) {
	for _, mode := range []authMode{authModeAPIKey, authModeBedrock} {
		var buf bytes.Buffer
		a := &authState{mode: mode}
		if err := a.confirmCredentials(context.Background(), slog.New(slog.NewTextHandler(&buf, nil))); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if buf.Len() != 0 {
			t.Errorf("%s mode logged %q", mode, buf.String())
		}
	}
}

func TestApplyAuth_APIKey(t *testing.T) {
	a := &authState{mode: authModeAPIKey, apiKey: "k"}
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	if err := a.applyAuth(context.Background(), req, nil); err != nil {
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

	if err := a.applyAuth(context.Background(), req, body); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(req.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		t.Errorf("Authorization missing or wrong prefix: %q", req.Header.Get("Authorization"))
	}
}

// =====================================================================
// Test helpers
// =====================================================================

// stubSource is a nexuscreds.Source that answers from fixed values, so the
// applyAuth and confirmCredentials tests exercise the plugin's wiring without
// any credential library in the way.
type stubSource struct {
	token string
	err   error
}

func (s stubSource) Token(context.Context) (string, error) { return s.token, s.err }

var _ nexuscreds.Source = stubSource{}

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
