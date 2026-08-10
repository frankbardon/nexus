package nexusauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Nothing in this file is a checked-in credential. Every key, token and key-set
// document is generated at run time: a fixture key looks like a leaked secret to
// a scanner, and a fixture token would pin the tests to a wall-clock instant and
// force every expiry case to sleep.

const (
	testIssuer   = "https://issuer.example/"
	testAudience = "nexus-broker"
)

// Key generation is the slowest thing in this package's tests, so the keys are
// generated once and shared. They are read-only after generation.
var (
	testKeysOnce sync.Once
	testRSAKeys  []*rsa.PrivateKey
	testECKey    *ecdsa.PrivateKey
	testKeysErr  error
)

func generateTestKeys() {
	testKeysOnce.Do(func() {
		for i := 0; i < 3; i++ {
			k, err := rsa.GenerateKey(rand.Reader, minRSAModulusBits)
			if err != nil {
				testKeysErr = fmt.Errorf("generating RSA key %d: %w", i, err)
				return
			}
			testRSAKeys = append(testRSAKeys, k)
		}
		testECKey, testKeysErr = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	})
}

// rsaTestKey returns one of the shared RSA keys. Index 0 is the key the fake IdP
// publishes by default; the others stand in for a rotated key and for a key the
// issuer never published.
func rsaTestKey(t *testing.T, index int) *rsa.PrivateKey {
	t.Helper()
	generateTestKeys()
	if testKeysErr != nil {
		t.Fatalf("generating test keys: %v", testKeysErr)
	}
	return testRSAKeys[index]
}

func ecTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	generateTestKeys()
	if testKeysErr != nil {
		t.Fatalf("generating test keys: %v", testKeysErr)
	}
	return testECKey
}

// fakeClock is a deterministic clock. Every time-dependent assertion in these
// tests — expiry, not-before, cache staleness, negative-cache expiry — moves this
// rather than sleeping.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func newFakeClock() *fakeClock {
	// A fixed, arbitrary instant. Nothing depends on the value, only on the
	// differences.
	return &fakeClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// fakeIDP is a JWKS endpoint. It counts requests, so a test can assert that a
// cache hit cost no round trip and that a flood of bad key ids did not amplify
// into issuer traffic.
type fakeIDP struct {
	server *httptest.Server

	mu       sync.Mutex
	keys     []map[string]any
	requests int
	status   int
	body     string
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	idp := &fakeIDP{}
	idp.server = httptest.NewServer(http.HandlerFunc(idp.serve))
	t.Cleanup(idp.server.Close)
	return idp
}

func (s *fakeIDP) serve(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.requests++
	status, body := s.status, s.body
	doc := map[string]any{"keys": append([]map[string]any(nil), s.keys...)}
	s.mu.Unlock()

	if status != 0 && status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if body != "" {
		_, _ = w.Write([]byte(body))
		return
	}
	_ = json.NewEncoder(w).Encode(doc)
}

// url is the JWKS endpoint. httptest listens on a loopback address, which is the
// one case validateJWKSURL lets through over plain http.
func (s *fakeIDP) url() string { return s.server.URL + "/jwks" }

func (s *fakeIDP) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// setStatus makes the endpoint answer with an HTTP error, standing in for an
// identity provider that is up but not serving keys.
func (s *fakeIDP) setStatus(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = code
}

// setBody replaces the generated document with a literal one, for malformed-JWKS
// cases.
func (s *fakeIDP) setBody(body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body = body
}

// shutdown stops the server so subsequent fetches fail to connect — an
// unreachable endpoint rather than an erroring one.
func (s *fakeIDP) shutdown() { s.server.Close() }

// publishRSA adds an RSA signature key to the published set. alg may be empty,
// meaning the issuer declares no algorithm for the key.
func (s *fakeIDP) publishRSA(kid string, key *rsa.PrivateKey, alg string) {
	entry := map[string]any{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}
	if alg != "" {
		entry["alg"] = alg
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, entry)
}

// publishEC adds an EC signature key to the published set.
func (s *fakeIDP) publishEC(kid string, key *ecdsa.PrivateKey, alg string) {
	size := (key.Curve.Params().BitSize + 7) / 8
	entry := map[string]any{
		"kty": "EC",
		"kid": kid,
		"use": "sig",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(leftPad(key.X.Bytes(), size)),
		"y":   base64.RawURLEncoding.EncodeToString(leftPad(key.Y.Bytes(), size)),
	}
	if alg != "" {
		entry["alg"] = alg
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, entry)
}

// document renders the currently published key set, for tests that exercise
// parseJWKS directly.
func (s *fakeIDP) document(t *testing.T) []byte {
	t.Helper()
	s.mu.Lock()
	keys := append([]map[string]any(nil), s.keys...)
	s.mu.Unlock()
	body, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		t.Fatalf("marshalling key set: %v", err)
	}
	return body
}

// leftPad zero-extends b to size bytes, which is how RFC 7518 encodes an EC
// coordinate.
func leftPad(b []byte, size int) []byte {
	if len(b) >= size {
		return b
	}
	out := make([]byte, size)
	copy(out[size-len(b):], b)
	return out
}

// validClaims is a claim set that passes every standard check for clock.
func validClaims(clock *fakeClock) jwt.MapClaims {
	now := clock.now()
	return jwt.MapClaims{
		"iss":   testIssuer,
		"aud":   testAudience,
		"sub":   "user-1",
		"org":   "acme",
		"scope": "leases.read leases.write",
		"iat":   now.Unix(),
		"nbf":   now.Add(-time.Minute).Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
}

// signToken mints a token with an explicit signing method, key and kid, so a test
// can deliberately mismatch any of the three.
func signToken(t *testing.T, method jwt.SigningMethod, key any, kid string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	if kid != "" {
		token.Header["kid"] = kid
	}
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	return signed
}

// newTestJWKSValidator builds a validator against idp with a deterministic clock.
// mutate may adjust any option before construction.
func newTestJWKSValidator(t *testing.T, idp *fakeIDP, clock *fakeClock, mutate func(*jwksOptions)) *JWKSValidator {
	t.Helper()
	opts := jwksOptions{
		Issuer:     testIssuer,
		JWKSURL:    idp.url(),
		Audiences:  []string{testAudience},
		Algorithms: []string{jwt.SigningMethodRS256.Alg()},
		Mapping: ClaimMapping{
			PrincipalClaim: "sub",
			TenantClaim:    "org",
			ScopesClaim:    "scope",
		},
		httpClient: idp.server.Client(),
		now:        clock.now,
	}
	if mutate != nil {
		mutate(&opts)
	}
	v, err := newJWKSValidator(opts)
	if err != nil {
		t.Fatalf("newJWKSValidator: %v", err)
	}
	return v
}

// validateBearer runs the validator over a request carrying token.
func validateBearer(t *testing.T, v *JWKSValidator, token string) (Principal, error) {
	t.Helper()
	return v.Validate(context.Background(), newRequest(t, "Bearer "+token))
}

// requireDenied asserts a denial classified as an invalid credential whose reason
// contains want.
func requireDenied(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a denial, got none")
	}
	if got := KindOf(err); got != KindInvalidCredential {
		t.Fatalf("want kind %q, got %q (%v)", KindInvalidCredential, got, err)
	}
	if want != "" && !strings.Contains(err.Error(), want) {
		t.Fatalf("want a denial mentioning %q, got %v", want, err)
	}
}

// --- key cache behaviour -----------------------------------------------------

// TestKeyCacheHitAvoidsNetwork is the performance contract: verification sits on
// POST /claim and on the WebSocket connect path, so a warm kid must cost no round
// trip.
func TestKeyCacheHitAvoidsNetwork(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishRSA("k1", rsaTestKey(t, 0), "")
	clock := newFakeClock()
	v := newTestJWKSValidator(t, idp, clock, nil)

	token := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 0), "k1", validClaims(clock))
	for i := 0; i < 5; i++ {
		if _, err := validateBearer(t, v, token); err != nil {
			t.Fatalf("validation %d: %v", i, err)
		}
	}
	if got := idp.requestCount(); got != 1 {
		t.Fatalf("want exactly 1 JWKS request across 5 validations, got %d", got)
	}
}

// TestKeyCacheRotationWithoutRestart covers the rotation requirement: a key added
// to the JWKS after the cache was populated must verify, with no restart.
func TestKeyCacheRotationWithoutRestart(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishRSA("k1", rsaTestKey(t, 0), "")
	clock := newFakeClock()
	v := newTestJWKSValidator(t, idp, clock, nil)

	// Populate the cache with the pre-rotation key set.
	old := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 0), "k1", validClaims(clock))
	if _, err := validateBearer(t, v, old); err != nil {
		t.Fatalf("pre-rotation token: %v", err)
	}

	// The issuer rotates: a second key appears alongside the first.
	idp.publishRSA("k2", rsaTestKey(t, 1), "")
	rotated := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 1), "k2", validClaims(clock))

	p, err := validateBearer(t, v, rotated)
	if err != nil {
		t.Fatalf("post-rotation token: %v", err)
	}
	if p.ID != "user-1" {
		t.Fatalf("unexpected principal: %+v", p)
	}
	if got := idp.requestCount(); got != 2 {
		t.Fatalf("want 2 JWKS requests (initial + on-demand for the new kid), got %d", got)
	}
	// The old key must keep working through the rotation window.
	if _, err := validateBearer(t, v, old); err != nil {
		t.Fatalf("pre-rotation token after rotation: %v", err)
	}
}

// TestKeyCacheNegativeCachesUnknownKID covers the negative cache: a repeated bad
// key id must not turn into repeated issuer traffic, and the entry must expire.
func TestKeyCacheNegativeCachesUnknownKID(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishRSA("k1", rsaTestKey(t, 0), "")
	clock := newFakeClock()
	v := newTestJWKSValidator(t, idp, clock, nil)

	warm := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 0), "k1", validClaims(clock))
	if _, err := validateBearer(t, v, warm); err != nil {
		t.Fatalf("warming the cache: %v", err)
	}

	forged := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 2), "nope", validClaims(clock))
	_, err := validateBearer(t, v, forged)
	requireDenied(t, err, "nope")
	if got := idp.requestCount(); got != 2 {
		t.Fatalf("want 2 requests after the first unknown kid, got %d", got)
	}

	// Ten more presentations of the same forged token must cost nothing.
	for i := 0; i < 10; i++ {
		if _, err := validateBearer(t, v, forged); err == nil {
			t.Fatalf("attempt %d: forged token was accepted", i)
		}
	}
	if got := idp.requestCount(); got != 2 {
		t.Fatalf("negative cache did not hold: want 2 requests, got %d", got)
	}

	// Once the negative entry expires the issuer may be asked again, which is
	// what lets a rotation that happened during the window be picked up.
	clock.advance(defaultJWKSNegativeCacheTTL + time.Second)
	if _, err := validateBearer(t, v, forged); err == nil {
		t.Fatalf("forged token was accepted after the negative cache expired")
	}
	if got := idp.requestCount(); got != 3 {
		t.Fatalf("want 3 requests after the negative cache expired, got %d", got)
	}
}

// TestKeyCacheBoundsFetchesForDistinctUnknownKIDs covers the amplification guard:
// a per-kid negative cache alone would let a caller inventing a fresh kid per
// request drive one JWKS fetch per request.
func TestKeyCacheBoundsFetchesForDistinctUnknownKIDs(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishRSA("k1", rsaTestKey(t, 0), "")
	clock := newFakeClock()
	v := newTestJWKSValidator(t, idp, clock, nil)

	warm := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 0), "k1", validClaims(clock))
	if _, err := validateBearer(t, v, warm); err != nil {
		t.Fatalf("warming the cache: %v", err)
	}

	for i := 0; i < 25; i++ {
		forged := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 2),
			fmt.Sprintf("forged-%d", i), validClaims(clock))
		if _, err := validateBearer(t, v, forged); err == nil {
			t.Fatalf("attempt %d: forged token was accepted", i)
		}
	}
	// One on-demand fetch for the first unknown kid, then the global bound holds
	// until it expires.
	if got := idp.requestCount(); got != 2 {
		t.Fatalf("want 2 JWKS requests across 25 distinct forged kids, got %d", got)
	}
}

// TestKeyCacheServesCachedKeyWhenEndpointUnreachable is the safe-degradation
// half of the JWKS-failure requirement: a key we already hold keeps verifying.
func TestKeyCacheServesCachedKeyWhenEndpointUnreachable(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishRSA("k1", rsaTestKey(t, 0), "")
	clock := newFakeClock()
	v := newTestJWKSValidator(t, idp, clock, nil)

	warm := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 0), "k1", validClaims(clock))
	if _, err := validateBearer(t, v, warm); err != nil {
		t.Fatalf("warming the cache: %v", err)
	}

	idp.shutdown()

	// Well past the TTL, so the cached snapshot is stale as well as unrefreshable.
	// The token is minted after the jump so its own expiry is not what is under
	// test.
	clock.advance(defaultJWKSCacheTTL * 10)
	token := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 0), "k1", validClaims(clock))
	p, err := validateBearer(t, v, token)
	if err != nil {
		t.Fatalf("cached key stopped verifying when the endpoint went away: %v", err)
	}
	if p.ID != "user-1" {
		t.Fatalf("unexpected principal: %+v", p)
	}
}

// TestKeyCacheDeniesUncachedKIDWhenEndpointUnreachable is the other half: an
// endpoint we cannot reach can never vouch for a key we do not hold, so the
// answer is a denial and never an allow.
func TestKeyCacheDeniesUncachedKIDWhenEndpointUnreachable(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishRSA("k1", rsaTestKey(t, 0), "")
	clock := newFakeClock()
	v := newTestJWKSValidator(t, idp, clock, nil)

	warm := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 0), "k1", validClaims(clock))
	if _, err := validateBearer(t, v, warm); err != nil {
		t.Fatalf("warming the cache: %v", err)
	}

	idp.shutdown()

	unseen := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 1), "k2", validClaims(clock))
	p, err := validateBearer(t, v, unseen)
	if err == nil {
		t.Fatalf("uncached kid was accepted with an unreachable endpoint: %+v", p)
	}
	requireDenied(t, err, "could not be fetched")
}

// TestKeyCacheDeniesFirstFetchFailure covers the cold-start failure: no cached
// key at all plus a broken endpoint is a denial, not an allow.
func TestKeyCacheDeniesFirstFetchFailure(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishRSA("k1", rsaTestKey(t, 0), "")
	idp.setStatus(http.StatusInternalServerError)
	clock := newFakeClock()
	v := newTestJWKSValidator(t, idp, clock, nil)

	token := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 0), "k1", validClaims(clock))
	_, err := validateBearer(t, v, token)
	requireDenied(t, err, "could not be fetched")

	// The failure is rate limited too: a broken endpoint must not be hammered.
	for i := 0; i < 5; i++ {
		if _, err := validateBearer(t, v, token); err == nil {
			t.Fatalf("attempt %d: accepted despite a broken endpoint", i)
		}
	}
	if got := idp.requestCount(); got != 1 {
		t.Fatalf("want 1 JWKS request while the endpoint is broken, got %d", got)
	}

	// Recovery must not need a restart.
	clock.advance(defaultJWKSNegativeCacheTTL + time.Second)
	idp.setStatus(http.StatusOK)
	if _, err := validateBearer(t, v, token); err != nil {
		t.Fatalf("validation after the endpoint recovered: %v", err)
	}
}

// TestKeyCacheBackgroundRefreshOnTTL asserts a stale snapshot is refreshed behind
// the request: the request that observes staleness is still served from cache,
// and the refresh happens without it waiting.
func TestKeyCacheBackgroundRefreshOnTTL(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishRSA("k1", rsaTestKey(t, 0), "")
	clock := newFakeClock()
	v := newTestJWKSValidator(t, idp, clock, nil)

	token := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 0), "k1", validClaims(clock))
	if _, err := validateBearer(t, v, token); err != nil {
		t.Fatalf("warming the cache: %v", err)
	}
	if got := idp.requestCount(); got != 1 {
		t.Fatalf("want 1 request after warming, got %d", got)
	}

	clock.advance(defaultJWKSCacheTTL + time.Second)
	if _, err := validateBearer(t, v, token); err != nil {
		t.Fatalf("validation with a stale snapshot must still succeed from cache: %v", err)
	}

	// The refresh is detached, so wait for it rather than assuming it has landed.
	deadline := time.Now().Add(5 * time.Second)
	for idp.requestCount() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("background refresh never fetched: still %d request(s)", idp.requestCount())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestKeyCacheRespectsHTTPTimeout asserts a hanging identity provider cannot hang
// a claim.
func TestKeyCacheRespectsHTTPTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	clock := newFakeClock()
	// httpClient deliberately left nil so newJWKSValidator builds one bounded by
	// HTTPTimeout — that binding is what is under test.
	v, err := newJWKSValidator(jwksOptions{
		Issuer:      testIssuer,
		JWKSURL:     server.URL + "/jwks",
		Audiences:   []string{testAudience},
		Algorithms:  []string{jwt.SigningMethodRS256.Alg()},
		Mapping:     ClaimMapping{PrincipalClaim: "sub"},
		HTTPTimeout: 100 * time.Millisecond,
		now:         clock.now,
	})
	if err != nil {
		t.Fatalf("newJWKSValidator: %v", err)
	}

	token := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 0), "k1", validClaims(clock))
	start := time.Now()
	if _, err := validateBearer(t, v, token); err == nil {
		t.Fatalf("a hanging endpoint must not authenticate anything")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("validation took %s against a hanging endpoint; the HTTP call is not bounded", elapsed)
	}
}

// --- JWKS document parsing ---------------------------------------------------

func TestParseJWKSAcceptsRSAAndEC(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishRSA("rsa-1", rsaTestKey(t, 0), "RS256")
	idp.publishEC("ec-1", ecTestKey(t), "ES256")

	set, err := parseJWKS(idp.document(t))
	if err != nil {
		t.Fatalf("parseJWKS: %v", err)
	}
	if len(set.all) != 2 {
		t.Fatalf("want 2 usable keys, got %d", len(set.all))
	}
	if entry, ok := set.lookup("rsa-1"); !ok || entry.alg != "RS256" {
		t.Fatalf("rsa-1 not resolved with its declared alg: %+v (ok=%v)", entry, ok)
	}
	if entry, ok := set.lookup("ec-1"); !ok {
		t.Fatalf("ec-1 not resolved: %+v", entry)
	}
	// No kid and more than one key must not resolve to an arbitrary key.
	if _, ok := set.lookup(""); ok {
		t.Fatalf("a kid-less lookup resolved against a multi-key set")
	}
}

func TestParseJWKSRejections(t *testing.T) {
	rsaKey := rsaTestKey(t, 0)
	goodN := base64.RawURLEncoding.EncodeToString(rsaKey.N.Bytes())
	goodE := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(rsaKey.E)).Bytes())

	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"not json", `{`, "decoding key set"},
		{"no keys", `{"keys":[]}`, "no keys"},
		{
			"encryption key only",
			`{"keys":[{"kty":"RSA","kid":"k","use":"enc","n":"` + goodN + `","e":"` + goodE + `"}]}`,
			`use is "enc"`,
		},
		{
			"unsupported key type",
			`{"keys":[{"kty":"OKP","kid":"k","crv":"Ed25519","x":"AAAA"}]}`,
			`unsupported key type "OKP"`,
		},
		{
			"weak modulus",
			`{"keys":[{"kty":"RSA","kid":"k","n":"AQAB","e":"` + goodE + `"}]}`,
			"minimum is 2048",
		},
		{
			"even exponent",
			`{"keys":[{"kty":"RSA","kid":"k","n":"` + goodN + `","e":"AgAA"}]}`,
			"public exponent must be odd",
		},
		{
			// A 128-bit exponent cannot fit rsa.PublicKey.E; truncating it would
			// build a key that silently verifies nothing.
			"exponent too large for an int",
			`{"keys":[{"kty":"RSA","kid":"k","n":"` + goodN + `","e":"AQAAAAAAAAAAAAAAAAAAAQ"}]}`,
			"public exponent is out of range",
		},
		{
			"exponent below 3",
			`{"keys":[{"kty":"RSA","kid":"k","n":"` + goodN + `","e":"AQ"}]}`,
			"public exponent is out of range",
		},
		{
			"modulus not base64url",
			`{"keys":[{"kty":"RSA","kid":"k","n":"!!!","e":"` + goodE + `"}]}`,
			"modulus: not base64url",
		},
		{
			"missing exponent",
			`{"keys":[{"kty":"RSA","kid":"k","n":"` + goodN + `"}]}`,
			"exponent: value is required",
		},
		{
			"unsupported curve",
			`{"keys":[{"kty":"EC","kid":"k","crv":"P-192","x":"AAAA","y":"AAAA"}]}`,
			`unsupported curve "P-192"`,
		},
		{
			"ec point not on curve",
			`{"keys":[{"kty":"EC","kid":"k","crv":"P-256",` +
				`"x":"AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",` +
				`"y":"AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}]}`,
			"invalid P-256 point",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseJWKS([]byte(tc.body))
			if err == nil {
				t.Fatalf("want an error containing %q, got none", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want an error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestParseJWKSSkipsUnusableKeyAlongsideUsableOne asserts a key type we do not
// support does not take the whole key set down with it.
func TestParseJWKSSkipsUnusableKeyAlongsideUsableOne(t *testing.T) {
	rsaKey := rsaTestKey(t, 0)
	body := `{"keys":[` +
		`{"kty":"OKP","kid":"unsupported","crv":"Ed25519","x":"AAAA"},` +
		`{"kty":"RSA","kid":"good","n":"` + base64.RawURLEncoding.EncodeToString(rsaKey.N.Bytes()) +
		`","e":"` + base64.RawURLEncoding.EncodeToString(big.NewInt(int64(rsaKey.E)).Bytes()) + `"}]}`

	set, err := parseJWKS([]byte(body))
	if err != nil {
		t.Fatalf("parseJWKS: %v", err)
	}
	if _, ok := set.lookup("good"); !ok {
		t.Fatalf("the usable key was dropped")
	}
	if len(set.all) != 1 {
		t.Fatalf("want 1 usable key, got %d", len(set.all))
	}
}

// TestValidatorRejectsMalformedJWKSDocument checks the fetch path surfaces a
// parse failure as a denial rather than a panic or an allow.
func TestValidatorRejectsMalformedJWKSDocument(t *testing.T) {
	idp := newFakeIDP(t)
	idp.setBody(`{"keys":[{"kty":"RSA","kid":"k1"}]}`)
	clock := newFakeClock()
	v := newTestJWKSValidator(t, idp, clock, nil)

	token := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 0), "k1", validClaims(clock))
	_, err := validateBearer(t, v, token)
	requireDenied(t, err, "could not be fetched")
}

// TestJWKSSingleKeyWithoutKID covers the kid-less fallback: it works with exactly
// one published key and nothing else.
func TestJWKSSingleKeyWithoutKID(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishRSA("", rsaTestKey(t, 0), "")
	clock := newFakeClock()
	v := newTestJWKSValidator(t, idp, clock, nil)

	token := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 0), "", validClaims(clock))
	if _, err := validateBearer(t, v, token); err != nil {
		t.Fatalf("single-key set with no kid: %v", err)
	}

	// A second key removes the "obviously this one" property, so the fallback
	// must stop resolving.
	idp2 := newFakeIDP(t)
	idp2.publishRSA("", rsaTestKey(t, 0), "")
	idp2.publishRSA("", rsaTestKey(t, 1), "")
	v2 := newTestJWKSValidator(t, idp2, clock, nil)
	_, err := validateBearer(t, v2, token)
	requireDenied(t, err, "names no key id")
}
