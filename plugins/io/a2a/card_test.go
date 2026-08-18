package a2a

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
)

// TestAgentCardIsServedAtTheWellKnownPath asserts the discovery contract: the
// configured card comes back, at the spec's path, with caching headers.
func TestAgentCardIsServedAtTheWellKnownPath(t *testing.T) {
	s := newTestServer(t, testConfig(t, map[string]any{
		"public_url": "https://agent.example.test",
	}))

	rec := do(t, s, http.MethodGet, a2a.AgentCardPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != a2a.ContentTypeAgentCard {
		t.Errorf("Content-Type = %q, want %q", ct, a2a.ContentTypeAgentCard)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Error("no ETag; specification section 8.6.1 asks for one")
	}
	if rec.Header().Get("Cache-Control") == "" {
		t.Error("no Cache-Control; specification section 8.6.1 asks for one")
	}

	card, err := a2a.DecodeAgentCard(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("served card does not decode: %v", err)
	}
	if err := a2a.ValidateAgentCard(card); err != nil {
		t.Fatalf("served card does not validate: %v", err)
	}
	if card.Name != "nexus-test" {
		t.Errorf("card name = %q, want the configured name", card.Name)
	}

	// Interfaces are derived from the listener, at the advertised public URL.
	jsonrpc, ok := card.InterfaceFor(a2a.BindingJSONRPC)
	if !ok || jsonrpc != "https://agent.example.test/a2a" {
		t.Errorf("JSONRPC interface = %q, %v", jsonrpc, ok)
	}
	rest, ok := card.InterfaceFor(a2a.BindingHTTPJSON)
	if !ok || rest != "https://agent.example.test/a2a/v1" {
		t.Errorf("HTTP+JSON interface = %q, %v", rest, ok)
	}
	for _, iface := range card.SupportedInterfaces {
		if iface.ProtocolVersion != a2a.ProtocolVersion {
			t.Errorf("interface %q advertises version %q, want %q", iface.URL, iface.ProtocolVersion, a2a.ProtocolVersion)
		}
	}

	// A conditional request is honoured.
	rec = do(t, s, http.MethodGet, a2a.AgentCardPath, func(r *http.Request) {
		r.Header.Set("If-None-Match", etag)
	})
	if rec.Code != http.StatusNotModified {
		t.Errorf("conditional GET status = %d, want 304", rec.Code)
	}
}

// TestAgentCardCapabilitiesAreDerivedNotConfigured pins the honesty rule: while
// no operation is implemented the card must not claim streaming.
func TestAgentCardCapabilitiesAreDerivedNotConfigured(t *testing.T) {
	s := newTestServer(t, testConfig(t, nil))
	rec := do(t, s, http.MethodGet, a2a.AgentCardPath)

	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	caps, _ := doc["capabilities"].(map[string]any)
	if caps == nil {
		t.Fatal("card has no capabilities object")
	}
	for key, want := range map[string]bool{
		"streaming":         operationImplemented(a2a.MethodSendStreamingMessage) || operationImplemented(a2a.MethodSubscribeToTask),
		"pushNotifications": false,
		"extendedAgentCard": false,
	} {
		if caps[key] != want {
			t.Errorf("capabilities.%s = %v, want %v (capabilities are derived from implementedOperations)", key, caps[key], want)
		}
	}
}

// TestSecuritySchemesDeriveFromValidators is the core of the auth-discovery
// contract: what the card advertises is what the chain enforces.
func TestSecuritySchemesDeriveFromValidators(t *testing.T) {
	t.Run("no auth means no schemes", func(t *testing.T) {
		s := newTestServer(t, testConfig(t, nil))
		if len(s.cfg.card.card.SecuritySchemes) != 0 {
			t.Errorf("securitySchemes = %v on an unauthenticated listener", s.cfg.card.card.SecuritySchemes)
		}
	})

	t.Run("bearer_token yields one bearer scheme", func(t *testing.T) {
		s := newTestServer(t, testConfig(t, map[string]any{"bearer_token": "s3cret"}))
		card := s.cfg.card.card
		scheme, ok := card.SecuritySchemes["static"]
		if !ok {
			t.Fatalf("no scheme named after the desugared validator: %v", card.SecuritySchemes)
		}
		if scheme.Kind() != a2a.SecuritySchemeHTTPAuth || scheme.HTTPAuth.Scheme != "Bearer" {
			t.Errorf("scheme = %+v, want an HTTP Bearer scheme", scheme)
		}
		if len(card.SecurityRequirements) != 1 {
			t.Fatalf("securityRequirements = %v, want one alternative", card.SecurityRequirements)
		}
		if _, ok := card.SecurityRequirements[0].Schemes["static"]; !ok {
			t.Errorf("requirement does not name the declared scheme: %v", card.SecurityRequirements[0])
		}
		// The serialized card must survive validation, which cross-checks that
		// every requirement names a declared scheme.
		if err := a2a.ValidateAgentCard(&card); err != nil {
			t.Fatalf("derived card does not validate: %v", err)
		}
	})

	t.Run("jwks yields a JWT bearer scheme naming the issuer", func(t *testing.T) {
		s := newTestServer(t, testConfig(t, map[string]any{
			"auth": map[string]any{"validators": []any{map[string]any{
				"type":            "jwks",
				"issuer":          "https://idp.example.test/",
				"jwks_url":        "https://idp.example.test/.well-known/jwks.json",
				"audience":        "nexus",
				"principal_claim": "sub",
			}}},
		}))
		scheme, ok := s.cfg.card.card.SecuritySchemes["jwks"]
		if !ok {
			t.Fatalf("no jwks scheme: %v", s.cfg.card.card.SecuritySchemes)
		}
		if scheme.HTTPAuth == nil || scheme.HTTPAuth.BearerFormat != "JWT" {
			t.Fatalf("scheme = %+v, want bearerFormat JWT", scheme)
		}
		if !strings.Contains(scheme.HTTPAuth.Description, "https://idp.example.test/") {
			t.Errorf("description does not name the issuer a client must obtain a token from: %q", scheme.HTTPAuth.Description)
		}
	})

	t.Run("proxy_headers advertises nothing", func(t *testing.T) {
		s := newTestServer(t, testConfig(t, map[string]any{
			"auth": map[string]any{"validators": []any{map[string]any{
				"type":                "proxy_headers",
				"trusted_proxy_cidrs": []any{"127.0.0.0/8"},
				"principal_header":    "X-Forwarded-User",
			}}},
		}))
		card := s.cfg.card.card
		if len(card.SecuritySchemes) != 0 {
			t.Errorf("securitySchemes = %v; a proxy-asserted identity is not a client credential", card.SecuritySchemes)
		}
		if len(card.SecurityRequirements) != 0 {
			t.Errorf("securityRequirements = %v", card.SecurityRequirements)
		}
		// Auth is still enforced even though the card advertises no scheme.
		if !s.chain.Enabled() {
			t.Error("the validator chain was not wired")
		}
	})

	t.Run("multiple validators become alternatives", func(t *testing.T) {
		s := newTestServer(t, testConfig(t, map[string]any{
			"auth": map[string]any{"validators": []any{
				map[string]any{
					"type":   "static",
					"tokens": []any{map[string]any{"token": "t", "principal": "p"}},
				},
				map[string]any{
					"type":              "introspect",
					"introspection_url": "https://idp.example.test/introspect",
					"client_id":         "nexus",
					"client_secret":     "shh",
					"principal_claim":   "sub",
				},
			}},
		}))
		card := s.cfg.card.card
		if len(card.SecuritySchemes) != 2 {
			t.Fatalf("securitySchemes = %v, want two", card.SecuritySchemes)
		}
		// Separate requirement entries, because the chain is first-success:
		// satisfying either validator suffices.
		if len(card.SecurityRequirements) != 2 {
			t.Fatalf("securityRequirements = %v, want two alternatives", card.SecurityRequirements)
		}
		for _, req := range card.SecurityRequirements {
			if len(req.Schemes) != 1 {
				t.Errorf("requirement %v names more than one scheme; that would mean AND, not OR", req)
			}
		}
		// The client secret must never reach the public document.
		body, err := a2a.EncodeAgentCard(&card)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if strings.Contains(string(body), "shh") {
			t.Fatalf("the served card leaked a configured secret:\n%s", body)
		}
	})
}

// TestCardFileIsLoadedAndOverlaid pins both halves of the card_file contract:
// the hand-authored content is honoured, and the derived half wins.
func TestCardFileIsLoadedAndOverlaid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "card.json")
	doc := `{
      "name": "from-file",
      "description": "Loaded from disk.",
      "version": "9.9.9",
      "supportedInterfaces": [{"url": "https://lies.example.test", "protocolBinding": "GRPC", "protocolVersion": "0.3"}],
      "capabilities": {"streaming": true, "pushNotifications": true, "extendedAgentCard": true},
      "securitySchemes": {"invented": {"mtlsSecurityScheme": {}}},
      "defaultInputModes": ["text/plain"],
      "defaultOutputModes": ["text/plain"],
      "skills": [{"id": "s", "name": "S", "description": "d", "tags": ["t"]}]
    }`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write card file: %v", err)
	}

	cfg, err := parseConfig(map[string]any{
		"bind":       freeAddr(t),
		"public_url": "https://agent.example.test",
		"card_file":  path,
	})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	served, err := buildCard(cfg)
	if err != nil {
		t.Fatalf("buildCard: %v", err)
	}

	if served.card.Name != "from-file" || served.card.Version != "9.9.9" {
		t.Errorf("hand-authored fields were not honoured: %+v", served.card)
	}
	// The derived half overwrites what the file claimed.
	if len(served.card.SupportedInterfaces) != 2 {
		t.Fatalf("supportedInterfaces = %v, want the two derived interfaces", served.card.SupportedInterfaces)
	}
	if url, _ := served.card.InterfaceFor(a2a.BindingJSONRPC); url != "https://agent.example.test/a2a" {
		t.Errorf("JSONRPC interface = %q; the file's claim should not survive", url)
	}
	if served.card.Capabilities.PushNotifications || served.card.Capabilities.ExtendedAgentCard {
		t.Error("the file's capability claims survived; capabilities must be derived")
	}
	if _, ok := served.card.SecuritySchemes["invented"]; ok {
		t.Error("the file declared a security scheme nothing enforces and it survived")
	}
}

// TestCardFileExpandsHome pins the engine.ExpandPath requirement: a ~-relative
// path is expanded rather than treated as a literal directory name.
func TestCardFileExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory available")
	}
	_, err = parseConfig(map[string]any{
		"bind":      freeAddr(t),
		"card_file": "~/definitely-not-a-real-nexus-a2a-card.json",
	})
	if err == nil {
		t.Fatal("a missing card file was accepted")
	}
	if !strings.Contains(err.Error(), home) {
		t.Errorf("error %q does not mention the expanded home directory %q; the path was not run through engine.ExpandPath", err, home)
	}
	if strings.Contains(err.Error(), "~/") {
		t.Errorf("error %q still carries the unexpanded ~ prefix", err)
	}
}

// TestCardMustBeServable pins the boot-time validation: a card missing a
// required field fails Init, not the first client request.
func TestCardMustBeServable(t *testing.T) {
	t.Run("no skills is rejected while parsing", func(t *testing.T) {
		_, err := parseConfig(testConfig(t, map[string]any{
			"card": map[string]any{"name": "n", "description": "d", "version": "1"},
		}))
		if err == nil {
			t.Fatal("a card with no skills was accepted")
		}
	})

	t.Run("an incomplete skill is rejected while rendering", func(t *testing.T) {
		cfg, err := parseConfig(testConfig(t, map[string]any{
			"card": map[string]any{
				"name": "n", "description": "d", "version": "1",
				"skills": []any{map[string]any{"name": "S", "description": "d"}},
			},
		}))
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if _, err := buildCard(cfg); err == nil {
			t.Fatal("a skill with no id was accepted")
		}
	})
}
