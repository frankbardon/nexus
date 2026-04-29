package gemini

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
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

func TestSignJWT_RoundTrip(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	a := &authState{mode: authModeVertex, saEmail: "sa@proj.iam.gserviceaccount.com", saKey: key}

	jwt, err := a.signJWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3-segment JWT, got %d", len(parts))
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var header map[string]string
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatalf("parse header: %v", err)
	}
	if header["alg"] != "RS256" {
		t.Fatalf("alg=%s, want RS256", header["alg"])
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["iss"] != a.saEmail {
		t.Fatalf("iss mismatch: %v", claims["iss"])
	}
	if claims["aud"] != "https://oauth2.googleapis.com/token" {
		t.Fatalf("aud mismatch: %v", claims["aud"])
	}
}

func TestParseServiceAccountJSON(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})

	sa := map[string]string{
		"client_email": "test@proj.iam.gserviceaccount.com",
		"private_key":  string(pemBytes),
	}
	saJSON, _ := json.Marshal(sa)

	email, parsedKey, err := parseServiceAccountJSON(saJSON)
	if err != nil {
		t.Fatal(err)
	}
	if email != sa["client_email"] {
		t.Fatalf("email mismatch: %s", email)
	}
	if parsedKey.N.Cmp(key.N) != 0 {
		t.Fatal("parsed key modulus mismatch")
	}
}

func TestParseServiceAccountJSON_InvalidPEM(t *testing.T) {
	saJSON := []byte(`{"client_email":"x","private_key":"not a pem"}`)
	if _, _, err := parseServiceAccountJSON(saJSON); err == nil {
		t.Fatal("expected error on bad PEM")
	}
}
