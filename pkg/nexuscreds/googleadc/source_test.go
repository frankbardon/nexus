package googleadc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/frankbardon/nexus/pkg/nexuscreds"
)

const testKeyEmail = "keyfile-agent@nexus-test-project.iam.gserviceaccount.com"

// writeServiceAccountKey writes a service_account credentials file whose
// token_uri points at tokenURL, signed by a freshly generated throwaway RSA
// key. The key is generated per call and never leaves the test's temp dir —
// there is no credential fixture anywhere in this repository, and there must
// not be one.
func writeServiceAccountKey(t *testing.T, dir, tokenURL string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating a throwaway RSA key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling the throwaway key: %v", err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	body, err := json.Marshal(map[string]string{
		"type":         "service_account",
		"project_id":   testProjectID,
		"private_key":  string(pemKey),
		"client_email": testKeyEmail,
		"client_id":    "1234567890",
		"token_uri":    tokenURL,
	})
	if err != nil {
		t.Fatalf("marshalling the key file: %v", err)
	}

	path := filepath.Join(dir, "service-account.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("writing the key file: %v", err)
	}
	return path
}

// newTokenEndpoint starts a fake OAuth2 token endpoint for the key-file path
// and reports how many times it was asked for a token.
func newTokenEndpoint(t *testing.T, token string, expiresIn int) (url string, hits *atomic.Int32) {
	t.Helper()
	hits = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": token,
			"token_type":   "Bearer",
			"expires_in":   expiresIn,
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, hits
}

func TestSource_RegistersUnderGoogleADC(t *testing.T) {
	if !nexuscreds.Registered(Name) {
		t.Fatalf("nexuscreds.Registered(%q) = false; the package init did not register the source", Name)
	}
	src, err := nexuscreds.Open(Name, nil)
	if err != nil {
		t.Fatalf("nexuscreds.Open(%q): unexpected error: %v", Name, err)
	}
	if _, ok := src.(*Source); !ok {
		t.Fatalf("Open returned %T, want *googleadc.Source", src)
	}
}

func TestSource_MintsATokenFromTheMetadataServer(t *testing.T) {
	fake := newFakeMetadata(t)

	src, err := New(nil)
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: unexpected error: %v", err)
	}
	if tok != "ya29.fake-metadata-token" {
		t.Fatalf("Token = %q, want the token the metadata server minted", tok)
	}
	if got := fake.hits(); got != 1 {
		t.Fatalf("token endpoint hits = %d, want 1", got)
	}
}

func TestSource_ReusesACachedTokenAcrossCalls(t *testing.T) {
	fake := newFakeMetadata(t)

	src, err := New(nil)
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	first, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("first Token: unexpected error: %v", err)
	}

	// Change what a fresh mint would return. If the second call went back to
	// the metadata server it would pick this up.
	fake.setToken("ya29.second-token", 3600)

	second, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("second Token: unexpected error: %v", err)
	}
	if second != first {
		t.Fatalf("second Token = %q, want the cached %q", second, first)
	}
	if got := fake.hits(); got != 1 {
		t.Fatalf("token endpoint hits = %d, want 1 — the oauth2.TokenSource should have cached", got)
	}
}

func TestSource_RefreshesAfterTheTokenExpires(t *testing.T) {
	fake := newFakeMetadata(t)
	// One second of life, against oauth2's ten-second early-refresh window, so
	// the first token is stale the moment it is issued. No sleeping involved.
	fake.setToken("ya29.short-lived", 1)

	src, err := New(nil)
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	first, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("first Token: unexpected error: %v", err)
	}
	if first != "ya29.short-lived" {
		t.Fatalf("first Token = %q, want %q", first, "ya29.short-lived")
	}

	fake.setToken("ya29.refreshed", 3600)
	second, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("second Token: unexpected error: %v", err)
	}
	if second != "ya29.refreshed" {
		t.Fatalf("second Token = %q, want the refreshed %q", second, "ya29.refreshed")
	}
	if got := fake.hits(); got != 2 {
		t.Fatalf("token endpoint hits = %d, want 2 — an expired token should have been refreshed", got)
	}
}

func TestSource_SurfacesTheForbiddenAPodGetsWithoutAWorkloadIdentityBinding(t *testing.T) {
	fake := newFakeMetadata(t)
	// What the metadata server actually says when the pod's Kubernetes service
	// account is not bound to a Google service account.
	fake.failToken(http.StatusForbidden, "Unable to generate access token; IAM returned 403 Forbidden")

	src, err := New(nil)
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	tok, err := src.Token(context.Background())
	if err == nil {
		t.Fatalf("Token returned %q and no error, want the metadata server's 403", tok)
	}
	if tok != "" {
		t.Fatalf("Token returned %q alongside an error, want the empty string", tok)
	}
	msg := err.Error()
	for _, want := range []string{"403", "IAM returned 403 Forbidden"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q — an operator needs the server's own words here", msg, want)
		}
	}
}

func TestSource_ResolveReportsTheMetadataCredential(t *testing.T) {
	newFakeMetadata(t)

	src, err := New(nil)
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	got, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if got.Type != TypeMetadata {
		t.Errorf("Resolve().Type = %q, want %q", got.Type, TypeMetadata)
	}
	if got.Principal != testSAEmail {
		t.Errorf("Resolve().Principal = %q, want %q", got.Principal, testSAEmail)
	}
	if got.ProjectID != testProjectID {
		t.Errorf("Resolve().ProjectID = %q, want %q", got.ProjectID, testProjectID)
	}
	if got.Origin != originADC {
		t.Errorf("Resolve().Origin = %q, want %q", got.Origin, originADC)
	}
}

func TestSource_ResolveToleratesAMetadataServerThatWillNotNameTheAccount(t *testing.T) {
	fake := newFakeMetadata(t)
	fake.mu.Lock()
	fake.emailStatus = http.StatusNotFound
	fake.mu.Unlock()

	src, err := New(nil)
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	got, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v — an unnamed principal must not fail the turn", err)
	}
	if got.Type != TypeMetadata {
		t.Errorf("Resolve().Type = %q, want %q", got.Type, TypeMetadata)
	}
	if got.Principal != "" {
		t.Errorf("Resolve().Principal = %q, want the empty string", got.Principal)
	}
}

func TestSource_MintsATokenFromAConfiguredKeyFile(t *testing.T) {
	newFakeMetadata(t) // present but must go unused: an explicit key file wins.
	tokenURL, hits := newTokenEndpoint(t, "ya29.from-key-file", 3600)
	path := writeServiceAccountKey(t, t.TempDir(), tokenURL)

	src, err := New(map[string]any{"service_account_json": path})
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: unexpected error: %v", err)
	}
	if tok != "ya29.from-key-file" {
		t.Fatalf("Token = %q, want the key file's token endpoint to have minted it", tok)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("key-file token endpoint hits = %d, want 1", got)
	}
}

func TestSource_ResolveReportsAServiceAccountKeyFile(t *testing.T) {
	tokenURL, _ := newTokenEndpoint(t, "ya29.unused", 3600)
	path := writeServiceAccountKey(t, t.TempDir(), tokenURL)

	src, err := New(map[string]any{"service_account_json": path})
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	got, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if got.Type != TypeServiceAccount {
		t.Errorf("Resolve().Type = %q, want %q", got.Type, TypeServiceAccount)
	}
	if got.Principal != testKeyEmail {
		t.Errorf("Resolve().Principal = %q, want %q", got.Principal, testKeyEmail)
	}
	if got.ProjectID != testProjectID {
		t.Errorf("Resolve().ProjectID = %q, want %q", got.ProjectID, testProjectID)
	}
	if got.Origin != "service_account_json" {
		t.Errorf("Resolve().Origin = %q, want the config key that named the file", got.Origin)
	}
}

func TestNew_ExpandsATildeInTheKeyFilePath(t *testing.T) {
	// TestMain points $HOME at a temp dir, so "~/..." resolves inside it.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolving the test home dir: %v", err)
	}
	tokenURL, _ := newTokenEndpoint(t, "ya29.unused", 3600)
	writeServiceAccountKey(t, home, tokenURL)

	src, err := New(map[string]any{"service_account_json": "~/service-account.json"})
	if err != nil {
		t.Fatalf("New with a tilde path: unexpected error: %v", err)
	}
	got, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if got.Principal != testKeyEmail {
		t.Fatalf("Resolve().Principal = %q, want %q — the tilde path did not reach the right file", got.Principal, testKeyEmail)
	}
}

func TestNew_ReadsTheKeyFilePathFromTheNamedEnvVar(t *testing.T) {
	tokenURL, _ := newTokenEndpoint(t, "ya29.unused", 3600)
	path := writeServiceAccountKey(t, t.TempDir(), tokenURL)
	t.Setenv("NEXUS_TEST_GOOGLE_KEY", path)

	src, err := New(map[string]any{"service_account_json_env": "NEXUS_TEST_GOOGLE_KEY"})
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	got, err := src.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if got.Origin != "service_account_json_env=NEXUS_TEST_GOOGLE_KEY" {
		t.Fatalf("Resolve().Origin = %q, want it to name the env var", got.Origin)
	}
}

func TestNew_ReportsAMissingKeyFileAtConstruction(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")

	src, err := New(map[string]any{"service_account_json": missing})
	if err == nil {
		t.Fatalf("New with a missing key file returned no error (source %#v)", src)
	}
	if !strings.Contains(err.Error(), "service_account_json") {
		t.Errorf("error %q does not name the config key at fault", err)
	}
}

func TestNew_RejectsAnEnvVarThatIsNotSet(t *testing.T) {
	t.Setenv("NEXUS_TEST_GOOGLE_KEY_UNSET", "")

	_, err := New(map[string]any{"service_account_json_env": "NEXUS_TEST_GOOGLE_KEY_UNSET"})
	if err == nil {
		t.Fatal("New with an unset env var returned no error — falling through to the ADC chain would silently change identity")
	}
	if !strings.Contains(err.Error(), "NEXUS_TEST_GOOGLE_KEY_UNSET") {
		t.Errorf("error %q does not name the variable", err)
	}
}

func TestNew_RejectsANonStringKeyFilePath(t *testing.T) {
	if _, err := New(map[string]any{"service_account_json": 42}); err == nil {
		t.Fatal("New accepted a non-string service_account_json")
	}
	if _, err := New(map[string]any{"service_account_json_env": []any{"a"}}); err == nil {
		t.Fatal("New accepted a non-string service_account_json_env")
	}
}

func TestNew_RejectsAnEmptyKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("writing the empty file: %v", err)
	}

	_, err := New(map[string]any{"service_account_json": path})
	if err == nil {
		t.Fatal("New accepted an empty key file")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error %q does not say the file is empty", err)
	}
}

func TestNew_NilConfigRunsTheFullADCChain(t *testing.T) {
	newFakeMetadata(t)

	src, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil): unexpected error: %v", err)
	}
	if src.keyJSON != nil {
		t.Fatal("New(nil) loaded key material from somewhere")
	}
	if src.origin != originADC {
		t.Fatalf("origin = %q, want %q", src.origin, originADC)
	}
}

func TestSource_RejectsAKeyFileOfAnUnsupportedType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "odd.json")
	body := []byte(`{"type":"magic_beans","client_email":"x@example.com"}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("writing the key file: %v", err)
	}

	src, err := New(map[string]any{"service_account_json": path})
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	_, err = src.Token(context.Background())
	if err == nil {
		t.Fatal("Token accepted a credentials file of an unknown type")
	}
	if !strings.Contains(err.Error(), "magic_beans") {
		t.Errorf("error %q does not name the type it found", err)
	}
	if !strings.Contains(err.Error(), "service_account") {
		t.Errorf("error %q does not list the types that are supported", err)
	}
}

func TestSource_TokenHonoursCallerCancellation(t *testing.T) {
	fake := newFakeMetadata(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	fake.mu.Lock()
	fake.blockToken = release
	fake.mu.Unlock()

	src, err := New(nil)
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := src.Token(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Token with a cancelled context: err = %v, want it to wrap context.Canceled", err)
	}
}

func TestSource_DoesNotMemoiseAFailedResolution(t *testing.T) {
	// A pod whose credential is not yet in place must recover on a later turn
	// rather than need a restart, so a failed resolution is not cached.
	newFakeMetadata(t)
	tokenURL, _ := newTokenEndpoint(t, "ya29.recovered", 3600)
	dir := t.TempDir()
	path := filepath.Join(dir, "adc.json")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)

	src, err := New(nil)
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("Token returned no error while GOOGLE_APPLICATION_CREDENTIALS named a file that does not exist")
	}
	if src.creds != nil {
		t.Fatal("a failed resolution was memoised; the process could never recover")
	}

	// The credential arrives. The next turn must pick it up.
	written := writeServiceAccountKey(t, dir, tokenURL)
	if err := os.Rename(written, path); err != nil {
		t.Fatalf("moving the key file into place: %v", err)
	}
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token after the credential appeared: unexpected error: %v", err)
	}
	if tok != "ya29.recovered" {
		t.Fatalf("Token = %q, want %q", tok, "ya29.recovered")
	}
}

func TestCloudPlatformScope_IsTheOnlyScopeRequested(t *testing.T) {
	// The scope is fixed, not configurable. Assert the constant so a change
	// has to be deliberate.
	const want = "https://www.googleapis.com/auth/cloud-platform"
	if CloudPlatformScope != want {
		t.Fatalf("CloudPlatformScope = %q, want %q", CloudPlatformScope, want)
	}
}

func TestSource_RequestsTheCloudPlatformScopeFromTheMetadataServer(t *testing.T) {
	var gotScopes atomic.Value
	gotScopes.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			gotScopes.Store(r.URL.Query().Get("scopes"))
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"access_token":"ya29.scoped","expires_in":3600,"token_type":"Bearer"}`)
			return
		}
		w.Header().Set("Metadata-Flavor", "Google")
		fmt.Fprint(w, testProjectID)
	}))
	defer srv.Close()
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(srv.URL, "http://"))

	src, err := New(nil)
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("Token: unexpected error: %v", err)
	}
	if got := gotScopes.Load().(string); got != CloudPlatformScope {
		t.Fatalf("metadata server was asked for scopes %q, want %q", got, CloudPlatformScope)
	}
}
