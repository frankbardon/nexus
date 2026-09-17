package nexuscreds

import (
	"context"
	"errors"
	"testing"
)

// staticSource is the minimum thing that satisfies Source: a fixed token and a
// fixed error. It stands in for a real source in the registry tests too.
type staticSource struct {
	token string
	err   error
}

func (s staticSource) Token(context.Context) (string, error) { return s.token, s.err }

// Compile-time assertions that the shipped shapes still match the interface and
// the factory signature. A change to either breaks here first, with a clearer
// message than a caller would produce.
var (
	_ Source  = staticSource{}
	_ Factory = func(map[string]any) (Source, error) { return staticSource{}, nil }
)

// ctxSource reports the caller's context back, so a test can prove Token is
// given the caller's own context and not a detached one.
type ctxSource struct{}

func (ctxSource) Token(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "live", nil
}

func TestSource_TokenHonoursCallerContext(t *testing.T) {
	var src Source = ctxSource{}

	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token with a live context: unexpected error: %v", err)
	}
	if tok != "live" {
		t.Fatalf("Token = %q, want %q", tok, "live")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.Token(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Token with a cancelled context: err = %v, want context.Canceled", err)
	}
}

func TestSource_TokenErrorPropagates(t *testing.T) {
	want := errors.New("token endpoint refused")
	var src Source = staticSource{err: want}

	tok, err := src.Token(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("Token err = %v, want %v", err, want)
	}
	if tok != "" {
		t.Fatalf("Token returned %q alongside an error, want the empty string", tok)
	}
}

func TestFactory_ConstructsSourceFromConfig(t *testing.T) {
	var f Factory = func(cfg map[string]any) (Source, error) {
		tok, _ := cfg["token"].(string)
		return staticSource{token: tok}, nil
	}

	src, err := f(map[string]any{"token": "from-config"})
	if err != nil {
		t.Fatalf("factory: unexpected error: %v", err)
	}
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: unexpected error: %v", err)
	}
	if tok != "from-config" {
		t.Fatalf("Token = %q, want %q", tok, "from-config")
	}
}

func TestFactory_ErrorPropagates(t *testing.T) {
	want := errors.New("bad credentials block")
	var f Factory = func(map[string]any) (Source, error) { return nil, want }

	src, err := f(nil)
	if !errors.Is(err, want) {
		t.Fatalf("factory err = %v, want %v", err, want)
	}
	if src != nil {
		t.Fatalf("factory returned a Source alongside an error: %#v", src)
	}
}
