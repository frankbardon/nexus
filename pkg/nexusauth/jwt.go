package nexusauth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ValidatorTypeJWKS is the config `type:` that selects JWKSValidator.
const ValidatorTypeJWKS = "jwks"

// Defaults and bounds for a `jwks` entry. Every one of these is a security or
// availability control, so each has a documented reason rather than a round
// number.
const (
	// defaultJWKSAlgorithm is the algorithm assumed when the operator lists none.
	// RS256 is the one algorithm every OIDC provider is required to support
	// (OpenID Connect Core §15.1), which makes it the only defensible default —
	// and it is asymmetric, so the default cannot be an algorithm-confusion
	// footgun.
	defaultJWKSAlgorithm = "RS256"

	// defaultJWKSCacheTTL is how long a fetched key set is considered fresh.
	// Beyond it the next request still answers from cache and a refresh runs
	// behind the request.
	defaultJWKSCacheTTL = 10 * time.Minute

	// defaultJWKSNegativeCacheTTL is how long an unresolvable `kid` is remembered
	// as unresolvable, and the minimum interval between on-demand fetches. It is
	// short enough that a genuine key rotation is picked up promptly and long
	// enough that a stream of forged tokens cannot become JWKS traffic.
	defaultJWKSNegativeCacheTTL = time.Minute

	// defaultJWKSHTTPTimeout bounds a single JWKS request. Key verification sits
	// on POST /claim and on the WebSocket connect path, so this is the worst-case
	// latency an unreachable identity provider can add to a cold claim.
	defaultJWKSHTTPTimeout = 5 * time.Second

	// defaultJWKSClockSkew is the leeway applied to `exp` and `nbf`. Distributed
	// clocks disagree by seconds; a minute absorbs that without meaningfully
	// extending a token's life.
	defaultJWKSClockSkew = time.Minute

	// maxJWKSClockSkew caps the leeway. A large skew silently extends the life of
	// every token the issuer ever minted, so a mistyped value must fail loudly
	// rather than quietly weaken expiry.
	maxJWKSClockSkew = 5 * time.Minute
)

// jwksSupportedAlgorithms are the JWS algorithms a `jwks` entry may be
// configured with.
//
// It is asymmetric algorithms only, and that is the point. `none` and the HMAC
// family are not merely absent from the default — they are unconfigurable, so
// the algorithm-confusion attack (re-sign the token with HS256 using the
// issuer's public key as the shared secret) has no configuration that could
// admit it. Symmetric verification against a *published* key is a contradiction;
// there is no deployment that wants it.
var jwksSupportedAlgorithms = []string{
	jwt.SigningMethodRS256.Alg(), jwt.SigningMethodRS384.Alg(), jwt.SigningMethodRS512.Alg(),
	jwt.SigningMethodPS256.Alg(), jwt.SigningMethodPS384.Alg(), jwt.SigningMethodPS512.Alg(),
	jwt.SigningMethodES256.Alg(), jwt.SigningMethodES384.Alg(), jwt.SigningMethodES512.Alg(),
}

// jwksOptions is the resolved, decoder-independent form of a `jwks` config
// entry. buildJWKSValidator fills it from the YAML map; tests fill it directly,
// which is how the clock and the HTTP client are injected without exporting a
// test-only constructor.
type jwksOptions struct {
	// Issuer is the exact value the token's `iss` claim must carry.
	Issuer string

	// JWKSURL is the issuer's published key set endpoint.
	JWKSURL string

	// Audiences are the acceptable `aud` values; a token matching any one of them
	// passes.
	Audiences []string

	// Algorithms is the allowed-algorithm list. Empty means defaultJWKSAlgorithm.
	Algorithms []string

	// Mapping names the claims that become the Principal's ID, tenant and scopes.
	Mapping ClaimMapping

	// CacheTTL, NegativeCacheTTL and HTTPTimeout are zero for "use the default".
	CacheTTL         time.Duration
	NegativeCacheTTL time.Duration
	HTTPTimeout      time.Duration

	// ClockSkew is the exp/nbf leeway. ClockSkewSet distinguishes "not configured"
	// (apply the default) from an explicit zero, which legitimately means "no
	// leeway at all".
	ClockSkew    time.Duration
	ClockSkewSet bool

	// httpClient overrides the client the key cache fetches with. Nil builds one
	// bounded by HTTPTimeout. It is a test seam.
	httpClient *http.Client

	// now overrides the clock used for token expiry and for cache freshness. Nil
	// means time.Now. It is a test seam — a fake clock is what lets the expiry,
	// rotation and negative-cache tests be deterministic instead of sleeping.
	now func() time.Time
}

// JWKSValidator verifies a JWS bearer token against the signature keys an OIDC
// issuer publishes at its JWKS endpoint, then maps the verified claims onto a
// Principal.
//
// It is generic OIDC: there are no provider-specific claim defaults, and the
// operator states the issuer, the audience, the allowed algorithms and which
// claim becomes the principal id.
//
// It is immutable after construction apart from the key cache, which is
// internally synchronized, so one instance serves every request.
type JWKSValidator struct {
	algorithms []string
	mapping    ClaimMapping
	keys       *keyCache
	parser     *jwt.Parser
}

// NewJWKSValidator is unexported-by-design: a JWKSValidator is always built from
// a config entry, which is what buildJWKSValidator does. newJWKSValidator is the
// single construction path so every guard below applies to every instance.
func newJWKSValidator(opts jwksOptions) (*JWKSValidator, error) {
	if opts.Issuer == "" {
		return nil, fmt.Errorf("%s is required", keyIssuer)
	}
	// The mapping is what turns a verified token into an identity, and an empty
	// Principal.ID would compare equal to the broker's anonymous owner and to
	// every other empty-id principal. Refusing at construction makes that
	// unreachable rather than a per-request rejection.
	if opts.Mapping.PrincipalClaim == "" {
		return nil, fmt.Errorf("%s is required: it names the claim that becomes the principal id", keyPrincipalClaim)
	}
	if err := validateJWKSURL(opts.JWKSURL); err != nil {
		return nil, err
	}
	audiences, err := requireNonEmptyList(opts.Audiences, keyAudience)
	if err != nil {
		return nil, err
	}
	algorithms, err := resolveJWKSAlgorithms(opts.Algorithms)
	if err != nil {
		return nil, err
	}

	cacheTTL, err := positiveOrDefault(opts.CacheTTL, defaultJWKSCacheTTL, keyCacheTTL)
	if err != nil {
		return nil, err
	}
	negTTL, err := positiveOrDefault(opts.NegativeCacheTTL, defaultJWKSNegativeCacheTTL, keyNegativeCacheTTL)
	if err != nil {
		return nil, err
	}
	timeout, err := positiveOrDefault(opts.HTTPTimeout, defaultJWKSHTTPTimeout, keyHTTPTimeout)
	if err != nil {
		return nil, err
	}
	skew := defaultJWKSClockSkew
	if opts.ClockSkewSet {
		skew = opts.ClockSkew
	}
	if skew < 0 || skew > maxJWKSClockSkew {
		return nil, fmt.Errorf("%s must be between 0 and %s, got %s", keyClockSkew, maxJWKSClockSkew, skew)
	}

	now := opts.now
	if now == nil {
		now = time.Now
	}
	client := opts.httpClient
	if client == nil {
		// A dedicated client, not http.DefaultClient: the default has no timeout,
		// and an identity provider that accepts a connection and never answers
		// would then park a claim indefinitely.
		client = &http.Client{Timeout: timeout}
	}

	v := &JWKSValidator{
		algorithms: algorithms,
		mapping:    opts.Mapping,
		keys:       newKeyCache(opts.JWKSURL, client, timeout, cacheTTL, negTTL, now),
		parser: jwt.NewParser(
			// The allowlist, enforced by the library in addition to the header
			// pre-check in Validate. Two independent enforcement points because
			// this is the whole defence against algorithm confusion.
			jwt.WithValidMethods(algorithms),
			jwt.WithIssuer(opts.Issuer),
			jwt.WithAudience(audiences...),
			// A token with no `exp` never expires. A broker lease is not something
			// an eternal credential should be able to claim, so absence is a
			// rejection rather than "valid forever".
			jwt.WithExpirationRequired(),
			jwt.WithLeeway(skew),
			jwt.WithTimeFunc(now),
			// Reject non-canonical base64 rather than accept two encodings of the
			// same token, which is a signature-stripping foothold.
			jwt.WithStrictDecoding(),
			// Decode numbers as json.Number so a large numeric claim survives
			// intact; claimString already handles that type.
			jwt.WithJSONNumber(),
		),
	}
	return v, nil
}

// Validate implements Validator.
func (v *JWKSValidator) Validate(ctx context.Context, r *http.Request) (Principal, error) {
	raw := BearerToken(r)
	if raw == "" {
		return Principal{}, NewError(KindNoCredential, "no bearer token in the Authorization header", nil)
	}

	// The algorithm is decided by configuration, never by the token. This check
	// duplicates jwt's own WithValidMethods on purpose: it runs before any key is
	// resolved, so a token naming `none` or an algorithm the operator did not
	// allow is refused without touching the JWKS cache at all, and the refusal
	// names the offending algorithm instead of reading as a signature failure.
	if err := v.checkTokenAlgorithm(raw); err != nil {
		return Principal{}, err
	}

	token, err := v.parser.Parse(raw, v.keyFunc(ctx))
	if err != nil {
		return Principal{}, classifyJWTError(err)
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		// Unreachable with the parser above (Parse defaults to MapClaims), but a
		// type assertion that could panic on a library change is not worth saving.
		return Principal{}, NewError(KindInvalidCredential, "token claims are not a JSON object", nil)
	}

	// ClaimMapping rejects a missing, empty, or non-scalar principal claim as an
	// invalid credential — see the reasoning in newJWKSValidator.
	return v.mapping.Principal(map[string]any(claims))
}

// checkTokenAlgorithm reads the `alg` from the token's JWS header and refuses
// anything the operator did not allow.
func (v *JWKSValidator) checkTokenAlgorithm(raw string) error {
	alg, err := jwsHeaderAlgorithm(raw)
	if err != nil {
		return NewError(KindInvalidCredential, "token header is not a valid JWS header", err)
	}
	// Matched case-insensitively so no spelling of the unsigned algorithm can
	// reach the key lookup, even though JWS algorithm names are case-sensitive.
	if strings.EqualFold(alg, "none") {
		return NewError(KindInvalidCredential, `the "none" algorithm is never accepted`, nil)
	}
	for _, allowed := range v.algorithms {
		if alg == allowed {
			return nil
		}
	}
	return NewError(KindInvalidCredential, fmt.Sprintf(
		"token algorithm %q is not one of the configured algorithms (%s)",
		truncateForLog(alg), strings.Join(v.algorithms, ", ")), nil)
}

// keyFunc resolves the verification key for a parsed token. Returning a
// classified *Error from here is deliberate: jwt wraps a keyfunc error with %w,
// so classifyJWTError recovers it and the precise reason (unknown kid, JWKS
// unreachable, key/algorithm mismatch) survives to the operator's log line.
func (v *JWKSValidator) keyFunc(ctx context.Context) jwt.Keyfunc {
	return func(token *jwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		entry, err := v.keys.key(ctx, kid)
		if err != nil {
			return nil, err
		}
		if entry.alg != "" && entry.alg != token.Method.Alg() {
			return nil, NewError(KindInvalidCredential, fmt.Sprintf(
				"the issuer publishes this key for %q, but the token is signed with %q",
				truncateForLog(entry.alg), truncateForLog(token.Method.Alg())), nil)
		}
		if err := assertKeyMatchesMethod(token.Method, entry.key); err != nil {
			return nil, err
		}
		return entry.key, nil
	}
}

// assertKeyMatchesMethod refuses a token whose signing method does not match the
// kind of key the issuer published.
//
// The case that matters is HMAC. The classic algorithm-confusion attack re-signs
// a token with HS256 using the issuer's *public* key as the shared secret, and a
// verifier that hands whatever key it looked up to whatever method the header
// named accepts it. The configured allowlist already makes that unreachable —
// jwksSupportedAlgorithms cannot express a symmetric algorithm — but the defence
// must not rest on a single line of config parsing, so it is asserted again at
// the point the key and the method meet.
func assertKeyMatchesMethod(method jwt.SigningMethod, key crypto.PublicKey) error {
	switch method.(type) {
	case *jwt.SigningMethodHMAC:
		return NewError(KindInvalidCredential,
			"token is HMAC-signed but the issuer publishes an asymmetric key", nil)
	case *jwt.SigningMethodRSAPSS, *jwt.SigningMethodRSA:
		if _, ok := key.(*rsa.PublicKey); !ok {
			return NewError(KindInvalidCredential,
				"token algorithm requires an RSA key but the issuer's key for this key id is not RSA", nil)
		}
	case *jwt.SigningMethodECDSA:
		if _, ok := key.(*ecdsa.PublicKey); !ok {
			return NewError(KindInvalidCredential,
				"token algorithm requires an EC key but the issuer's key for this key id is not EC", nil)
		}
	default:
		// Covers `none` (whose method type is unexported) and anything a future
		// library version registers that we have not reasoned about.
		return NewError(KindInvalidCredential, "token signing method is not accepted", nil)
	}
	return nil
}

// classifyJWTError turns a jwt parse failure into a classified denial with an
// operator-facing reason. It branches on the library's sentinel errors, never on
// message text.
func classifyJWTError(err error) error {
	// A denial our own keyfunc produced wins: it is more specific than the
	// "unverifiable" wrapper jwt puts around it.
	var typed *Error
	if errors.As(err, &typed) {
		return typed
	}

	var reason string
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		reason = "token has expired"
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		reason = "token is not valid yet"
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		reason = "token audience does not match the configured audience"
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		reason = "token issuer does not match the configured issuer"
	case errors.Is(err, jwt.ErrTokenRequiredClaimMissing):
		reason = "token is missing a required claim"
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		reason = "token signature does not verify against the issuer's key"
	case errors.Is(err, jwt.ErrTokenMalformed):
		reason = "token is malformed"
	case errors.Is(err, jwt.ErrTokenUnverifiable):
		reason = "token could not be verified"
	case errors.Is(err, jwt.ErrTokenInvalidClaims):
		reason = "token claims did not validate"
	default:
		reason = "token rejected"
	}
	return NewError(KindInvalidCredential, reason, err)
}

// jwsHeaderAlgorithm decodes only the JWS header segment and returns its `alg`.
// It exists so the algorithm allowlist can be applied before any key lookup;
// everything else about the token is left to the library.
func jwsHeaderAlgorithm(token string) (string, error) {
	dot := strings.IndexByte(token, '.')
	if dot <= 0 {
		return "", fmt.Errorf("token has no header segment")
	}
	segment := token[:dot]
	if len(segment) > maxJWSHeaderBytes {
		return "", fmt.Errorf("header segment is larger than %d bytes", maxJWSHeaderBytes)
	}
	// RFC 7515 §2 fixes JWS on unpadded base64url; there is no interoperability
	// reason to be tolerant here, and the library is strict too.
	decoded, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return "", fmt.Errorf("header is not unpadded base64url: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(decoded, &header); err != nil {
		return "", fmt.Errorf("header is not a JSON object: %w", err)
	}
	if header.Alg == "" {
		return "", fmt.Errorf("header declares no alg")
	}
	return header.Alg, nil
}

// validateJWKSURL checks the configured endpoint at load time, so a typo or an
// unencrypted endpoint fails the boot rather than the first claim.
//
// Plain HTTP is refused except to a loopback host. The key set is the entire
// basis for trusting a token, so fetching it over a channel an on-path attacker
// can rewrite would let that attacker substitute their own signing key and mint
// any principal they like. The loopback exemption is what makes a local test
// server and a sidecar-fronted deployment workable without an
// allow-insecure-anything switch that would inevitably be set in production.
func validateJWKSURL(raw string) error {
	return validateAuthorityURL(raw, keyJWKSURL)
}

// validateAuthorityURL is the shared endpoint check for every config key that
// names a remote authority the package will trust — the JWKS endpoint and the
// introspection endpoint alike. The transport requirement is identical for both
// (a rewritable channel lets an on-path attacker decide who the caller is), so
// there is one implementation and the key name is a parameter, which keeps each
// caller's error messages naming its own key.
func validateAuthorityURL(raw, key string) error {
	if raw == "" {
		return fmt.Errorf("%s is required", key)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	if u.Host == "" {
		return fmt.Errorf("%s: want an absolute URL, got %q", key, raw)
	}
	if u.User != nil {
		return fmt.Errorf("%s: userinfo is not supported", key)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("%s: http is only allowed for a loopback host, got %q", key, u.Hostname())
	default:
		return fmt.Errorf("%s: scheme must be https (or http to a loopback host), got %q", key, u.Scheme)
	}
}

// isLoopbackHost reports whether host names the local machine.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// resolveJWKSAlgorithms applies the default and rejects anything outside
// jwksSupportedAlgorithms, naming `none` and the HMAC family explicitly because
// those are the two an operator might reach for and must not get.
func resolveJWKSAlgorithms(configured []string) ([]string, error) {
	if len(configured) == 0 {
		return []string{defaultJWKSAlgorithm}, nil
	}
	out := make([]string, 0, len(configured))
	seen := make(map[string]struct{}, len(configured))
	for i, alg := range configured {
		if alg == "" {
			return nil, fmt.Errorf("%s[%d]: must not be empty", keyAlgorithms, i)
		}
		if strings.EqualFold(alg, "none") {
			return nil, fmt.Errorf("%s[%d]: %q is never accepted — an unsigned token proves nothing", keyAlgorithms, i, alg)
		}
		if isHMACAlgorithm(alg) {
			return nil, fmt.Errorf("%s[%d]: %q is symmetric and cannot be used with a published key set; "+
				"allowing it is the algorithm-confusion attack", keyAlgorithms, i, alg)
		}
		if !containsString(jwksSupportedAlgorithms, alg) {
			return nil, fmt.Errorf("%s[%d]: unsupported algorithm %q (supported: %s)",
				keyAlgorithms, i, truncateForLog(alg), strings.Join(jwksSupportedAlgorithms, ", "))
		}
		if _, dup := seen[alg]; dup {
			return nil, fmt.Errorf("%s[%d]: duplicate algorithm %q", keyAlgorithms, i, alg)
		}
		seen[alg] = struct{}{}
		out = append(out, alg)
	}
	return out, nil
}

// isHMACAlgorithm reports whether alg names a symmetric JWS algorithm, so the
// error can say why rather than just "unsupported".
func isHMACAlgorithm(alg string) bool {
	return containsString([]string{
		jwt.SigningMethodHS256.Alg(), jwt.SigningMethodHS384.Alg(), jwt.SigningMethodHS512.Alg(),
	}, alg)
}

// containsString is an exact, case-sensitive membership test.
func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// requireNonEmptyList validates a list config value that must have at least one
// non-blank entry.
func requireNonEmptyList(values []string, key string) ([]string, error) {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s is required", key)
	}
	return out, nil
}

// positiveOrDefault resolves a duration option: zero means "not configured, use
// the default", and a negative value is an operator error rather than something
// to normalize away.
func positiveOrDefault(configured, fallback time.Duration, key string) (time.Duration, error) {
	if configured < 0 {
		return 0, fmt.Errorf("%s must not be negative, got %s", key, configured)
	}
	if configured == 0 {
		return fallback, nil
	}
	return configured, nil
}
