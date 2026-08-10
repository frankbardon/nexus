package nexusauth

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ClaimMapping names the claims a token-based validator reads to build a
// Principal. Nexus targets generic OIDC and ships no provider-specific defaults:
// an operator states which claim carries the principal id, which carries the
// tenant, and which carries the scopes. An empty field means "not configured",
// and the consuming validator decides whether that is fatal or simply means the
// resulting Principal field stays empty.
//
// The mapping is parsed and carried for every validator entry even though only
// the claim-bearing validators consume it, so the config surface is uniform.
type ClaimMapping struct {
	// PrincipalClaim is the claim whose value becomes Principal.ID.
	PrincipalClaim string

	// TenantClaim is the claim whose value becomes Principal.Tenant. Optional.
	TenantClaim string

	// ScopesClaim is the claim whose value becomes Principal.Scopes. Its value
	// may be either a space-delimited string (the OAuth 2.0 "scope" convention)
	// or an array of strings — see ParseScopes.
	ScopesClaim string
}

// IsZero reports whether no claim names are configured at all.
func (m ClaimMapping) IsZero() bool {
	return m.PrincipalClaim == "" && m.TenantClaim == "" && m.ScopesClaim == ""
}

// Principal projects a verified claim set onto a Principal using the mapping.
// The full claim set is carried on Principal.Claims for audit regardless of which
// claims the mapping names.
//
// It returns an *Error of kind KindInvalidCredential when the credential itself
// is at fault (the principal claim is missing, empty, or not a scalar), and a
// plain error when the mapping is unusable (no principal claim configured) —
// those are an operator's bug, not a caller's.
func (m ClaimMapping) Principal(claims map[string]any) (Principal, error) {
	if m.PrincipalClaim == "" {
		return Principal{}, fmt.Errorf("nexusauth: claim mapping: principal_claim is not configured")
	}

	raw, ok := claims[m.PrincipalClaim]
	if !ok {
		return Principal{}, NewError(KindInvalidCredential,
			fmt.Sprintf("credential has no %q claim", m.PrincipalClaim), nil)
	}
	id, err := claimString(raw)
	if err != nil {
		return Principal{}, NewError(KindInvalidCredential,
			fmt.Sprintf("claim %q is not a usable principal id", m.PrincipalClaim), err)
	}
	if id == "" {
		return Principal{}, NewError(KindInvalidCredential,
			fmt.Sprintf("claim %q is empty", m.PrincipalClaim), nil)
	}

	p := Principal{ID: id, Claims: claims}

	// A missing tenant or scopes claim is not an error — the credential simply
	// carries no tenant or grants no scopes. A *present but unusable* value is
	// an error, because silently dropping it would weaken any policy layered on
	// top of it.
	if m.TenantClaim != "" {
		if raw, ok := claims[m.TenantClaim]; ok {
			tenant, err := claimString(raw)
			if err != nil {
				return Principal{}, NewError(KindInvalidCredential,
					fmt.Sprintf("claim %q is not a usable tenant", m.TenantClaim), err)
			}
			p.Tenant = tenant
		}
	}
	if m.ScopesClaim != "" {
		if raw, ok := claims[m.ScopesClaim]; ok {
			scopes, err := ParseScopes(raw)
			if err != nil {
				return Principal{}, NewError(KindInvalidCredential,
					fmt.Sprintf("claim %q is not a usable scope set", m.ScopesClaim), err)
			}
			p.Scopes = scopes
		}
	}
	return p.Clone(), nil
}

// ParseScopes normalizes a scope set into individual scopes. It accepts the two
// shapes real credentials and configs use:
//
//   - a space-delimited string, per OAuth 2.0's "scope" parameter
//     ("leases.read leases.write")
//   - an array of strings, as JSON- or YAML-decoded ([]any of string)
//
// nil yields nil. Blank entries are dropped so trailing separators and empty
// YAML list items do not produce phantom scopes.
func ParseScopes(v any) ([]string, error) {
	switch value := v.(type) {
	case nil:
		return nil, nil
	case string:
		return splitScopes(value), nil
	case []string:
		out := make([]string, 0, len(value))
		for _, s := range value {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return nil, nil
		}
		return out, nil
	case []any:
		out := make([]string, 0, len(value))
		for i, elem := range value {
			s, err := claimString(elem)
			if err != nil {
				return nil, fmt.Errorf("scope %d: %w", i, err)
			}
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return nil, nil
		}
		return out, nil
	default:
		return nil, fmt.Errorf("want a space-delimited string or a list of strings, got %T", v)
	}
}

// splitScopes splits a space-delimited scope string. strings.Fields also folds
// tabs and newlines, which is the tolerant reading a hand-written config wants.
func splitScopes(s string) []string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// claimString coerces a decoded claim value to a string. Claims decode from JSON
// or YAML, so a numeric identifier arrives as float64 or json.Number depending on
// the decoder — both are accepted rather than rejected as "not a string", since
// some issuers really do mint numeric subjects.
func claimString(v any) (string, error) {
	switch value := v.(type) {
	case string:
		return value, nil
	case json.Number:
		return value.String(), nil
	case float64:
		// Formatted with 'f' and the shortest exact representation so a whole
		// number does not come back as "1.234e+06".
		return strconv.FormatFloat(value, 'f', -1, 64), nil
	case int:
		return strconv.Itoa(value), nil
	case int64:
		return strconv.FormatInt(value, 10), nil
	default:
		return "", fmt.Errorf("want a string, got %T", v)
	}
}
