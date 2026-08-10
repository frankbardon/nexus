package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// routeMux is the subset of *http.ServeMux the per-concern Register methods
// use. It exists purely so a Register call can be pointed at either the raw mux
// (routes that must stay open) or an authenticating wrapper, without any handler
// having to know which. *http.ServeMux satisfies it directly, so tests that pass
// a plain mux keep working.
type routeMux interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// principalContextKey is the unexported context key the resolved Principal is
// stashed under. An unexported struct type cannot collide with a key set by any
// other package.
type principalContextKey struct{}

// withPrincipal returns a shallow copy of r carrying p in its context. The
// Principal travels in the request context rather than through every handler
// signature because the routes that need it (ownership checks, ticket minting)
// are added by later stories and each would otherwise force a signature change
// through the whole call chain.
func withPrincipal(r *http.Request, p nexusauth.Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalContextKey{}, p))
}

// PrincipalFrom returns the authenticated Principal for a request, and reports
// whether one was present. It is absent when auth is disabled or the route is
// not behind the guard, so a handler must treat !ok as "unauthenticated caller"
// rather than as an error.
func PrincipalFrom(ctx context.Context) (nexusauth.Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(nexusauth.Principal)
	return p, ok
}

// callerPrincipal returns the identity a lease-scoped route must attribute r to.
//
// An absent Principal is a supported state, not an error: it means either that
// the broker runs with no `auth:` block or that the route sits outside the guard.
// Both collapse to the anonymous identity, which is exactly the owner stamped on
// every lease claimed without authentication — so an id-equality check admits
// everyone when auth is off, precisely as the broker behaved before ownership
// existed.
func callerPrincipal(r *http.Request) nexusauth.Principal {
	if p, ok := PrincipalFrom(r.Context()); ok {
		return p
	}
	return anonymousOwner()
}

// unknownLeaseError is the ONE refusal message every lease-scoped route returns
// for a lease the caller may not act on, whether it never existed, was already
// released, or belongs to another principal.
//
// Keeping those cases indistinguishable is a security property rather than
// tidiness: a lease id is still the bearer secret for the instance dial-back
// path, so a caller able to tell "no such lease" from "not yours" could
// enumerate live lease ids by differencing responses. Both branches must write
// the same status AND the same body — see ownsLease.
const unknownLeaseError = "unknown lease"

// writeUnknownLease writes THE refusal for a lease the caller may not act on.
//
// Every JSON lease-scoped route calls this rather than assembling its own
// response, so the "unknown and unowned are indistinguishable" property holds by
// construction instead of by two handlers happening to spell the same status,
// Content-Type and body. A route that wrote its own would drift the day someone
// reworded one of them, and the drift would be a lease-id oracle rather than a
// cosmetic difference.
//
// The WebSocket route is the deliberate exception: it cannot answer JSON before
// an upgrade, so it writes the same message as plain text.
func writeUnknownLease(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": unknownLeaseError})
}

// ownsLease reports whether caller may act on the lease named by leaseID. It is
// the single ownership predicate behind every caller-facing lease route
// (POST /release/{lease_id}, WS /lease/{lease_id}).
//
// Ownership is principal-ID equality and nothing else — see
// nexusauth.Principal.ID. An unknown lease returns false through the SAME branch
// as an unowned one so no caller can tell the two apart; LeaseOwner's
// (anonymous, false) is deliberately NOT collapsed into (anonymous, true) here,
// because that would make every unknown lease id look anonymously owned and
// therefore releasable by an anonymous caller.
func ownsLease(reg *Registry, leaseID string, caller nexusauth.Principal) bool {
	owner, known := reg.LeaseOwner(leaseID)
	return known && owner.ID == caller.ID
}

// logLeaseDenied emits the audit record for an ownership refusal. It names the
// requesting principal, the lease id and the route, which is what an operator
// needs to tell a cross-principal probe apart from a client retrying its own
// already-released lease.
//
// The record deliberately does NOT say whether the lease existed. The response
// cannot distinguish the two (see unknownLeaseError) and neither does this line,
// so nobody can later "helpfully" surface a distinction the log had already
// drawn.
func logLeaseDenied(logger *slog.Logger, r *http.Request, caller nexusauth.Principal, leaseID string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("lease access denied",
		slog.String("route", routeLabel(r)),
		slog.String("principal_id", caller.ID),
		slog.String("lease_id", leaseID),
		slog.String("reason", "unknown lease or not owned by this principal"))
}

// authGuard authenticates client-facing broker routes against a nexusauth
// Chain. It holds no per-request state and is safe for concurrent use.
//
// Every allow and every deny emits exactly one structured slog record. Those
// records ARE the broker's audit trail — there is deliberately no separate audit
// sink, so the log must carry enough to reconstruct who was let in where.
type authGuard struct {
	logger *slog.Logger
	chain  *nexusauth.Chain
}

// newAuthGuard builds a guard over chain. A nil or empty chain yields a disabled
// guard, which is a legitimate deployment state (see Guard).
func newAuthGuard(logger *slog.Logger, chain *nexusauth.Chain) *authGuard {
	if logger == nil {
		logger = slog.Default()
	}
	return &authGuard{logger: logger, chain: chain}
}

// enabled reports whether any validator is configured.
func (g *authGuard) enabled() bool { return g != nil && g.chain.Enabled() }

// logStartupState emits the single startup record describing what the guard will
// do, and nothing else: exactly one line either way, so an operator scanning the
// first screen of output cannot miss it and cannot mistake one state for the
// other.
//
// The disabled case is a WARN rather than an INFO because running the broker
// open is a legitimate configuration but a dangerous default to drift into
// unnoticed — anyone who can reach the port can claim, release and enumerate
// leases.
func (g *authGuard) logStartupState() {
	if g.enabled() {
		g.logger.Info("client authentication enabled", "validators", g.chain.Names())
		return
	}
	g.logger.Warn("client authentication is DISABLED: broker config has no auth block, " +
		"so any caller that can reach this broker can claim, release and list leases")
}

// Guard returns a registrar that authenticates every route registered through
// it. Registering a route via the returned value is what makes it authenticated,
// so a route added later is guarded by construction rather than by someone
// remembering to wrap its handler.
//
// When no validator is configured Guard returns mux **unchanged**. That is not a
// shortcut: a broker.yaml with no `auth:` block must behave byte-for-byte as it
// did before authentication existed, and the surest way to guarantee that is for
// the disabled path to add no code to the request path at all.
func (g *authGuard) Guard(mux routeMux) routeMux {
	if !g.enabled() {
		return mux
	}
	return guardedMux{mux: mux, guard: g}
}

// guardedMux wraps each handler registered through it with the guard's
// middleware before handing it to the underlying mux.
type guardedMux struct {
	mux   routeMux
	guard *authGuard
}

// HandleFunc implements routeMux.
func (m guardedMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.mux.HandleFunc(pattern, m.guard.wrap(handler))
}

// wrap authenticates a request before invoking next, and passes the resolved
// Principal down in the request context.
func (g *authGuard) wrap(next func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := g.chain.Validate(r.Context(), r)
		if err != nil {
			g.deny(w, r, err)
			return
		}
		g.logger.Info("auth allowed", g.recordAttrs(r, p.ID, "")...)
		next(w, withPrincipal(r, p))
	}
}

// resolvePrincipal authenticates a request that is NOT registered through Guard,
// returning the caller's identity or the chain's classified denial.
//
// It exists for the client WebSocket route, which stays on the raw mux on
// purpose: that route grows a single-use ticket credential passed as a query
// parameter, which bearer-header middleware cannot express, so it resolves its
// own credential here instead of being moved behind the guard.
//
// A disabled guard yields the anonymous identity and no error — the same
// "auth is off, everyone is anonymous" reading callerPrincipal takes — so the
// route behaves exactly as it did before authentication existed.
func (g *authGuard) resolvePrincipal(r *http.Request) (nexusauth.Principal, error) {
	if !g.enabled() {
		return anonymousOwner(), nil
	}
	p, err := g.chain.Validate(r.Context(), r)
	if err != nil {
		return nexusauth.Principal{}, err
	}
	return p, nil
}

// deny maps a nexusauth denial onto a status code and writes the broker's usual
// JSON error envelope.
//
// The classification comes from nexusauth.KindOf, never from matching the error
// string: the kinds are the package's transport contract and string matching
// would silently reclassify a denial the day a reason is reworded.
//
// The client is told the KIND of refusal (so it can tell "send a credential"
// from "your credential is bad" from "you lack authority") but not which
// validator refused or why in detail. The full per-validator diagnosis goes to
// the log instead: it names the operator's validators, which is topology an
// unauthenticated caller has no business learning.
func (g *authGuard) deny(w http.ResponseWriter, r *http.Request, err error) {
	kind := nexusauth.KindOf(err)

	status := http.StatusUnauthorized
	reason := "credential rejected"
	switch kind {
	case nexusauth.KindNoCredential:
		reason = "authentication required"
	case nexusauth.KindInsufficientScope:
		status = http.StatusForbidden
		reason = "insufficient scope"
	case nexusauth.KindUnavailable:
		// The credential was neither accepted nor rejected: a validator that has
		// to ask a remote authority (an RFC 7662 introspection endpoint) could
		// not get an answer. Reporting that as 401 would tell every client at
		// once to go and re-authenticate against the provider that is already
		// failing, so the honest answer is "try again shortly" — and Retry-After
		// gives a well-behaved client a number to back off by instead of
		// hot-looping. Still a refusal: no lease is claimed, nothing is
		// released.
		status = http.StatusServiceUnavailable
		reason = "authentication temporarily unavailable"
	default:
		// KindInvalidCredential, and anything the package could not classify
		// (a bare transport error from a network-backed validator, say). Failing
		// closed on an unclassified denial is the only safe reading.
	}

	attrs := g.recordAttrs(r, "", reason)
	attrs = append(attrs, slog.Int("status", status), slog.Any("denial", err))
	g.logger.Warn("auth denied", attrs...)

	// RFC 6750: a 401 carries a challenge naming the scheme, and distinguishes a
	// missing credential from a rejected one via the error code.
	switch status {
	case http.StatusUnauthorized:
		if kind == nexusauth.KindNoCredential {
			w.Header().Set("WWW-Authenticate", `Bearer realm="nexus-broker"`)
		} else {
			w.Header().Set("WWW-Authenticate", `Bearer realm="nexus-broker", error="invalid_token"`)
		}
	case http.StatusForbidden:
		w.Header().Set("WWW-Authenticate", `Bearer realm="nexus-broker", error="insufficient_scope"`)
	case http.StatusServiceUnavailable:
		// Deliberately no WWW-Authenticate: a 503 is not a challenge, and
		// emitting one would invite exactly the re-authentication this status
		// exists to avoid.
		w.Header().Set("Retry-After", "5")
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": reason})
}

// recordAttrs builds the attributes shared by the allow and deny records:
// the route, the principal id (empty on a deny, because there isn't one), the
// lease id when the route has one in its path, and the reason on a deny.
//
// The lease id is read with PathValue, which is populated by the ServeMux during
// pattern matching — i.e. before the wrapped handler (and therefore this
// middleware) runs — so an attempt on someone else's lease is auditable from the
// allow record alone, before the handler's ownership check refuses it. Every
// lease-scoped route names its wildcard {lease_id} so this lookup finds it.
func (g *authGuard) recordAttrs(r *http.Request, principalID, reason string) []any {
	attrs := []any{
		slog.String("route", routeLabel(r)),
		slog.String("principal_id", principalID),
	}
	if leaseID := r.PathValue("lease_id"); leaseID != "" {
		attrs = append(attrs, slog.String("lease_id", leaseID))
	}
	if reason != "" {
		attrs = append(attrs, slog.String("reason", reason))
	}
	return attrs
}

// routeLabel names the route for an audit record. The matched mux pattern is
// preferred because it is a bounded, low-cardinality label ("POST
// /release/{lease_id}") rather than one value per lease; the concrete
// method+path is the fallback for a handler invoked outside a pattern match.
func routeLabel(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	return r.Method + " " + r.URL.Path
}
