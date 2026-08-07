package nexusauth

import (
	"context"
	"net/http"
	"strings"
)

// Validator verifies the credential on an inbound request. It is the single
// extension point of this package: everything else — the chain, the errors, the
// config parser — is shared plumbing around implementations of this one method.
//
// A Validator returns a Principal with a non-empty ID on success. On failure it
// returns a classified *Error (see NewError) so the chain can aggregate the
// reason and the caller can map it to a status code. Implementations must be
// safe for concurrent use: one instance serves every request.
type Validator interface {
	Validate(ctx context.Context, r *http.Request) (Principal, error)
}

// NamedValidator pairs a Validator with the operator-facing name a Chain uses in
// its denial records. The name is not part of the Validator interface on
// purpose: a validator does not know what an operator called it, and keeping the
// interface to one method keeps implementations trivial.
type NamedValidator struct {
	// Name identifies the validator in logs and DeniedError attempts, e.g.
	// "static" or "jwks#2".
	Name string

	// Validator is the implementation.
	Validator Validator
}

// Chain tries validators in configured order and returns the first success. It
// is immutable after construction and therefore safe for concurrent use.
//
// Chain itself satisfies Validator, so a chain can nest inside another chain.
type Chain struct {
	validators []NamedValidator
}

// NewChain builds a Chain over validators, in the order given. Order is
// significant: it is the operator's cheap-to-expensive ordering (a static token
// table before a network round-trip), and the first success short-circuits the
// rest.
//
// A Chain over zero validators is disabled — see Enabled and ErrAuthDisabled.
func NewChain(validators ...NamedValidator) *Chain {
	c := &Chain{}
	if len(validators) > 0 {
		c.validators = make([]NamedValidator, len(validators))
		copy(c.validators, validators)
	}
	return c
}

// Enabled reports whether the chain has any validators. A caller checks this
// once at startup to decide what an unconfigured chain means for it; the package
// takes no position beyond refusing to authenticate.
func (c *Chain) Enabled() bool { return c != nil && len(c.validators) > 0 }

// Names returns the validator names in chain order, for a startup log line that
// tells an operator exactly which validators are live and in what order.
func (c *Chain) Names() []string {
	if c == nil {
		return nil
	}
	names := make([]string, 0, len(c.validators))
	for _, nv := range c.validators {
		names = append(names, nv.Name)
	}
	return names
}

// Validate tries each validator in order and returns the first Principal that
// verifies. If every validator denies, it returns a *DeniedError carrying one
// Attempt per validator so a single log record explains the whole refusal.
//
// A disabled chain (no validators, including a nil *Chain) returns
// ErrAuthDisabled and never a Principal: "not configured" must be
// distinguishable from both "allowed" and "denied", and it is the caller's job
// to decide the policy.
func (c *Chain) Validate(ctx context.Context, r *http.Request) (Principal, error) {
	if !c.Enabled() {
		return Principal{}, ErrAuthDisabled
	}
	denied := &DeniedError{Attempts: make([]Attempt, 0, len(c.validators))}
	for _, nv := range c.validators {
		p, err := nv.Validator.Validate(ctx, r)
		if err != nil {
			denied.add(nv.Name, err)
			continue
		}
		// Defence in depth against a buggy or misconfigured validator: an empty
		// ID would sail through every downstream ownership comparison as a
		// wildcard, so treat it as a denial rather than trust it.
		if p.ID == "" {
			denied.addReason(nv.Name, KindInvalidCredential, "validator returned an empty principal id")
			continue
		}
		return p, nil
	}
	return Principal{}, denied
}

// BearerToken extracts the token from an RFC 6750 "Authorization: Bearer
// <token>" header. It returns "" when the header is absent, uses a different
// scheme, or carries an empty token. The scheme is matched case-insensitively
// because RFC 7235 defines auth schemes as case-insensitive, and real clients
// send "bearer".
//
// It lives here rather than inside the static validator because every
// token-bearing validator needs exactly this extraction.
func BearerToken(r *http.Request) string {
	if r == nil {
		return ""
	}
	const scheme = "bearer"
	h := r.Header.Get("Authorization")
	if len(h) <= len(scheme) || !strings.EqualFold(h[:len(scheme)], scheme) {
		return ""
	}
	// Require a space or tab between scheme and token so "bearertoken" is not
	// read as a bearer credential.
	rest := h[len(scheme):]
	if rest[0] != ' ' && rest[0] != '\t' {
		return ""
	}
	return strings.TrimSpace(rest)
}
