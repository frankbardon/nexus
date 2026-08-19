package a2aremote

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// ---- Helpers ----

// syncBuffer is a concurrency-safe log sink. The plugin logs from the
// goroutines that run remote calls as well as from Init, so an ordinary
// bytes.Buffer here would be a data race the -race build catches.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// bootLogged boots the plugin with every log record captured, so a test can
// assert on what was — and above all what was NOT — written.
func bootLogged(t *testing.T, cfg map[string]any) (*contract.ContractHarness, *syncBuffer) {
	t.Helper()
	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := contract.NewContract(t, New, contract.WithPluginConfig(cfg), contract.WithLogger(logger))
	return h, buf
}

// initErr runs Init directly, so a test can assert on a boot failure the
// contract harness would turn into a t.Fatal.
func initErr(t *testing.T, cfg map[string]any) error {
	t.Helper()
	p := New()
	return p.Init(engine.PluginContext{
		Config:   cfg,
		Bus:      engine.NewEventBus(),
		Logger:   slog.New(slog.NewTextHandler(&syncBuffer{}, nil)),
		PluginID: pluginID,
	})
}

// credAgent builds a one-agent config with a credentials block.
func credAgent(baseURL string, creds map[string]any) map[string]any {
	return oneAgent(baseURL, map[string]any{cfgKeyCredentials: creds})
}

// ---- The OAuth2 token endpoint ----

type tokenServerConfig struct {
	// expiresIn is the lifetime the endpoint reports, in seconds. Zero omits
	// the field entirely, which RFC 6749 permits.
	expiresIn int
	// delay is applied before answering, so a concurrency test has a window in
	// which a stampede would be visible.
	delay time.Duration
	// status and body replace the successful answer.
	status int
	body   string
	// rotate issues a distinct token per request, so a test can tell a reused
	// token from a re-fetched one.
	rotate bool
}

type tokenServer struct {
	srv *httptest.Server

	mu    sync.Mutex
	hits  int
	forms []url.Values
	auths []string
}

func newTokenServer(t *testing.T, cfg tokenServerConfig) *tokenServer {
	t.Helper()
	ts := &tokenServer{}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		ts.mu.Lock()
		ts.hits++
		n := ts.hits
		ts.forms = append(ts.forms, r.PostForm)
		ts.auths = append(ts.auths, r.Header.Get("Authorization"))
		ts.mu.Unlock()

		if cfg.delay > 0 {
			time.Sleep(cfg.delay)
		}
		if cfg.status != 0 && (cfg.status < 200 || cfg.status >= 300) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(cfg.status)
			_, _ = w.Write([]byte(cfg.body))
			return
		}

		token := "access-token"
		if cfg.rotate {
			token = fmt.Sprintf("access-token-%d", n)
		}
		payload := map[string]any{"access_token": token, "token_type": "Bearer"}
		if cfg.expiresIn > 0 {
			payload["expires_in"] = cfg.expiresIn
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(ts.srv.Close)
	return ts
}

func (ts *tokenServer) URL() string { return ts.srv.URL }

func (ts *tokenServer) count() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.hits
}

func (ts *tokenServer) lastForm() url.Values {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.forms) == 0 {
		return nil
	}
	return ts.forms[len(ts.forms)-1]
}

func (ts *tokenServer) lastAuth() string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.auths) == 0 {
		return ""
	}
	return ts.auths[len(ts.auths)-1]
}

// bearerAuthorizer gates the test agent on an exact Authorization header.
func bearerAuthorizer(expected string) func(*http.Request) bool {
	return func(r *http.Request) bool { return r.Header.Get("Authorization") == expected }
}

// ---- Bearer ----

func TestBearerTokenFromInlineConfigRidesOnEveryRequest(t *testing.T) {
	const token = "inline-bearer-token"
	agent := newTestAgent(t, testAgentConfig{authorize: bearerAuthorizer("Bearer " + token)})

	h := boot(t, credAgent(agent.URL(), map[string]any{
		cfgKeyCredType: string(credBearer),
		cfgKeyToken:    token,
	}))

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "go"})
	if res.Error != "" {
		t.Fatalf("authenticated call failed: %s", res.Error)
	}

	headers := agent.seenHeaders()
	if len(headers) < 2 {
		t.Fatalf("want the card fetch and the message to both be seen, got %d requests", len(headers))
	}
	for i, hd := range headers {
		if got := hd.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("request %d carried Authorization %q, want the bearer token", i, got)
		}
	}
}

func TestBearerTokenFromEnvVarFollowsTheProviderConvention(t *testing.T) {
	const token = "env-bearer-token"
	t.Setenv("A2A_TEST_REMOTE_TOKEN", token)
	agent := newTestAgent(t, testAgentConfig{authorize: bearerAuthorizer("Bearer " + token)})

	h := boot(t, credAgent(agent.URL(), map[string]any{
		cfgKeyCredType: string(credBearer),
		cfgKeyTokenEnv: "A2A_TEST_REMOTE_TOKEN",
	}))

	if res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "go"}); res.Error != "" {
		t.Fatalf("env-sourced bearer call failed: %s", res.Error)
	}
}

func TestInlineTokenWinsOverTheEnvVar(t *testing.T) {
	t.Setenv("A2A_TEST_REMOTE_TOKEN", "from-env")
	cfg, err := parseConfig(credAgent("https://x.test", map[string]any{
		cfgKeyCredType: string(credBearer),
		cfgKeyToken:    "from-config",
		cfgKeyTokenEnv: "A2A_TEST_REMOTE_TOKEN",
	}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if got := cfg.agents[0].credentials.token; got != "from-config" {
		t.Errorf("token = %q, want the inline value to take precedence", got)
	}
}

func TestBearerHeaderAndSchemeAreConfigurable(t *testing.T) {
	const token = "api-key-value"
	agent := newTestAgent(t, testAgentConfig{
		authorize: func(r *http.Request) bool { return r.Header.Get("X-Api-Key") == token },
	})

	h := boot(t, credAgent(agent.URL(), map[string]any{
		cfgKeyCredType:    string(credBearer),
		cfgKeyToken:       token,
		cfgKeyCredHeader:  "X-Api-Key",
		cfgKeyBearerSchem: "",
	}))

	if res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "go"}); res.Error != "" {
		t.Fatalf("api-key style call failed: %s", res.Error)
	}
	for i, hd := range agent.seenHeaders() {
		if got := hd.Get("X-Api-Key"); got != token {
			t.Errorf("request %d carried X-Api-Key %q, want the bare token", i, got)
		}
		if hd.Get("Authorization") != "" {
			t.Errorf("request %d also sent an Authorization header", i)
		}
	}
}

func TestNoCredentialsBlockSendsNothing(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{})
	h := boot(t, oneAgent(agent.URL(), nil))

	if res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "go"}); res.Error != "" {
		t.Fatalf("open-endpoint call failed: %s", res.Error)
	}
	for i, hd := range agent.seenHeaders() {
		if got := hd.Get("Authorization"); got != "" {
			t.Errorf("request %d sent Authorization %q with no credentials configured", i, got)
		}
	}
}

// ---- Validation at Init ----

func TestCredentialMisconfigurationFailsAtInit(t *testing.T) {
	tests := []struct {
		name    string
		creds   map[string]any
		needles []string
	}{
		{
			name:    "no type",
			creds:   map[string]any{cfgKeyToken: "x"},
			needles: []string{cfgKeyCredType, "required"},
		},
		{
			name:    "unknown type",
			creds:   map[string]any{cfgKeyCredType: "kerberos"},
			needles: []string{"unknown credential type", "kerberos"},
		},
		{
			name:    "bearer with no token",
			creds:   map[string]any{cfgKeyCredType: string(credBearer)},
			needles: []string{cfgKeyToken, cfgKeyTokenEnv},
		},
		{
			name:    "bearer naming an unset env var",
			creds:   map[string]any{cfgKeyCredType: string(credBearer), cfgKeyTokenEnv: "A2A_DEFINITELY_UNSET_VAR"},
			needles: []string{"A2A_DEFINITELY_UNSET_VAR", "unset or empty"},
		},
		{
			name: "a key from another credential type",
			creds: map[string]any{
				cfgKeyCredType: string(credBearer),
				cfgKeyToken:    "x",
				cfgKeyCertFile: "/tmp/nope.pem",
			},
			needles: []string{cfgKeyCertFile, "does not belong"},
		},
		{
			name:    "oauth2 with no client id",
			creds:   map[string]any{cfgKeyCredType: string(credOAuth2), cfgKeyClientSecret: "s"},
			needles: []string{cfgKeyClientID},
		},
		{
			name:    "oauth2 with no client secret",
			creds:   map[string]any{cfgKeyCredType: string(credOAuth2), cfgKeyClientID: "c"},
			needles: []string{cfgKeyClientSecret},
		},
		{
			name:    "oauth2 with an unknown auth style",
			creds:   map[string]any{cfgKeyCredType: string(credOAuth2), cfgKeyClientID: "c", cfgKeyClientSecret: "s", cfgKeyAuthStyle: "post"},
			needles: []string{cfgKeyAuthStyle, "post"},
		},
		{
			name:    "mtls with no key",
			creds:   map[string]any{cfgKeyCredType: string(credMTLS), cfgKeyCertFile: "/tmp/c.pem"},
			needles: []string{cfgKeyKeyFile},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := initErr(t, credAgent("https://remote.test", tc.creds))
			if err == nil {
				t.Fatal("Init accepted a misconfigured credential; want a boot failure")
			}
			for _, needle := range tc.needles {
				if !strings.Contains(err.Error(), needle) {
					t.Errorf("error does not mention %q:\n%v", needle, err)
				}
			}
		})
	}
}

func TestOAuth2WithoutATokenURLOrACardFailsAtInit(t *testing.T) {
	cfg := map[string]any{"agents": []any{map[string]any{
		"name":                "pinned",
		cfgKeyJSONRPCEndpoint: "https://legacy.test/a2a",
		cfgKeyCredentials: map[string]any{
			cfgKeyCredType:     string(credOAuth2),
			cfgKeyClientID:     "client",
			cfgKeyClientSecret: "secret",
		},
	}}}
	err := initErr(t, cfg)
	if err == nil {
		t.Fatal("Init accepted an oauth2 remote with no way to reach a token endpoint")
	}
	if !strings.Contains(err.Error(), cfgKeyTokenURL) {
		t.Errorf("error does not name %s:\n%v", cfgKeyTokenURL, err)
	}
}

// ---- OAuth2 ----

func TestOAuth2TokenIsFetchedOnceAndReused(t *testing.T) {
	ts := newTokenServer(t, tokenServerConfig{expiresIn: 3600, rotate: true})
	agent := newTestAgent(t, testAgentConfig{authorize: bearerAuthorizer("Bearer access-token-1")})

	h := boot(t, credAgent(agent.URL(), map[string]any{
		cfgKeyCredType:     string(credOAuth2),
		cfgKeyClientID:     "client-id",
		cfgKeyClientSecret: "client-secret",
		cfgKeyTokenURL:     ts.URL(),
		cfgKeyScopes:       []any{"a2a.invoke", "a2a.read"},
	}))

	for i, task := range []string{"first", "second"} {
		if res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": task}); res.Error != "" {
			t.Fatalf("call %d failed: %s", i, res.Error)
		}
	}
	if got := ts.count(); got != 1 {
		t.Errorf("token endpoint hit %d times, want 1 — the token must be cached across calls", got)
	}
	if got := ts.lastForm().Get("grant_type"); got != "client_credentials" {
		t.Errorf("grant_type = %q", got)
	}
	if got := ts.lastForm().Get("scope"); got != "a2a.invoke a2a.read" {
		t.Errorf("scope = %q, want the space-delimited configured scopes", got)
	}
	if !strings.HasPrefix(ts.lastAuth(), "Basic ") {
		t.Errorf("token request authenticated with %q, want HTTP Basic by default", ts.lastAuth())
	}
}

func TestOAuth2RefreshesBeforeExpiry(t *testing.T) {
	ts := newTokenServer(t, tokenServerConfig{expiresIn: 1, rotate: true})
	src := newOAuth2Source(credentialConfig{
		clientID:      "c",
		clientSecret:  "s",
		tokenURL:      ts.URL(),
		refreshLeeway: 900 * time.Millisecond,
	}, "researcher", discardLogger(), nil)

	first, err := src.accessToken(context.Background())
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	// The stated lifetime is 1s and the leeway 900ms, so the token is trusted
	// for ~100ms; after that a refresh must happen without the server having
	// expired anything.
	time.Sleep(200 * time.Millisecond)
	second, err := src.accessToken(context.Background())
	if err != nil {
		t.Fatalf("second token: %v", err)
	}
	if first == second {
		t.Error("the token was not refreshed ahead of its expiry")
	}
	if got := ts.count(); got != 2 {
		t.Errorf("token endpoint hit %d times, want 2", got)
	}
}

// A quoted expires_in is a real-world wart, and rejecting the whole token over
// it would be a worse failure than tolerating it.
func TestOAuth2ToleratesAQuotedExpiresIn(t *testing.T) {
	quoted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","token_type":"Bearer","expires_in":"3600"}`))
	}))
	t.Cleanup(quoted.Close)

	src := newOAuth2Source(credentialConfig{
		clientID: "c", clientSecret: "s", tokenURL: quoted.URL,
	}, "researcher", discardLogger(), nil)

	token, err := src.accessToken(context.Background())
	if err != nil {
		t.Fatalf("a quoted expires_in was rejected: %v", err)
	}
	if token != "at" {
		t.Errorf("token = %q", token)
	}
	// An hour minus the default leeway is comfortably in the future, which is
	// what proves the quoted value was actually read rather than defaulted.
	src.mu.Lock()
	expires := src.expires
	src.mu.Unlock()
	if time.Until(expires) < 30*time.Minute {
		t.Errorf("expiry is %s away; the quoted expires_in was not read", time.Until(expires))
	}
}

func TestOAuth2AuthStyleBodySendsCredentialsInTheForm(t *testing.T) {
	ts := newTokenServer(t, tokenServerConfig{expiresIn: 3600})
	src := newOAuth2Source(credentialConfig{
		clientID:     "client-id",
		clientSecret: "client-secret",
		tokenURL:     ts.URL(),
		authStyle:    authStyleBody,
	}, "researcher", discardLogger(), nil)

	if _, err := src.accessToken(context.Background()); err != nil {
		t.Fatalf("accessToken: %v", err)
	}
	if got := ts.lastForm().Get("client_id"); got != "client-id" {
		t.Errorf("client_id in form = %q", got)
	}
	if got := ts.lastForm().Get("client_secret"); got != "client-secret" {
		t.Errorf("client_secret in form = %q", got)
	}
	if ts.lastAuth() != "" {
		t.Errorf("body style still sent an Authorization header: %q", ts.lastAuth())
	}
}

// TestOAuth2ConcurrentCallersDoNotStampedeTheTokenEndpoint is the whole reason
// the cache is single-flight: a model that fans out produces a burst of tool
// calls that reach Apply within microseconds of each other, and an
// authorization server answers a burst of identical grants with a 429.
func TestOAuth2ConcurrentCallersDoNotStampedeTheTokenEndpoint(t *testing.T) {
	ts := newTokenServer(t, tokenServerConfig{expiresIn: 3600, delay: 80 * time.Millisecond, rotate: true})
	src := newOAuth2Source(credentialConfig{
		clientID:     "c",
		clientSecret: "s",
		tokenURL:     ts.URL(),
	}, "researcher", discardLogger(), nil)

	const callers = 16
	var (
		start   = make(chan struct{})
		wg      sync.WaitGroup
		mu      sync.Mutex
		tokens  = map[string]int{}
		failure error
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			token, err := src.accessToken(context.Background())
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failure = err
				return
			}
			tokens[token]++
		}()
	}
	close(start)
	wg.Wait()

	if failure != nil {
		t.Fatalf("a concurrent caller failed: %v", failure)
	}
	if got := ts.count(); got != 1 {
		t.Errorf("token endpoint hit %d times for %d concurrent callers, want exactly 1", got, callers)
	}
	if len(tokens) != 1 {
		t.Errorf("callers saw %d distinct tokens, want 1: %v", len(tokens), tokens)
	}
	for token, n := range tokens {
		if n != callers {
			t.Errorf("token %q went to %d callers, want %d", token, n, callers)
		}
	}
}

// A token endpoint that is down must not be hammered once per caller either:
// the waiters take the fetcher's failure rather than each starting their own.
func TestOAuth2ConcurrentCallersShareOneFailure(t *testing.T) {
	ts := newTokenServer(t, tokenServerConfig{
		delay:  50 * time.Millisecond,
		status: http.StatusServiceUnavailable,
		body:   `{"error":"temporarily_unavailable"}`,
	})
	src := newOAuth2Source(credentialConfig{
		clientID: "c", clientSecret: "s", tokenURL: ts.URL(),
	}, "researcher", discardLogger(), nil)

	var (
		start = make(chan struct{})
		wg    sync.WaitGroup
		mu    sync.Mutex
		errs  int
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := src.accessToken(context.Background()); err != nil {
				mu.Lock()
				errs++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if errs != 8 {
		t.Errorf("%d of 8 callers saw the failure, want all of them", errs)
	}
	if got := ts.count(); got != 1 {
		t.Errorf("token endpoint hit %d times while failing, want 1", got)
	}
	// A later call is a fresh attempt: a transient failure must not be sticky.
	_, _ = src.accessToken(context.Background())
	if got := ts.count(); got != 2 {
		t.Errorf("token endpoint hit %d times after a later retry, want 2", got)
	}
}

func TestOAuth2TokenEndpointIsDiscoveredFromTheAgentCard(t *testing.T) {
	ts := newTokenServer(t, tokenServerConfig{expiresIn: 3600})
	agent := newTestAgent(t, testAgentConfig{
		securitySchemes: map[string]a2a.SecurityScheme{
			"oauth": {OAuth2: &a2a.OAuth2SecurityScheme{
				Flows: a2a.OAuthFlows{ClientCredentials: &a2a.ClientCredentialsOAuthFlow{
					TokenURL: ts.URL(),
					Scopes:   map[string]string{"a2a.invoke": "invoke the agent"},
				}},
			}},
		},
		authorize: func(r *http.Request) bool {
			// The well-known card is public, so only the operation request has
			// to carry a token.
			if strings.HasSuffix(r.URL.Path, a2a.AgentCardPath) {
				return true
			}
			return r.Header.Get("Authorization") == "Bearer access-token"
		},
	})

	h := boot(t, credAgent(agent.URL(), map[string]any{
		cfgKeyCredType:     string(credOAuth2),
		cfgKeyClientID:     "c",
		cfgKeyClientSecret: "s",
	}))

	if res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "go"}); res.Error != "" {
		t.Fatalf("card-discovered token endpoint call failed: %s", res.Error)
	}
	if got := ts.count(); got != 1 {
		t.Errorf("token endpoint hit %d times, want 1", got)
	}
}

func TestOAuth2CardWithNoClientCredentialsFlowFailsTheCallNotTheBoot(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{})

	// Boot succeeds: the remote might well declare a flow, and nothing is
	// fetched until the first call.
	h := boot(t, credAgent(agent.URL(), map[string]any{
		cfgKeyCredType:     string(credOAuth2),
		cfgKeyClientID:     "c",
		cfgKeyClientSecret: "s",
	}))

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "go"})
	if res.Error == "" {
		t.Fatal("want a clean tool error when no token endpoint can be found")
	}
	if !strings.Contains(res.Error, cfgKeyTokenURL) {
		t.Errorf("tool error does not name %s:\n%s", cfgKeyTokenURL, res.Error)
	}
}

// ---- mTLS ----

// mtlsAgent starts a TLS test agent that REQUIRES and VERIFIES a client
// certificate, which is the only arrangement that can prove the certificate was
// wired into the transport rather than merely parsed.
func mtlsAgent(t *testing.T, ca *testCA) *testAgent {
	t.Helper()
	serverCertPEM, serverKeyPEM := ca.issue(t, "127.0.0.1", true)
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatalf("server keypair: %v", err)
	}
	return newTestAgent(t, testAgentConfig{tlsConf: &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.pool(),
		MinVersion:   tls.VersionTLS12,
	}})
}

func TestMutualTLSPresentsTheClientCertificate(t *testing.T) {
	ca := newTestCA(t)
	agent := mtlsAgent(t, ca)

	clientCertPEM, clientKeyPEM := ca.issue(t, "nexus-client", false)
	certPath := writeFile(t, "client.pem", clientCertPEM)
	keyPath := writeFile(t, "client-key.pem", clientKeyPEM)
	caPath := writeFile(t, "ca.pem", ca.certPEM)

	h := boot(t, credAgent(agent.URL(), map[string]any{
		cfgKeyCredType: string(credMTLS),
		cfgKeyCertFile: certPath,
		cfgKeyKeyFile:  keyPath,
		cfgKeyCAFile:   caPath,
	}))

	if res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "go"}); res.Error != "" {
		t.Fatalf("mutual-TLS call failed: %s", res.Error)
	}
	if cards, sends := agent.counts(); cards == 0 || sends == 0 {
		t.Errorf("agent saw %d card fetches and %d sends; want the handshake to have succeeded for both", cards, sends)
	}
}

// A client certificate from a CA the remote does not trust must be REFUSED at
// the handshake. It is the other half of the previous test: together they show
// the certificate is genuinely verified rather than merely presented.
func TestMutualTLSCertificateFromAnotherCAIsRefused(t *testing.T) {
	ca := newTestCA(t)
	agent := mtlsAgent(t, ca)

	rogue := newTestCA(t)
	rogueCertPEM, rogueKeyPEM := rogue.issue(t, "impostor", false)

	h := boot(t, credAgent(agent.URL(), map[string]any{
		cfgKeyCredType: string(credMTLS),
		cfgKeyCertFile: writeFile(t, "rogue.pem", rogueCertPEM),
		cfgKeyKeyFile:  writeFile(t, "rogue-key.pem", rogueKeyPEM),
		// The server's own certificate IS trusted, so the only thing that can
		// fail is the client certificate.
		cfgKeyCAFile: writeFile(t, "ca.pem", ca.certPEM),
	}))

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "go"})
	if res.Error == "" {
		t.Fatal("the remote accepted a client certificate from an untrusted CA")
	}
	if !strings.Contains(res.Error, "unreachable") {
		t.Errorf("want a clean transport-failure tool error, got:\n%s", res.Error)
	}
}

func TestMutualTLSPathsGoThroughExpandPath(t *testing.T) {
	ca := newTestCA(t)
	agent := mtlsAgent(t, ca)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	clientCertPEM, clientKeyPEM := ca.issue(t, "nexus-client", false)
	writeInto(t, home, "client.pem", clientCertPEM)
	writeInto(t, home, "client-key.pem", clientKeyPEM)
	writeInto(t, home, "ca.pem", ca.certPEM)

	h := boot(t, credAgent(agent.URL(), map[string]any{
		cfgKeyCredType: string(credMTLS),
		cfgKeyCertFile: "~/client.pem",
		cfgKeyKeyFile:  "~/client-key.pem",
		cfgKeyCAFile:   "~/ca.pem",
	}))

	if res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "go"}); res.Error != "" {
		t.Fatalf("tilde-configured mutual-TLS call failed: %s", res.Error)
	}
}

func TestMutualTLSMaterialIsValidatedAtInit(t *testing.T) {
	ca := newTestCA(t)
	certPEM, keyPEM := ca.issue(t, "nexus-client", false)
	_, otherKeyPEM := ca.issue(t, "someone-else", false)

	t.Run("missing file", func(t *testing.T) {
		err := initErr(t, credAgent("https://remote.test", map[string]any{
			cfgKeyCredType: string(credMTLS),
			cfgKeyCertFile: "/nonexistent/client.pem",
			cfgKeyKeyFile:  "/nonexistent/client-key.pem",
		}))
		if err == nil {
			t.Fatal("Init accepted an unreadable client certificate")
		}
	})

	t.Run("key does not match the certificate", func(t *testing.T) {
		err := initErr(t, credAgent("https://remote.test", map[string]any{
			cfgKeyCredType: string(credMTLS),
			cfgKeyCertFile: writeFile(t, "client.pem", certPEM),
			cfgKeyKeyFile:  writeFile(t, "wrong-key.pem", otherKeyPEM),
		}))
		if err == nil {
			t.Fatal("Init accepted a key that does not match its certificate")
		}
	})

	t.Run("ca bundle with no certificate", func(t *testing.T) {
		err := initErr(t, credAgent("https://remote.test", map[string]any{
			cfgKeyCredType: string(credMTLS),
			cfgKeyCertFile: writeFile(t, "client.pem", certPEM),
			cfgKeyKeyFile:  writeFile(t, "client-key.pem", keyPEM),
			cfgKeyCAFile:   writeFile(t, "ca.pem", []byte("not a certificate")),
		}))
		if err == nil {
			t.Fatal("Init accepted a CA bundle containing no PEM certificate")
		}
		if !strings.Contains(err.Error(), cfgKeyCAFile) {
			t.Errorf("error does not name %s:\n%v", cfgKeyCAFile, err)
		}
	})
}

// ---- Card / credential mismatch ----

func mtlsOnlyCard() map[string]a2a.SecurityScheme {
	return map[string]a2a.SecurityScheme{
		"mtls": {MutualTLS: &a2a.MutualTlsSecurityScheme{Description: "client certificate"}},
	}
}

func TestMismatchedCredentialWarnsButStillCalls(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{securitySchemes: mtlsOnlyCard()})

	h, logs := bootLogged(t, credAgent(agent.URL(), map[string]any{
		cfgKeyCredType: string(credBearer),
		cfgKeyToken:    "a-token-the-remote-will-ignore",
	}))

	// The warning is a first-USE event, not a boot event: the card has not been
	// fetched yet.
	if strings.Contains(logs.String(), "does not match any security scheme") {
		t.Fatal("the mismatch was reported at boot; the card must not be fetched then")
	}

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "go"})
	if res.Error != "" {
		t.Fatalf("a mismatch must warn, not refuse: %s", res.Error)
	}
	out := logs.String()
	if !strings.Contains(out, "does not match any security scheme") {
		t.Fatalf("no mismatch warning was logged:\n%s", out)
	}
	if !strings.Contains(out, "mutualTls") {
		t.Errorf("the warning does not name the schemes the card declares:\n%s", out)
	}
	if strings.Contains(out, "a-token-the-remote-will-ignore") {
		t.Error("the warning leaked the credential value")
	}
}

func TestCompatibleCredentialDoesNotWarn(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{securitySchemes: map[string]a2a.SecurityScheme{
		"bearer": {HTTPAuth: &a2a.HTTPAuthSecurityScheme{Scheme: "Bearer"}},
	}})

	h, logs := bootLogged(t, credAgent(agent.URL(), map[string]any{
		cfgKeyCredType: string(credBearer),
		cfgKeyToken:    "token",
	}))
	if res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "go"}); res.Error != "" {
		t.Fatalf("call failed: %s", res.Error)
	}
	if strings.Contains(logs.String(), "does not match any security scheme") {
		t.Errorf("a bearer token against an httpAuth card warned:\n%s", logs.String())
	}
}

func TestMissingCredentialsAgainstAProtectedRemoteWarns(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{securitySchemes: mtlsOnlyCard()})
	h, logs := bootLogged(t, oneAgent(agent.URL(), nil))

	if res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "go"}); res.Error != "" {
		t.Fatalf("call failed: %s", res.Error)
	}
	if !strings.Contains(logs.String(), "sends no credentials to a remote that declares security schemes") {
		t.Errorf("no warning for an unauthenticated call to a protected remote:\n%s", logs.String())
	}
}

func TestMismatchIsWarnedAboutOnlyOnce(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{securitySchemes: mtlsOnlyCard()})
	h, logs := bootLogged(t, credAgent(agent.URL(), map[string]any{
		cfgKeyCredType: string(credBearer),
		cfgKeyToken:    "token",
	}))

	for i, task := range []string{"one", "two", "three"} {
		if res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": task}); res.Error != "" {
			t.Fatalf("call %d failed: %s", i, res.Error)
		}
	}
	if n := strings.Count(logs.String(), "does not match any security scheme"); n != 1 {
		t.Errorf("the mismatch was warned about %d times, want exactly 1", n)
	}
}

// ---- Secret hygiene ----

// TestNoCredentialValueIsEverLogged drives every credential through both a
// successful and a failing path with the log level pinned to DEBUG, then reads
// the entire log and the tool results back looking for the secrets. This is the
// acceptance criterion the rest of the file cannot prove piecemeal.
func TestNoCredentialValueIsEverLogged(t *testing.T) {
	const (
		bearerToken  = "SECRET-BEARER-0123456789"
		clientSecret = "SECRET-CLIENT-9876543210"
		clientID     = "SECRET-CLIENTID-abcdef"
	)

	// A token endpoint that refuses, and echoes the secret back in the
	// free-text description the way a careless server would. Nothing this
	// plugin writes may repeat it.
	ts := newTokenServer(t, tokenServerConfig{
		status: http.StatusUnauthorized,
		body: fmt.Sprintf(`{"error":"invalid_client","error_description":"client_secret %s is wrong"}`,
			clientSecret),
	})

	agent := newTestAgent(t, testAgentConfig{
		securitySchemes: mtlsOnlyCard(),
		authorize: func(r *http.Request) bool {
			return !strings.Contains(r.URL.Path, "/a2a")
		},
	})
	down := newTestAgent(t, testAgentConfig{cardStatus: http.StatusInternalServerError})

	cfg := map[string]any{"agents": []any{
		map[string]any{
			"name":     "bearer",
			"base_url": agent.URL(),
			cfgKeyCredentials: map[string]any{
				cfgKeyCredType: string(credBearer),
				cfgKeyToken:    bearerToken,
			},
		},
		map[string]any{
			"name":     "oauth",
			"base_url": down.URL(),
			cfgKeyCredentials: map[string]any{
				cfgKeyCredType:     string(credOAuth2),
				cfgKeyClientID:     clientID,
				cfgKeyClientSecret: clientSecret,
				cfgKeyTokenURL:     ts.URL(),
			},
		},
	}}

	h, logs := bootLogged(t, cfg)

	results := []string{}
	results = append(results, invoke(t, h, "delegate_a2a_bearer", map[string]any{"task": "go"}).Error)
	results = append(results, invoke(t, h, "delegate_a2a_oauth", map[string]any{"task": "go"}).Error)

	// Both calls must have failed, or the test is not exercising the error
	// paths it exists to check.
	for i, res := range results {
		if res == "" {
			t.Fatalf("call %d succeeded; this test needs both failure paths", i)
		}
	}

	haystacks := map[string]string{"the log": logs.String()}
	for i, res := range results {
		haystacks[fmt.Sprintf("tool result %d", i)] = res
	}
	for _, secret := range []string{bearerToken, clientSecret, clientID} {
		for where, haystack := range haystacks {
			if strings.Contains(haystack, secret) {
				t.Errorf("%s contains a credential value (%q)", where, secret)
			}
		}
	}
	// The free-text error_description is dropped wholesale, not just scrubbed.
	if strings.Contains(logs.String(), "is wrong") {
		t.Error("the token endpoint's free-text error_description reached the log")
	}
	// What IS reported is the fixed RFC 6749 error code, which is what an
	// operator needs.
	if !strings.Contains(results[1], "invalid_client") {
		t.Errorf("the oauth2 failure does not name the RFC 6749 error code:\n%s", results[1])
	}
}

func TestCredentialConfigRedactsItselfWhenLogged(t *testing.T) {
	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, nil))

	for _, cc := range []credentialConfig{
		{kind: credBearer, token: "TOKENVALUE"},
		{kind: credOAuth2, clientID: "IDVALUE", clientSecret: "SECRETVALUE", tokenURL: "https://issuer.test/token"},
	} {
		logger.Info("credential", "cred", cc)
	}

	out := buf.String()
	for _, secret := range []string{"TOKENVALUE", "IDVALUE", "SECRETVALUE"} {
		if strings.Contains(out, secret) {
			t.Errorf("logging a credentialConfig leaked %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, redacted) {
		t.Errorf("the redaction marker is missing:\n%s", out)
	}
}

// ---- Small helpers ----

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&syncBuffer{}, nil))
}

func writeInto(t *testing.T, dir, name string, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
