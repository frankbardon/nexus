package gemini

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/frankbardon/nexus/pkg/nexuscreds"

	// Side-effect import: registers the "google-adc" credential source so the
	// Vertex tests below can open it by name. Only the test binary imports it —
	// the shipped gemini code opens sources through the nexuscreds registry and
	// must never depend on a particular one.
	_ "github.com/frankbardon/nexus/pkg/nexuscreds/googleadc"
)

func TestResolveAuth_APIKey(t *testing.T) {
	a, err := resolveAuth(map[string]any{"api_key": "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	if a.mode != authModeAPIKey {
		t.Fatalf("expected api_key mode, got %s", a.mode)
	}
	if a.apiKey != "test-key" {
		t.Fatalf("expected api_key=test-key, got %q", a.apiKey)
	}
}

func TestResolveAuth_APIKey_FromEnv(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "env-key")
	a, err := resolveAuth(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if a.apiKey != "env-key" {
		t.Fatalf("expected env fallback, got %q", a.apiKey)
	}
}

func TestResolveAuth_APIKey_FallsBackToGoogleAPIKey(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "google-fallback")
	a, err := resolveAuth(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if a.apiKey != "google-fallback" {
		t.Fatalf("expected GOOGLE_API_KEY fallback, got %q", a.apiKey)
	}
}

func TestResolveAuth_NoCreds(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	if _, err := resolveAuth(map[string]any{}); err == nil {
		t.Fatal("expected error when no API key set")
	}
}

func TestAPIURL_PublicEndpoint(t *testing.T) {
	a := &authState{mode: authModeAPIKey}
	got := a.apiURL("gemini-2.5-flash", "generateContent")
	want := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}

	got = a.apiURL("gemini-2.5-flash", "streamGenerateContent")
	if !strings.HasSuffix(got, "?alt=sse") {
		t.Fatalf("streaming URL should append ?alt=sse, got %q", got)
	}
}

func TestAPIURL_VertexEndpoint(t *testing.T) {
	a := &authState{mode: authModeVertex, projectID: "myproj", location: "us-central1"}
	got := a.apiURL("gemini-2.5-flash", "generateContent")
	want := "https://us-central1-aiplatform.googleapis.com/v1/projects/myproj/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent"
	if got != want {
		t.Fatalf("vertex URL: got %q want %q", got, want)
	}
}

// --- Vertex credential wiring -------------------------------------------------

func TestResolveAuth_Vertex_DefaultsToGoogleADC(t *testing.T) {
	a, err := resolveAuth(map[string]any{"auth": "vertex", "project_id": "myproj"})
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if a.mode != authModeVertex {
		t.Fatalf("mode = %s, want vertex", a.mode)
	}
	if a.location != "us-central1" {
		t.Fatalf("location = %q, want the us-central1 default", a.location)
	}
	if a.creds == nil {
		t.Fatal("vertex mode resolved without a credential source")
	}
}

func TestResolveAuth_Vertex_RequiresProjectID(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	_, err := resolveAuth(map[string]any{"auth": "vertex"})
	if err == nil {
		t.Fatal("expected an error when no project is configured")
	}
	if !strings.Contains(err.Error(), "project_id") {
		t.Fatalf("error should name project_id, got %v", err)
	}
}

func TestResolveAuth_Vertex_UnknownCredentialSource(t *testing.T) {
	_, err := resolveAuth(map[string]any{
		"auth":        "vertex",
		"project_id":  "myproj",
		"credentials": "no-such-source",
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

func TestResolveAuth_Vertex_CredentialsMustBeAString(t *testing.T) {
	_, err := resolveAuth(map[string]any{
		"auth":        "vertex",
		"project_id":  "myproj",
		"credentials": 42,
	})
	if err == nil {
		t.Fatal("expected an error for a non-string credentials key")
	}
	if !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("error should name the credentials key, got %v", err)
	}
}

// TestResolveAuth_Vertex_ForwardsServiceAccountJSON is the replacement for the
// old parseServiceAccountJSON coverage: the key file is still generated with a
// throwaway RSA key, but it is now read and understood by ADC, so what the test
// asserts is that the config key reaches the credential source at all.
func TestResolveAuth_Vertex_ForwardsServiceAccountJSON(t *testing.T) {
	path := writeServiceAccountKey(t, "https://oauth2.googleapis.com/token")

	a, err := resolveAuth(map[string]any{
		"auth":                 "vertex",
		"project_id":           "myproj",
		"service_account_json": path,
	})
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if a.creds == nil {
		t.Fatal("vertex mode resolved without a credential source")
	}
}

func TestResolveAuth_Vertex_ForwardsServiceAccountJSONEnv(t *testing.T) {
	path := writeServiceAccountKey(t, "https://oauth2.googleapis.com/token")
	t.Setenv("NEXUS_TEST_GEMINI_KEY", path)

	a, err := resolveAuth(map[string]any{
		"auth":                     "vertex",
		"project_id":               "myproj",
		"service_account_json_env": "NEXUS_TEST_GEMINI_KEY",
	})
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if a.creds == nil {
		t.Fatal("vertex mode resolved without a credential source")
	}
}

// A bad key-file path must fail boot, not the first LLM call.
func TestResolveAuth_Vertex_MissingKeyFileFailsAtInit(t *testing.T) {
	_, err := resolveAuth(map[string]any{
		"auth":                 "vertex",
		"project_id":           "myproj",
		"service_account_json": filepath.Join(t.TempDir(), "absent.json"),
	})
	if err == nil {
		t.Fatal("expected an error for a missing key file")
	}
	if !strings.Contains(err.Error(), "service_account_json") {
		t.Fatalf("error should name the config key, got %v", err)
	}
}

// --- applyAuth ----------------------------------------------------------------

func TestApplyAuth_APIKeySetsGoogHeader(t *testing.T) {
	a := &authState{mode: authModeAPIKey, apiKey: "secret"}
	req := httptest.NewRequest(http.MethodPost, "https://example.invalid/", nil)
	if err := a.applyAuth(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("x-goog-api-key"); got != "secret" {
		t.Fatalf("x-goog-api-key = %q, want secret", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("api_key mode must not set Authorization, got %q", got)
	}
}

func TestApplyAuth_VertexSetsBearerFromSource(t *testing.T) {
	registerFakeSource(t, "gemini-test-source", stubSource{token: "tok-123"})

	a, err := resolveAuth(map[string]any{
		"auth":        "vertex",
		"project_id":  "myproj",
		"credentials": "gemini-test-source",
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "https://example.invalid/", nil)
	if err := a.applyAuth(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok-123" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer tok-123")
	}
	if got := req.Header.Get("x-goog-api-key"); got != "" {
		t.Fatalf("vertex mode must not set x-goog-api-key, got %q", got)
	}
}

func TestApplyAuth_VertexPropagatesSourceError(t *testing.T) {
	registerFakeSource(t, "gemini-test-broken", stubSource{err: errors.New("metadata server unreachable")})

	a, err := resolveAuth(map[string]any{
		"auth":        "vertex",
		"project_id":  "myproj",
		"credentials": "gemini-test-broken",
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "https://example.invalid/", nil)
	err = a.applyAuth(context.Background(), req)
	if err == nil {
		t.Fatal("expected the source error to surface")
	}
	if !strings.Contains(err.Error(), "metadata server unreachable") {
		t.Fatalf("underlying error should survive wrapping, got %v", err)
	}
}

// TestApplyAuth_VertexKeyFileMintsBearer walks the whole key-file path end to
// end — RSA key, service-account JSON, ADC, token exchange, header — with the
// key file's own token_uri pointed at an httptest server, so nothing leaves the
// process. It is the behavioural replacement for the deleted signJWT round-trip
// test: the assertion moved from "we built a correct JWT" to "a service-account
// key file produces a bearer token", which is what actually has to hold now
// that the signing belongs to the library.
func TestApplyAuth_VertexKeyFileMintsBearer(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "ya29.from-key-file",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	path := writeServiceAccountKey(t, srv.URL)
	a, err := resolveAuth(map[string]any{
		"auth":                 "vertex",
		"project_id":           "myproj",
		"service_account_json": path,
	})
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "https://example.invalid/", nil)
	if err := a.applyAuth(context.Background(), req); err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer ya29.from-key-file" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer ya29.from-key-file")
	}

	// A second request must reuse the cached token: caching moved to the
	// oauth2.TokenSource when the hand-rolled cache was deleted, and the only
	// way to show it still happens is to count round trips.
	req2 := httptest.NewRequest(http.MethodPost, "https://example.invalid/", nil)
	if err := a.applyAuth(context.Background(), req2); err != nil {
		t.Fatalf("applyAuth (second): %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("token endpoint hit %d times, want 1 — the token is not being cached", got)
	}
}

// --- helpers ------------------------------------------------------------------

// stubSource is a nexuscreds.Source that answers from fixed values, so the
// applyAuth tests exercise the plugin's wiring without any credential library
// in the way.
type stubSource struct {
	token string
	err   error
}

func (s stubSource) Token(context.Context) (string, error) { return s.token, s.err }

// registerFakeSource registers src under name for the duration of the test. The
// nexuscreds registry is process-global and Register panics on a duplicate, so
// the removal is not optional.
func registerFakeSource(t *testing.T, name string, src nexuscreds.Source) {
	t.Helper()
	nexuscreds.Register(name, func(map[string]any) (nexuscreds.Source, error) {
		return src, nil
	})
	t.Cleanup(func() { nexuscreds.Unregister(name) })
}

// writeServiceAccountKey writes a throwaway service-account key file with a
// freshly generated RSA key and returns its path. tokenURI is written into the
// file so ADC exchanges the assertion wherever the test wants it to.
func writeServiceAccountKey(t *testing.T, tokenURI string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	sa := map[string]string{
		"type":           "service_account",
		"project_id":     "myproj",
		"private_key_id": "test-key-id",
		"private_key":    string(pemBytes),
		"client_email":   "test@myproj.iam.gserviceaccount.com",
		"client_id":      "1234567890",
		"token_uri":      tokenURI,
	}
	data, err := json.Marshal(sa)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "service-account.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
