package nexusauth

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// statusFor is the mapping a caller is expected to apply. It lives in the test
// rather than the package because the package deliberately ships no HTTP layer.
func statusFor(err error) int {
	switch KindOf(err) {
	case KindInsufficientScope:
		return http.StatusForbidden
	case KindNoCredential, KindInvalidCredential:
		return http.StatusUnauthorized
	default:
		return http.StatusInternalServerError
	}
}

func TestErrorKindMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantKind   Kind
		wantIs     error
		wantStatus int
	}{
		{
			"no credential",
			NewError(KindNoCredential, "no bearer token", nil),
			KindNoCredential, ErrNoCredential, http.StatusUnauthorized,
		},
		{
			"invalid credential",
			NewError(KindInvalidCredential, "token expired", nil),
			KindInvalidCredential, ErrInvalidCredential, http.StatusUnauthorized,
		},
		{
			"insufficient scope",
			NewError(KindInsufficientScope, "requires scope leases.admin", nil),
			KindInsufficientScope, ErrInsufficientScope, http.StatusForbidden,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := KindOf(tc.err); got != tc.wantKind {
				t.Fatalf("KindOf = %q, want %q", got, tc.wantKind)
			}
			if !errors.Is(tc.err, tc.wantIs) {
				t.Fatalf("errors.Is(%v, %v) = false", tc.err, tc.wantIs)
			}
			if got := statusFor(tc.err); got != tc.wantStatus {
				t.Fatalf("status = %d, want %d", got, tc.wantStatus)
			}
			// Wrapping must not lose the kind — callers add context routinely.
			wrapped := fmt.Errorf("claim: %w", tc.err)
			if got := KindOf(wrapped); got != tc.wantKind {
				t.Fatalf("KindOf(wrapped) = %q, want %q", got, tc.wantKind)
			}
		})
	}
}

func TestErrorKindsAreDistinct(t *testing.T) {
	// A kind must not satisfy another kind's sentinel, or a caller mapping to a
	// status code would answer 401 where it owes 403.
	noCred := NewError(KindNoCredential, "nothing presented", nil)
	if errors.Is(noCred, ErrInsufficientScope) || errors.Is(noCred, ErrInvalidCredential) {
		t.Fatalf("no-credential error matched a foreign sentinel")
	}
	scope := NewError(KindInsufficientScope, "needs admin", nil)
	if errors.Is(scope, ErrNoCredential) || errors.Is(scope, ErrInvalidCredential) {
		t.Fatalf("insufficient-scope error matched a foreign sentinel")
	}
}

func TestErrorUnwrapsCause(t *testing.T) {
	cause := errors.New("x509: certificate signed by unknown authority")
	err := NewError(KindInvalidCredential, "jwks fetch failed", cause)
	if !errors.Is(err, cause) {
		t.Fatalf("cause not reachable through Unwrap")
	}
	if !strings.Contains(err.Error(), "jwks fetch failed") || !strings.Contains(err.Error(), "unknown authority") {
		t.Fatalf("message dropped detail: %v", err)
	}
}

func TestDeniedErrorKindPrecedence(t *testing.T) {
	cases := []struct {
		name  string
		kinds []Kind
		want  Kind
	}{
		{"all missing", []Kind{KindNoCredential, KindNoCredential}, KindNoCredential},
		{"rejection outranks missing", []Kind{KindNoCredential, KindInvalidCredential}, KindInvalidCredential},
		{"scope outranks rejection", []Kind{KindInvalidCredential, KindInsufficientScope}, KindInsufficientScope},
		{"scope outranks missing", []Kind{KindNoCredential, KindInsufficientScope}, KindInsufficientScope},
		{"empty fails closed", nil, KindInvalidCredential},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &DeniedError{}
			for i, k := range tc.kinds {
				d.add(fmt.Sprintf("v%d", i), NewError(k, "because", nil))
			}
			if got := d.Kind(); got != tc.want {
				t.Fatalf("Kind = %q, want %q", got, tc.want)
			}
			if s := tc.want.sentinel(); !errors.Is(d, s) {
				t.Fatalf("errors.Is(%v, %v) = false", d, s)
			}
		})
	}
}

func TestKindOfNonAuthErrors(t *testing.T) {
	if got := KindOf(nil); got != "" {
		t.Fatalf("KindOf(nil) = %q, want empty", got)
	}
	if got := KindOf(errors.New("boom")); got != "" {
		t.Fatalf("KindOf(plain) = %q, want empty", got)
	}
	// A disabled chain is a deployment state, not a verdict: it must not map to
	// a 401/403 by accident.
	if got := KindOf(ErrAuthDisabled); got != "" {
		t.Fatalf("KindOf(ErrAuthDisabled) = %q, want empty", got)
	}
	if got := statusFor(ErrAuthDisabled); got != http.StatusInternalServerError {
		t.Fatalf("statusFor(ErrAuthDisabled) = %d, want a non-auth status", got)
	}
}

func TestAttemptString(t *testing.T) {
	a := Attempt{Validator: "jwks", Kind: KindInvalidCredential, Reason: "audience mismatch"}
	if got, want := a.String(), "jwks: invalid_credential: audience mismatch"; got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
	b := Attempt{Validator: "static", Kind: KindNoCredential}
	if got, want := b.String(), "static: no_credential"; got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
}

func TestDeniedErrorAddFallsBackToCauseText(t *testing.T) {
	// A typed error with no reason still has to say something useful.
	d := &DeniedError{}
	d.add("introspect", NewError(KindInvalidCredential, "", errors.New("502 bad gateway")))
	if got := d.Attempts[0].Reason; got != "502 bad gateway" {
		t.Fatalf("Reason = %q, want the cause text", got)
	}
}
