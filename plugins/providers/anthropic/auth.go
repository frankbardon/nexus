package anthropic

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
)

// authMode determines how requests are authenticated and routed.
type authMode string

const (
	// authModeAPIKey uses the public api.anthropic.com endpoint with an
	// x-api-key header.
	authModeAPIKey authMode = "api_key"
	// authModeBedrock routes requests through AWS Bedrock with SigV4-signed
	// requests against bedrock-runtime.<region>.amazonaws.com.
	authModeBedrock authMode = "bedrock"
	// authModeVertex routes requests through GCP Vertex AI with an OAuth2
	// service-account JWT bearer token.
	authModeVertex authMode = "vertex"
)

// Body version markers for non-direct backends. The direct API uses the
// anthropic-version: 2023-06-01 HTTP header; Bedrock + Vertex want the version
// in the request body instead.
const (
	bedrockBodyVersion = "bedrock-2023-05-31"
	vertexBodyVersion  = "vertex-2023-10-16"
)

// authState holds resolved auth configuration and any cached credentials.
//
// One authState is constructed at Init time and reused for every request.
// Token caching for Vertex is guarded by mu; the API-key and Bedrock paths
// don't need locking.
type authState struct {
	mode authMode

	// API-key path. Also kept populated by the Bedrock and Vertex paths if
	// the user provided one — the Files API and a few other endpoints are
	// only available on the public Anthropic endpoint, so callers that need
	// them can fall back to direct auth even in cross-cloud mode.
	apiKey string

	// Bedrock path (AWS SigV4).
	bedrockRegion       string
	bedrockAccessKeyID  string
	bedrockSecretKey    string
	bedrockSessionToken string

	// Vertex path (GCP OAuth2 JWT).
	vertexProject string
	vertexRegion  string
	saEmail       string
	saKey         *rsa.PrivateKey

	mu        sync.Mutex
	token     string
	tokExpiry time.Time

	// tokenEndpointOverride redirects the OAuth2 token exchange to a test
	// server. Production code leaves this empty; tests set it to an
	// httptest.Server URL.
	tokenEndpointOverride string
}

// parseAuthConfig builds an authState from the raw plugin config map.
//
// Backwards compatible: if auth_mode is missing or "api_key", the existing
// api_key / api_key_env top-level keys are honored.
func parseAuthConfig(cfg map[string]any) (*authState, error) {
	mode := authModeAPIKey
	if v, ok := cfg["auth_mode"].(string); ok && v != "" {
		switch authMode(v) {
		case authModeAPIKey, authModeBedrock, authModeVertex:
			mode = authMode(v)
		default:
			return nil, fmt.Errorf("anthropic: unknown auth_mode %q (expected api_key, bedrock, or vertex)", v)
		}
	}

	a := &authState{mode: mode}

	// Read the API key from top-level keys regardless of mode — Bedrock and
	// Vertex don't require it, but optional fall-back to direct auth for the
	// Files API stays useful.
	if key, ok := cfg["api_key"].(string); ok && key != "" {
		a.apiKey = key
	} else {
		envVar, _ := cfg["api_key_env"].(string)
		if envVar == "" {
			envVar = "ANTHROPIC_API_KEY"
		}
		if v := os.Getenv(envVar); v != "" {
			a.apiKey = v
		}
	}

	switch mode {
	case authModeAPIKey:
		if a.apiKey == "" {
			return nil, fmt.Errorf("anthropic: no API key configured (set api_key in config or ANTHROPIC_API_KEY env var)")
		}

	case authModeBedrock:
		raw, _ := cfg["bedrock"].(map[string]any)
		if raw == nil {
			raw = map[string]any{}
		}

		region, _ := raw["region"].(string)
		if region == "" {
			region = os.Getenv("AWS_REGION")
		}
		if region == "" {
			region = os.Getenv("AWS_DEFAULT_REGION")
		}
		if region == "" {
			return nil, fmt.Errorf("anthropic: bedrock auth requires bedrock.region (or AWS_REGION env var)")
		}
		a.bedrockRegion = region

		akid := readEnvOrLiteral(raw, "access_key_id", "access_key_id_env", "AWS_ACCESS_KEY_ID")
		if akid == "" {
			return nil, fmt.Errorf("anthropic: bedrock auth requires access_key_id (config or AWS_ACCESS_KEY_ID env var)")
		}
		a.bedrockAccessKeyID = akid

		secret := readEnvOrLiteral(raw, "secret_access_key", "secret_access_key_env", "AWS_SECRET_ACCESS_KEY")
		if secret == "" {
			return nil, fmt.Errorf("anthropic: bedrock auth requires secret_access_key (config or AWS_SECRET_ACCESS_KEY env var)")
		}
		a.bedrockSecretKey = secret

		// Session token is optional (only needed for STS-issued temporary creds).
		a.bedrockSessionToken = readEnvOrLiteral(raw, "session_token", "session_token_env", "AWS_SESSION_TOKEN")

	case authModeVertex:
		raw, _ := cfg["vertex"].(map[string]any)
		if raw == nil {
			raw = map[string]any{}
		}

		project, _ := raw["project"].(string)
		if project == "" {
			project, _ = raw["project_id"].(string)
		}
		if project == "" {
			project = os.Getenv("GOOGLE_CLOUD_PROJECT")
		}
		if project == "" {
			return nil, fmt.Errorf("anthropic: vertex auth requires vertex.project (or GOOGLE_CLOUD_PROJECT env var)")
		}
		a.vertexProject = project

		region, _ := raw["region"].(string)
		if region == "" {
			region, _ = raw["location"].(string)
		}
		if region == "" {
			region = "us-east5"
		}
		a.vertexRegion = region

		var saJSON []byte
		if path, ok := raw["sa_key_file"].(string); ok && path != "" {
			data, err := os.ReadFile(engine.ExpandPath(path))
			if err != nil {
				return nil, fmt.Errorf("anthropic: read sa_key_file: %w", err)
			}
			saJSON = data
		} else if path, ok := raw["service_account_json"].(string); ok && path != "" {
			data, err := os.ReadFile(engine.ExpandPath(path))
			if err != nil {
				return nil, fmt.Errorf("anthropic: read service_account_json: %w", err)
			}
			saJSON = data
		} else {
			envVar, _ := raw["sa_key_file_env"].(string)
			if envVar == "" {
				envVar, _ = raw["service_account_json_env"].(string)
			}
			if envVar == "" {
				envVar = "GOOGLE_APPLICATION_CREDENTIALS"
			}
			if path := os.Getenv(envVar); path != "" {
				data, err := os.ReadFile(engine.ExpandPath(path))
				if err != nil {
					return nil, fmt.Errorf("anthropic: read %s=%s: %w", envVar, path, err)
				}
				saJSON = data
			}
		}
		if saJSON == nil {
			return nil, fmt.Errorf("anthropic: vertex auth requires sa_key_file (or GOOGLE_APPLICATION_CREDENTIALS env var)")
		}

		email, key, err := parseAnthropicVertexServiceAccount(saJSON)
		if err != nil {
			return nil, fmt.Errorf("anthropic: parse service account: %w", err)
		}
		a.saEmail = email
		a.saKey = key
	}

	return a, nil
}

// readEnvOrLiteral reads a config field that may be supplied either inline
// (key) or by env-var indirection (key+"_env"); returns "" if neither is set.
// fallbackEnv is consulted when the config block omits both keys, mirroring
// AWS' standard env-var conventions.
func readEnvOrLiteral(raw map[string]any, literalKey, envKey, fallbackEnv string) string {
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

// buildURL returns the full request URL for the given model + stream flag.
//
//	api_key:  https://api.anthropic.com/v1/messages
//	bedrock:  https://bedrock-runtime.<region>.amazonaws.com/model/<model>/invoke[-with-response-stream]
//	vertex:   https://<region>-aiplatform.googleapis.com/v1/projects/<project>/locations/<region>/publishers/anthropic/models/<model>:rawPredict[-streamed]
func (a *authState) buildURL(model string, stream bool) string {
	switch a.mode {
	case authModeBedrock:
		op := "invoke"
		if stream {
			op = "invoke-with-response-stream"
		}
		// Bedrock model ids contain ":" (e.g. anthropic.claude-sonnet-4-5-v1:0)
		// which url.PathEscape considers a valid pchar and leaves alone.
		// SigV4's canonical URI path explicitly percent-encodes ":" though, so
		// the signature wouldn't match the URL on the wire if we left the
		// colon raw. Force the encoding via sigv4Escape (which honors the
		// RFC 3986 unreserved set) on the model segment so request URL and
		// signing input agree.
		return fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com/model/%s/%s",
			a.bedrockRegion, sigv4Escape(model), op)
	case authModeVertex:
		op := "rawPredict"
		if stream {
			op = "streamRawPredict"
		}
		return fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:%s",
			a.vertexRegion, a.vertexProject, a.vertexRegion, url.PathEscape(model), op)
	default:
		return "https://api.anthropic.com/v1/messages"
	}
}

// bodyVersionField returns the value to inject into body["anthropic_version"]
// for Bedrock/Vertex paths, or "" for the API-key path (which uses the
// anthropic-version: 2023-06-01 HTTP header instead).
func (a *authState) bodyVersionField() string {
	switch a.mode {
	case authModeBedrock:
		return bedrockBodyVersion
	case authModeVertex:
		return vertexBodyVersion
	default:
		return ""
	}
}

// stripModelFromBody reports whether the model field should be omitted from
// the JSON request body. Bedrock encodes the model id in the URL path and
// rejects bodies that also carry "model"; Vertex and the direct API both
// expect the field in the body.
func (a *authState) stripModelFromBody() bool {
	return a.mode == authModeBedrock
}

// applyAuth attaches the right auth credentials (and signs the body for
// Bedrock SigV4) onto the outgoing request. body is required for Bedrock so
// the SigV4 payload hash matches what the server sees; pass the same byte
// slice that's wrapped in req.Body. For other modes, body is ignored.
//
// Bedrock SigV4 must be recomputed on every retry because the timestamp is
// part of the signature — callers funnel this through the doWithRetry
// closure so each attempt gets a fresh signature.
func (a *authState) applyAuth(ctx context.Context, req *http.Request, body []byte, client *http.Client) error {
	switch a.mode {
	case authModeAPIKey:
		req.Header.Set("x-api-key", a.apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		return nil

	case authModeBedrock:
		return a.signBedrockSigV4(req, body, time.Now().UTC())

	case authModeVertex:
		token, err := a.vertexAccessToken(ctx, client)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
	return fmt.Errorf("anthropic: unknown auth mode %q", a.mode)
}

// =====================================================================
// Bedrock SigV4
// =====================================================================

const (
	awsSigningAlgorithm = "AWS4-HMAC-SHA256"
	bedrockServiceName  = "bedrock"
)

// signBedrockSigV4 is the per-mode wrapper that fixes service=bedrock and
// reuses the credentials stored on authState. The actual SigV4 math lives in
// signSigV4 so it's reusable for tests against the AWS-published "Get example"
// vector (which signs against service=iam).
func (a *authState) signBedrockSigV4(req *http.Request, body []byte, now time.Time) error {
	return signSigV4(req, body, sigv4Creds{
		Service:      bedrockServiceName,
		Region:       a.bedrockRegion,
		AccessKey:    a.bedrockAccessKeyID,
		SecretKey:    a.bedrockSecretKey,
		SessionToken: a.bedrockSessionToken,
	}, now)
}

// sigv4Creds bundles the inputs needed for one SigV4 signature.
type sigv4Creds struct {
	Service      string
	Region       string
	AccessKey    string
	SecretKey    string
	SessionToken string // optional STS session token
}

// signSigV4 computes an AWS Signature Version 4 signature for req and sets
// the matching headers in place. Follows
// https://docs.aws.amazon.com/general/latest/gr/sigv4_signing.html.
//
// Hand-rolled (no AWS SDK) per the project's minimal-dependency convention.
// Body bytes must be supplied separately because http.Request.Body is an
// io.ReadCloser; the canonical request needs the hex sha256 of the payload.
func signSigV4(req *http.Request, body []byte, creds sigv4Creds, now time.Time) error {
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	payloadHashHex := hex.EncodeToString(sha256Sum(body))

	// Required signed headers. host is implicitly populated from req.URL.Host
	// (Go won't expose it via req.Header until the request is sent), so we
	// stash it under "host" for canonicalization purposes.
	req.Header.Set("host", req.URL.Host)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHashHex)
	if creds.SessionToken != "" {
		req.Header.Set("x-amz-security-token", creds.SessionToken)
	}

	canonHeaders, signedHeaders := canonicalHeaders(req)
	canonicalQuery := canonicalQueryString(req.URL)

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURIPath(req.URL.Path),
		canonicalQuery,
		canonHeaders, // already terminated by trailing newlines per spec
		signedHeaders,
		payloadHashHex,
	}, "\n")

	scope := strings.Join([]string{dateStamp, creds.Region, creds.Service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		awsSigningAlgorithm,
		amzDate,
		scope,
		hex.EncodeToString(sha256Sum([]byte(canonicalRequest))),
	}, "\n")

	signingKey := deriveSigningKey(creds.SecretKey, dateStamp, creds.Region, creds.Service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	auth := fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		awsSigningAlgorithm,
		creds.AccessKey,
		scope,
		signedHeaders,
		signature)
	req.Header.Set("Authorization", auth)
	return nil
}

// canonicalHeaders returns (joinedCanonHeaders, signedHeaders) per SigV4 spec.
// All headers currently set on req are signed.
func canonicalHeaders(req *http.Request) (string, string) {
	names := make([]string, 0, len(req.Header))
	for name := range req.Header {
		names = append(names, strings.ToLower(name))
	}
	sort.Strings(names)

	var canon strings.Builder
	for _, name := range names {
		// Use the original header name to fetch values — http.Header is
		// case-insensitive on Get but we want the lowered name for the
		// canonical form output.
		values := req.Header.Values(name)
		canon.WriteString(name)
		canon.WriteByte(':')
		for i, v := range values {
			if i > 0 {
				canon.WriteByte(',')
			}
			canon.WriteString(strings.TrimSpace(v))
		}
		canon.WriteByte('\n')
	}
	return canon.String(), strings.Join(names, ";")
}

// canonicalURIPath URI-encodes the path per SigV4 (each segment is percent-
// encoded, but "/" between segments is preserved). For Bedrock we don't
// double-encode — the spec says non-S3 services do encode once.
func canonicalURIPath(p string) string {
	if p == "" {
		return "/"
	}
	// Split + escape each segment, preserving leading "/".
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = sigv4Escape(seg)
	}
	return strings.Join(segs, "/")
}

// canonicalQueryString sorts query parameters by name (then by value) and
// percent-encodes each. SigV4 requires keys and values escaped with the
// same RFC 3986 rules used for the path.
func canonicalQueryString(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	first := true
	for _, k := range keys {
		values := q[k]
		sort.Strings(values)
		ek := sigv4Escape(k)
		for _, v := range values {
			if !first {
				b.WriteByte('&')
			}
			first = false
			b.WriteString(ek)
			b.WriteByte('=')
			b.WriteString(sigv4Escape(v))
		}
	}
	return b.String()
}

// sigv4Escape percent-encodes per RFC 3986 unreserved set ([A-Za-z0-9-_.~]).
// Differs from net/url.QueryEscape in that "+" encodes spaces as %20 and
// nothing else gets a special pass.
func sigv4Escape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z',
			c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// deriveSigningKey runs the four-step HMAC chain from the spec:
//
//	kDate    = HMAC("AWS4" + secret, date)
//	kRegion  = HMAC(kDate, region)
//	kService = HMAC(kRegion, service)
//	kSigning = HMAC(kService, "aws4_request")
func deriveSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256Sum(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}

// =====================================================================
// Vertex JWT bearer
// =====================================================================

// vertexAccessToken returns a valid OAuth2 access token, minting a new one
// when the cached token is empty or within 60s of expiry.
func (a *authState) vertexAccessToken(ctx context.Context, client *http.Client) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.token != "" && time.Now().Before(a.tokExpiry) {
		return a.token, nil
	}

	jwt, err := a.signVertexJWT()
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", jwt)

	endpoint := vertexTokenEndpoint
	if a.tokenEndpointOverride != "" {
		endpoint = a.tokenEndpointOverride
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
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

	a.token = tr.AccessToken
	expiresIn := time.Duration(tr.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 1 * time.Hour
	}
	a.tokExpiry = time.Now().Add(expiresIn - 60*time.Second)
	return a.token, nil
}

// signVertexJWT mints an RS256-signed JWT bearer assertion suitable for
// Google's OAuth2 token endpoint.
func (a *authState) signVertexJWT() (string, error) {
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

// parseAnthropicVertexServiceAccount extracts client_email + RSA private key
// from a GCP service-account JSON blob. Mirrors the Gemini provider's helper
// — kept package-local to avoid cross-plugin imports.
func parseAnthropicVertexServiceAccount(data []byte) (string, *rsa.PrivateKey, error) {
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

// vertexTokenEndpoint is Google's public OAuth2 token exchange URL. Tests
// override authState.tokenEndpointOverride to point at an httptest server.
const vertexTokenEndpoint = "https://oauth2.googleapis.com/token"
