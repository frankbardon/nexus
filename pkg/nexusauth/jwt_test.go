package nexusauth

import (
	"crypto/x509"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TestJWKSValidatorStandardClaims is the main table: one fake IdP, one
// runtime-generated RSA key, and one row per way a token can be good or bad.
func TestJWKSValidatorStandardClaims(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishRSA("k1", rsaTestKey(t, 0), "")
	clock := newFakeClock()
	v := newTestJWKSValidator(t, idp, clock, nil)
	now := clock.now()

	cases := []struct {
		name string
		// claims is applied over validClaims; a nil value deletes the claim.
		claims map[string]any
		// signKey overrides the signing key, for the wrong-key case.
		signKey any
		// raw overrides the whole token string.
		raw string

		wantReason string
		wantID     string
		wantTenant string
		wantScopes []string
	}{
		{
			name:       "valid",
			wantID:     "user-1",
			wantTenant: "acme",
			wantScopes: []string{"leases.read", "leases.write"},
		},
		{
			name:       "expired",
			claims:     map[string]any{"exp": now.Add(-time.Hour).Unix()},
			wantReason: "token has expired",
		},
		{
			name:       "not valid yet",
			claims:     map[string]any{"nbf": now.Add(10 * time.Minute).Unix()},
			wantReason: "token is not valid yet",
		},
		{
			name:       "wrong audience",
			claims:     map[string]any{"aud": "some-other-service"},
			wantReason: "audience does not match",
		},
		{
			// An absent `aud` is a rejection, not a pass: jwt requires the claim
			// once an audience is expected, so a token minted for no particular
			// service cannot claim a broker lease.
			name:       "missing audience",
			claims:     map[string]any{"aud": nil},
			wantReason: "missing a required claim",
		},
		{
			name:       "wrong issuer",
			claims:     map[string]any{"iss": "https://attacker.example/"},
			wantReason: "issuer does not match",
		},
		{
			name:       "missing expiry",
			claims:     map[string]any{"exp": nil},
			wantReason: "missing a required claim",
		},
		{
			name:       "missing principal claim",
			claims:     map[string]any{"sub": nil},
			wantReason: `has no "sub" claim`,
		},
		{
			name:       "empty principal claim",
			claims:     map[string]any{"sub": ""},
			wantReason: `claim "sub" is empty`,
		},
		{
			name:       "principal claim is not a scalar",
			claims:     map[string]any{"sub": []any{"user-1"}},
			wantReason: "not a usable principal id",
		},
		{
			name:       "signed by a key the issuer never published",
			signKey:    rsaTestKey(t, 2),
			wantReason: "signature does not verify",
		},
		{
			name:       "malformed token",
			raw:        "not-a-token",
			wantReason: "not a valid JWS header",
		},
		{
			name:       "tampered payload",
			raw:        "eyJhbGciOiJSUzI1NiIsImtpZCI6ImsxIn0.e30.AAAA",
			wantReason: "signature does not verify",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.raw
			if raw == "" {
				claims := validClaims(clock)
				for k, val := range tc.claims {
					if val == nil {
						delete(claims, k)
						continue
					}
					claims[k] = val
				}
				key := any(rsaTestKey(t, 0))
				if tc.signKey != nil {
					key = tc.signKey
				}
				raw = signToken(t, jwt.SigningMethodRS256, key, "k1", claims)
			}

			p, err := validateBearer(t, v, raw)
			if tc.wantReason != "" {
				requireDenied(t, err, tc.wantReason)
				if p.ID != "" {
					t.Fatalf("a denied validation returned a principal: %+v", p)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected denial: %v", err)
			}
			if p.ID != tc.wantID {
				t.Fatalf("principal id: want %q, got %q", tc.wantID, p.ID)
			}
			if p.Tenant != tc.wantTenant {
				t.Fatalf("tenant: want %q, got %q", tc.wantTenant, p.Tenant)
			}
			for _, scope := range tc.wantScopes {
				if !p.HasScope(scope) {
					t.Fatalf("scope %q missing from %v", scope, p.Scopes)
				}
			}
			// The whole verified claim set is carried for audit.
			if p.Claims["iss"] != testIssuer {
				t.Fatalf("claims not carried on the principal: %+v", p.Claims)
			}
		})
	}
}

// --- the three algorithm defences -------------------------------------------
//
// Each is asserted on its own, and each asserts the JWKS endpoint was never
// contacted: the algorithm is a configuration decision, so a token naming an
// algorithm the operator did not allow must be refused before a key is resolved.

// TestJWKSValidatorRejectsNoneAlgorithm covers `alg: none`. An unsigned token
// proves nothing, and the refusal must say so rather than read as a signature
// failure.
func TestJWKSValidatorRejectsNoneAlgorithm(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishRSA("k1", rsaTestKey(t, 0), "")
	clock := newFakeClock()
	v := newTestJWKSValidator(t, idp, clock, nil)

	unsigned := signToken(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, "k1", validClaims(clock))
	p, err := validateBearer(t, v, unsigned)
	if err == nil {
		t.Fatalf("an unsigned token was accepted as %+v", p)
	}
	requireDenied(t, err, `the "none" algorithm is never accepted`)
	if got := idp.requestCount(); got != 0 {
		t.Fatalf("the JWKS endpoint was contacted %d time(s) for an unsigned token", got)
	}
}

// TestJWKSValidatorRejectsHMACSignedToken covers algorithm confusion: the token
// is re-signed with HS256 using the issuer's public key as the shared secret, the
// exact shape that defeats a verifier which trusts the header's `alg`.
func TestJWKSValidatorRejectsHMACSignedToken(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishRSA("k1", rsaTestKey(t, 0), "")
	clock := newFakeClock()
	v := newTestJWKSValidator(t, idp, clock, nil)

	// The attacker's "secret" is the issuer's public key, which is public.
	publicKeyBytes, err := x509.MarshalPKIXPublicKey(&rsaTestKey(t, 0).PublicKey)
	if err != nil {
		t.Fatalf("marshalling the public key: %v", err)
	}
	forged := signToken(t, jwt.SigningMethodHS256, publicKeyBytes, "k1", validClaims(clock))

	p, err := validateBearer(t, v, forged)
	if err == nil {
		t.Fatalf("an HMAC-signed token was accepted as %+v", p)
	}
	requireDenied(t, err, "not one of the configured algorithms")
	if got := idp.requestCount(); got != 0 {
		t.Fatalf("the JWKS endpoint was contacted %d time(s) for an HMAC-signed token", got)
	}
}

// TestJWKSValidatorRejectsAlgorithmOutsideAllowlist covers the allowlist itself:
// a token signed with a genuinely published key, correctly, in an algorithm the
// operator did not list.
func TestJWKSValidatorRejectsAlgorithmOutsideAllowlist(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishRSA("k1", rsaTestKey(t, 0), "")
	clock := newFakeClock()
	v := newTestJWKSValidator(t, idp, clock, nil) // allowlist is RS256 only

	rs512 := signToken(t, jwt.SigningMethodRS512, rsaTestKey(t, 0), "k1", validClaims(clock))
	p, err := validateBearer(t, v, rs512)
	if err == nil {
		t.Fatalf("RS512 was accepted with an RS256-only allowlist: %+v", p)
	}
	requireDenied(t, err, `token algorithm "RS512" is not one of the configured algorithms`)
	if got := idp.requestCount(); got != 0 {
		t.Fatalf("the JWKS endpoint was contacted %d time(s) for a disallowed algorithm", got)
	}
}

// TestJWKSValidatorAcceptsConfiguredNonDefaultAlgorithm is the other half of the
// allowlist proof: the same RS512 token the previous test rejects verifies once
// the operator allows RS512. Without this, "RS512 was rejected" could just mean
// RS512 never worked.
func TestJWKSValidatorAcceptsConfiguredNonDefaultAlgorithm(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishRSA("k1", rsaTestKey(t, 0), "")
	clock := newFakeClock()
	v := newTestJWKSValidator(t, idp, clock, func(o *jwksOptions) {
		o.Algorithms = []string{jwt.SigningMethodRS512.Alg()}
	})

	rs512 := signToken(t, jwt.SigningMethodRS512, rsaTestKey(t, 0), "k1", validClaims(clock))
	p, err := validateBearer(t, v, rs512)
	if err != nil {
		t.Fatalf("RS512 rejected despite being allowed: %v", err)
	}
	if p.ID != "user-1" {
		t.Fatalf("unexpected principal: %+v", p)
	}
}

// TestAssertKeyMatchesMethod pins the second, independent layer of the
// key/algorithm defence: the check made where the resolved key and the token's
// signing method meet, so the defence does not rest solely on config parsing.
func TestAssertKeyMatchesMethod(t *testing.T) {
	rsaPub := &rsaTestKey(t, 0).PublicKey
	ecPub := &ecTestKey(t).PublicKey

	cases := []struct {
		name       string
		method     jwt.SigningMethod
		key        any
		wantReason string
	}{
		{name: "RS256 with an RSA key", method: jwt.SigningMethodRS256, key: rsaPub},
		{name: "PS256 with an RSA key", method: jwt.SigningMethodPS256, key: rsaPub},
		{name: "ES256 with an EC key", method: jwt.SigningMethodES256, key: ecPub},
		{
			name:       "HS256 with an RSA key",
			method:     jwt.SigningMethodHS256,
			key:        rsaPub,
			wantReason: "HMAC-signed but the issuer publishes an asymmetric key",
		},
		{
			name:       "HS256 with an EC key",
			method:     jwt.SigningMethodHS256,
			key:        ecPub,
			wantReason: "HMAC-signed but the issuer publishes an asymmetric key",
		},
		{
			name:       "RS256 with an EC key",
			method:     jwt.SigningMethodRS256,
			key:        ecPub,
			wantReason: "requires an RSA key",
		},
		{
			name:       "ES256 with an RSA key",
			method:     jwt.SigningMethodES256,
			key:        rsaPub,
			wantReason: "requires an EC key",
		},
		{
			name:       "none with an RSA key",
			method:     jwt.SigningMethodNone,
			key:        rsaPub,
			wantReason: "signing method is not accepted",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := assertKeyMatchesMethod(tc.method, tc.key)
			if tc.wantReason == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			requireDenied(t, err, tc.wantReason)
		})
	}
}

// TestJWKSValidatorHonoursPublishedKeyAlgorithm covers RFC 7517 §4.4: when the
// issuer says a key is for one algorithm, that key must not verify another, even
// if both algorithms are allowed.
func TestJWKSValidatorHonoursPublishedKeyAlgorithm(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishRSA("k1", rsaTestKey(t, 0), "RS256")
	clock := newFakeClock()
	v := newTestJWKSValidator(t, idp, clock, func(o *jwksOptions) {
		o.Algorithms = []string{jwt.SigningMethodRS256.Alg(), jwt.SigningMethodRS512.Alg()}
	})

	// RS256 matches what the issuer published for this key.
	ok := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 0), "k1", validClaims(clock))
	if _, err := validateBearer(t, v, ok); err != nil {
		t.Fatalf("RS256 token rejected: %v", err)
	}

	// RS512 is allowed by the operator but not by the issuer, for this key.
	mismatched := signToken(t, jwt.SigningMethodRS512, rsaTestKey(t, 0), "k1", validClaims(clock))
	_, err := validateBearer(t, v, mismatched)
	requireDenied(t, err, `publishes this key for "RS256"`)
}

// --- ECDSA -------------------------------------------------------------------

func TestJWKSValidatorVerifiesECDSA(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishEC("ec1", ecTestKey(t), "ES256")
	clock := newFakeClock()
	v := newTestJWKSValidator(t, idp, clock, func(o *jwksOptions) {
		o.Algorithms = []string{jwt.SigningMethodES256.Alg()}
	})

	token := signToken(t, jwt.SigningMethodES256, ecTestKey(t), "ec1", validClaims(clock))
	p, err := validateBearer(t, v, token)
	if err != nil {
		t.Fatalf("ES256 token rejected: %v", err)
	}
	if p.ID != "user-1" || p.Tenant != "acme" {
		t.Fatalf("unexpected principal: %+v", p)
	}
}

// --- claim mapping -----------------------------------------------------------

// TestJWKSValidatorClaimMapping covers the mapping shapes real issuers emit.
func TestJWKSValidatorClaimMapping(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishRSA("k1", rsaTestKey(t, 0), "")
	clock := newFakeClock()

	cases := []struct {
		name       string
		mapping    ClaimMapping
		claims     map[string]any
		wantID     string
		wantTenant string
		wantScopes []string
	}{
		{
			name:       "space-delimited scopes",
			mapping:    ClaimMapping{PrincipalClaim: "sub", TenantClaim: "org", ScopesClaim: "scope"},
			wantID:     "user-1",
			wantTenant: "acme",
			wantScopes: []string{"leases.read", "leases.write"},
		},
		{
			name:       "scopes as a JSON array",
			mapping:    ClaimMapping{PrincipalClaim: "sub", ScopesClaim: "permissions"},
			claims:     map[string]any{"permissions": []any{"leases.read", "nexus.broker.admin"}},
			wantID:     "user-1",
			wantScopes: []string{"leases.read", "nexus.broker.admin"},
		},
		{
			name:    "non-standard principal claim",
			mapping: ClaimMapping{PrincipalClaim: "client_id"},
			claims:  map[string]any{"client_id": "service-account-7"},
			wantID:  "service-account-7",
		},
		{
			name:    "numeric principal claim",
			mapping: ClaimMapping{PrincipalClaim: "uid"},
			claims:  map[string]any{"uid": 4815162342},
			wantID:  "4815162342",
		},
		{
			name:       "tenant claim absent from the token",
			mapping:    ClaimMapping{PrincipalClaim: "sub", TenantClaim: "tid"},
			wantID:     "user-1",
			wantTenant: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := newTestJWKSValidator(t, idp, clock, func(o *jwksOptions) {
				o.Mapping = tc.mapping
			})
			claims := validClaims(clock)
			for k, val := range tc.claims {
				claims[k] = val
			}
			token := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 0), "k1", claims)

			p, err := validateBearer(t, v, token)
			if err != nil {
				t.Fatalf("unexpected denial: %v", err)
			}
			if p.ID != tc.wantID {
				t.Fatalf("principal id: want %q, got %q", tc.wantID, p.ID)
			}
			if p.Tenant != tc.wantTenant {
				t.Fatalf("tenant: want %q, got %q", tc.wantTenant, p.Tenant)
			}
			if len(p.Scopes) != len(tc.wantScopes) {
				t.Fatalf("scopes: want %v, got %v", tc.wantScopes, p.Scopes)
			}
			for _, scope := range tc.wantScopes {
				if !p.HasScope(scope) {
					t.Fatalf("scope %q missing from %v", scope, p.Scopes)
				}
			}
		})
	}
}

// TestJWKSValidatorNoCredential asserts a missing credential is classified apart
// from a rejected one, so the broker can answer 401 with a challenge.
func TestJWKSValidatorNoCredential(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishRSA("k1", rsaTestKey(t, 0), "")
	v := newTestJWKSValidator(t, idp, newFakeClock(), nil)

	for _, header := range []string{"", "Basic abc", "Bearer "} {
		_, err := v.Validate(t.Context(), newRequest(t, header))
		if got := KindOf(err); got != KindNoCredential {
			t.Fatalf("header %q: want %q, got %q (%v)", header, KindNoCredential, got, err)
		}
		if !errors.Is(err, ErrNoCredential) {
			t.Fatalf("header %q: errors.Is(ErrNoCredential) failed for %v", header, err)
		}
	}
	if got := idp.requestCount(); got != 0 {
		t.Fatalf("the JWKS endpoint was contacted %d time(s) with no credential presented", got)
	}
}

// TestJWKSValidatorLeeway asserts the configured clock skew is applied to expiry,
// and that a token beyond the skew is still refused.
func TestJWKSValidatorLeeway(t *testing.T) {
	idp := newFakeIDP(t)
	idp.publishRSA("k1", rsaTestKey(t, 0), "")
	clock := newFakeClock()
	v := newTestJWKSValidator(t, idp, clock, func(o *jwksOptions) {
		o.ClockSkew = 30 * time.Second
		o.ClockSkewSet = true
	})

	claims := validClaims(clock)
	claims["exp"] = clock.now().Add(-10 * time.Second).Unix()
	within := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 0), "k1", claims)
	if _, err := validateBearer(t, v, within); err != nil {
		t.Fatalf("token expired inside the skew window was rejected: %v", err)
	}

	claims["exp"] = clock.now().Add(-90 * time.Second).Unix()
	beyond := signToken(t, jwt.SigningMethodRS256, rsaTestKey(t, 0), "k1", claims)
	_, err := validateBearer(t, v, beyond)
	requireDenied(t, err, "token has expired")
}

// --- construction guards -----------------------------------------------------

func TestNewJWKSValidatorRejectsBadOptions(t *testing.T) {
	base := func() jwksOptions {
		return jwksOptions{
			Issuer:    testIssuer,
			JWKSURL:   "https://issuer.example/jwks",
			Audiences: []string{testAudience},
			Mapping:   ClaimMapping{PrincipalClaim: "sub"},
		}
	}
	cases := []struct {
		name    string
		mutate  func(*jwksOptions)
		wantErr string
	}{
		{"no issuer", func(o *jwksOptions) { o.Issuer = "" }, "issuer is required"},
		{"no jwks url", func(o *jwksOptions) { o.JWKSURL = "" }, "jwks_url is required"},
		{"no audience", func(o *jwksOptions) { o.Audiences = nil }, "audience is required"},
		{"blank audience", func(o *jwksOptions) { o.Audiences = []string{"  "} }, "audience is required"},
		{
			"no principal claim",
			func(o *jwksOptions) { o.Mapping = ClaimMapping{} },
			"principal_claim is required",
		},
		{
			"relative jwks url",
			func(o *jwksOptions) { o.JWKSURL = "/jwks" },
			"want an absolute URL",
		},
		{
			"plain http to a remote host",
			func(o *jwksOptions) { o.JWKSURL = "http://issuer.example/jwks" },
			"http is only allowed for a loopback host",
		},
		{
			"unsupported scheme",
			func(o *jwksOptions) { o.JWKSURL = "ftp://issuer.example/jwks" },
			"scheme must be https",
		},
		{
			"scheme with no host",
			func(o *jwksOptions) { o.JWKSURL = "file:///etc/jwks.json" },
			"want an absolute URL",
		},
		{
			"userinfo in the url",
			func(o *jwksOptions) { o.JWKSURL = "https://user:pass@issuer.example/jwks" },
			"userinfo is not supported",
		},
		{
			"none algorithm",
			func(o *jwksOptions) { o.Algorithms = []string{"none"} },
			"an unsigned token proves nothing",
		},
		{
			"hmac algorithm",
			func(o *jwksOptions) { o.Algorithms = []string{"HS256"} },
			"algorithm-confusion attack",
		},
		{
			"unknown algorithm",
			func(o *jwksOptions) { o.Algorithms = []string{"RS255"} },
			`unsupported algorithm "RS255"`,
		},
		{
			"duplicate algorithm",
			func(o *jwksOptions) { o.Algorithms = []string{"RS256", "RS256"} },
			"duplicate algorithm",
		},
		{
			"negative cache ttl",
			func(o *jwksOptions) { o.CacheTTL = -time.Minute },
			"cache_ttl must not be negative",
		},
		{
			"negative http timeout",
			func(o *jwksOptions) { o.HTTPTimeout = -time.Second },
			"http_timeout must not be negative",
		},
		{
			"clock skew beyond the cap",
			func(o *jwksOptions) { o.ClockSkew = time.Hour; o.ClockSkewSet = true },
			"clock_skew must be between",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := base()
			tc.mutate(&opts)
			_, err := newJWKSValidator(opts)
			if err == nil {
				t.Fatalf("want an error containing %q, got none", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want an error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestNewJWKSValidatorAppliesDefaults asserts the documented defaults, since the
// configuration reference states them as contract.
func TestNewJWKSValidatorAppliesDefaults(t *testing.T) {
	v, err := newJWKSValidator(jwksOptions{
		Issuer:    testIssuer,
		JWKSURL:   "https://issuer.example/jwks",
		Audiences: []string{testAudience},
		Mapping:   ClaimMapping{PrincipalClaim: "sub"},
	})
	if err != nil {
		t.Fatalf("newJWKSValidator: %v", err)
	}
	if len(v.algorithms) != 1 || v.algorithms[0] != defaultJWKSAlgorithm {
		t.Fatalf("want the default algorithm %q, got %v", defaultJWKSAlgorithm, v.algorithms)
	}
	if v.keys.ttl != defaultJWKSCacheTTL {
		t.Fatalf("cache ttl: want %s, got %s", defaultJWKSCacheTTL, v.keys.ttl)
	}
	if v.keys.negTTL != defaultJWKSNegativeCacheTTL {
		t.Fatalf("negative cache ttl: want %s, got %s", defaultJWKSNegativeCacheTTL, v.keys.negTTL)
	}
	if v.keys.timeout != defaultJWKSHTTPTimeout {
		t.Fatalf("http timeout: want %s, got %s", defaultJWKSHTTPTimeout, v.keys.timeout)
	}
	if v.keys.client.Timeout != defaultJWKSHTTPTimeout {
		t.Fatalf("the constructed HTTP client is unbounded: %s", v.keys.client.Timeout)
	}
}

// TestJWSHeaderAlgorithm covers the pre-verification header read directly, since
// it runs on unauthenticated input.
func TestJWSHeaderAlgorithm(t *testing.T) {
	cases := []struct {
		name    string
		token   string
		wantAlg string
		wantErr string
	}{
		{"rs256", "eyJhbGciOiJSUzI1NiJ9.e30.AA", "RS256", ""},
		{"no dot", "abc", "", "no header segment"},
		{"leading dot", ".e30.AA", "", "no header segment"},
		{"not base64url", "!!!.e30.AA", "", "not unpadded base64url"},
		{"not json", "YWJj.e30.AA", "", "not a JSON object"},
		{"no alg", "e30.e30.AA", "", "declares no alg"},
		{
			"oversized header",
			strings.Repeat("A", maxJWSHeaderBytes+1) + ".e30.AA",
			"",
			"larger than",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			alg, err := jwsHeaderAlgorithm(tc.token)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want an error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if alg != tc.wantAlg {
				t.Fatalf("want alg %q, got %q", tc.wantAlg, alg)
			}
		})
	}
}
