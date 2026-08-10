package nexusauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ValidatorTypeIntrospect is the config `type:` that selects IntrospectValidator.
const ValidatorTypeIntrospect = "introspect"

// Client authentication methods an `introspect` entry may use when it calls the
// introspection endpoint. RFC 7662 §2.1 requires the endpoint to authorize its
// callers but does not fix how; these are the two ways OAuth 2.0 clients
// actually do it (RFC 6749 §2.3.1).
const (
	// ClientAuthBasic sends the client credentials in an HTTP Basic header. It
	// is the default because RFC 6749 §2.3.1 requires every authorization
	// server to support it, and because it keeps the secret out of the request
	// body (and therefore out of anything that logs bodies).
	ClientAuthBasic = "basic"

	// ClientAuthPost sends client_id/client_secret as form parameters, for the
	// providers that only accept `client_secret_post`.
	ClientAuthPost = "post"
)

// Defaults and bounds for an `introspect` entry.
const (
	// defaultIntrospectCacheTTL is the ceiling on how long a verdict is reused
	// without asking the endpoint again. A minute keeps the endpoint off the
	// request path for a busy client while keeping a revocation's blast radius
	// to about one minute.
	defaultIntrospectCacheTTL = time.Minute

	// defaultIntrospectNegativeCacheTTL is how long a *definitive* refusal is
	// remembered. It is shorter than the positive TTL because a wrong negative
	// is a user-visible outage while a wrong positive is only a stale allow, and
	// it exists mainly so a client retrying a dead token in a loop cannot turn
	// itself into introspection traffic.
	defaultIntrospectNegativeCacheTTL = 30 * time.Second

	// defaultIntrospectHTTPTimeout bounds a single introspection request. This
	// validator sits on POST /claim, which has its own ready-timeout budget to
	// respect, so this is the worst-case latency a hung identity provider can
	// add to a claim.
	defaultIntrospectHTTPTimeout = 5 * time.Second

	// maxIntrospectCacheTTL caps both TTLs. Introspection exists precisely so a
	// token can be revoked *before* it expires; a cache TTL of hours would throw
	// that away silently, so an over-long value fails the boot instead.
	maxIntrospectCacheTTL = 15 * time.Minute

	// maxIntrospectCacheEntries bounds the response cache. Entries are keyed by
	// a hash of a caller-supplied token, so without a bound a flood of distinct
	// junk tokens would be an unauthenticated memory-growth lever.
	maxIntrospectCacheEntries = 4096

	// maxIntrospectionBodyBytes caps the response document we will read. An RFC
	// 7662 response is a small JSON object; the cap stops a compromised or
	// misconfigured endpoint turning every claim into unbounded allocation.
	maxIntrospectionBodyBytes = 1 << 20 // 1 MiB
)

// introspectOptions is the resolved, decoder-independent form of an
// `introspect` config entry. buildIntrospectValidator fills it from the YAML
// map; tests fill it directly, which is how the clock and the HTTP client are
// injected without exporting a test-only constructor.
type introspectOptions struct {
	// IntrospectionURL is the RFC 7662 introspection endpoint.
	IntrospectionURL string

	// ClientID identifies this broker to the introspection endpoint.
	ClientID string

	// ClientSecret is the already-resolved secret (inline or read from the
	// environment by resolveSecret). It is never logged and never appears in an
	// error message.
	ClientSecret string

	// ClientAuth selects how the credentials are presented: ClientAuthBasic or
	// ClientAuthPost. Empty means the default.
	ClientAuth string

	// TokenTypeHint is the optional RFC 7662 §2.1 `token_type_hint` parameter.
	// TokenTypeHintSet distinguishes "not configured" (send the default) from an
	// explicit empty value, which means "send no hint at all".
	TokenTypeHint    string
	TokenTypeHintSet bool

	// Mapping names the claims that become the Principal's ID, tenant and scopes.
	Mapping ClaimMapping

	// CacheTTL, NegativeCacheTTL and HTTPTimeout are zero for "use the default".
	CacheTTL         time.Duration
	NegativeCacheTTL time.Duration
	HTTPTimeout      time.Duration

	// httpClient overrides the client used to call the endpoint. Nil builds one
	// bounded by HTTPTimeout. It is a test seam.
	httpClient *http.Client

	// now overrides the clock used for cache expiry. Nil means time.Now. It is a
	// test seam — a fake clock is what lets the TTL tests be deterministic
	// instead of sleeping.
	now func() time.Time
}

// IntrospectValidator authenticates a bearer token by asking the issuer about
// it, per RFC 7662 (OAuth 2.0 Token Introspection).
//
// It exists because not every identity provider issues JWTs. An opaque token
// carries no claims and no signature to check, so the only thing that can say
// whether it is live is the authority that minted it. The validator POSTs the
// token to the configured introspection endpoint using its own client
// credentials, reads the `active` verdict and the returned claims, and maps
// those claims onto a Principal with the same ClaimMapping the `jwks` validator
// uses.
//
// The response cache is not an optimization. This sits on POST /claim and on the
// WebSocket connect path, so an uncached implementation would put the identity
// provider's latency in front of every session and its request rate in
// proportion to Nexus traffic.
//
// It is immutable after construction apart from the cache, which is internally
// synchronized, so one instance serves every request.
type IntrospectValidator struct {
	endpoint      string
	clientID      string
	clientSecret  string
	clientAuth    string
	tokenTypeHint string
	mapping       ClaimMapping
	client        *http.Client
	timeout       time.Duration
	cache         *introspectionCache
}

// newIntrospectValidator is the single construction path, so every guard below
// applies to every instance. Nothing here reaches the network: an identity
// provider that is briefly unreachable must not stop the broker booting.
func newIntrospectValidator(opts introspectOptions) (*IntrospectValidator, error) {
	if err := validateAuthorityURL(opts.IntrospectionURL, keyIntrospectionURL); err != nil {
		return nil, err
	}
	// The mapping is what turns an active verdict into an identity, and an empty
	// Principal.ID would compare equal to the broker's anonymous owner and to
	// every other empty-id principal. Refusing at construction makes that
	// unreachable rather than a per-request rejection.
	if opts.Mapping.PrincipalClaim == "" {
		return nil, fmt.Errorf("%s is required: it names the claim that becomes the principal id", keyPrincipalClaim)
	}
	// RFC 7662 §2.1: the endpoint MUST require authorization from the protected
	// resource calling it. Requiring client_id makes that explicit in config
	// rather than leaving an operator to discover it from the endpoint's 401.
	if opts.ClientID == "" {
		return nil, fmt.Errorf("%s is required", keyClientID)
	}

	clientAuth := opts.ClientAuth
	switch clientAuth {
	case "":
		clientAuth = ClientAuthBasic
	case ClientAuthBasic, ClientAuthPost:
	default:
		return nil, fmt.Errorf("%s must be %q or %q, got %q",
			keyClientAuth, ClientAuthBasic, ClientAuthPost, truncateForLog(clientAuth))
	}

	hint := "access_token"
	if opts.TokenTypeHintSet {
		hint = opts.TokenTypeHint
	}

	ttl, err := boundedTTL(opts.CacheTTL, defaultIntrospectCacheTTL, keyCacheTTL)
	if err != nil {
		return nil, err
	}
	negTTL, err := boundedTTL(opts.NegativeCacheTTL, defaultIntrospectNegativeCacheTTL, keyNegativeCacheTTL)
	if err != nil {
		return nil, err
	}
	timeout, err := positiveOrDefault(opts.HTTPTimeout, defaultIntrospectHTTPTimeout, keyHTTPTimeout)
	if err != nil {
		return nil, err
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

	return &IntrospectValidator{
		endpoint:      opts.IntrospectionURL,
		clientID:      opts.ClientID,
		clientSecret:  opts.ClientSecret,
		clientAuth:    clientAuth,
		tokenTypeHint: hint,
		mapping:       opts.Mapping,
		client:        client,
		timeout:       timeout,
		cache:         newIntrospectionCache(ttl, negTTL, maxIntrospectCacheEntries, now),
	}, nil
}

// Validate implements Validator.
func (v *IntrospectValidator) Validate(ctx context.Context, r *http.Request) (Principal, error) {
	raw := BearerToken(r)
	if raw == "" {
		return Principal{}, NewError(KindNoCredential, "no bearer token in the Authorization header", nil)
	}
	return v.cache.resolve(ctx, raw, v.introspect)
}

// introspect performs one introspection round trip and turns the answer into a
// verdict. The second return value is the token's expiry as the endpoint
// reported it, or the zero time when it reported none; the cache uses it to
// bound how long the verdict may be reused.
//
// Every failure path here is a denial. The only "allow" is a 2xx JSON body whose
// `active` is literally true and whose claims produce a usable Principal.
func (v *IntrospectValidator) introspect(ctx context.Context, token string) (Principal, time.Time, error) {
	// A second bound on top of the client's own Timeout. The client timeout
	// covers the request in isolation; the context bound is what makes the whole
	// call abortable and keeps this validator inside POST /claim's budget even if
	// a future caller swaps the client for one without a timeout.
	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()

	req, err := v.buildRequest(ctx, token)
	if err != nil {
		return Principal{}, time.Time{}, NewError(KindUnavailable, "the introspection request could not be built", err)
	}

	resp, err := v.client.Do(req)
	if err != nil {
		// Deliberately not echoing err into the reason: a transport error's text
		// can contain the full request URL, and the reason may be surfaced to the
		// caller. The cause is carried on the *Error for the operator's log.
		return Principal{}, time.Time{}, NewError(KindUnavailable,
			"the introspection endpoint could not be reached", err)
	}
	defer func() {
		// Drain within the cap so the connection can be reused, then close.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxIntrospectionBodyBytes))
		_ = resp.Body.Close()
	}()

	// RFC 7662 encodes "this token is no good" as 200 with active:false. A
	// non-2xx therefore says nothing about the caller's token — it says the
	// endpoint is broken, or that OUR client credentials were refused. Calling
	// that KindInvalidCredential would tell an innocent client to re-authenticate
	// and aim a re-auth storm at an already-failing provider.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Principal{}, time.Time{}, NewError(KindUnavailable,
			fmt.Sprintf("the introspection endpoint answered %s", resp.Status), nil)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIntrospectionBodyBytes+1))
	if err != nil {
		return Principal{}, time.Time{}, NewError(KindUnavailable,
			"the introspection response could not be read", err)
	}
	if len(body) > maxIntrospectionBodyBytes {
		return Principal{}, time.Time{}, NewError(KindUnavailable,
			fmt.Sprintf("the introspection response is larger than %d bytes", maxIntrospectionBodyBytes), nil)
	}

	claims, err := decodeIntrospectionBody(body)
	if err != nil {
		// A body we cannot parse is a broken endpoint, not a bad token — same
		// reasoning as the non-2xx case. Either way it is never an allow.
		return Principal{}, time.Time{}, NewError(KindUnavailable,
			"the introspection response is not a JSON object", err)
	}

	// `active` is the one REQUIRED member of the response (RFC 7662 §2.2).
	// Absent, or present as anything but a boolean, is a refusal: an endpoint
	// that does not answer the only question we asked cannot be read as "yes".
	rawActive, present := claims["active"]
	if !present {
		return Principal{}, time.Time{}, NewError(KindInvalidCredential,
			`the introspection response has no "active" member`, nil)
	}
	active, ok := rawActive.(bool)
	if !ok {
		return Principal{}, time.Time{}, NewError(KindInvalidCredential,
			fmt.Sprintf(`the introspection response's "active" member is %T, not a boolean`, rawActive), nil)
	}
	if !active {
		return Principal{}, time.Time{}, NewError(KindInvalidCredential,
			"the issuer reports this token as not active", nil)
	}

	// ClaimMapping rejects a missing, empty, or non-scalar principal claim as an
	// invalid credential — see the reasoning in newIntrospectValidator. This is
	// the same mapping code the jwks validator uses; the two must not diverge.
	p, err := v.mapping.Principal(claims)
	if err != nil {
		return Principal{}, time.Time{}, err
	}
	return p, introspectionExpiry(claims), nil
}

// buildRequest assembles the RFC 7662 §2.1 form POST, applying the configured
// client-authentication method.
func (v *IntrospectValidator) buildRequest(ctx context.Context, token string) (*http.Request, error) {
	form := url.Values{}
	form.Set("token", token)
	if v.tokenTypeHint != "" {
		form.Set("token_type_hint", v.tokenTypeHint)
	}
	if v.clientAuth == ClientAuthPost {
		form.Set("client_id", v.clientID)
		if v.clientSecret != "" {
			form.Set("client_secret", v.clientSecret)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// RFC 7662 §2.1 asks the caller not to let the response be cached, since it
	// is a point-in-time statement about a live credential.
	req.Header.Set("Cache-Control", "no-store")

	if v.clientAuth == ClientAuthBasic {
		// RFC 6749 §2.3.1 requires the client id and secret to be
		// form-urlencoded BEFORE base64 encoding them into the Basic header.
		// http.Request.SetBasicAuth does not do that, so credentials containing
		// a ':' or a non-ASCII byte would otherwise be presented wrong and read
		// as "your client credentials are invalid" rather than as an encoding
		// bug.
		req.SetBasicAuth(url.QueryEscape(v.clientID), url.QueryEscape(v.clientSecret))
	}
	return req, nil
}

// decodeIntrospectionBody decodes the response into a claim map. Numbers are
// decoded as json.Number so a large `exp` or a numeric subject survives intact —
// claimString already handles that type.
func decodeIntrospectionBody(body []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var claims map[string]any
	if err := dec.Decode(&claims); err != nil {
		return nil, err
	}
	if claims == nil {
		return nil, fmt.Errorf("body is JSON null")
	}
	return claims, nil
}

// introspectionExpiry reads the response's `exp` (RFC 7662 §2.2, seconds since
// the epoch). It returns the zero time when the member is absent or unusable —
// the caller then falls back to the configured maximum TTL, and never to
// unbounded caching.
func introspectionExpiry(claims map[string]any) time.Time {
	raw, present := claims["exp"]
	if !present {
		return time.Time{}
	}
	secs, ok := claimInt64(raw)
	if !ok || secs <= 0 {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

// claimInt64 coerces a decoded numeric claim to seconds. json.Number is what the
// decoder above produces; the other cases cover a caller that built the claim map
// some other way.
func claimInt64(v any) (int64, bool) {
	switch value := v.(type) {
	case json.Number:
		n, err := value.Int64()
		if err != nil {
			// A fractional or oversized value: not a timestamp we will act on.
			f, ferr := value.Float64()
			if ferr != nil {
				return 0, false
			}
			return int64(f), true
		}
		return n, true
	case float64:
		return int64(value), true
	case int64:
		return value, true
	case int:
		return int64(value), true
	default:
		return 0, false
	}
}

// tokenKey is a cache key: the SHA-256 of a bearer token, never the token.
//
// Hashing is a containment property, not obfuscation. The cache is a long-lived
// in-memory map of live credentials, so a heap dump, a core file, or a debugger
// printing the map would otherwise hand over working tokens for every currently
// active caller. A fixed-size array (rather than a hex string) also makes it
// structurally impossible for a key to *be* a token.
type tokenKey [sha256.Size]byte

// hashToken derives the cache key for a bearer token.
func hashToken(token string) tokenKey {
	return sha256.Sum256([]byte(token))
}

// cachedVerdict is one remembered answer: either a Principal or a denial, plus
// when it stops being reusable.
type cachedVerdict struct {
	principal Principal
	err       error
	expiresAt time.Time
}

// introspectionCache remembers introspection verdicts so a validated token costs
// at most one round trip per TTL rather than one per request.
//
// The policy in one place:
//
//   - Keys are hashes of tokens, never tokens — see tokenKey.
//   - A positive verdict lives for min(configured ttl, the token's own `exp`), so
//     a cache entry can never outlive the credential it describes. No `exp` in
//     the response falls back to the configured ttl, never to forever.
//   - A *definitive* refusal ("not active", unusable principal claim) is cached
//     for negTTL, so a client retrying a dead token in a loop does not become
//     introspection traffic.
//   - An "unavailable" refusal is NEVER cached. Caching it would extend an
//     identity-provider outage past its actual end, which is the opposite of
//     what the kind exists for.
//   - Concurrent lookups of the same token collapse onto one request. Without
//     that, a client opening several leases at once would multiply the very
//     round trip the cache exists to avoid.
//   - The map is bounded. Entries are keyed by caller-supplied material, so an
//     unbounded map would be a memory-growth lever for an unauthenticated
//     caller.
type introspectionCache struct {
	ttl    time.Duration
	negTTL time.Duration
	max    int
	now    func() time.Time

	mu       sync.Mutex
	entries  map[tokenKey]cachedVerdict
	inflight map[tokenKey]*introspectionCall
}

// introspectionCall is one in-flight lookup other callers can wait on.
type introspectionCall struct {
	done      chan struct{}
	principal Principal
	err       error
}

// newIntrospectionCache builds a cache. The caller has already validated the
// durations and the bound.
func newIntrospectionCache(ttl, negTTL time.Duration, max int, now func() time.Time) *introspectionCache {
	return &introspectionCache{
		ttl:      ttl,
		negTTL:   negTTL,
		max:      max,
		now:      now,
		entries:  make(map[tokenKey]cachedVerdict),
		inflight: make(map[tokenKey]*introspectionCall),
	}
}

// fetchFunc performs one live lookup, returning the Principal, the credential's
// own expiry (zero when unknown), and any denial.
type fetchFunc func(ctx context.Context, token string) (Principal, time.Time, error)

// resolve answers from cache when it can and calls fetch when it cannot.
func (c *introspectionCache) resolve(ctx context.Context, token string, fetch fetchFunc) (Principal, error) {
	key := hashToken(token)

	if v, ok := c.lookup(key); ok {
		if v.err != nil {
			return Principal{}, v.err
		}
		return v.principal.Clone(), nil
	}

	call, leader := c.join(key)
	if !leader {
		// Someone else is already asking. Wait for their answer rather than
		// issuing a second identical request — but keep honouring this caller's
		// own context, so one slow lookup cannot pin an unrelated request.
		select {
		case <-call.done:
			if call.err != nil {
				return Principal{}, call.err
			}
			return call.principal.Clone(), nil
		case <-ctx.Done():
			return Principal{}, NewError(KindUnavailable,
				"gave up waiting for an in-flight token introspection", ctx.Err())
		}
	}

	// The shared fetch deliberately does NOT inherit this request's cancellation.
	// Its result is handed to every waiter, so letting the first caller's
	// disconnect abort it would deny callers who are still connected. It stays
	// bounded: the fetch applies its own timeout.
	principal, expiry, err := fetch(context.WithoutCancel(ctx), token)

	c.publish(key, call, principal, expiry, err)
	if err != nil {
		return Principal{}, err
	}
	return principal.Clone(), nil
}

// lookup returns a live verdict for key, if there is one.
func (c *introspectionCache) lookup(key tokenKey) (cachedVerdict, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.entries[key]
	if !ok {
		return cachedVerdict{}, false
	}
	if !c.now().Before(v.expiresAt) {
		delete(c.entries, key)
		return cachedVerdict{}, false
	}
	return v, true
}

// join registers this caller as the leader of a lookup for key, or returns the
// existing leader's call to wait on.
func (c *introspectionCache) join(key tokenKey) (*introspectionCall, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if call, ok := c.inflight[key]; ok {
		return call, false
	}
	call := &introspectionCall{done: make(chan struct{})}
	c.inflight[key] = call
	return call, true
}

// publish stores the verdict (when it is cacheable), releases the waiters, and
// clears the in-flight marker.
func (c *introspectionCache) publish(key tokenKey, call *introspectionCall, p Principal, expiry time.Time, err error) {
	c.mu.Lock()
	now := c.now()
	if ttl := c.cacheableFor(err, expiry, now); ttl > 0 {
		c.evictLocked()
		c.entries[key] = cachedVerdict{principal: p.Clone(), err: err, expiresAt: now.Add(ttl)}
	}
	delete(c.inflight, key)
	c.mu.Unlock()

	call.principal = p
	call.err = err
	close(call.done)
}

// cacheableFor computes how long a verdict may be reused, or zero for "do not
// cache this one".
func (c *introspectionCache) cacheableFor(err error, expiry, now time.Time) time.Duration {
	if err != nil {
		// An outage must not be remembered — see the type comment.
		if KindOf(err) == KindUnavailable {
			return 0
		}
		return c.negTTL
	}
	ttl := c.ttl
	if !expiry.IsZero() {
		// Bounded by BOTH the configured maximum and the credential's own expiry,
		// taking the lower, so an entry can never outlive the token it describes.
		if remaining := expiry.Sub(now); remaining < ttl {
			ttl = remaining
		}
	}
	return ttl
}

// evictLocked keeps the entry count under the bound. It runs with c.mu held.
//
// Expired entries go first. If that is not enough, arbitrary entries are dropped
// down to a low-water mark — Go's map iteration order makes the choice random,
// which is fine here because the worst case of evicting a live entry is one
// extra round trip, never a wrong verdict. A random trim is deliberately chosen
// over clearing the whole map: a wholesale clear would turn one busy moment into
// a thundering herd of re-validations against the identity provider.
func (c *introspectionCache) evictLocked() {
	if len(c.entries) < c.max {
		return
	}
	now := c.now()
	for k, v := range c.entries {
		if !now.Before(v.expiresAt) {
			delete(c.entries, k)
		}
	}
	target := c.max * 3 / 4
	for k := range c.entries {
		if len(c.entries) <= target {
			return
		}
		delete(c.entries, k)
	}
}

// boundedTTL resolves a cache-duration option: zero means "use the default", a
// negative value is an operator error, and anything above maxIntrospectCacheTTL
// is refused rather than silently accepted — see that constant for why.
func boundedTTL(configured, fallback time.Duration, key string) (time.Duration, error) {
	d, err := positiveOrDefault(configured, fallback, key)
	if err != nil {
		return 0, err
	}
	if d > maxIntrospectCacheTTL {
		return 0, fmt.Errorf("%s must not exceed %s, got %s: a longer cache would keep a revoked token working",
			key, maxIntrospectCacheTTL, d)
	}
	return d, nil
}
