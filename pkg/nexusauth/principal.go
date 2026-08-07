package nexusauth

// Principal is a verified caller identity. It is a value type: validators hand
// back a fully populated copy, so a caller can stash or mutate it without
// touching the validator's own configuration.
//
// ID is the only field authorization ever compares — lease ownership is
// principal-id equality and nothing else. Tenant, Scopes and Claims exist for
// policy decisions layered on top (a scope check, a tenant-scoped listing) and
// for audit records; nothing in the package keys on them.
type Principal struct {
	// ID is the stable identifier of the caller. It must be non-empty for a
	// validation to count as a success; the Chain rejects an empty ID rather
	// than pass an anonymous principal upstream.
	ID string

	// Tenant is an optional organization/workspace identifier. Empty when the
	// credential carries no tenant notion.
	Tenant string

	// Scopes are the granted scopes, already split into individual tokens. Nil
	// when the credential carries none.
	Scopes []string

	// Claims is the raw claim set the identity was derived from, kept for audit
	// and for policy that needs a claim the Principal does not model. Nil for
	// credentials with no claim structure (e.g. a static token).
	Claims map[string]any
}

// HasScope reports whether the principal was granted the exact scope s. The
// comparison is case-sensitive: scopes are opaque identifiers, and folding case
// would make "Admin" silently equivalent to "admin".
func (p Principal) HasScope(s string) bool {
	for _, got := range p.Scopes {
		if got == s {
			return true
		}
	}
	return false
}

// clone deep-copies the reference-typed fields so a validator can return a
// Principal derived from its own long-lived configuration without handing the
// caller a slice or map it could mutate under a concurrent request.
func (p Principal) clone() Principal {
	out := p
	if p.Scopes != nil {
		out.Scopes = make([]string, len(p.Scopes))
		copy(out.Scopes, p.Scopes)
	}
	if p.Claims != nil {
		out.Claims = make(map[string]any, len(p.Claims))
		for k, v := range p.Claims {
			out.Claims[k] = v
		}
	}
	return out
}
