package anthropic

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/nexuscreds"

	// gcemeta supplies GCE metadata facts only — the project and region this
	// process runs in — and registers nothing. This plugin is in
	// pkg/engine/allplugins, so an import that registered a credential source
	// from init would put that source in the registry of every binary carrying
	// the plugin, and vertex.credentials defaults to google-adc: an embedder
	// shipping their own Vault or SPIFFE source would get Google ADC silently
	// selected by a config that merely omitted the key. The token itself is
	// opened by NAME through nexuscreds, so no credential implementation is
	// compiled in by the signing path either.
	"github.com/frankbardon/nexus/pkg/nexuscreds/gcemeta"
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
	// authModeVertex routes requests through GCP Vertex AI with a bearer
	// token obtained from a nexuscreds source (google-adc by default).
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
//
// The Vertex path deliberately holds no token, no expiry and no mutex:
// minting, caching and refreshing an access token all belong to the
// nexuscreds.Source, which for google-adc is an oauth2.TokenSource that
// already does each of them for every Google credential type rather than only
// for a service-account key file. Bedrock signs per request, so it needs no
// cache either; the API-key path has nothing to cache.
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

	// Vertex path (GCP, via a nexuscreds source).
	vertexProject string
	vertexRegion  string
	vertexCreds   nexuscreds.Source

	// vertexCredsName is the nexuscreds source name that was opened, and
	// vertexRegionSource which rung of the region chain answered. Neither
	// affects a request; both exist so confirmCredentials can say at boot
	// where this process's auth came from.
	vertexCredsName    string
	vertexRegionOrigin regionOrigin
}

// regionOrigin names which rung of the Vertex region chain answered.
//
// It exists for the boot log alone. resolveVertexRegion falls back silently,
// so without this an operator reading "us-east5" cannot tell a deliberate
// configuration from a metadata server that failed and left the default
// standing — two very different deployments that produce the same word.
type regionOrigin string

const (
	regionFromConfig   regionOrigin = "config"
	regionFromMetadata regionOrigin = "metadata"
	regionFromDefault  regionOrigin = "default"
)

// defaultVertexCredentialSource is the nexuscreds source used when the config
// names none. It is google-adc so a GKE deployment running under Workload
// Identity needs no credential config at all: no key file exists on such a
// pod, and the whole point of the default is that the operator does not have
// to say so.
const defaultVertexCredentialSource = "google-adc"

// defaultVertexRegion is the region used when neither the config nor the GCE
// metadata server can say where this process runs. Claude models on Vertex are
// served from us-east5, which is why this differs from the Gemini provider's
// us-central1.
const defaultVertexRegion = "us-east5"

// vertexMetadataTimeout bounds each GCE metadata lookup made while resolving
// auth. These run during plugin Init, so an unreachable metadata server must
// not be able to hang boot indefinitely — on a host that is not on GCE the
// helpers return without any request at all, so this only bites where the
// metadata server exists but is wedged.
const vertexMetadataTimeout = 5 * time.Second

// vertexCredentialProbeTimeout bounds the single token mint made at Init to
// prove the Vertex credential actually works. It is looser than
// vertexMetadataTimeout because the mint may be a full OAuth2 exchange with a
// public Google endpoint — DNS, TLS and a round trip — rather than a
// link-local metadata read, but it is bounded all the same: this runs on the
// boot path, and a wedged token endpoint must fail the boot rather than hang
// it.
const vertexCredentialProbeTimeout = 20 * time.Second

// The GCE metadata lookups are reached through variables rather than called
// directly so tests can pin them.
//
// The reason is memoisation, not style: metadata.OnGCE — which gcemeta
// consults before every lookup — fixes its answer in a package-level sync.Once
// on the first call in a process, and with GCE_METADATA_HOST unset that call
// probes the real 169.254.169.254. A test that needs "not on GCE" therefore
// cannot get there by clearing an environment variable.
var (
	metadataProjectID = gcemeta.ProjectID
	metadataRegion    = gcemeta.Region
)

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

		project, err := resolveVertexProject(raw)
		if err != nil {
			return nil, err
		}
		a.vertexProject = project
		a.vertexRegion, a.vertexRegionOrigin = resolveVertexRegion(raw)

		source, name, err := resolveVertexCredentials(raw)
		if err != nil {
			return nil, err
		}
		a.vertexCreds = source
		a.vertexCredsName = name
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
func (a *authState) applyAuth(ctx context.Context, req *http.Request, body []byte) error {
	switch a.mode {
	case authModeAPIKey:
		req.Header.Set("x-api-key", a.apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		return nil

	case authModeBedrock:
		return a.signBedrockSigV4(req, body, time.Now().UTC())

	case authModeVertex:
		if a.vertexCreds == nil {
			return fmt.Errorf("anthropic: vertex auth has no credential source")
		}
		token, err := a.vertexCreds.Token(ctx)
		if err != nil {
			return fmt.Errorf("anthropic: vertex credentials: %w", err)
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
// Vertex credentials
// =====================================================================

// resolveVertexProject resolves the Vertex project through three steps, in
// order: the project (or project_id) config key, the GOOGLE_CLOUD_PROJECT
// environment variable, and the GCE metadata server.
//
// The last step is what lets a GKE deployment configure nothing GCP-specific
// beyond auth_mode: vertex. A pod already knows which project it belongs to,
// and making an operator repeat that in YAML is a chance to get it wrong. It
// comes last so an explicit setting always wins over the environment it
// happens to run in, and gcemeta.ProjectID answers ErrNotOnGCE without a
// request when this process is not on GCE, so a non-GCP host pays no network
// probe.
//
// Failure is still fatal at Init, because a Vertex URL cannot be built without
// a project — but now only when every step has failed, rather than whenever
// the config key was absent.
func resolveVertexProject(raw map[string]any) (string, error) {
	project, _ := raw["project"].(string)
	if project == "" {
		project, _ = raw["project_id"].(string)
	}
	if project != "" {
		return project, nil
	}
	if v := os.Getenv("GOOGLE_CLOUD_PROJECT"); v != "" {
		return v, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), vertexMetadataTimeout)
	defer cancel()

	project, err := metadataProjectID(ctx)
	switch {
	case err == nil:
		return project, nil
	case errors.Is(err, gcemeta.ErrNotOnGCE):
		return "", fmt.Errorf("anthropic: vertex auth requires vertex.project config or GOOGLE_CLOUD_PROJECT env var (this process is not on GCE, so the metadata server could not supply one)")
	default:
		return "", fmt.Errorf("anthropic: vertex auth has no vertex.project config and no GOOGLE_CLOUD_PROJECT env var, and the GCE metadata server could not supply one: %w", err)
	}
}

// resolveVertexRegion resolves the Vertex region through the region (or
// location) config key, then the region this process runs in as derived from
// the GCE metadata server's zone, then defaultVertexRegion.
//
// Unlike the project, an unresolvable region is not fatal: there is a usable
// default, and the deployment is better served by booting than by refusing to.
// Note the accepted risk that follows from deriving it: Vertex model
// availability is per-region and Claude models are served from a short list of
// them, so a pod in a quiet region may resolve its own region and then 404 at
// first inference rather than at boot. The mitigation is documentation,
// deliberately not a boot-time availability probe.
func resolveVertexRegion(raw map[string]any) (string, regionOrigin) {
	region, _ := raw["region"].(string)
	if region == "" {
		region, _ = raw["location"].(string)
	}
	if region != "" {
		return region, regionFromConfig
	}

	ctx, cancel := context.WithTimeout(context.Background(), vertexMetadataTimeout)
	defer cancel()

	if r, err := metadataRegion(ctx); err == nil {
		return r, regionFromMetadata
	}
	return defaultVertexRegion, regionFromDefault
}

// resolveVertexCredentials opens the nexuscreds source named by the
// credentials key and returns it along with the name that won, which the boot
// log reports.
//
// The error from an unknown name is passed through rather than reworded:
// nexuscreds.Open already says that the binary must blank-import the package
// registering the source, and lists what this build does have, which is the
// actionable half of the message.
func resolveVertexCredentials(raw map[string]any) (nexuscreds.Source, string, error) {
	name := defaultVertexCredentialSource
	if v, ok := raw["credentials"]; ok {
		str, ok := v.(string)
		if !ok {
			return nil, "", fmt.Errorf("anthropic: vertex.credentials must be a string naming a credential source, got %T", v)
		}
		if str != "" {
			name = str
		}
	}

	source, err := nexuscreds.Open(name, vertexCredentialSourceConfig(raw))
	if err != nil {
		return nil, "", fmt.Errorf("anthropic: vertex auth: %w", err)
	}
	return source, name, nil
}

// vertexCredentialSourceConfig builds the config block handed to the
// credential source.
//
// The four key-file spellings predate the nexuscreds seam and are kept
// verbatim so no existing deployment has to edit its YAML. They are normalised
// onto the two names the source understands and forwarded individually rather
// than by passing the whole vertex block, so a source can never observe — or
// collide with — an unrelated key such as project or region.
//
// A key absent from the config is left absent rather than forwarded empty:
// google-adc distinguishes "not configured" (run the full ADC chain, the
// keyless-pod case) from "configured to something empty". That is also why the
// old GOOGLE_APPLICATION_CREDENTIALS fallback is gone rather than ported — the
// ADC chain reads that variable itself, one rung down from an explicitly
// configured path, which is exactly where it belonged all along.
func vertexCredentialSourceConfig(raw map[string]any) map[string]any {
	out := make(map[string]any, 2)
	for _, pair := range [][2]string{
		{"sa_key_file", "service_account_json"},
		{"service_account_json", "service_account_json"},
		{"sa_key_file_env", "service_account_json_env"},
		{"service_account_json_env", "service_account_json_env"},
	} {
		if _, taken := out[pair[1]]; taken {
			continue
		}
		if v, ok := raw[pair[0]]; ok {
			out[pair[1]] = v
		}
	}
	return out
}

// confirmCredentials proves at boot that the resolved Vertex credential can
// actually mint a token, and emits the one line that says so.
//
// The mint is the point. nexuscreds.Factory forbids network I/O at
// construction, so parseAuthConfig on its own only catches an unknown source
// name or an unreadable key file: a pod whose Workload Identity binding is
// broken — the Kubernetes service account not annotated, or the Google service
// account missing the workloadIdentityUser role — resolves perfectly cleanly
// and then fails on the first user message, which is the worst possible place
// to learn it.
//
// It is a no-op outside Vertex mode: an API key's validity is not knowable
// without spending a real request, and Bedrock SigV4 mints nothing to probe.
func (a *authState) confirmCredentials(ctx context.Context, logger *slog.Logger) error {
	if a.mode != authModeVertex {
		return nil
	}
	if a.vertexCreds == nil {
		return fmt.Errorf("anthropic: vertex auth has no credential source")
	}

	ctx, cancel := context.WithTimeout(ctx, vertexCredentialProbeTimeout)
	defer cancel()

	if _, err := a.vertexCreds.Token(ctx); err != nil {
		return fmt.Errorf("anthropic: vertex credential source %q could not obtain a token: %w", a.vertexCredsName, err)
	}

	// DELIBERATE LOGGING-RUBRIC EXCEPTION — do not demote this to DEBUG.
	//
	// docs/src/operations/logging.md places "config resolution / which branch
	// was taken" at DEBUG and reserves INFO for lifecycle. This line reads
	// like the former and is at INFO anyway, for two reasons: it fires exactly
	// once per process, so it cannot become chatter, and it is the primary ops
	// diagnostic for a pod that cannot authenticate. "Did a credential connect
	// at all, and against which project and region" has to be answerable from
	// default-level logs, because the deployment that needs the answer is the
	// one nobody can turn DEBUG on for.
	//
	// What it deliberately does NOT say: the token, any key material, the
	// credential type, or the principal the credential belongs to. It reports
	// that a credential connected and which configured source supplied it —
	// nothing about who.
	logger.Info("vertex credentials confirmed",
		"provider", "anthropic",
		"credential_source", a.vertexCredsName,
		"project", a.vertexProject,
		"region", a.vertexRegion,
		"region_source", string(a.vertexRegionOrigin),
	)
	return nil
}
