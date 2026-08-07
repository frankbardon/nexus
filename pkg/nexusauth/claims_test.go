package nexusauth

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseScopesAcceptsStringAndArray(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []string
	}{
		{"nil", nil, nil},
		{"space delimited", "leases.read leases.write", []string{"leases.read", "leases.write"}},
		{"single scope", "admin", []string{"admin"}},
		{"extra whitespace", "  leases.read \t leases.write  ", []string{"leases.read", "leases.write"}},
		{"empty string", "   ", nil},
		{"decoded array", []any{"leases.read", "leases.write"}, []string{"leases.read", "leases.write"}},
		{"array with blanks", []any{"admin", "", "  "}, []string{"admin"}},
		{"string slice", []string{"admin", " ops "}, []string{"admin", "ops"}},
		{"empty array", []any{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseScopes(tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseScopes(%#v) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseScopesRejectsOtherShapes(t *testing.T) {
	for _, in := range []any{42, map[string]any{"a": 1}, []any{true}} {
		if _, err := ParseScopes(in); err == nil {
			t.Fatalf("ParseScopes(%#v) accepted an unusable shape", in)
		}
	}
}

func TestClaimMappingPrincipal(t *testing.T) {
	m := ClaimMapping{PrincipalClaim: "sub", TenantClaim: "org_id", ScopesClaim: "scope"}

	// The OAuth 2.0 space-delimited form.
	p, err := m.Principal(map[string]any{"sub": "user-1", "org_id": "acme", "scope": "leases.read leases.write"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ID != "user-1" || p.Tenant != "acme" {
		t.Fatalf("unexpected principal: %+v", p)
	}
	if !reflect.DeepEqual(p.Scopes, []string{"leases.read", "leases.write"}) {
		t.Fatalf("unexpected scopes: %#v", p.Scopes)
	}
	if p.Claims["sub"] != "user-1" {
		t.Fatalf("raw claims not carried for audit: %#v", p.Claims)
	}

	// The array form, as a JSON-decoded token body delivers it.
	var claims map[string]any
	if err := json.Unmarshal([]byte(`{"sub":"user-2","scope":["a","b"]}`), &claims); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p, err = m.Principal(claims)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ID != "user-2" || !reflect.DeepEqual(p.Scopes, []string{"a", "b"}) {
		t.Fatalf("unexpected principal: %+v", p)
	}
	if p.Tenant != "" {
		t.Fatalf("absent tenant claim must leave Tenant empty, got %q", p.Tenant)
	}
}

func TestClaimMappingNumericPrincipal(t *testing.T) {
	m := ClaimMapping{PrincipalClaim: "uid"}
	for _, raw := range []any{float64(1234), json.Number("1234"), 1234, int64(1234)} {
		p, err := m.Principal(map[string]any{"uid": raw})
		if err != nil {
			t.Fatalf("%T: unexpected error: %v", raw, err)
		}
		if p.ID != "1234" {
			t.Fatalf("%T: ID = %q, want \"1234\"", raw, p.ID)
		}
	}
}

func TestClaimMappingRejectsUnusableCredentials(t *testing.T) {
	m := ClaimMapping{PrincipalClaim: "sub", TenantClaim: "org", ScopesClaim: "scope"}
	cases := []struct {
		name   string
		claims map[string]any
		want   string
	}{
		{"missing principal claim", map[string]any{"other": "x"}, `has no "sub" claim`},
		{"empty principal claim", map[string]any{"sub": ""}, `claim "sub" is empty`},
		{"non-scalar principal", map[string]any{"sub": []any{"a"}}, "not a usable principal id"},
		{"unusable tenant", map[string]any{"sub": "u", "org": []any{"a"}}, "not a usable tenant"},
		{"unusable scopes", map[string]any{"sub": "u", "scope": 7}, "not a usable scope set"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.Principal(tc.claims)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := KindOf(err); got != KindInvalidCredential {
				t.Fatalf("want %q, got %q (%v)", KindInvalidCredential, got, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("message %q missing %q", err.Error(), tc.want)
			}
		})
	}
}

func TestClaimMappingUnconfiguredPrincipalClaimIsAnOperatorError(t *testing.T) {
	var m ClaimMapping
	if !m.IsZero() {
		t.Fatal("zero mapping does not report IsZero")
	}
	_, err := m.Principal(map[string]any{"sub": "user-1"})
	if err == nil {
		t.Fatal("want an error")
	}
	// Misconfiguration is not a verdict about the caller's credential.
	if got := KindOf(err); got != "" {
		t.Fatalf("want no denial kind, got %q", got)
	}
	if !strings.Contains(err.Error(), "principal_claim is not configured") {
		t.Fatalf("unhelpful message: %v", err)
	}
}
