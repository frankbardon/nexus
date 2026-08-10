package agui

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/frankbardon/nexus/pkg/engine"
)

// Nothing in this file is a checked-in credential. The RSA key, the JWKS
// document and every token are generated at run time — a fixture key looks like
// a leaked secret to a scanner, and a fixture token would pin the test to a
// wall-clock instant.

// startPluginWithConfig boots the plugin against a real bus with cfg, waits for
// the listener, and returns the endpoint URL. It is the seam these tests need
// that newTestPlugin does not expose: the auth surface is decided entirely by
// Init's reading of the config map.
func startPluginWithConfig(t *testing.T, cfg map[string]any) string {
	t.Helper()
	addr := freeAddr(t)
	cfg["bind"] = addr

	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Config: cfg,
		Bus:    engine.NewEventBus(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := p.Ready(); err != nil {
		t.Fatalf("ready: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(t.Context()) })

	url := "http://" + addr + agentPath
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodOptions, url, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			return url
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("server did not come up")
	return ""
}

// initConfig runs Init alone (no listener) and returns its error, for the config
// validation cases.
func initConfig(t *testing.T, cfg map[string]any) error {
	t.Helper()
	cfg["bind"] = freeAddr(t)
	p := New().(*Plugin)
	return p.Init(engine.PluginContext{
		Config: cfg,
		Bus:    engine.NewEventBus(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// TestAuthInlineTokenPath asserts the `bearer_token` key still gates the
// endpoint end to end after the switch to the shared chain.
func TestAuthInlineTokenPath(t *testing.T) {
	url := startPluginWithConfig(t, map[string]any{"bearer_token": "inline-secret"})

	resp := post(t, url, "inline-secret", "", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid-token status = %d, want 200", resp.StatusCode)
	}

	resp = post(t, url, "nope", "", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-token status = %d, want 401", resp.StatusCode)
	}
}

// TestAuthEnvTokenPath asserts `bearer_token_env` names an environment variable
// that is read at Init, unchanged.
func TestAuthEnvTokenPath(t *testing.T) {
	t.Setenv("AGUI_TEST_TOKEN", "from-env")
	url := startPluginWithConfig(t, map[string]any{"bearer_token_env": "AGUI_TEST_TOKEN"})

	resp := post(t, url, "from-env", "", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("env-token status = %d, want 200", resp.StatusCode)
	}

	resp = post(t, url, "", "", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", resp.StatusCode)
	}
}

// TestAuthInlineTokenWinsOverEnv pins today's precedence: an inline token wins
// and the env var is not consulted at all.
func TestAuthInlineTokenWinsOverEnv(t *testing.T) {
	t.Setenv("AGUI_TEST_TOKEN", "from-env")
	url := startPluginWithConfig(t, map[string]any{
		"bearer_token":     "inline-secret",
		"bearer_token_env": "AGUI_TEST_TOKEN",
	})

	resp := post(t, url, "inline-secret", "", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("inline-token status = %d, want 200", resp.StatusCode)
	}

	resp = post(t, url, "from-env", "", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("env-token status = %d, want 401 (inline must win)", resp.StatusCode)
	}
}

// TestAuthDisabledByDefault asserts that no token and no `auth:` block still
// means every request is admitted, exactly as before.
func TestAuthDisabledByDefault(t *testing.T) {
	url := startPluginWithConfig(t, map[string]any{})

	resp := post(t, url, "", "", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("no-auth status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "" {
		t.Fatalf("WWW-Authenticate = %q, want none when auth is disabled", got)
	}
}

// TestAuthChallengeDistinguishesMissingFromInvalid asserts the RFC 6750
// challenge tells a client which of the two 401s it got, which is the signal a
// browser front-end branches on.
func TestAuthChallengeDistinguishesMissingFromInvalid(t *testing.T) {
	url := startPluginWithConfig(t, map[string]any{"bearer_token": "secret"})

	resp := post(t, url, "", "", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="nexus-agui"` {
		t.Fatalf("no-credential challenge = %q", got)
	}

	resp = post(t, url, "wrong", "", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, `error="invalid_token"`) {
		t.Fatalf("invalid-credential challenge = %q, want error=\"invalid_token\"", got)
	}
}

// TestAuthBlockAndBearerTokenAreMutuallyExclusive asserts the defined outcome
// for configuring both spellings: a boot error naming both keys, not a silent
// precedence rule.
func TestAuthBlockAndBearerTokenAreMutuallyExclusive(t *testing.T) {
	cases := map[string]map[string]any{
		"inline": {
			"bearer_token": "secret",
			"auth": map[string]any{
				"validators": []any{map[string]any{
					"type":   "static",
					"tokens": []any{map[string]any{"token": "t", "principal": "p"}},
				}},
			},
		},
		"env": {
			"bearer_token_env": "AGUI_TEST_TOKEN_UNSET",
			"auth": map[string]any{
				"validators": []any{map[string]any{
					"type":   "static",
					"tokens": []any{map[string]any{"token": "t", "principal": "p"}},
				}},
			},
		},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			err := initConfig(t, cfg)
			if err == nil {
				t.Fatal("Init accepted both auth: and bearer_token*, want an error")
			}
			for _, want := range []string{"auth", "bearer_token"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

// TestAuthBlockRejectsUnknownKeys asserts the shared parser's
// additionalProperties:false posture reaches this host too — including
// admin_scope, which is a broker-owned key and not part of the chain config.
func TestAuthBlockRejectsUnknownKeys(t *testing.T) {
	err := initConfig(t, map[string]any{
		"auth": map[string]any{"admin_scope": "nexus.broker.admin"},
	})
	if err == nil {
		t.Fatal("Init accepted an unknown auth key, want an error")
	}
	if !strings.Contains(err.Error(), "admin_scope") {
		t.Fatalf("error %q does not name the offending key", err)
	}
}

// TestAuthBlockStaticValidator asserts an `auth:` block with a static validator
// gates the endpoint, so the block is wired and not merely parsed.
func TestAuthBlockStaticValidator(t *testing.T) {
	url := startPluginWithConfig(t, map[string]any{
		"auth": map[string]any{
			"validators": []any{map[string]any{
				"type": "static",
				"tokens": []any{map[string]any{
					"token":     "chain-token",
					"principal": "ci-runner",
				}},
			}},
		},
	})

	resp := post(t, url, "chain-token", "", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid-token status = %d, want 200", resp.StatusCode)
	}

	resp = post(t, url, "nope", "", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-token status = %d, want 401", resp.StatusCode)
	}
}

// TestAuthBlockJWKS is the payoff of the story: an `auth:` block with a `jwks`
// validator accepts a token minted by a fake OIDC issuer, with no AG-UI-specific
// code involved.
func TestAuthBlockJWKS(t *testing.T) {
	const (
		issuer   = "https://issuer.example/"
		audience = "nexus-agui"
	)
	idp := newTestIDP(t)

	url := startPluginWithConfig(t, map[string]any{
		"auth": map[string]any{
			"validators": []any{map[string]any{
				"type":            "jwks",
				"issuer":          issuer,
				"jwks_url":        idp.url(),
				"audience":        audience,
				"algorithms":      []any{"RS256"},
				"principal_claim": "sub",
				"scopes_claim":    "scope",
			}},
		},
	})

	token := idp.mint(t, jwt.MapClaims{
		"iss":   issuer,
		"aud":   audience,
		"sub":   "user-42",
		"scope": "agui.run",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})

	resp := post(t, url, token, "", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("issuer-minted token status = %d, want 200", resp.StatusCode)
	}

	// A token for another audience must not pass: the validator, not this
	// transport, is what enforces that.
	wrongAud := idp.mint(t, jwt.MapClaims{
		"iss": issuer,
		"aud": "someone-else",
		"sub": "user-42",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	resp = post(t, url, wrongAud, "", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-audience status = %d, want 401", resp.StatusCode)
	}

	resp = post(t, url, "not-a-jwt", "", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("garbage-token status = %d, want 401", resp.StatusCode)
	}
}

// TestAuthUnavailableMapsTo503 asserts the fourth denial kind reaches the wire
// as a 503 with a Retry-After and no challenge — an identity provider outage
// must not read to a client as "re-authenticate".
func TestAuthUnavailableMapsTo503(t *testing.T) {
	// A server that is closed immediately: the address is well-formed and
	// loopback (so config validation accepts it) but nothing answers.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL + "/introspect"
	dead.Close()

	url := startPluginWithConfig(t, map[string]any{
		"auth": map[string]any{
			"validators": []any{map[string]any{
				"type":              "introspect",
				"introspection_url": deadURL,
				"client_id":         "nexus-agui",
				"client_secret":     "shh",
				"principal_claim":   "sub",
				"http_timeout":      "1s",
			}},
		},
	})

	resp := post(t, url, "some-opaque-token", "", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unreachable-introspection status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Error("503 carried no Retry-After")
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "" {
		t.Errorf("503 carried a challenge %q, want none", got)
	}
}

// --- fake OIDC issuer -------------------------------------------------------

// testIDP is a minimal JWKS endpoint over a runtime-generated RSA key.
type testIDP struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	kid    string
}

func newTestIDP(t *testing.T) *testIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	idp := &testIDP{key: key, kid: "test-key-1"}
	idp.server = httptest.NewServer(http.HandlerFunc(idp.serve))
	t.Cleanup(idp.server.Close)
	return idp
}

// url is the JWKS endpoint. httptest listens on loopback, the one case the
// validator lets through over plain http.
func (s *testIDP) url() string { return s.server.URL + "/jwks" }

func (s *testIDP) serve(w http.ResponseWriter, _ *http.Request) {
	doc := map[string]any{"keys": []map[string]any{{
		"kty": "RSA",
		"kid": s.kid,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(s.key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(s.key.E)).Bytes()),
	}}}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

// mint signs claims with the published key.
func (s *testIDP) mint(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.key)
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	return signed
}
