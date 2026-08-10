package nexusauth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ValidatorTypeProxyHeaders is the config `type:` that selects
// ProxyHeadersValidator.
const ValidatorTypeProxyHeaders = "proxy_headers"

// Keys a `proxy_headers` validator entry accepts, consumed by
// buildProxyHeadersValidator.
const (
	keyTrustedProxyCIDRs = "trusted_proxy_cidrs"
	keyPrincipalHeader   = "principal_header"
	keyTenantHeader      = "tenant_header"
	keyScopesHeader      = "scopes_header"
)

// proxyHeadersOptions is the resolved, decoder-independent form of a
// `proxy_headers` config entry. buildProxyHeadersValidator fills it from the
// YAML map; tests fill it directly.
type proxyHeadersOptions struct {
	// TrustedProxyCIDRs lists the networks whose peers are allowed to assert an
	// identity through headers. It must be non-empty — see
	// newProxyHeadersValidator.
	TrustedProxyCIDRs []string

	// PrincipalHeader names the header carrying the principal id. Required:
	// X-Forwarded-User, X-Auth-Request-Email and X-Forwarded-Preferred-Username
	// are all real conventions and none of them is right for everybody.
	PrincipalHeader string

	// TenantHeader and ScopesHeader name the optional tenant and scope headers.
	// Empty means the corresponding Principal field is never populated.
	TenantHeader string
	ScopesHeader string
}

// ProxyHeadersValidator trusts an identity a fronting reverse proxy has already
// established and passed down in request headers — oauth2-proxy, an OIDC-aware
// ingress, a service-mesh sidecar.
//
// It is the most dangerous validator in this package, because a header is not a
// credential: anyone who can open a socket to the broker can send any header
// they like. The single thing that makes it safe is the network check. Headers
// are read ONLY when the request's own peer address falls inside a configured
// CIDR allowlist, i.e. only when the connection demonstrably came from the
// proxy. Every other request is denied without the headers being consulted at
// all.
//
// Three deliberate non-negotiables follow from that:
//
//   - The allowlist may not be empty. An absent allowlist is a boot error, never
//     an implicit allow-everything — failing open here converts the broker into
//     an open door with no signal that anything is wrong.
//   - X-Forwarded-For is never consulted. It is written by whoever is talking to
//     us and can name any address; using it for the trust decision would hand
//     the allowlist to the attacker. Only r.RemoteAddr, which is the kernel's
//     view of the peer, is used.
//   - A peer address that cannot be parsed (unset, malformed, or a Unix socket,
//     which has no IP at all) is denied. There is no "probably local" fallback.
//
// It composes with the other validators through a Chain: a deployment can accept
// proxy headers from its ingress network and JWTs from anywhere, in the order
// the operator listed them.
//
// It is immutable after construction and safe for concurrent use.
type ProxyHeadersValidator struct {
	trusted         []netip.Prefix
	principalHeader string
	tenantHeader    string
	scopesHeader    string
}

// newProxyHeadersValidator is the single construction path, so every guard
// applies to every instance.
func newProxyHeadersValidator(opts proxyHeadersOptions) (*ProxyHeadersValidator, error) {
	// The allowlist is the entire security model of this validator, so an empty
	// one is refused at boot rather than treated as "trust everyone". This is the
	// difference between a deployment that fails to start and a deployment that
	// silently accepts a forged X-Forwarded-User from the public internet.
	if len(opts.TrustedProxyCIDRs) == 0 {
		return nil, fmt.Errorf(
			"%s is required and must list at least one CIDR: headers are forgeable by any peer, "+
				"so an empty allowlist would trust every caller's claimed identity", keyTrustedProxyCIDRs)
	}
	trusted := make([]netip.Prefix, 0, len(opts.TrustedProxyCIDRs))
	for i, raw := range opts.TrustedProxyCIDRs {
		// netip.ParsePrefix handles IPv4 and IPv6 identically, so both families
		// are supported with no branch here.
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %q is not a CIDR block such as \"10.0.0.0/8\" or \"fd00::/8\": %w",
				keyTrustedProxyCIDRs, i, truncateForLog(raw), err)
		}
		// Masked so a prefix written with host bits set ("10.1.2.3/8") is stored
		// in the form it actually matches on, which keeps a logged or printed
		// allowlist honest about what it covers.
		trusted = append(trusted, prefix.Masked())
	}

	// No default header name. Every real deployment spells this differently, and
	// guessing would mean an operator who mistyped the key still gets a working
	// validator reading somebody else's convention.
	if opts.PrincipalHeader == "" {
		return nil, fmt.Errorf("%s is required: it names the header that carries the principal id", keyPrincipalHeader)
	}
	if err := validateHeaderName(opts.PrincipalHeader, keyPrincipalHeader); err != nil {
		return nil, err
	}
	if err := validateHeaderName(opts.TenantHeader, keyTenantHeader); err != nil {
		return nil, err
	}
	if err := validateHeaderName(opts.ScopesHeader, keyScopesHeader); err != nil {
		return nil, err
	}

	return &ProxyHeadersValidator{
		trusted:         trusted,
		principalHeader: opts.PrincipalHeader,
		tenantHeader:    opts.TenantHeader,
		scopesHeader:    opts.ScopesHeader,
	}, nil
}

// Validate implements Validator.
//
// The order of the checks is load-bearing: the peer address is settled before a
// single header is read, so there is no code path on which a header from an
// untrusted peer can influence anything.
func (v *ProxyHeadersValidator) Validate(_ context.Context, r *http.Request) (Principal, error) {
	if r == nil {
		return Principal{}, NewError(KindNoCredential, "no request to authenticate", nil)
	}

	peer, err := peerAddr(r.RemoteAddr)
	if err != nil {
		// Unset, malformed, or a Unix-socket peer with no IP. Denial is the only
		// safe reading: we cannot show the connection came from the proxy.
		return Principal{}, NewError(KindNoCredential, "peer address is not a usable IP, so proxy headers are not trusted", err)
	}
	if !v.trusts(peer) {
		// KindNoCredential, not KindInvalidCredential, and deliberately so. From
		// an untrusted peer these headers are not a credential that failed — they
		// are not a credential at all, because this validator never looked at
		// them. Reporting "rejected" would (a) confirm to a prober that its
		// forged headers were considered, and (b) outrank a genuine
		// "no credential" from a sibling validator in the Chain's aggregate kind,
		// turning a client that simply forgot its bearer token into a
		// "credential rejected" with no challenge inviting it to present one.
		return Principal{}, NewError(KindNoCredential,
			fmt.Sprintf("peer %s is not inside %s, so proxy headers are ignored",
				truncateForLog(peer.String()), keyTrustedProxyCIDRs), nil)
	}

	id, err := singleHeaderValue(r, v.principalHeader, keyPrincipalHeader)
	if err != nil {
		return Principal{}, err
	}
	if id == "" {
		// Absent or blank. Either way the proxy asserted no usable identity, and
		// an empty Principal.ID would compare equal to the broker's anonymous
		// owner and to every other empty-id principal — a privilege-escalation
		// path, not a cosmetic gap. Same rule as jwks and introspect.
		return Principal{}, NewError(KindInvalidCredential,
			fmt.Sprintf("trusted peer sent no %s header value", v.principalHeader), nil)
	}

	p := Principal{ID: id}
	if v.tenantHeader != "" {
		tenant, err := singleHeaderValue(r, v.tenantHeader, keyTenantHeader)
		if err != nil {
			return Principal{}, err
		}
		p.Tenant = tenant
	}
	if v.scopesHeader != "" {
		raw, err := singleHeaderValue(r, v.scopesHeader, keyScopesHeader)
		if err != nil {
			return Principal{}, err
		}
		scopes, err := ParseScopes(normalizeScopeDelimiters(raw))
		if err != nil {
			return Principal{}, NewError(KindInvalidCredential,
				fmt.Sprintf("%s header is not a usable scope set", v.scopesHeader), err)
		}
		p.Scopes = scopes
	}

	// Principal.Claims stays nil: proxy headers have no claim set, exactly as a
	// static token has none. Copying attacker-influenceable header text into an
	// audit map would suggest a structure that is not there.
	return p, nil
}

// trusts reports whether peer falls inside any configured prefix.
func (v *ProxyHeadersValidator) trusts(peer netip.Addr) bool {
	for _, prefix := range v.trusted {
		if prefix.Contains(peer) {
			return true
		}
	}
	return false
}

// TrustedCIDRs returns the allowlist in configured order, for a startup log line
// that lets an operator confirm what the broker will actually trust.
func (v *ProxyHeadersValidator) TrustedCIDRs() []string {
	out := make([]string, 0, len(v.trusted))
	for _, p := range v.trusted {
		out = append(out, p.String())
	}
	return out
}

// peerAddr parses the IP out of an http.Request RemoteAddr.
//
// It is only ever given r.RemoteAddr, which the server fills in from the
// accepted connection — never a header. X-Forwarded-For is caller-controlled and
// is not consulted anywhere in this file.
//
// The bare-host fallback exists because RemoteAddr is "IP:port" for a real
// TCP listener but frequently a bare address in tests and in some proxy
// harnesses, and a bare IPv6 literal makes net.SplitHostPort fail rather than
// return the address.
func peerAddr(remoteAddr string) (netip.Addr, error) {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if remoteAddr == "" {
		return netip.Addr{}, fmt.Errorf("remote address is empty")
	}
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		// A Unix-socket peer lands here (RemoteAddr is a filesystem path, or the
		// empty string), as does anything malformed. Both are denials.
		return netip.Addr{}, fmt.Errorf("remote address %q is not an IP", truncateForLog(remoteAddr))
	}
	// Unmap so a dual-stack listener reporting ::ffff:10.0.0.1 is matched by the
	// 10.0.0.0/8 the operator wrote, and drop the zone because netip.Prefix
	// never carries one — Contains returns false for ANY zoned address, which
	// would make a link-local ingress impossible to allowlist. A zone is chosen
	// by the local kernel from the receiving interface, not by the peer, so it
	// carries no trust signal to discard.
	return addr.Unmap().WithZone(""), nil
}

// singleHeaderValue reads one header, refusing a header that arrived more than
// once.
//
// A duplicate is not pedantry. Several proxies APPEND their value to a header
// the client already sent rather than replacing it, which leaves
// "X-Forwarded-User: attacker, real-user" as two values — and http.Header.Get
// returns the FIRST, which is the caller's. Refusing an ambiguous assertion is
// the only reading that cannot be turned into an impersonation; a correctly
// configured proxy always sends exactly one.
func singleHeaderValue(r *http.Request, header, key string) (string, error) {
	values := r.Header.Values(header)
	if len(values) > 1 {
		return "", NewError(KindInvalidCredential,
			fmt.Sprintf("%s (%s) arrived %d times; a duplicated header means the fronting proxy appended to the caller's value instead of replacing it",
				header, key, len(values)), nil)
	}
	if len(values) == 0 {
		return "", nil
	}
	return strings.TrimSpace(values[0]), nil
}

// normalizeScopeDelimiters lets one scopes header serve both conventions in the
// wild: OAuth 2.0's space-delimited "scope" string and the comma-delimited lists
// proxies such as oauth2-proxy emit for groups. Commas become spaces and
// ParseScopes — the same splitter every other validator uses — does the rest.
//
// Rewriting rather than adding a separator option is safe because RFC 6749's
// scope-token grammar excludes both the comma and the space, so no scope can
// legitimately contain either.
func normalizeScopeDelimiters(s string) string {
	return strings.ReplaceAll(s, ",", " ")
}

// validateHeaderName rejects a configured header name that could never match, so
// a typo fails the boot instead of silently producing an unauthenticated
// deployment. An empty name means "not configured" and is the caller's business
// to require or not.
//
// Header lookups themselves are case-insensitive (net/http canonicalizes both
// sides), so "x-forwarded-user" and "X-Forwarded-User" are the same key.
func validateHeaderName(name, key string) error {
	if name == "" {
		return nil
	}
	if strings.ContainsAny(name, " \t\r\n:") {
		return fmt.Errorf("%s: %q is not a valid header name", key, truncateForLog(name))
	}
	return nil
}
