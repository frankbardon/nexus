package gemini

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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
	authModeAPIKey authMode = "api_key" // public Gemini Generative Language API
	authModeVertex authMode = "vertex"  // Vertex AI on Google Cloud
)

// authState holds resolved auth configuration and cached credentials.
type authState struct {
	mode authMode

	// API-key path.
	apiKey string

	// Vertex path.
	saEmail   string
	saKey     *rsa.PrivateKey
	projectID string
	location  string // e.g. "us-central1"

	mu          sync.Mutex
	tokenCache  string
	tokenExpiry time.Time
}

// resolveAuth reads auth config and returns a configured authState.
func resolveAuth(cfg map[string]any) (*authState, error) {
	mode := authModeAPIKey
	if v, ok := cfg["auth"].(string); ok {
		switch authMode(v) {
		case authModeAPIKey, authModeVertex:
			mode = authMode(v)
		default:
			return nil, fmt.Errorf("gemini: unknown auth mode %q (expected api_key or vertex)", v)
		}
	}

	a := &authState{mode: mode}

	switch mode {
	case authModeAPIKey:
		if key, ok := cfg["api_key"].(string); ok && key != "" {
			a.apiKey = key
		} else {
			envVar, _ := cfg["api_key_env"].(string)
			if envVar != "" {
				a.apiKey = os.Getenv(envVar)
			} else {
				if v := os.Getenv("GEMINI_API_KEY"); v != "" {
					a.apiKey = v
				} else if v := os.Getenv("GOOGLE_API_KEY"); v != "" {
					a.apiKey = v
				}
			}
		}
		if a.apiKey == "" {
			return nil, fmt.Errorf("gemini: no API key configured (set api_key in config or GEMINI_API_KEY / GOOGLE_API_KEY env var)")
		}

	case authModeVertex:
		project, _ := cfg["project_id"].(string)
		if project == "" {
			project = os.Getenv("GOOGLE_CLOUD_PROJECT")
		}
		if project == "" {
			return nil, fmt.Errorf("gemini: vertex auth requires project_id config or GOOGLE_CLOUD_PROJECT env var")
		}
		a.projectID = project

		loc, _ := cfg["location"].(string)
		if loc == "" {
			loc = "us-central1"
		}
		a.location = loc

		var saJSON []byte
		if path, ok := cfg["service_account_json"].(string); ok && path != "" {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("gemini: read service_account_json: %w", err)
			}
			saJSON = data
		} else {
			envVar, _ := cfg["service_account_json_env"].(string)
			if envVar == "" {
				envVar = "GOOGLE_APPLICATION_CREDENTIALS"
			}
			if path := os.Getenv(envVar); path != "" {
				data, err := os.ReadFile(path)
				if err != nil {
					return nil, fmt.Errorf("gemini: read %s=%s: %w", envVar, path, err)
				}
				saJSON = data
			}
		}
		if saJSON == nil {
			return nil, fmt.Errorf("gemini: vertex auth requires service_account_json or service_account_json_env")
		}

		email, key, err := parseServiceAccountJSON(saJSON)
		if err != nil {
			return nil, fmt.Errorf("gemini: parse service account: %w", err)
		}
		a.saEmail = email
		a.saKey = key
	}

	return a, nil
}

// apiURL builds the request URL for a model + operation.
// op is "generateContent" or "streamGenerateContent". For streaming, alt=sse
// is appended.
func (a *authState) apiURL(model, op string) string {
	switch a.mode {
	case authModeVertex:
		base := fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:%s",
			a.location, a.projectID, a.location, model, op)
		if op == "streamGenerateContent" {
			base += "?alt=sse"
		}
		return base
	default:
		base := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:%s", model, op)
		if op == "streamGenerateContent" {
			base += "?alt=sse"
		}
		return base
	}
}

// applyAuth attaches credentials to an outgoing request. For Vertex it may
// fetch and cache a fresh OAuth2 token.
func (a *authState) applyAuth(ctx context.Context, req *http.Request, httpClient *http.Client) error {
	switch a.mode {
	case authModeAPIKey:
		req.Header.Set("x-goog-api-key", a.apiKey)
		return nil
	case authModeVertex:
		token, err := a.vertexToken(ctx, httpClient)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
	return fmt.Errorf("gemini: unknown auth mode")
}

// filesAPIBaseURL returns the base URL for the Files API. Files API is only
// available on the public endpoint; in Vertex mode an empty string is returned
// to signal callers to use inline data.
func (a *authState) filesAPIBaseURL() string {
	if a.mode == authModeVertex {
		return ""
	}
	return "https://generativelanguage.googleapis.com"
}

// cachedContentsURL returns the base URL for the cachedContents resource.
func (a *authState) cachedContentsURL() string {
	switch a.mode {
	case authModeVertex:
		return fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/cachedContents",
			a.location, a.projectID, a.location)
	default:
		return "https://generativelanguage.googleapis.com/v1beta/cachedContents"
	}
}

// vertexToken returns a valid OAuth2 access token, minting a new one if
// needed. Tokens are cached in-memory until expiry minus 60s skew.
func (a *authState) vertexToken(ctx context.Context, client *http.Client) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.tokenCache != "" && time.Now().Before(a.tokenExpiry) {
		return a.tokenCache, nil
	}

	jwt, err := a.signJWT()
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", jwt)

	req, err := http.NewRequestWithContext(ctx, "POST", "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, string(body))
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("empty access_token in response: %s", string(body))
	}

	a.tokenCache = tr.AccessToken
	expiresIn := time.Duration(tr.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 1 * time.Hour
	}
	a.tokenExpiry = time.Now().Add(expiresIn - 60*time.Second)

	return a.tokenCache, nil
}

// signJWT mints an RS256-signed JWT bearer assertion for Google's token endpoint.
func (a *authState) signJWT() (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	headerJSON, _ := json.Marshal(header)

	now := time.Now().Unix()
	claims := map[string]any{
		"iss":   a.saEmail,
		"scope": "https://www.googleapis.com/auth/cloud-platform",
		"aud":   "https://oauth2.googleapis.com/token",
		"exp":   now + 3600,
		"iat":   now,
	}
	claimsJSON, _ := json.Marshal(claims)

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)

	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.saKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// parseServiceAccountJSON extracts client_email and the private RSA key.
func parseServiceAccountJSON(data []byte) (string, *rsa.PrivateKey, error) {
	var sa struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
	}
	if err := json.Unmarshal(data, &sa); err != nil {
		return "", nil, fmt.Errorf("decode JSON: %w", err)
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return "", nil, fmt.Errorf("missing client_email or private_key")
	}

	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return "", nil, fmt.Errorf("invalid PEM in private_key")
	}

	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return sa.ClientEmail, k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", nil, fmt.Errorf("parse private_key: %w", err)
	}
	rsaKey, ok := k.(*rsa.PrivateKey)
	if !ok {
		return "", nil, fmt.Errorf("private_key is not RSA")
	}
	return sa.ClientEmail, rsaKey, nil
}
