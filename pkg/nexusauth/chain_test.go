package nexusauth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// validatorFunc adapts a function to Validator for tests.
type validatorFunc func(context.Context, *http.Request) (Principal, error)

func (f validatorFunc) Validate(ctx context.Context, r *http.Request) (Principal, error) {
	return f(ctx, r)
}

func newRequest(t *testing.T, authHeader string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/claim", nil)
	if authHeader != "" {
		r.Header.Set("Authorization", authHeader)
	}
	return r
}

func TestChainDisabledWhenEmpty(t *testing.T) {
	c := NewChain()
	if c.Enabled() {
		t.Fatal("chain with no validators reports enabled")
	}
	if names := c.Names(); len(names) != 0 {
		t.Fatalf("want no names, got %v", names)
	}

	p, err := c.Validate(context.Background(), newRequest(t, "Bearer anything"))
	if !errors.Is(err, ErrAuthDisabled) {
		t.Fatalf("want ErrAuthDisabled, got %v", err)
	}
	if p.ID != "" {
		t.Fatalf("disabled chain returned a principal: %+v", p)
	}
	// "Disabled" must not be mistakable for a denial verdict.
	if k := KindOf(err); k != "" {
		t.Fatalf("want no kind for a disabled chain, got %q", k)
	}
}

func TestChainNilReceiverIsDisabled(t *testing.T) {
	var c *Chain
	if c.Enabled() {
		t.Fatal("nil chain reports enabled")
	}
	if _, err := c.Validate(context.Background(), newRequest(t, "")); !errors.Is(err, ErrAuthDisabled) {
		t.Fatalf("want ErrAuthDisabled, got %v", err)
	}
}

func TestChainOrderingSecondSucceeds(t *testing.T) {
	var order []string
	first := validatorFunc(func(context.Context, *http.Request) (Principal, error) {
		order = append(order, "first")
		return Principal{}, NewError(KindInvalidCredential, "not mine", nil)
	})
	second := validatorFunc(func(context.Context, *http.Request) (Principal, error) {
		order = append(order, "second")
		return Principal{ID: "user-2"}, nil
	})
	third := validatorFunc(func(context.Context, *http.Request) (Principal, error) {
		order = append(order, "third")
		return Principal{ID: "user-3"}, nil
	})

	c := NewChain(
		NamedValidator{Name: "first", Validator: first},
		NamedValidator{Name: "second", Validator: second},
		NamedValidator{Name: "third", Validator: third},
	)
	p, err := c.Validate(context.Background(), newRequest(t, "Bearer x"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ID != "user-2" {
		t.Fatalf("want principal from the second validator, got %q", p.ID)
	}
	if strings.Join(order, ",") != "first,second" {
		t.Fatalf("want first then second and no third, got %v", order)
	}
	if names := strings.Join(c.Names(), ","); names != "first,second,third" {
		t.Fatalf("unexpected names %q", names)
	}
}

func TestChainAggregatesEveryDenial(t *testing.T) {
	c := NewChain(
		NamedValidator{Name: "static", Validator: validatorFunc(func(context.Context, *http.Request) (Principal, error) {
			return Principal{}, NewError(KindNoCredential, "no bearer token", nil)
		})},
		NamedValidator{Name: "jwks", Validator: validatorFunc(func(context.Context, *http.Request) (Principal, error) {
			return Principal{}, NewError(KindInvalidCredential, "signature does not verify", nil)
		})},
	)

	_, err := c.Validate(context.Background(), newRequest(t, "Bearer nope"))
	var denied *DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("want *DeniedError, got %T (%v)", err, err)
	}
	if len(denied.Attempts) != 2 {
		t.Fatalf("want 2 attempts, got %+v", denied.Attempts)
	}
	if denied.Attempts[0].Validator != "static" || denied.Attempts[1].Validator != "jwks" {
		t.Fatalf("attempts not in chain order: %+v", denied.Attempts)
	}
	// A rejection outranks a missing credential in the aggregate kind.
	if got := denied.Kind(); got != KindInvalidCredential {
		t.Fatalf("want aggregate kind %q, got %q", KindInvalidCredential, got)
	}

	// One line must name every validator and every reason.
	msg := err.Error()
	for _, want := range []string{"static", "no bearer token", "jwks", "signature does not verify"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("aggregate message %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "\n") {
		t.Fatalf("aggregate message must be single-line, got %q", msg)
	}
}

func TestDeniedErrorLogsAsOneRecord(t *testing.T) {
	c := NewChain(
		NamedValidator{Name: "static", Validator: validatorFunc(func(context.Context, *http.Request) (Principal, error) {
			return Principal{}, NewError(KindInvalidCredential, "unknown token", nil)
		})},
		NamedValidator{Name: "proxy_headers", Validator: validatorFunc(func(context.Context, *http.Request) (Principal, error) {
			return Principal{}, NewError(KindNoCredential, "no principal header", nil)
		})},
	)
	_, err := c.Validate(context.Background(), newRequest(t, "Bearer nope"))

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Warn("auth denied", slog.Any("denial", err))

	out := buf.String()
	if lines := strings.Count(strings.TrimSpace(out), "\n"); lines != 0 {
		t.Fatalf("want a single log record, got %d newlines: %s", lines, out)
	}
	for _, want := range []string{"denial.kind=invalid_credential", "static", "unknown token", "proxy_headers", "no principal header"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log record %q missing %q", out, want)
		}
	}
}

func TestChainRejectsEmptyPrincipalID(t *testing.T) {
	c := NewChain(NamedValidator{Name: "buggy", Validator: validatorFunc(func(context.Context, *http.Request) (Principal, error) {
		return Principal{Tenant: "acme"}, nil // success with no ID
	})})
	p, err := c.Validate(context.Background(), newRequest(t, "Bearer x"))
	if err == nil {
		t.Fatalf("want a denial, got principal %+v", p)
	}
	if got := KindOf(err); got != KindInvalidCredential {
		t.Fatalf("want %q, got %q", KindInvalidCredential, got)
	}
	if !strings.Contains(err.Error(), "empty principal id") {
		t.Fatalf("unhelpful message: %v", err)
	}
}

func TestChainRecordsBareValidatorError(t *testing.T) {
	// A validator that fails for a non-credential reason (a network error in a
	// network-backed validator) must still fail closed and be attributable.
	boom := errors.New("dial tcp: connection refused")
	c := NewChain(NamedValidator{Name: "introspect", Validator: validatorFunc(func(context.Context, *http.Request) (Principal, error) {
		return Principal{}, boom
	})})
	_, err := c.Validate(context.Background(), newRequest(t, "Bearer x"))
	if got := KindOf(err); got != KindInvalidCredential {
		t.Fatalf("want %q, got %q", KindInvalidCredential, got)
	}
	var denied *DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("want *DeniedError, got %T", err)
	}
	if !errors.Is(denied.Attempts[0].Err, boom) {
		t.Fatalf("attempt lost the underlying error: %+v", denied.Attempts[0])
	}
}

func TestChainIsAValidator(t *testing.T) {
	// A Chain must be usable wherever a Validator is, so chains can nest.
	var _ Validator = NewChain()
}

func TestBearerToken(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
	}{
		{"absent", "", ""},
		{"canonical", "Bearer abc123", "abc123"},
		{"lowercase scheme", "bearer abc123", "abc123"},
		{"mixed case scheme", "BeArEr abc123", "abc123"},
		{"extra spaces trimmed", "Bearer   abc123  ", "abc123"},
		{"tab separator", "Bearer\tabc123", "abc123"},
		{"scheme only", "Bearer", ""},
		{"scheme with no token", "Bearer ", ""},
		{"other scheme", "Basic abc123", ""},
		{"no separator", "Bearerabc123", ""},
		{"short header", "abc", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BearerToken(newRequest(t, tc.header)); got != tc.want {
				t.Fatalf("BearerToken(%q) = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
	if got := BearerToken(nil); got != "" {
		t.Fatalf("BearerToken(nil) = %q, want empty", got)
	}
}
