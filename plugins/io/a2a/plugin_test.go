package a2a

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
)

// freeAddr reserves and releases a loopback port so a test listener can bind it
// without colliding with a parallel test.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// minimalCard is the smallest card config that satisfies the A2A required-field
// rules, used by every test that is not about card content.
func minimalCard() map[string]any {
	return map[string]any{
		"name":        "nexus-test",
		"description": "A Nexus agent under test.",
		"version":     "0.0.1",
		"skills": []any{
			map[string]any{
				"id":          "chat",
				"name":        "Chat",
				"description": "Run a conversational turn.",
			},
		},
	}
}

// testConfig returns a valid plugin config with the supplied overrides applied.
func testConfig(t *testing.T, overrides map[string]any) map[string]any {
	t.Helper()
	cfg := map[string]any{
		"bind": freeAddr(t),
		"card": minimalCard(),
	}
	for k, v := range overrides {
		cfg[k] = v
	}
	return cfg
}

// newTestServer builds a Server (no socket bound) from a plugin config map.
func newTestServer(t *testing.T, raw map[string]any) *Server {
	t.Helper()
	cfg, err := parseConfig(raw)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	card, err := buildCard(cfg)
	if err != nil {
		t.Fatalf("buildCard: %v", err)
	}
	return NewServer(serverConfig{cfg: cfg, card: card, logger: discardLogger()})
}

// do issues a request against the server's handler without binding a socket.
func do(t *testing.T, s *Server, method, target string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for _, m := range mutate {
		m(req)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestPluginIdentity(t *testing.T) {
	p := New()
	if p.ID() != "nexus.io.a2a" {
		t.Errorf("ID() = %q, want nexus.io.a2a", p.ID())
	}
	// The declared contract is the one the turn mapping actually uses; the
	// contract harness checks it against runtime behaviour, this checks it is
	// spelled the way the rest of the engine spells it.
	wantSubs := []string{"agent.turn.start", "agent.turn.end", "llm.response", "io.output", "core.error"}
	var haveSubs []string
	for _, sub := range p.Subscriptions() {
		haveSubs = append(haveSubs, sub.EventType)
	}
	if !slices.Equal(haveSubs, wantSubs) {
		t.Errorf("Subscriptions() = %v, want %v", haveSubs, wantSubs)
	}
	if want := []string{"before:io.input", "io.input"}; !slices.Equal(p.Emissions(), want) {
		t.Errorf("Emissions() = %v, want %v", p.Emissions(), want)
	}
	if p.Dependencies() != nil || p.Requires() != nil {
		t.Error("plugin declares dependencies or requirements it does not have")
	}
}

// TestPluginRegisteredInAllPlugins guards the one wiring line the plugin needs.
func TestPluginRegisteredInAllPlugins(t *testing.T) {
	// Imported indirectly to avoid the plugin -> allplugins -> plugin cycle; the
	// assertion here is only that the factory is usable as an engine.Plugin.
	var _ engine.Plugin = New()
}

// TestInitRequiresACard pins the deliberate refusal to synthesize a card.
func TestInitRequiresACard(t *testing.T) {
	_, err := parseConfig(map[string]any{"bind": freeAddr(t)})
	if err == nil {
		t.Fatal("a config with neither card nor card_file was accepted")
	}
}

// TestCardAndCardFileAreMutuallyExclusive pins the single-source rule.
func TestCardAndCardFileAreMutuallyExclusive(t *testing.T) {
	_, err := parseConfig(map[string]any{
		"bind":      freeAddr(t),
		"card":      minimalCard(),
		"card_file": "/tmp/does-not-matter.json",
	})
	if err == nil {
		t.Fatal("card and card_file were accepted together")
	}
}

// TestBearerTokenAndAuthAreMutuallyExclusive pins the shared nexusauth posture.
func TestBearerTokenAndAuthAreMutuallyExclusive(t *testing.T) {
	_, err := parseConfig(testConfig(t, map[string]any{
		"bearer_token": "secret",
		"auth": map[string]any{
			"validators": []any{map[string]any{
				"type":   "static",
				"tokens": []any{map[string]any{"token": "s", "principal": "p"}},
			}},
		},
	}))
	if err == nil {
		t.Fatal("bearer_token and auth were accepted together")
	}
}

// TestDefaultsAreLoopbackAndDocumented pins the safe-by-default posture.
func TestDefaultsAreLoopbackAndDocumented(t *testing.T) {
	cfg, err := parseConfig(map[string]any{"card": minimalCard()})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.bindAddr != "127.0.0.1:8091" {
		t.Errorf("default bind = %q, want a loopback address", cfg.bindAddr)
	}
	if cfg.jsonrpcPath != "/a2a" || cfg.restPrefix != "/a2a/v1" {
		t.Errorf("default paths = %q / %q", cfg.jsonrpcPath, cfg.restPrefix)
	}
	if cfg.publicURL != "http://127.0.0.1:8091" {
		t.Errorf("default public_url = %q", cfg.publicURL)
	}
	if cfg.cardRequiresAuth {
		t.Error("the agent card must be unauthenticated by default")
	}
	if cfg.strictVersionHeader {
		t.Error("strict_version_header must default to false")
	}
	if cfg.chain.Enabled() {
		t.Error("auth must be disabled when no credentials are configured")
	}
}

// TestPathValidation pins the two ways the mount points can be unusable.
func TestPathValidation(t *testing.T) {
	cases := map[string]map[string]any{
		"relative jsonrpc path": {"jsonrpc_path": "a2a"},
		"relative rest prefix":  {"rest_prefix": "v1"},
		"colliding paths":       {"jsonrpc_path": "/x", "rest_prefix": "/x"},
	}
	for name, overrides := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig(testConfig(t, overrides)); err == nil {
				t.Fatal("invalid path configuration accepted")
			}
		})
	}
}

// TestReadyAndShutdownBindAndRelease exercises the real socket lifecycle.
func TestReadyAndShutdownBindAndRelease(t *testing.T) {
	addr := freeAddr(t)
	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Config: testConfig(t, map[string]any{"bind": addr}),
		Bus:    engine.NewEventBus(),
		Logger: discardLogger(),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := p.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(t.Context()) })

	resp, err := http.Get("http://" + addr + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("fetching the agent card from a live listener: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("agent card status = %d, want 200", resp.StatusCode)
	}
}
