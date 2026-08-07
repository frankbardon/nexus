package nexusauth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestStaticValidatorSuccess(t *testing.T) {
	v, err := NewStaticValidator([]StaticToken{
		{Token: "tok-a", Principal: Principal{ID: "ops-cli", Tenant: "acme", Scopes: []string{"leases.read"}}},
		{Token: "tok-b", Principal: Principal{ID: "ci"}},
	})
	if err != nil {
		t.Fatalf("NewStaticValidator: %v", err)
	}

	p, err := v.Validate(context.Background(), newRequest(t, "Bearer tok-a"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ID != "ops-cli" || p.Tenant != "acme" {
		t.Fatalf("unexpected principal: %+v", p)
	}
	if !p.HasScope("leases.read") {
		t.Fatalf("scopes not carried: %+v", p.Scopes)
	}

	// A later entry must be reachable — the table is scanned in full.
	p, err = v.Validate(context.Background(), newRequest(t, "Bearer tok-b"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ID != "ci" {
		t.Fatalf("unexpected principal: %+v", p)
	}
}

func TestStaticValidatorReturnsIsolatedPrincipal(t *testing.T) {
	scopes := []string{"admin"}
	v, err := NewStaticValidator([]StaticToken{
		{Token: "tok", Principal: Principal{ID: "ops", Scopes: scopes}},
	})
	if err != nil {
		t.Fatalf("NewStaticValidator: %v", err)
	}
	// Mutating the caller's original slice must not reach the validator.
	scopes[0] = "hacked"

	p, err := v.Validate(context.Background(), newRequest(t, "Bearer tok"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !p.HasScope("admin") {
		t.Fatalf("validator aliased its config: %+v", p.Scopes)
	}
	// Mutating the returned principal must not reach the next request either.
	p.Scopes[0] = "hacked"
	again, err := v.Validate(context.Background(), newRequest(t, "Bearer tok"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !again.HasScope("admin") {
		t.Fatalf("returned principal aliased validator state: %+v", again.Scopes)
	}
}

func TestStaticValidatorUnknownToken(t *testing.T) {
	v, err := NewStaticValidator([]StaticToken{{Token: "tok", Principal: Principal{ID: "ops"}}})
	if err != nil {
		t.Fatalf("NewStaticValidator: %v", err)
	}
	p, err := v.Validate(context.Background(), newRequest(t, "Bearer wrong"))
	if err == nil {
		t.Fatalf("want a denial, got %+v", p)
	}
	if got := KindOf(err); got != KindInvalidCredential {
		t.Fatalf("want %q, got %q", KindInvalidCredential, got)
	}
	if !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("errors.Is(ErrInvalidCredential) failed for %v", err)
	}
	// The rejection must never echo the presented credential.
	if strings.Contains(err.Error(), "wrong") {
		t.Fatalf("denial leaked the presented token: %v", err)
	}
}

func TestStaticValidatorNoCredential(t *testing.T) {
	v, err := NewStaticValidator([]StaticToken{{Token: "tok", Principal: Principal{ID: "ops"}}})
	if err != nil {
		t.Fatalf("NewStaticValidator: %v", err)
	}
	for _, header := range []string{"", "Basic tok", "Bearer "} {
		_, err := v.Validate(context.Background(), newRequest(t, header))
		if got := KindOf(err); got != KindNoCredential {
			t.Fatalf("header %q: want %q, got %q (%v)", header, KindNoCredential, got, err)
		}
		if !errors.Is(err, ErrNoCredential) {
			t.Fatalf("header %q: errors.Is(ErrNoCredential) failed for %v", header, err)
		}
	}
}

func TestNewStaticValidatorRejectsBadTables(t *testing.T) {
	cases := []struct {
		name    string
		tokens  []StaticToken
		wantErr string
	}{
		{"empty table", nil, "at least one token"},
		{"missing token", []StaticToken{{Principal: Principal{ID: "ops"}}}, "token must not be empty"},
		{"missing principal", []StaticToken{{Token: "tok"}}, "principal must not be empty"},
		{
			"duplicate token",
			[]StaticToken{
				{Token: "tok", Principal: Principal{ID: "a"}},
				{Token: "tok", Principal: Principal{ID: "b"}},
			},
			"duplicate token",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewStaticValidator(tc.tokens); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
