package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// staticAuthYAML is a broker config enabling one static token bound to a known
// principal. It is the only validator type that exists yet, and it is enough to
// exercise every branch of the guard except insufficient-scope (which no static
// token can produce — see stubValidator).
const staticAuthYAML = `
auth:
  validators:
    - type: static
      tokens:
        - token: "good-token"
          principal: "ci-runner"
          tenant: "acme"
`

// twoPrincipalAuthYAML configures two DIFFERENT valid credentials. Ownership
// enforcement needs a second identity that authenticates perfectly and is still
// refused a lease it does not own — "invalid credential" and "valid credential,
// wrong lease" are separate properties and a one-token fixture can only test the
// first.
const twoPrincipalAuthYAML = `
auth:
  validators:
    - type: static
      tokens:
        - token: "` + ownerToken + `"
          principal: "` + ownerPrincipal + `"
        - token: "` + otherToken + `"
          principal: "` + otherPrincipal + `"
`

// Credentials twoPrincipalAuthYAML binds. ownerToken resolves to the principal
// that claims the lease under test; otherToken to a valid caller that owns
// nothing.
const (
	ownerToken     = "owner-token"
	ownerPrincipal = "owner-principal"

	otherToken     = "other-token"
	otherPrincipal = "other-principal"
)

// mustAuthChain builds the validator chain a broker `auth:` block describes,
// going through the real config loader so a test cannot accidentally prove
// enforcement against a chain no operator could configure.
func mustAuthChain(t *testing.T, yaml string) *nexusauth.Chain {
	t.Helper()
	chain := mustLoadConfig(t, yaml).AuthChain
	if !chain.Enabled() {
		t.Fatalf("mustAuthChain: YAML produced a DISABLED chain; the test would prove nothing:\n%s", yaml)
	}
	return chain
}

// assertIdenticalRefusals fails unless two refusals are indistinguishable to the
// caller: same status, same Content-Type, same body bytes.
//
// This is the assertion behind "an unknown lease and someone else's lease look
// the same". It is a security property, not cosmetics: any observable difference
// turns a lease-scoped route into an oracle a caller can use to enumerate live
// lease ids, which still matter because a lease id is the bearer secret on the
// instance dial-back path.
func assertIdenticalRefusals(t *testing.T, wantStatus int, unknown, unowned *http.Response) {
	t.Helper()
	if unknown.StatusCode != wantStatus || unowned.StatusCode != wantStatus {
		t.Fatalf("statuses = %d (unknown) / %d (unowned), want %d for both",
			unknown.StatusCode, unowned.StatusCode, wantStatus)
	}
	if a, b := unknown.Header.Get("Content-Type"), unowned.Header.Get("Content-Type"); a != b {
		t.Errorf("Content-Type differs: %q (unknown) vs %q (unowned)", a, b)
	}
	unknownBody, err := io.ReadAll(unknown.Body)
	if err != nil {
		t.Fatalf("read unknown-lease body: %v", err)
	}
	unownedBody, err := io.ReadAll(unowned.Body)
	if err != nil {
		t.Fatalf("read unowned-lease body: %v", err)
	}
	if !bytes.Equal(unknownBody, unownedBody) {
		t.Errorf("bodies differ — this is a lease-id oracle:\n unknown: %q\n unowned: %q",
			unknownBody, unownedBody)
	}
}

// newBrokerTestServer wires the same route topology run() does — healthz
// unguarded, claim/release/leases behind the guard — over an httptest server.
// The WebSocket routes are omitted: they are not guarded by this middleware and
// need no coverage here.
//
// extra lets a test register an additional route through the same guard, which
// is how the "a new route is authenticated by construction" and
// "Principal reaches the handler" properties are checked.
func newBrokerTestServer(t *testing.T, cfg Config, extra func(routeMux)) (*httptest.Server, *Registry) {
	t.Helper()
	logger := testLogger()
	reg := NewRegistry(logger, 0)

	guard := newAuthGuard(logger, cfg.AuthChain)
	// Mirror run()'s ticket wiring: the store's inert/live state comes from the
	// guard, and the registry invalidates a lease's tickets on teardown.
	tickets := newTicketStore(logger, guard.enabled())
	reg.useTicketStore(tickets)

	claims := NewClaimServer(logger, reg, cfg, &fakeRunner{started: make(chan spawnSpec, 1)}, tickets)
	releases := NewReleaseServer(logger, reg, cfg.ReleaseGrace)
	leases := NewLeasesServer(logger, reg, guard, cfg.AdminScope)
	ticketsServer := NewTicketServer(logger, reg, tickets)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	guarded := guard.Guard(mux)
	claims.Register(guarded)
	releases.Register(guarded)
	leases.Register(guarded)
	ticketsServer.Register(guarded)
	if extra != nil {
		extra(guarded)
	}

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, reg
}

// mustLoadConfig loads a broker config from YAML, failing the test on error.
func mustLoadConfig(t *testing.T, yaml string) Config {
	t.Helper()
	cfg, err := LoadConfigFromBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	return cfg
}

// doAuthed performs a request against base+path with an optional bearer token
// ("" sends no Authorization header at all).
func doAuthed(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// guardedRoute describes one wrapped route plus the status its handler returns
// for an authenticated request. Each authorized case is chosen to reach the
// handler and stop there — an empty claim body is rejected by handleClaim, an
// unknown lease id by handleRelease — so the test proves "the guard let it
// through" without spawning an instance.
type guardedRoute struct {
	name       string
	method     string
	path       string
	body       string
	wantAuthed int
}

func guardedRoutes() []guardedRoute {
	return []guardedRoute{
		{"claim", http.MethodPost, "/claim", `{}`, http.StatusBadRequest},
		{"release", http.MethodPost, "/release/no-such-lease", "", http.StatusNotFound},
		{"leases", http.MethodGet, "/leases", "", http.StatusOK},
		{"ticket", http.MethodPost, "/ticket/no-such-lease", "", http.StatusNotFound},
	}
}

// TestAuthGuard_WrappedRoutes covers the three status outcomes on every wrapped
// route: a valid credential reaches the handler, no credential is 401, and a
// presented-but-wrong credential is 401.
func TestAuthGuard_WrappedRoutes(t *testing.T) {
	cfg := mustLoadConfig(t, staticAuthYAML)
	ts, _ := newBrokerTestServer(t, cfg, nil)

	for _, rt := range guardedRoutes() {
		t.Run(rt.name+"/valid credential passes", func(t *testing.T) {
			resp := doAuthed(t, rt.method, ts.URL+rt.path, "good-token", rt.body)
			if resp.StatusCode != rt.wantAuthed {
				t.Fatalf("status = %d, want %d (guard should not have refused)", resp.StatusCode, rt.wantAuthed)
			}
		})

		t.Run(rt.name+"/no credential is 401", func(t *testing.T) {
			resp := doAuthed(t, rt.method, ts.URL+rt.path, "", rt.body)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
				t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
			}
			assertErrorBody(t, resp, "authentication required")
		})

		t.Run(rt.name+"/rejected credential is 401 with a reason", func(t *testing.T) {
			resp := doAuthed(t, rt.method, ts.URL+rt.path, "wrong-token", rt.body)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, `error="invalid_token"`) {
				t.Errorf("WWW-Authenticate = %q, want error=\"invalid_token\"", got)
			}
			assertErrorBody(t, resp, "credential rejected")
		})
	}
}

// TestAuthGuard_HealthzOpenWithAuthEnabled pins the deliberate exemption:
// healthz is registered outside the guard, so a probe with no credential still
// gets 200 while auth is on.
func TestAuthGuard_HealthzOpenWithAuthEnabled(t *testing.T) {
	cfg := mustLoadConfig(t, staticAuthYAML)
	if !cfg.AuthChain.Enabled() {
		t.Fatal("precondition: auth should be enabled for this test")
	}
	ts, _ := newBrokerTestServer(t, cfg, nil)

	resp := doAuthed(t, http.MethodGet, ts.URL+"/healthz", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestAuthGuard_DisabledLeavesRoutesUntouched is the backward-compatibility
// test: with no auth block, Guard hands back the very same mux and every route
// answers an unauthenticated caller exactly as it did before auth existed.
func TestAuthGuard_DisabledLeavesRoutesUntouched(t *testing.T) {
	cfg := mustLoadConfig(t, "")

	mux := http.NewServeMux()
	if got := newAuthGuard(testLogger(), cfg.AuthChain).Guard(mux); got != routeMux(mux) {
		t.Error("Guard() on a disabled chain must return the mux unchanged")
	}

	ts, _ := newBrokerTestServer(t, cfg, nil)
	for _, rt := range guardedRoutes() {
		resp := doAuthed(t, rt.method, ts.URL+rt.path, "", rt.body)
		if resp.StatusCode != rt.wantAuthed {
			t.Errorf("%s: status = %d, want %d with auth disabled", rt.name, resp.StatusCode, rt.wantAuthed)
		}
	}
}

// TestAuthGuard_PrincipalReachesHandler proves the resolved Principal is
// available to a handler through the request context — the property every later
// ownership check depends on.
func TestAuthGuard_PrincipalReachesHandler(t *testing.T) {
	cfg := mustLoadConfig(t, staticAuthYAML)

	type seen struct {
		OK     bool   `json:"ok"`
		ID     string `json:"id"`
		Tenant string `json:"tenant"`
	}
	ts, _ := newBrokerTestServer(t, cfg, func(m routeMux) {
		m.HandleFunc("GET /probe", func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			_ = json.NewEncoder(w).Encode(seen{OK: ok, ID: p.ID, Tenant: p.Tenant})
		})
	})

	resp := doAuthed(t, http.MethodGet, ts.URL+"/probe", "good-token", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got seen
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || got.ID != "ci-runner" || got.Tenant != "acme" {
		t.Errorf("principal in handler = %+v, want {true ci-runner acme}", got)
	}

	// The same route added through the guard is authenticated by construction.
	if resp := doAuthed(t, http.MethodGet, ts.URL+"/probe", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated /probe status = %d, want 401", resp.StatusCode)
	}
}

// stubValidator returns a fixed verdict. It exists because the static validator
// can only ever report no-credential or invalid-credential, and the 403 branch
// needs a validator that verifies the identity but refuses the authority.
type stubValidator struct {
	principal nexusauth.Principal
	err       error
}

func (s stubValidator) Validate(context.Context, *http.Request) (nexusauth.Principal, error) {
	return s.principal, s.err
}

// TestAuthGuard_InsufficientScopeIs403 pins the third status: the credential
// verified, the authority did not, so the honest answer is 403 rather than 401.
func TestAuthGuard_InsufficientScopeIs403(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AuthChain = nexusauth.NewChain(nexusauth.NamedValidator{
		Name: "stub",
		Validator: stubValidator{
			err: nexusauth.NewError(nexusauth.KindInsufficientScope, "missing scope broker.claim", nil),
		},
	})
	ts, _ := newBrokerTestServer(t, cfg, nil)

	resp := doAuthed(t, http.MethodGet, ts.URL+"/leases", "any-token", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, `error="insufficient_scope"`) {
		t.Errorf("WWW-Authenticate = %q, want error=\"insufficient_scope\"", got)
	}
	assertErrorBody(t, resp, "insufficient scope")
}

// TestAuthGuard_LogsAllowAndDeny asserts the audit trail: one structured record
// per decision, carrying the route, the principal id (empty on a deny), the
// lease id where the route has one, and a reason on a deny.
func TestAuthGuard_LogsAllowAndDeny(t *testing.T) {
	cfg := mustLoadConfig(t, staticAuthYAML)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	reg := NewRegistry(testLogger(), 0)
	releases := NewReleaseServer(testLogger(), reg, cfg.ReleaseGrace)

	mux := http.NewServeMux()
	releases.Register(newAuthGuard(logger, cfg.AuthChain).Guard(mux))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	doAuthed(t, http.MethodPost, ts.URL+"/release/lease-42", "good-token", "")
	doAuthed(t, http.MethodPost, ts.URL+"/release/lease-99", "wrong-token", "")

	records := decodeLogRecords(t, buf.Bytes())
	if len(records) != 2 {
		t.Fatalf("got %d auth records, want exactly 2 (one per decision):\n%s", len(records), buf.String())
	}

	allow, deny := records[0], records[1]
	if allow["msg"] != "auth allowed" {
		t.Errorf("first record msg = %v, want \"auth allowed\"", allow["msg"])
	}
	if allow["principal_id"] != "ci-runner" {
		t.Errorf("allow principal_id = %v, want ci-runner", allow["principal_id"])
	}
	if allow["lease_id"] != "lease-42" {
		t.Errorf("allow lease_id = %v, want lease-42", allow["lease_id"])
	}
	if allow["route"] != "POST /release/{lease_id}" {
		t.Errorf("allow route = %v, want the matched pattern", allow["route"])
	}

	if deny["msg"] != "auth denied" {
		t.Errorf("second record msg = %v, want \"auth denied\"", deny["msg"])
	}
	if deny["principal_id"] != "" {
		t.Errorf("deny principal_id = %v, want empty", deny["principal_id"])
	}
	if deny["lease_id"] != "lease-99" {
		t.Errorf("deny lease_id = %v, want lease-99", deny["lease_id"])
	}
	if deny["route"] != "POST /release/{lease_id}" {
		t.Errorf("deny route = %v, want the matched pattern", deny["route"])
	}
	if deny["reason"] != "credential rejected" {
		t.Errorf("deny reason = %v, want \"credential rejected\"", deny["reason"])
	}
	// The per-validator diagnosis stays server-side, in the log only.
	if !strings.Contains(buf.String(), "static") {
		t.Error("deny record should name the validator that refused")
	}
}

// TestOwnershipDenial_LogsPrincipalLeaseAndRoute asserts the audit record an
// ownership refusal leaves behind. A 404 that says nothing else is
// indistinguishable from a client retrying its own released lease, so the
// operator-side record is the only place a cross-principal probe is visible: it
// must name the requesting principal, the lease id, and the route.
//
// Both surfaces are covered because they log through the same helper but from
// different call sites — the release handler behind the guard, and the client WS
// handler which resolves its own credential.
func TestOwnershipDenial_LogsPrincipalLeaseAndRoute(t *testing.T) {
	chain := mustAuthChain(t, twoPrincipalAuthYAML)

	cases := []struct {
		name      string
		wantRoute string
		// do performs one refused request against a server built over logger,
		// returning the lease id that was targeted.
		do func(t *testing.T, logger *slog.Logger) string
	}{
		{
			name:      "release",
			wantRoute: "POST /release/{lease_id}",
			do: func(t *testing.T, logger *slog.Logger) string {
				reg := NewRegistry(testLogger(), 0)
				id, err := reg.NewLease(nexusauth.Principal{ID: ownerPrincipal})
				if err != nil {
					t.Fatalf("NewLease: %v", err)
				}
				mux := http.NewServeMux()
				NewReleaseServer(logger, reg, time.Second).
					Register(newAuthGuard(testLogger(), chain).Guard(mux))
				ts := httptest.NewServer(mux)
				t.Cleanup(ts.Close)

				doAuthed(t, http.MethodPost, ts.URL+"/release/"+id, otherToken, "")
				return id
			},
		},
		{
			name:      "client websocket",
			wantRoute: "GET /lease/{lease_id}",
			do: func(t *testing.T, logger *slog.Logger) string {
				reg := NewRegistry(testLogger(), 0)
				id, err := reg.NewLease(nexusauth.Principal{ID: ownerPrincipal})
				if err != nil {
					t.Fatalf("NewLease: %v", err)
				}
				mux := http.NewServeMux()
				gw := NewGateway(logger, reg, newAuthGuard(testLogger(), chain))
				t.Cleanup(gw.Shutdown)
				gw.Register(mux)
				ts := httptest.NewServer(mux)
				t.Cleanup(ts.Close)

				doAuthed(t, http.MethodGet, ts.URL+ClientWSPath(id), otherToken, "")
				return id
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

			leaseID := tc.do(t, logger)

			var denial map[string]any
			for _, rec := range decodeLogRecords(t, buf.Bytes()) {
				if rec["msg"] == "lease access denied" {
					denial = rec
				}
			}
			if denial == nil {
				t.Fatalf("no ownership-denial record emitted:\n%s", buf.String())
			}
			if denial["level"] != "WARN" {
				t.Errorf("denial level = %v, want WARN", denial["level"])
			}
			if denial["principal_id"] != otherPrincipal {
				t.Errorf("denial principal_id = %v, want %q", denial["principal_id"], otherPrincipal)
			}
			if denial["lease_id"] != leaseID {
				t.Errorf("denial lease_id = %v, want %q", denial["lease_id"], leaseID)
			}
			if denial["route"] != tc.wantRoute {
				t.Errorf("denial route = %v, want %q", denial["route"], tc.wantRoute)
			}
			if denial["reason"] == "" || denial["reason"] == nil {
				t.Error("denial record carries no reason")
			}
		})
	}
}

// TestAuthGuard_StartupState pins the boot-time announcement: with no auth block
// the broker logs exactly ONE record, a WARN saying it is unauthenticated; with
// auth configured it logs exactly one INFO naming the live validators. "Exactly
// one" matters — this line is the only signal an operator gets that a deployment
// is open.
func TestAuthGuard_StartupState(t *testing.T) {
	cases := []struct {
		name      string
		yaml      string
		wantLevel string
		wantIn    []string
	}{
		{
			name:      "absent auth block warns once",
			yaml:      "",
			wantLevel: "WARN",
			wantIn:    []string{"DISABLED", "no auth block"},
		},
		{
			name:      "configured auth logs the validator order",
			yaml:      staticAuthYAML,
			wantLevel: "INFO",
			wantIn:    []string{"authentication enabled", "static"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mustLoadConfig(t, tc.yaml)

			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
			newAuthGuard(logger, cfg.AuthChain).logStartupState()

			records := decodeLogRecords(t, buf.Bytes())
			if len(records) != 1 {
				t.Fatalf("got %d startup records, want exactly 1:\n%s", len(records), buf.String())
			}
			if records[0]["level"] != tc.wantLevel {
				t.Errorf("level = %v, want %s", records[0]["level"], tc.wantLevel)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("startup record %q does not mention %q", buf.String(), want)
				}
			}
		})
	}
}

// assertErrorBody checks the JSON error envelope's reason.
func assertErrorBody(t *testing.T, resp *http.Response, want string) {
	t.Helper()
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body["error"] != want {
		t.Errorf("error = %q, want %q", body["error"], want)
	}
}

// decodeLogRecords parses newline-delimited JSON slog output.
func decodeLogRecords(t *testing.T, out []byte) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		records = append(records, rec)
	}
	return records
}
