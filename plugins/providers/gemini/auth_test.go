package gemini

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/nexuscreds"

	// Side-effect import: registers the "google-adc" credential source the
	// Vertex tests below open by name. It belongs in a TEST file and only in a
	// test file — non-test gemini code deliberately does not import googleadc,
	// because this plugin is in pkg/engine/allplugins and would otherwise
	// register a credential source into every binary carrying it. The canary
	// that holds that line lives in internal/optincheck.
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
	pinNotOnGCE(t)

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
	pinNotOnGCE(t)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	_, err := resolveAuth(map[string]any{"auth": "vertex"})
	if err == nil {
		t.Fatal("expected an error when no project is configured")
	}
	if !strings.Contains(err.Error(), "project_id") {
		t.Fatalf("error should name project_id, got %v", err)
	}
}

// TestResolveAuth_Vertex_BootsFromMetadataAlone is the pod-identity case: a
// config saying nothing GCP-specific beyond the auth mode must boot, with both
// the project and the location coming off the metadata server. It is the one
// test here that runs the real gcemeta + metadata-library path rather than
// pinning it.
func TestResolveAuth_Vertex_BootsFromMetadataAlone(t *testing.T) {
	newFakeMetadata(t)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	a, err := resolveAuth(map[string]any{"auth": "vertex"})
	if err != nil {
		t.Fatalf("resolveAuth with only auth: vertex should boot on GCE, got %v", err)
	}
	if a.projectID != testMetadataProjectID {
		t.Fatalf("projectID = %q, want %q from the metadata server", a.projectID, testMetadataProjectID)
	}
	if a.location != testMetadataRegion {
		t.Fatalf("location = %q, want %q derived from zone %q", a.location, testMetadataRegion, testMetadataZone)
	}
	if a.creds == nil {
		t.Fatal("vertex mode resolved without a credential source")
	}
}

// --- project_id resolution chain ----------------------------------------------

func TestResolveProjectID_PrefersConfigOverEnvAndMetadata(t *testing.T) {
	pinMetadataProjectID(t, "metadata-project", nil)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "env-project")

	got, err := resolveProjectID(map[string]any{"project_id": "config-project"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "config-project" {
		t.Fatalf("project = %q, want the configured value to win", got)
	}
}

func TestResolveProjectID_FallsBackToTheEnvVar(t *testing.T) {
	pinMetadataProjectID(t, "metadata-project", nil)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "env-project")

	got, err := resolveProjectID(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "env-project" {
		t.Fatalf("project = %q, want the env var to win over metadata", got)
	}
}

func TestResolveProjectID_FallsBackToTheMetadataServer(t *testing.T) {
	pinMetadataProjectID(t, "metadata-project", nil)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	got, err := resolveProjectID(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "metadata-project" {
		t.Fatalf("project = %q, want the metadata server's answer", got)
	}
}

func TestResolveProjectID_FailsOnlyWhenEveryStepFails(t *testing.T) {
	pinNotOnGCE(t)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	_, err := resolveProjectID(map[string]any{})
	if err == nil {
		t.Fatal("expected an error when config, env and metadata all fail")
	}
	if !strings.Contains(err.Error(), "project_id") {
		t.Fatalf("error should name project_id, got %v", err)
	}
	if !strings.Contains(err.Error(), "GOOGLE_CLOUD_PROJECT") {
		t.Fatalf("error should name the env var, got %v", err)
	}
}

// A metadata server that answers with an error is a different situation from
// one that does not exist, and the reason must survive into the boot failure.
func TestResolveProjectID_SurfacesAMetadataServerError(t *testing.T) {
	pinMetadataProjectID(t, "", errors.New("metadata server returned 500"))
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	_, err := resolveProjectID(map[string]any{})
	if err == nil {
		t.Fatal("expected an error when the metadata server fails")
	}
	if !strings.Contains(err.Error(), "metadata server returned 500") {
		t.Fatalf("underlying error should survive wrapping, got %v", err)
	}
}

// --- location resolution chain ------------------------------------------------

func TestResolveLocation_PrefersConfigOverMetadata(t *testing.T) {
	pinMetadataRegion(t, "europe-west4", nil)

	got, origin := resolveLocation(map[string]any{"location": "asia-northeast1"})
	if got != "asia-northeast1" {
		t.Fatalf("location = %q, want the configured value to win", got)
	}
	if origin != locationFromConfig {
		t.Fatalf("origin = %q, want %q", origin, locationFromConfig)
	}
}

func TestResolveLocation_DerivesTheRegionFromTheMetadataServer(t *testing.T) {
	pinMetadataRegion(t, "europe-west4", nil)

	got, origin := resolveLocation(map[string]any{})
	if got != "europe-west4" {
		t.Fatalf("location = %q, want the region derived from this pod's zone", got)
	}
	if origin != locationFromMetadata {
		t.Fatalf("origin = %q, want %q", origin, locationFromMetadata)
	}
}

func TestResolveLocation_FallsBackToTheDefaultWhenNotOnGCE(t *testing.T) {
	pinNotOnGCE(t)

	got, origin := resolveLocation(map[string]any{})
	if got != "us-central1" {
		t.Fatalf("location = %q, want the us-central1 default off GCE", got)
	}
	if origin != locationFromDefault {
		t.Fatalf("origin = %q, want %q", origin, locationFromDefault)
	}
}

// A metadata server that exists but cannot answer must not fail the boot: the
// location has a usable default, unlike the project.
func TestResolveLocation_FallsBackToTheDefaultOnAMetadataError(t *testing.T) {
	pinMetadataRegion(t, "", errors.New("metadata server returned 500"))

	got, origin := resolveLocation(map[string]any{})
	if got != "us-central1" {
		t.Fatalf("location = %q, want the us-central1 default, got a failure instead", got)
	}
	// The whole reason the origin exists: "us-central1" here means the
	// metadata server failed, not that anybody chose it.
	if origin != locationFromDefault {
		t.Fatalf("origin = %q, want %q", origin, locationFromDefault)
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

// --- boot-time credential confirmation ----------------------------------------

// TestInit_VertexConfirmsAMetadataCredentialWithOneInfoLine is the pod-identity
// case end to end: a config saying nothing but auth: vertex resolves its
// project and region off the metadata server, mints a token from it, and says
// so exactly once.
func TestInit_VertexConfirmsAMetadataCredentialWithOneInfoLine(t *testing.T) {
	fake := newFakeMetadata(t)
	isolateADC(t)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	records, err := initWithCapturedLog(t, map[string]any{"auth": "vertex"})
	if err != nil {
		t.Fatalf("Init on a pod with a working metadata server should boot, got %v", err)
	}

	rec := onlyConfirmationRecord(t, records)
	if got := rec["level"]; got != "INFO" {
		t.Errorf("level = %v, want INFO", got)
	}
	if got := rec["credential_source"]; got != defaultCredentialSource {
		t.Errorf("credential_source = %v, want %q", got, defaultCredentialSource)
	}
	if got := rec["project"]; got != testMetadataProjectID {
		t.Errorf("project = %v, want %q", got, testMetadataProjectID)
	}
	if got := rec["location"]; got != testMetadataRegion {
		t.Errorf("location = %v, want %q", got, testMetadataRegion)
	}
	if got := rec["location_source"]; got != string(locationFromMetadata) {
		t.Errorf("location_source = %v, want %q", got, locationFromMetadata)
	}

	if got := fake.hits(); got != 1 {
		t.Errorf("token endpoint hit %d times during Init, want exactly 1", got)
	}
}

// The other half of the same guarantee: a key-file credential is confirmed the
// same way, and the location's origin says "default" rather than leaving an
// operator to wonder whether us-central1 was chosen or merely left standing.
func TestInit_VertexConfirmsAKeyFileCredentialWithOneInfoLine(t *testing.T) {
	pinNotOnGCE(t)

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

	records, err := initWithCapturedLog(t, map[string]any{
		"auth":                 "vertex",
		"project_id":           "myproj",
		"service_account_json": writeServiceAccountKey(t, srv.URL),
	})
	if err != nil {
		t.Fatalf("Init with a usable key file should boot, got %v", err)
	}

	rec := onlyConfirmationRecord(t, records)
	if got := rec["level"]; got != "INFO" {
		t.Errorf("level = %v, want INFO", got)
	}
	if got := rec["credential_source"]; got != defaultCredentialSource {
		t.Errorf("credential_source = %v, want %q", got, defaultCredentialSource)
	}
	if got := rec["project"]; got != "myproj" {
		t.Errorf("project = %v, want myproj", got)
	}
	if got := rec["location_source"]; got != string(locationFromDefault) {
		t.Errorf("location_source = %v, want %q", got, locationFromDefault)
	}

	if got := hits.Load(); got != 1 {
		t.Errorf("token endpoint hit %d times during Init, want exactly 1", got)
	}
}

// The line must never carry the token it just minted, nor anything else that
// would be unsafe to ship to a log aggregator.
func TestInit_VertexConfirmationCarriesNoTokenMaterial(t *testing.T) {
	newFakeMetadata(t)
	isolateADC(t)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	records, err := initWithCapturedLog(t, map[string]any{"auth": "vertex"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	for _, rec := range records {
		for key, val := range rec {
			s, ok := val.(string)
			if !ok {
				continue
			}
			if strings.Contains(s, testMetadataToken) {
				t.Fatalf("log field %q leaked the access token", key)
			}
			if strings.Contains(s, testMetadataSAEmail) {
				t.Fatalf("log field %q leaked the acting principal", key)
			}
		}
	}
}

// TestInit_VertexFailsBootWhenTheMetadataServerRefusesAToken is the whole
// reason Init mints at all. Construction of the credential source succeeds on
// a pod whose Workload Identity binding is broken — nothing is unreadable, no
// name is unknown — so without the mint this deployment boots clean and 403s
// on its first user message.
func TestInit_VertexFailsBootWhenTheMetadataServerRefusesAToken(t *testing.T) {
	fake := newFakeMetadata(t)
	fake.failToken(http.StatusForbidden)
	isolateADC(t)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	records, err := initWithCapturedLog(t, map[string]any{"auth": "vertex"})
	if err == nil {
		t.Fatal("Init must fail when no token can be obtained")
	}
	if !strings.Contains(err.Error(), defaultCredentialSource) {
		t.Errorf("error should name the credential source, got %v", err)
	}
	for _, rec := range records {
		if rec["msg"] == credentialConfirmedMsg {
			t.Fatal("a failed boot must not claim credentials were confirmed")
		}
	}
}

func TestInit_VertexFailsBootWhenTheCredentialSourceCannotMint(t *testing.T) {
	pinNotOnGCE(t)
	registerFakeSource(t, "gemini-test-unmintable", stubSource{err: errors.New("metadata server unreachable")})

	_, err := initWithCapturedLog(t, map[string]any{
		"auth":        "vertex",
		"project_id":  "myproj",
		"credentials": "gemini-test-unmintable",
	})
	if err == nil {
		t.Fatal("Init must fail when the credential source cannot mint a token")
	}
	if !strings.Contains(err.Error(), "metadata server unreachable") {
		t.Errorf("underlying error should survive wrapping, got %v", err)
	}
}

// api_key mode has no credential to connect with, so it gains no log line —
// and Init stays free of INFO-level output entirely.
func TestInit_APIKeyModeLogsNothingNew(t *testing.T) {
	records, err := initWithCapturedLog(t, map[string]any{"api_key": "test-key-not-real"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	for _, rec := range records {
		if rec["level"] == "INFO" {
			t.Errorf("api_key Init emitted an INFO record: %v", rec)
		}
	}
}

// --- boot-log helpers ---------------------------------------------------------

// credentialConfirmedMsg is the message confirmCredentials logs. Spelled out
// here rather than shared with the source so a silent rename of the operator-
// facing string is a test failure.
const credentialConfirmedMsg = "vertex credentials confirmed"

// initWithCapturedLog runs a real Init against a real bus with a logger
// writing JSON into a buffer, and returns the decoded records along with
// Init's error.
func initWithCapturedLog(t *testing.T, cfg map[string]any) ([]map[string]any, error) {
	t.Helper()

	var buf bytes.Buffer
	// Well below DEBUG, so nothing Init emits at any level can escape the
	// assertions below by being filtered out.
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.Level(-16)}))

	p := &Plugin{}
	err := p.Init(engine.PluginContext{
		Config: cfg,
		Bus:    engine.NewEventBus(),
		Logger: logger,
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
	return records, err
}

// onlyConfirmationRecord asserts the confirmation line was emitted exactly
// once and returns it.
func onlyConfirmationRecord(t *testing.T, records []map[string]any) map[string]any {
	t.Helper()

	var found []map[string]any
	for _, rec := range records {
		if rec["msg"] == credentialConfirmedMsg {
			found = append(found, rec)
		}
	}
	if len(found) != 1 {
		t.Fatalf("got %d %q log lines, want exactly 1", len(found), credentialConfirmedMsg)
	}
	return found[0]
}
