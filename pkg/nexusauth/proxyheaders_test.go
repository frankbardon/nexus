package nexusauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The header names used throughout these tests are deliberately three different
// real-world conventions, to keep the "configurable, not hardcoded" property
// honest: nothing here would pass if the validator quietly read a built-in name.
const (
	hdrUser   = "X-Forwarded-User"
	hdrTenant = "X-Auth-Request-Org"
	hdrScopes = "X-Forwarded-Groups"
)

// newProxyRequest builds a request the way an http.Server would: RemoteAddr set
// from the accepted connection, headers set by whoever is upstream.
func newProxyRequest(remoteAddr string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/claim", nil)
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// testProxyValidator is the validator every case below shares: one IPv4 and one
// IPv6 ingress network, all three headers configured.
func testProxyValidator(t *testing.T) *ProxyHeadersValidator {
	t.Helper()
	v, err := newProxyHeadersValidator(proxyHeadersOptions{
		TrustedProxyCIDRs: []string{"10.4.0.0/16", "fd00:1ce::/64"},
		PrincipalHeader:   hdrUser,
		TenantHeader:      hdrTenant,
		ScopesHeader:      hdrScopes,
	})
	if err != nil {
		t.Fatalf("newProxyHeadersValidator: %v", err)
	}
	return v
}

func TestProxyHeadersValidator(t *testing.T) {
	// validHeaders is one set of perfectly well-formed proxy headers, reused
	// verbatim by both the allowed-peer and the denied-peer cases so that the
	// peer address is the ONLY difference between them.
	validHeaders := map[string]string{
		hdrUser:   "alice@example.com",
		hdrTenant: "acme",
		hdrScopes: "broker.claim broker.release",
	}

	cases := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		wantID     string
		wantTenant string
		wantScopes []string
		wantKind   Kind
	}{
		{
			name:       "allowed IPv4 peer with valid headers",
			remoteAddr: "10.4.7.9:52344",
			headers:    validHeaders,
			wantID:     "alice@example.com",
			wantTenant: "acme",
			wantScopes: []string{"broker.claim", "broker.release"},
		},
		{
			name:       "allowed IPv6 peer with valid headers",
			remoteAddr: "[fd00:1ce::5]:41000",
			headers:    validHeaders,
			wantID:     "alice@example.com",
			wantTenant: "acme",
			wantScopes: []string{"broker.claim", "broker.release"},
		},
		{
			// THE test. Identical headers to the first case, different peer.
			// If the CIDR check were removed this case would pass validation and
			// this expectation would fail — see TestProxyHeadersDeniedPeerTestIsNonVacuous.
			name:       "denied peer presenting perfectly valid headers",
			remoteAddr: "203.0.113.9:52344",
			headers:    validHeaders,
			wantKind:   KindNoCredential,
		},
		{
			name:       "denied IPv6 peer outside the allowlist",
			remoteAddr: "[2001:db8::1]:41000",
			headers:    validHeaders,
			wantKind:   KindNoCredential,
		},
		{
			name:       "peer just outside the IPv4 prefix",
			remoteAddr: "10.5.0.1:52344",
			headers:    validHeaders,
			wantKind:   KindNoCredential,
		},
		{
			name:       "allowed peer with no principal header",
			remoteAddr: "10.4.0.1:52344",
			headers:    map[string]string{hdrTenant: "acme"},
			wantKind:   KindInvalidCredential,
		},
		{
			name:       "allowed peer with blank principal header",
			remoteAddr: "10.4.0.1:52344",
			headers:    map[string]string{hdrUser: "   "},
			wantKind:   KindInvalidCredential,
		},
		{
			name:       "malformed remote address",
			remoteAddr: "not-an-address",
			headers:    validHeaders,
			wantKind:   KindNoCredential,
		},
		{
			name:       "empty remote address",
			remoteAddr: "",
			headers:    validHeaders,
			wantKind:   KindNoCredential,
		},
		{
			// A Unix-socket peer has no IP at all. Denial, not a panic.
			name:       "unix socket peer",
			remoteAddr: "/var/run/nexus-broker.sock",
			headers:    validHeaders,
			wantKind:   KindNoCredential,
		},
		{
			// Some harnesses and proxies hand over a bare address with no port.
			name:       "allowed peer with no port in RemoteAddr",
			remoteAddr: "10.4.1.1",
			headers:    validHeaders,
			wantID:     "alice@example.com",
			wantTenant: "acme",
			wantScopes: []string{"broker.claim", "broker.release"},
		},
		{
			name:       "denied bare IPv6 peer with no port",
			remoteAddr: "2001:db8::9",
			headers:    validHeaders,
			wantKind:   KindNoCredential,
		},
		{
			// A dual-stack listener can report the IPv4 peer in mapped form; the
			// operator wrote an IPv4 prefix and expects it to match.
			name:       "IPv4-mapped IPv6 peer matches the IPv4 prefix",
			remoteAddr: "[::ffff:10.4.2.2]:52344",
			headers:    validHeaders,
			wantID:     "alice@example.com",
			wantTenant: "acme",
			wantScopes: []string{"broker.claim", "broker.release"},
		},
		{
			name:       "comma-delimited scopes",
			remoteAddr: "10.4.0.1:52344",
			headers:    map[string]string{hdrUser: "bob", hdrScopes: "broker.claim,broker.release, admin"},
			wantID:     "bob",
			wantScopes: []string{"broker.claim", "broker.release", "admin"},
		},
		{
			name:       "no tenant or scopes headers sent",
			remoteAddr: "10.4.0.1:52344",
			headers:    map[string]string{hdrUser: "bob"},
			wantID:     "bob",
		},
		{
			// Header lookup is case-insensitive on both sides.
			name:       "lower-cased header names still match",
			remoteAddr: "10.4.0.1:52344",
			headers:    map[string]string{"x-forwarded-user": "carol"},
			wantID:     "carol",
		},
	}

	v := testProxyValidator(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := v.Validate(context.Background(), newProxyRequest(tc.remoteAddr, tc.headers))
			if tc.wantKind != "" {
				if err == nil {
					t.Fatalf("want a denial of kind %q, got principal %+v", tc.wantKind, p)
				}
				if got := KindOf(err); got != tc.wantKind {
					t.Fatalf("want kind %q, got %q (%v)", tc.wantKind, got, err)
				}
				if p.ID != "" {
					t.Fatalf("a denial must produce no principal, got %+v", p)
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
			if strings.Join(p.Scopes, " ") != strings.Join(tc.wantScopes, " ") {
				t.Fatalf("scopes: want %v, got %v", tc.wantScopes, p.Scopes)
			}
		})
	}
}

// TestProxyHeadersDeniedPeerTestIsNonVacuous proves the denied-peer case above
// is actually testing the CIDR check and not some incidental property of the
// request. The same headers, byte for byte, are accepted from a peer inside the
// allowlist and refused from one outside it — so if the network check were
// deleted, the refusal expectation could not still hold.
func TestProxyHeadersDeniedPeerTestIsNonVacuous(t *testing.T) {
	v := testProxyValidator(t)
	headers := map[string]string{hdrUser: "alice@example.com", hdrTenant: "acme"}

	allowed := newProxyRequest("10.4.7.9:52344", headers)
	denied := newProxyRequest("203.0.113.9:52344", headers)
	if allowed.Header.Get(hdrUser) != denied.Header.Get(hdrUser) {
		t.Fatal("the two requests must differ only in RemoteAddr")
	}

	p, err := v.Validate(context.Background(), allowed)
	if err != nil || p.ID != "alice@example.com" {
		t.Fatalf("in-CIDR peer must be accepted: %+v %v", p, err)
	}
	if _, err := v.Validate(context.Background(), denied); err == nil {
		t.Fatal("out-of-CIDR peer with identical headers must be denied")
	}
}

// TestProxyHeadersIgnoresForwardedFor is the other half of the trust model:
// X-Forwarded-For is written by whoever is talking to us, so consulting it would
// hand the allowlist to the caller. Neither spoofing an allowed address from a
// denied peer nor spoofing a denied address from an allowed peer may change the
// verdict.
func TestProxyHeadersIgnoresForwardedFor(t *testing.T) {
	v := testProxyValidator(t)

	forged := newProxyRequest("203.0.113.9:52344", map[string]string{hdrUser: "alice"})
	forged.Header.Set("X-Forwarded-For", "10.4.7.9")
	forged.Header.Add("X-Forwarded-For", "fd00:1ce::5")
	forged.Header.Set("X-Real-IP", "10.4.7.9")
	if _, err := v.Validate(context.Background(), forged); err == nil {
		t.Fatal("a forged X-Forwarded-For must not make an untrusted peer trusted")
	} else if got := KindOf(err); got != KindNoCredential {
		t.Fatalf("want %q, got %q (%v)", KindNoCredential, got, err)
	}

	honest := newProxyRequest("10.4.7.9:52344", map[string]string{hdrUser: "alice"})
	honest.Header.Set("X-Forwarded-For", "203.0.113.9")
	p, err := v.Validate(context.Background(), honest)
	if err != nil {
		t.Fatalf("a forged X-Forwarded-For must not untrust a trusted peer: %v", err)
	}
	if p.ID != "alice" {
		t.Fatalf("unexpected principal: %+v", p)
	}
}

// TestProxyHeadersRejectsDuplicateHeader covers the proxy that appends its value
// to the caller's instead of replacing it, which would otherwise let the caller's
// forged value win the http.Header.Get race.
func TestProxyHeadersRejectsDuplicateHeader(t *testing.T) {
	v := testProxyValidator(t)
	for _, header := range []string{hdrUser, hdrTenant, hdrScopes} {
		t.Run(header, func(t *testing.T) {
			r := newProxyRequest("10.4.7.9:52344", map[string]string{
				hdrUser:   "real-user",
				hdrTenant: "real-org",
				hdrScopes: "real.scope",
			})
			// The proxy appended instead of replacing, so the caller's own value
			// is still there — and it is the one http.Header.Get would return
			// when it comes first.
			r.Header.Add(header, "attacker")
			p, err := v.Validate(context.Background(), r)
			if err == nil {
				t.Fatalf("duplicated %s must be refused, got %+v", header, p)
			}
			if got := KindOf(err); got != KindInvalidCredential {
				t.Fatalf("want %q, got %q (%v)", KindInvalidCredential, got, err)
			}
		})
	}
}

func TestProxyHeadersDenialNeverLeaksAnIdentity(t *testing.T) {
	v := testProxyValidator(t)
	_, err := v.Validate(context.Background(), newProxyRequest("203.0.113.9:52344",
		map[string]string{hdrUser: "alice@example.com"}))
	if err == nil {
		t.Fatal("want a denial")
	}
	if strings.Contains(err.Error(), "alice@example.com") {
		t.Fatalf("denial echoed the asserted identity: %v", err)
	}
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("errors.Is(ErrNoCredential) failed for %v", err)
	}
}

func TestProxyHeadersReturnsIsolatedPrincipal(t *testing.T) {
	v := testProxyValidator(t)
	r := newProxyRequest("10.4.7.9:52344", map[string]string{hdrUser: "alice", hdrScopes: "admin"})
	p, err := v.Validate(context.Background(), r)
	if err != nil {
		t.Fatalf("unexpected denial: %v", err)
	}
	p.Scopes[0] = "hacked"

	again, err := v.Validate(context.Background(), r)
	if err != nil {
		t.Fatalf("unexpected denial: %v", err)
	}
	if !again.HasScope("admin") {
		t.Fatalf("returned principal aliased validator state: %+v", again.Scopes)
	}
	if again.Claims != nil {
		t.Fatalf("proxy headers carry no claim set, got %+v", again.Claims)
	}
}

func TestNewProxyHeadersValidatorRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name    string
		opts    proxyHeadersOptions
		wantErr string
	}{
		{
			// The single most important boot failure in this package: an absent
			// allowlist must never be read as "trust everyone".
			name:    "no allowlist",
			opts:    proxyHeadersOptions{PrincipalHeader: hdrUser},
			wantErr: "trusted_proxy_cidrs is required",
		},
		{
			name:    "empty allowlist",
			opts:    proxyHeadersOptions{TrustedProxyCIDRs: []string{}, PrincipalHeader: hdrUser},
			wantErr: "trusted_proxy_cidrs is required",
		},
		{
			name:    "malformed CIDR",
			opts:    proxyHeadersOptions{TrustedProxyCIDRs: []string{"10.4.0.0/33"}, PrincipalHeader: hdrUser},
			wantErr: "trusted_proxy_cidrs[0]",
		},
		{
			name:    "bare address instead of a CIDR",
			opts:    proxyHeadersOptions{TrustedProxyCIDRs: []string{"10.4.0.1"}, PrincipalHeader: hdrUser},
			wantErr: "is not a CIDR block",
		},
		{
			name:    "malformed CIDR in a later position",
			opts:    proxyHeadersOptions{TrustedProxyCIDRs: []string{"10.4.0.0/16", "nonsense"}, PrincipalHeader: hdrUser},
			wantErr: "trusted_proxy_cidrs[1]",
		},
		{
			name:    "no principal header",
			opts:    proxyHeadersOptions{TrustedProxyCIDRs: []string{"10.4.0.0/16"}},
			wantErr: "principal_header is required",
		},
		{
			name: "header name with a colon",
			opts: proxyHeadersOptions{
				TrustedProxyCIDRs: []string{"10.4.0.0/16"},
				PrincipalHeader:   "X-Forwarded-User: alice",
			},
			wantErr: "is not a valid header name",
		},
		{
			name: "tenant header name with whitespace",
			opts: proxyHeadersOptions{
				TrustedProxyCIDRs: []string{"10.4.0.0/16"},
				PrincipalHeader:   hdrUser,
				TenantHeader:      "X Auth Org",
			},
			wantErr: "tenant_header",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := newProxyHeadersValidator(tc.opts)
			if err == nil {
				t.Fatalf("want a config error, got a validator: %+v", v)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestProxyHeadersConfigFromYAML(t *testing.T) {
	auth := decodeYAML(t, `
auth:
  validators:
    - type: proxy_headers
      trusted_proxy_cidrs:
        - 10.4.0.0/16
        - fd00:1ce::/64
      principal_header: X-Forwarded-User
      tenant_header: X-Auth-Request-Org
      scopes_header: X-Forwarded-Groups
`)
	chain, err := ChainFromMap(auth)
	if err != nil {
		t.Fatalf("ChainFromMap: %v", err)
	}
	if names := chain.Names(); len(names) != 1 || names[0] != ValidatorTypeProxyHeaders {
		t.Fatalf("unexpected chain names: %v", names)
	}

	p, err := chain.Validate(context.Background(), newProxyRequest("10.4.9.9:1234", map[string]string{
		hdrUser:   "alice",
		hdrTenant: "acme",
		hdrScopes: "broker.claim",
	}))
	if err != nil {
		t.Fatalf("unexpected denial: %v", err)
	}
	if p.ID != "alice" || p.Tenant != "acme" || !p.HasScope("broker.claim") {
		t.Fatalf("unexpected principal: %+v", p)
	}
}

// A single CIDR may be written as a scalar rather than a list, like every other
// string-or-list key in this package.
func TestProxyHeadersConfigAcceptsScalarCIDR(t *testing.T) {
	auth := decodeYAML(t, `
auth:
  validators:
    - type: proxy_headers
      trusted_proxy_cidrs: 10.4.0.0/16
      principal_header: X-Forwarded-User
`)
	if _, err := ChainFromMap(auth); err != nil {
		t.Fatalf("ChainFromMap: %v", err)
	}
}

func TestProxyHeadersConfigRejectsBadEntries(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{
			name: "empty allowlist fails the boot",
			doc: `
auth:
  validators:
    - type: proxy_headers
      trusted_proxy_cidrs: []
      principal_header: X-Forwarded-User
`,
			wantErr: "trusted_proxy_cidrs is required",
		},
		{
			name: "absent allowlist fails the boot",
			doc: `
auth:
  validators:
    - type: proxy_headers
      principal_header: X-Forwarded-User
`,
			wantErr: "trusted_proxy_cidrs is required",
		},
		{
			name: "malformed CIDR fails the boot",
			doc: `
auth:
  validators:
    - type: proxy_headers
      trusted_proxy_cidrs: [10.4.0.0/16, "not-a-cidr"]
      principal_header: X-Forwarded-User
`,
			wantErr: "is not a CIDR block",
		},
		{
			name: "unknown key",
			doc: `
auth:
  validators:
    - type: proxy_headers
      trusted_proxy_cidrs: [10.4.0.0/16]
      principal_header: X-Forwarded-User
      trust_forwarded_for: true
`,
			wantErr: `unknown key(s) "trust_forwarded_for"`,
		},
		{
			name: "wrongly typed CIDR list",
			doc: `
auth:
  validators:
    - type: proxy_headers
      trusted_proxy_cidrs: 42
      principal_header: X-Forwarded-User
`,
			wantErr: "trusted_proxy_cidrs",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ChainFromMap(decodeYAML(t, tc.doc))
			if err == nil {
				t.Fatal("want a config error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestProxyHeadersComposesInChain is the deployment the validator exists for:
// proxy headers from the ingress network, bearer tokens from anywhere else, in a
// defined order — with the aggregate denial still naming every validator that
// refused.
func TestProxyHeadersComposesInChain(t *testing.T) {
	auth := decodeYAML(t, `
auth:
  validators:
    - type: proxy_headers
      trusted_proxy_cidrs: [10.4.0.0/16]
      principal_header: X-Forwarded-User
    - type: static
      tokens:
        - token: tok-ci
          principal: ci
`)
	chain, err := ChainFromMap(auth)
	if err != nil {
		t.Fatalf("ChainFromMap: %v", err)
	}

	// Through the ingress: the header identity wins, and the static table is
	// never consulted.
	p, err := chain.Validate(context.Background(), newProxyRequest("10.4.0.7:100", map[string]string{hdrUser: "alice"}))
	if err != nil {
		t.Fatalf("unexpected denial: %v", err)
	}
	if p.ID != "alice" {
		t.Fatalf("unexpected principal: %+v", p)
	}

	// Direct from the internet with a valid static token: the proxy_headers
	// entry declines and the static entry accepts.
	direct := newProxyRequest("203.0.113.9:100", map[string]string{hdrUser: "alice"})
	direct.Header.Set("Authorization", "Bearer tok-ci")
	p, err = chain.Validate(context.Background(), direct)
	if err != nil {
		t.Fatalf("unexpected denial: %v", err)
	}
	if p.ID != "ci" {
		t.Fatalf("forged header beat the static token: %+v", p)
	}

	// Direct from the internet with nothing valid: denied, with both refusals
	// recorded, and the aggregate kind is a challengeable 401 rather than
	// "credential rejected".
	_, err = chain.Validate(context.Background(), newProxyRequest("203.0.113.9:100", map[string]string{hdrUser: "alice"}))
	if err == nil {
		t.Fatal("want a denial")
	}
	var denied *DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("want a *DeniedError, got %T", err)
	}
	if len(denied.Attempts) != 2 {
		t.Fatalf("want one attempt per validator, got %d: %v", len(denied.Attempts), denied.Attempts)
	}
	if denied.Attempts[0].Validator != ValidatorTypeProxyHeaders || denied.Attempts[1].Validator != ValidatorTypeStatic {
		t.Fatalf("chain order lost: %v", denied.Attempts)
	}
	if got := denied.Kind(); got != KindNoCredential {
		t.Fatalf("want %q, got %q", KindNoCredential, got)
	}
}

func TestProxyHeadersTrustedCIDRsReported(t *testing.T) {
	v, err := newProxyHeadersValidator(proxyHeadersOptions{
		// Host bits set on purpose: the reported form must be the masked prefix
		// that actually decides the match.
		TrustedProxyCIDRs: []string{"10.4.1.2/16", "fd00:1ce::7/64"},
		PrincipalHeader:   hdrUser,
	})
	if err != nil {
		t.Fatalf("newProxyHeadersValidator: %v", err)
	}
	got := strings.Join(v.TrustedCIDRs(), " ")
	if want := "10.4.0.0/16 fd00:1ce::/64"; got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
	// And a host-bits prefix must still match, rather than matching nothing.
	if _, err := v.Validate(context.Background(),
		newProxyRequest("10.4.250.1:9", map[string]string{hdrUser: "alice"})); err != nil {
		t.Fatalf("masked prefix did not match: %v", err)
	}
}

func TestProxyHeadersNilRequest(t *testing.T) {
	v := testProxyValidator(t)
	if _, err := v.Validate(context.Background(), nil); err == nil {
		t.Fatal("a nil request must be denied, not panic")
	}
}
