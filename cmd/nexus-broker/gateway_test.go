package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/frankbardon/nexus/pkg/brokerframe"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// newTestGateway spins up an httptest server hosting the gateway endpoints with
// authentication DISABLED, and returns the ws:// base URL plus the registry and a
// cleanup func.
func newTestGateway(t *testing.T) (string, *Registry) {
	t.Helper()
	wsURL, _, reg := newTestGatewayWithAuth(t, nil)
	return wsURL, reg
}

// newTestGatewayWithAuth is newTestGateway over a specific validator chain. A nil
// chain disables authentication, which is the pre-auth wiring every other gateway
// test wants. It also returns the http:// base URL, because the ownership
// refusals are ordinary HTTP responses written before any upgrade and are
// compared byte-for-byte.
func newTestGatewayWithAuth(t *testing.T, chain *nexusauth.Chain) (wsURL, httpURL string, reg *Registry) {
	t.Helper()
	env := newTestGatewayEnv(t, chain, nil)
	return env.wsURL, env.httpURL, env.reg
}

// testGatewayEnv is a running gateway plus everything a ticket test needs to
// drive it: both base URLs, the registry, the SAME ticket store the gateway
// redeems from, and the clock both of them run on.
//
// The store is handed back rather than reconstructed because a fixture-local copy
// would let a test mint into a store the served gateway never consults — the
// tests would pass while proving nothing. The clock is shared for the same reason
// TTL assertions advance time instead of sleeping.
type testGatewayEnv struct {
	wsURL   string
	httpURL string
	reg     *Registry
	tickets *ticketStore
	clk     *fakeClock
}

// newTestGatewayEnv wires a gateway exactly as run() does — guard plus ticket
// store, the store's inert/live state decided by the guard, and the registry
// invalidating a lease's tickets on teardown — over an httptest server.
//
// A nil chain disables authentication (and therefore ticket issuance). A nil
// logger discards output; a test that asserts on log records passes its own.
func newTestGatewayEnv(t *testing.T, chain *nexusauth.Chain, logger *slog.Logger) *testGatewayEnv {
	t.Helper()
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	clk := newFakeClock()
	registry := NewRegistry(logger, 0)
	registry.now = clk.now
	guard := newAuthGuard(logger, chain)
	tickets := newTicketStore(logger, guard.enabled())
	tickets.now = clk.now
	registry.useTicketStore(tickets)
	gateway := NewGateway(logger, registry, guard, tickets)

	mux := http.NewServeMux()
	gateway.Register(mux)
	srv := httptest.NewServer(mux)

	t.Cleanup(func() {
		gateway.Shutdown()
		srv.Close()
	})

	return &testGatewayEnv{
		wsURL:   "ws" + strings.TrimPrefix(srv.URL, "http"),
		httpURL: srv.URL,
		reg:     registry,
		tickets: tickets,
		clk:     clk,
	}
}

// mintTicket issues a ticket for leaseID bound to principalID, failing the test
// if the store refuses (which would make every assertion downstream vacuous).
func (e *testGatewayEnv) mintTicket(t *testing.T, leaseID, principalID string) string {
	t.Helper()
	value, err := e.tickets.mint(leaseID, principalID)
	if err != nil {
		t.Fatalf("mint ticket: %v", err)
	}
	if value == "" {
		t.Fatal("mint returned an empty ticket; the store is not issuing")
	}
	return value
}

// ticketURL builds the client WebSocket URL for a lease carrying a ticket in the
// query string — the browser-shaped request, since a browser can present a
// credential no other way.
func (e *testGatewayEnv) ticketURL(base, leaseID, ticket string) string {
	return base + ClientWSPath(leaseID) + "?" + ticketQueryParam + "=" + url.QueryEscape(ticket)
}

// dialLease dials the client WebSocket endpoint for a lease with an optional
// bearer token ("" sends no Authorization header at all) and returns the
// handshake outcome without asserting it, so a test can inspect a refusal. On a
// failed handshake conn is nil and resp carries the broker's response.
func dialLease(t *testing.T, wsURL, leaseID, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var opts *websocket.DialOptions
	if token != "" {
		opts = &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}},
		}
	}
	return websocket.Dial(ctx, wsURL+ClientWSPath(leaseID), opts)
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	return conn
}

func writeFrame(t *testing.T, conn *websocket.Conn, f brokerframe.Frame) {
	t.Helper()
	data, err := brokerframe.Encode(f)
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

func readFrame(t *testing.T, conn *websocket.Conn) brokerframe.Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	f, err := brokerframe.Decode(data)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return f
}

// waitFor polls cond until it returns true or the deadline elapses.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func TestGateway_RegisterAndRoundTrip(t *testing.T) {
	wsURL, registry := newTestGateway(t)

	leaseID, err := registry.NewLease(anonymousOwner())
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}

	// Instance dials back and registers, presenting the secret the claim path
	// would have recorded on the lease before exec'ing it.
	registry.SetSpawnSecret(leaseID, testSpawnSecret)
	instance := dial(t, wsURL+instanceWSPath)
	defer instance.Close(websocket.StatusNormalClosure, "")
	writeFrame(t, instance, brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalRegister,
		Secret:  testSpawnSecret,
	})
	waitFor(t, func() bool { return registry.InstanceConn(leaseID) != nil })

	// Client connects to the per-lease endpoint.
	client := dial(t, wsURL+ClientWSPath(leaseID))
	defer client.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return registry.ClientConn(leaseID) != nil })

	// Client -> instance.
	writeFrame(t, client, brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalIO,
		Payload: []byte(`{"from":"client"}`),
	})
	got := readFrame(t, instance)
	if got.Signal != brokerframe.SignalIO || string(got.Payload) != `{"from":"client"}` {
		t.Fatalf("instance got unexpected frame: %+v", got)
	}

	// Instance -> client.
	writeFrame(t, instance, brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalIO,
		Payload: []byte(`{"from":"instance"}`),
	})
	got = readFrame(t, client)
	if got.Signal != brokerframe.SignalIO || string(got.Payload) != `{"from":"instance"}` {
		t.Fatalf("client got unexpected frame: %+v", got)
	}
}

func TestGateway_RejectsUnknownLease(t *testing.T) {
	wsURL, _ := newTestGateway(t)

	instance := dial(t, wsURL+instanceWSPath)
	defer instance.Close(websocket.StatusNormalClosure, "")
	writeFrame(t, instance, brokerframe.Frame{
		LeaseID: "does-not-exist",
		Signal:  brokerframe.SignalRegister,
	})

	// The gateway must close the connection; the next read fails.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := instance.Read(ctx); err == nil {
		t.Fatal("expected connection to be closed for unknown lease")
	}
}

func TestGateway_RejectsNonRegisterFirstFrame(t *testing.T) {
	wsURL, registry := newTestGateway(t)
	leaseID, err := registry.NewLease(anonymousOwner())
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}

	instance := dial(t, wsURL+instanceWSPath)
	defer instance.Close(websocket.StatusNormalClosure, "")
	writeFrame(t, instance, brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalIO,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := instance.Read(ctx); err == nil {
		t.Fatal("expected connection to be closed for non-register first frame")
	}
}

func TestGateway_ClientUnknownLease404(t *testing.T) {
	wsURL, _ := newTestGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := websocket.Dial(ctx, wsURL+ClientWSPath("nope"), nil); err == nil {
		t.Fatal("expected client dial to a unknown lease to fail")
	}
}

// TestGatewayClient_OwnerConnectsNonOwnerRefused is the WS half of ownership
// enforcement: the principal that owns a lease attaches, and a DIFFERENT
// principal presenting a perfectly valid credential is refused with the same 404
// an unknown lease gets.
//
// The refusal is asserted at the handshake — status 404, never 101 — and by the
// absence of an attached client connection, which together prove the refusal
// preceded websocket.Accept. An accept-then-close would satisfy neither.
func TestGatewayClient_OwnerConnectsNonOwnerRefused(t *testing.T) {
	wsURL, _, reg := newTestGatewayWithAuth(t, mustAuthChain(t, twoPrincipalAuthYAML))

	leaseID, err := reg.NewLease(nexusauth.Principal{ID: ownerPrincipal})
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}

	// A valid credential for the WRONG principal is refused.
	conn, resp, err := dialLease(t, wsURL, leaseID, otherToken)
	if err == nil {
		conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("a non-owner completed the client WebSocket handshake")
	}
	if resp == nil {
		t.Fatalf("non-owner dial returned no response: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("non-owner handshake status = %d, want 404 (identical to an unknown lease)", resp.StatusCode)
	}
	if got := reg.ClientConn(leaseID); got != nil {
		t.Error("a refused client was attached to the lease: the check ran after the upgrade")
	}

	// The owner's own credential still gets through, so the 404 above means
	// "refused", not "this endpoint is broken".
	owner, _, err := dialLease(t, wsURL, leaseID, ownerToken)
	if err != nil {
		t.Fatalf("the lease owner was refused its own lease: %v", err)
	}
	defer owner.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return reg.ClientConn(leaseID) != nil })
}

// TestGatewayClient_UnknownAndUnownedRefusalsAreIdentical pins the oracle
// property on the WS route. Both requests are made as a valid-but-unentitled
// principal; the responses must be byte-identical, so the caller cannot learn
// whether the lease id it guessed exists.
//
// The requests are plain GETs with no upgrade headers on purpose: the refusal is
// written before websocket.Accept is ever reached, so this compares exactly the
// bytes a dialing client would receive.
func TestGatewayClient_UnknownAndUnownedRefusalsAreIdentical(t *testing.T) {
	_, httpURL, reg := newTestGatewayWithAuth(t, mustAuthChain(t, twoPrincipalAuthYAML))

	leaseID, err := reg.NewLease(nexusauth.Principal{ID: ownerPrincipal})
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}

	unknown := doAuthed(t, http.MethodGet, httpURL+ClientWSPath("no-such-lease"), otherToken, "")
	unowned := doAuthed(t, http.MethodGet, httpURL+ClientWSPath(leaseID), otherToken, "")
	assertIdenticalRefusals(t, http.StatusNotFound, unknown, unowned)
}

// TestGatewayClient_MissingOrBadCredentialIsRefused covers the two credential
// failures on the client WS route while auth is ON. Neither reaches the registry,
// so neither can leak whether the lease exists — the lease used here does exist.
func TestGatewayClient_MissingOrBadCredentialIsRefused(t *testing.T) {
	wsURL, _, reg := newTestGatewayWithAuth(t, mustAuthChain(t, twoPrincipalAuthYAML))

	leaseID, err := reg.NewLease(nexusauth.Principal{ID: ownerPrincipal})
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}

	for _, tc := range []struct{ name, token string }{
		{"no credential", ""},
		{"unconfigured credential", "not-a-configured-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, resp, err := dialLease(t, wsURL, leaseID, tc.token)
			if err == nil {
				conn.Close(websocket.StatusNormalClosure, "")
				t.Fatal("handshake completed without a valid credential")
			}
			if resp == nil {
				t.Fatalf("dial returned no response: %v", err)
			}
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("handshake status = %d, want 401", resp.StatusCode)
			}
			if got := reg.ClientConn(leaseID); got != nil {
				t.Error("a refused client was attached: the check ran after the upgrade")
			}
		})
	}
}

// TestGatewayClient_AuthDisabledAllowsAnonymousConnect is the
// backward-compatibility guarantee, asserted explicitly rather than inferred:
// with no `auth:` block every lease is stamped with anonymousOwner() and every
// caller resolves to it, so an anonymous client attaches exactly as it did before
// ownership existed — no credential, no refusal.
func TestGatewayClient_AuthDisabledAllowsAnonymousConnect(t *testing.T) {
	wsURL, _, reg := newTestGatewayWithAuth(t, nil)

	leaseID, err := reg.NewLease(anonymousOwner())
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}

	conn, _, err := dialLease(t, wsURL, leaseID, "")
	if err != nil {
		t.Fatalf("anonymous client refused with auth disabled: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return reg.ClientConn(leaseID) != nil })
}

// ---------------------------------------------------------------------------
// Spawn secret on the instance dial-back
// ---------------------------------------------------------------------------

// syncBuffer is a mutex-guarded log sink. slog handlers are safe for concurrent
// use but the io.Writer beneath them must be too, and a test that reads its own
// log output while gateway goroutines are still writing to it needs that.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// seedSecretLease mints a lease with a known spawn secret, the way the claim
// path does before invoking the runner. It returns the lease id.
func seedSecretLease(t *testing.T, reg *Registry, secret string) string {
	t.Helper()
	id, err := reg.NewLease(anonymousOwner())
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}
	reg.SetSpawnSecret(id, secret)
	return id
}

// sendRegisterFrame writes a register frame with an EXPLICIT schema version.
//
// It marshals by hand rather than through brokerframe.Encode because Encode
// always stamps the current version — there is no way to put a skewed frame on
// the wire through it, which is the whole point of the version being a wire
// value rather than a compile-time one. The field set matches Frame's tags,
// including `secret,omitempty`, so an empty secret produces the same bytes an
// instance binary predating the field would send: no `secret` key at all.
func sendRegisterFrame(t *testing.T, conn *websocket.Conn, version int, leaseID, secret string) {
	t.Helper()
	data, err := json.Marshal(struct {
		Version int    `json:"version"`
		LeaseID string `json:"lease_id"`
		Signal  string `json:"signal"`
		Secret  string `json:"secret,omitempty"`
	}{version, leaseID, string(brokerframe.SignalRegister), secret})
	if err != nil {
		t.Fatalf("marshal register frame: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write register frame: %v", err)
	}
}

// registerInstance dials the instance endpoint, sends a current-version register
// frame carrying secret, and reports whether the gateway ACCEPTED it — observed
// as the socket staying open and an instance connection appearing on the lease,
// not as the absence of an immediate error (the dial itself always succeeds; the
// refusal is a close on the established socket).
func registerInstance(t *testing.T, wsURL string, reg *Registry, leaseID, secret string) bool {
	t.Helper()
	return registerInstanceAtVersion(t, wsURL, reg, brokerframe.Version, leaseID, secret)
}

// registerInstanceAtVersion is registerInstance for a chosen schema version.
func registerInstanceAtVersion(t *testing.T, wsURL string, reg *Registry, version int, leaseID, secret string) bool {
	t.Helper()
	conn := dial(t, wsURL+instanceWSPath)
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	sendRegisterFrame(t, conn, version, leaseID, secret)

	// Poll for the attach rather than reading: an ACCEPTED registration sends
	// nothing back, so a read would block until the deadline in the success case.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if reg.InstanceConn(leaseID) != nil {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// registerRefusal is the observable outcome of a refused dial-back: the close
// code and reason the gateway hung up with. Every cause must produce the same
// pair, so tests compare these values rather than asserting on each separately.
type registerRefusal struct {
	code   websocket.StatusCode
	reason string
}

// refuseRegister sends a register frame and returns what the gateway closed
// with. It fails the test if the gateway did NOT refuse, or if it attached the
// instance anyway.
func refuseRegister(t *testing.T, wsURL string, reg *Registry, version int, leaseID, secret string) registerRefusal {
	t.Helper()
	conn := dial(t, wsURL+instanceWSPath)
	defer conn.Close(websocket.StatusNormalClosure, "")
	sendRegisterFrame(t, conn, version, leaseID, secret)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if err == nil {
		t.Fatal("the gateway kept the connection open; the registration was not refused")
	}
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("the socket failed without a close frame (%v); the refusal must be an explicit close", err)
	}
	if reg != nil && leaseID != "" && reg.InstanceConn(leaseID) != nil {
		t.Error("a refused instance was attached to the lease")
	}
	return registerRefusal{code: ce.Code, reason: ce.Reason}
}

// assertRegisterRefused fails unless the gateway both declined to attach the
// instance AND closed the socket with the policy-violation close it uses for an
// unknown lease — the two halves of "refused indistinguishably".
func assertRegisterRefused(t *testing.T, wsURL string, reg *Registry, leaseID, secret string) {
	t.Helper()
	got := refuseRegister(t, wsURL, reg, brokerframe.Version, leaseID, secret)
	if got.code != websocket.StatusPolicyViolation {
		t.Errorf("close status = %v, want %v (the same close an unknown lease gets)",
			got.code, websocket.StatusPolicyViolation)
	}
}

// TestGatewayInstance_SpawnSecretEnforcedWhenAuthEnabled covers the register
// frame's secret on a broker that authenticates its clients: it must be the one
// the broker minted for that lease.
//
// The `auth:` block is no longer what turns the check on — see
// TestGatewayInstance_SpawnSecretRequiredWithNoAuthBlock for the same assertions
// without one — but an authenticated broker is the deployment an operator is
// most likely to be reasoning about, so it keeps its own coverage.
//
// Each subtest gets its OWN lease because a successful attach is single-shot
// (a lease refuses a second instance connection), which would otherwise make a
// later refusal ambiguous. The wrong-secret and absent-secret cases are followed
// by a correct-secret registration ON THE SAME lease and gateway: without that,
// a gateway that refused every dial-back would pass both of them.
func TestGatewayInstance_SpawnSecretEnforcedWhenAuthEnabled(t *testing.T) {
	const secret = "b8d3f0a15c274e6981aa4f0c2e7d5b93"

	t.Run("matching secret registers", func(t *testing.T) {
		env := newTestGatewayEnv(t, mustAuthChain(t, twoPrincipalAuthYAML), nil)
		leaseID := seedSecretLease(t, env.reg, secret)
		if !registerInstance(t, env.wsURL, env.reg, leaseID, secret) {
			t.Fatal("an instance presenting the minted secret was refused")
		}
	})

	t.Run("wrong secret refused", func(t *testing.T) {
		env := newTestGatewayEnv(t, mustAuthChain(t, twoPrincipalAuthYAML), nil)
		leaseID := seedSecretLease(t, env.reg, secret)

		// A well-formed value of the same shape — this is what someone who
		// learned the lease id (from a ws_url, a log, a client request) but not
		// the secret would present.
		assertRegisterRefused(t, env.wsURL, env.reg, leaseID, "00000000000000000000000000000000")

		// Non-vacuity: the SAME lease on the SAME gateway accepts the real
		// secret, so the refusal above is the secret check and not a broken
		// endpoint.
		if !registerInstance(t, env.wsURL, env.reg, leaseID, secret) {
			t.Fatal("the correct secret was also refused; the rejection above proves nothing")
		}
	})

	t.Run("absent secret refused", func(t *testing.T) {
		env := newTestGatewayEnv(t, mustAuthChain(t, twoPrincipalAuthYAML), nil)
		leaseID := seedSecretLease(t, env.reg, secret)

		// This is the version-skew shape: a register frame with no secret at all,
		// exactly what a nexus binary predating the protocol sends.
		assertRegisterRefused(t, env.wsURL, env.reg, leaseID, "")

		if !registerInstance(t, env.wsURL, env.reg, leaseID, secret) {
			t.Fatal("the correct secret was also refused; the rejection above proves nothing")
		}
	})
}

// TestGatewayInstance_SpawnSecretRequiredWithNoAuthBlock is the behaviour
// change this story exists for, and the security boundary the broker used to
// leave open.
//
// Enforcement used to be gated on the broker having an `auth:` block. With no
// block — the documented default — the presented secret was not consulted at
// all, so the dial-back socket was authenticated by the lease id alone. Lease
// ids are not secret: they travel in ws_urls, client requests and logs, so
// anything that saw one could register as that lease's instance the moment its
// real socket dropped and inherit the client's session.
//
// Every subtest runs on a gateway with a nil auth chain. The refusal cases are
// each followed by a correct registration ON THE SAME lease, so a gateway that
// simply refused every dial-back could not pass them.
func TestGatewayInstance_SpawnSecretRequiredWithNoAuthBlock(t *testing.T) {
	const secret = "c7e2b91d40a6538f2ba1cd07e4936f15"

	newEnv := func(t *testing.T) *testGatewayEnv {
		t.Helper()
		env := newTestGatewayEnv(t, nil, nil)
		if env.tickets.issuing() {
			t.Fatal("precondition: auth must be OFF for this fixture")
		}
		return env
	}

	t.Run("matching secret registers", func(t *testing.T) {
		env := newEnv(t)
		leaseID := seedSecretLease(t, env.reg, secret)
		if !registerInstance(t, env.wsURL, env.reg, leaseID, secret) {
			t.Fatal("an instance presenting the minted secret was refused")
		}
	})

	t.Run("absent secret refused", func(t *testing.T) {
		env := newEnv(t)
		leaseID := seedSecretLease(t, env.reg, secret)

		// No `secret` key on the wire at all: what an instance binary predating
		// the protocol sends, and what USED to be admitted here.
		assertRegisterRefused(t, env.wsURL, env.reg, leaseID, "")

		if !registerInstance(t, env.wsURL, env.reg, leaseID, secret) {
			t.Fatal("the correct secret was also refused; the rejection above proves nothing")
		}
	})

	t.Run("wrong secret refused", func(t *testing.T) {
		env := newEnv(t)
		leaseID := seedSecretLease(t, env.reg, secret)

		// A well-formed value of the same shape — what someone who learned the
		// lease id but not the secret would present.
		assertRegisterRefused(t, env.wsURL, env.reg, leaseID, "00000000000000000000000000000000")

		if !registerInstance(t, env.wsURL, env.reg, leaseID, secret) {
			t.Fatal("the correct secret was also refused; the rejection above proves nothing")
		}
	})

	t.Run("skewed frame version refused even with the right secret", func(t *testing.T) {
		env := newEnv(t)
		leaseID := seedSecretLease(t, env.reg, secret)

		// The secret is CORRECT here. Only the declared schema version is wrong,
		// so nothing but the version check can be doing the refusing.
		got := refuseRegister(t, env.wsURL, env.reg, brokerframe.Version+1, leaseID, secret)
		if got.code != websocket.StatusPolicyViolation {
			t.Errorf("close status = %v, want %v", got.code, websocket.StatusPolicyViolation)
		}

		if !registerInstanceAtVersion(t, env.wsURL, env.reg, brokerframe.Version, leaseID, secret) {
			t.Fatal("the current version was also refused; the rejection above proves nothing")
		}
	})
}

// TestGatewayInstance_RefusalsAreIndistinguishableOnTheWire is the
// non-enumeration rule, asserted across ALL FOUR causes at once.
//
// A dialer that could tell "no such lease" from "that lease exists and you got
// its secret wrong" could enumerate live lease ids by differencing the two —
// which would hand back exactly what the secret was added to take away. So every
// refusal closes with the same code and the same reason, and the causes are
// distinguished only in the broker's own log (asserted below).
func TestGatewayInstance_RefusalsAreIndistinguishableOnTheWire(t *testing.T) {
	const minted = "5a0c8e37b1d94f26a8073cbe15d2f409"

	// Each cause gets its own gateway and its own log sink, so the message
	// captured for one cannot be another's.
	type cause struct {
		name string
		// seed returns the lease id to register against; "" means never minted.
		seed    func(t *testing.T, env *testGatewayEnv) string
		version int
		secret  string
	}
	live := func(t *testing.T, env *testGatewayEnv) string {
		t.Helper()
		return seedSecretLease(t, env.reg, minted)
	}
	causes := []cause{
		{
			name:    "unknown lease",
			seed:    func(*testing.T, *testGatewayEnv) string { return "no-such-lease-id" },
			version: brokerframe.Version,
			secret:  minted,
		},
		{
			name:    "absent secret",
			seed:    live,
			version: brokerframe.Version,
			secret:  "",
		},
		{
			name:    "wrong secret",
			seed:    live,
			version: brokerframe.Version,
			secret:  "00000000000000000000000000000000",
		},
		{
			name:    "skewed frame version",
			seed:    live,
			version: brokerframe.Version + 1,
			secret:  minted,
		},
	}

	var (
		wire = make([]registerRefusal, 0, len(causes))
		logs = make(map[string]string, len(causes))
	)
	for _, c := range causes {
		var buf syncBuffer
		logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		env := newTestGatewayEnv(t, nil, logger)
		leaseID := c.seed(t, env)

		wire = append(wire, refuseRegister(t, env.wsURL, env.reg, c.version, leaseID, c.secret))

		msg := refusalLogMessage(t, buf.String())
		if msg == "" {
			t.Fatalf("%s: no refusal record was logged; the distinguishability check would be vacuous", c.name)
		}
		if prev, dup := logs[msg]; dup {
			t.Errorf("%s and %s log the SAME message, so an operator cannot tell them apart:\n%s",
				c.name, prev, msg)
		}
		logs[msg] = c.name
	}

	// The wire outcome is identical for all four.
	for i, got := range wire {
		if got != wire[0] {
			t.Errorf("%s closed with %v/%q but %s closed with %v/%q; the two are distinguishable on the wire",
				causes[i].name, got.code, got.reason,
				causes[0].name, wire[0].code, wire[0].reason)
		}
	}
	if wire[0].code != websocket.StatusPolicyViolation {
		t.Errorf("close status = %v, want %v", wire[0].code, websocket.StatusPolicyViolation)
	}
	if wire[0].reason != instanceRefusedReason {
		t.Errorf("close reason = %q, want %q", wire[0].reason, instanceRefusedReason)
	}
	// And all four are distinct in the log.
	if len(logs) != len(causes) {
		t.Errorf("%d distinct refusal messages for %d causes: %v", len(logs), len(causes), logs)
	}
}

// refusalLogMessage extracts the `msg` of the first refusal record in JSON log
// output. It returns "" when there is none.
func refusalLogMessage(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON (%v): %s", err, line)
		}
		if strings.HasPrefix(rec.Msg, "rejecting instance registration") {
			return rec.Msg
		}
	}
	return ""
}

// TestGatewayInstance_SpawnSecretIsNeverLogged pins the non-disclosure rule on
// the one component that handles both the expected and the presented value.
//
// Both an admitted and a refused registration are exercised at DEBUG level, and
// the assertion is on the raw log bytes rather than on any particular field, so a
// value smuggled into a message string or an error would fail it just the same.
func TestGatewayInstance_SpawnSecretIsNeverLogged(t *testing.T) {
	const (
		minted    = "a1b2c3d4e5f60718293a4b5c6d7e8f90"
		presented = "ffeeddccbbaa99887766554433221100"
	)
	// A synchronized sink, not a bare bytes.Buffer: an admitted registration
	// leaves a read pump running, so the gateway is still logging (read pump
	// ended, instance disconnected) on its own goroutine while this test reads
	// the output back.
	var buf syncBuffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	env := newTestGatewayEnv(t, mustAuthChain(t, twoPrincipalAuthYAML), logger)

	// A refusal for a WRONG secret: the presented value must not be logged.
	mismatchLease := seedSecretLease(t, env.reg, minted)
	assertRegisterRefused(t, env.wsURL, env.reg, mismatchLease, presented)

	// A refusal for an ABSENT secret: the pre-protocol binary's shape, whose
	// record must name the cause.
	oldBinaryLease := seedSecretLease(t, env.reg, minted)
	assertRegisterRefused(t, env.wsURL, env.reg, oldBinaryLease, "")

	// A refusal for a SKEWED frame version, carrying the correct secret — so the
	// record must name the skew without disclosing the value it never checked.
	skewLease := seedSecretLease(t, env.reg, minted)
	if got := refuseRegister(t, env.wsURL, env.reg, brokerframe.Version+1, skewLease, minted); got.code != websocket.StatusPolicyViolation {
		t.Fatalf("skewed register close status = %v, want %v", got.code, websocket.StatusPolicyViolation)
	}

	// And an admitted registration, so the expected value passes through the
	// success path too.
	admittedLease := seedSecretLease(t, env.reg, minted)
	if !registerInstance(t, env.wsURL, env.reg, admittedLease, minted) {
		t.Fatal("the correct secret was refused; the log assertions would cover the wrong path")
	}

	out := buf.String()
	if out == "" {
		t.Fatal("no log output captured; the assertions below would be vacuous")
	}
	for name, value := range map[string]string{"minted": minted, "presented": presented} {
		if strings.Contains(out, value) {
			t.Errorf("the %s spawn secret VALUE appears in the log output:\n%s", name, out)
		}
	}
	// Non-vacuity: the events WERE recorded and name their leases, so the
	// absences above are the values being withheld rather than nothing having
	// been logged.
	if !strings.Contains(out, "instance registered") || !strings.Contains(out, admittedLease) {
		t.Errorf("no registration record naming the admitted lease:\n%s", out)
	}
	if !strings.Contains(out, "rejecting instance registration") || !strings.Contains(out, mismatchLease) {
		t.Errorf("no refusal record naming the refused lease:\n%s", out)
	}
	// And the absent-secret refusal says out loud that the binary may predate the
	// protocol, because an operator who cannot see that will chase a network
	// fault instead.
	if !strings.Contains(out, "predates the spawn-secret protocol") {
		t.Errorf("the absent-secret refusal does not name the pre-protocol cause:\n%s", out)
	}
	// The version-skew refusal names BOTH versions and the fix. Without them the
	// symptom (a claim timing out) is indistinguishable from a network fault.
	for _, want := range []string{
		"schema version this broker does not speak",
		"upgrade whichever is older",
		`"instance_frame_version":` + strconv.Itoa(brokerframe.Version+1),
		`"broker_frame_version":` + strconv.Itoa(brokerframe.Version),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the version-skew refusal does not carry %q:\n%s", want, out)
		}
	}
}

// ---------------------------------------------------------------------------
// `?ticket=` on the client WebSocket
// ---------------------------------------------------------------------------

// dialLeaseWith dials the client WebSocket for a lease presenting any
// combination of credentials: a ticket in the query string (the browser's only
// option), a bearer token in the header (the Go/CLI path), both, or neither. It
// asserts nothing, so a caller can inspect a refusal; on a failed handshake conn
// is nil and resp carries the broker's response.
func dialLeaseWith(t *testing.T, env *testGatewayEnv, leaseID, ticket, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	target := env.wsURL + ClientWSPath(leaseID)
	if ticket != "" {
		target = env.ticketURL(env.wsURL, leaseID, ticket)
	}
	var opts *websocket.DialOptions
	if token != "" {
		opts = &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}},
		}
	}
	return websocket.Dial(ctx, target, opts)
}

// newOwnedLease mints a lease owned by principalID.
func newOwnedLease(t *testing.T, reg *Registry, principalID string) string {
	t.Helper()
	id, err := reg.NewLease(nexusauth.Principal{ID: principalID})
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}
	return id
}

// assertRefusedBeforeUpgrade fails unless a dial was refused with wantStatus and
// no socket was ever opened.
//
// The status assertion is the load-bearing one: a handshake that reached `101`
// and was then closed is observably different from a clean refusal (it confirms
// the lease id reached a live handler), which is exactly what the
// refuse-before-accept rule exists to prevent.
func assertRefusedBeforeUpgrade(t *testing.T, wantStatus int, conn *websocket.Conn, resp *http.Response, err error) {
	t.Helper()
	if err == nil {
		conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("the handshake completed; the caller should have been refused")
	}
	if resp == nil {
		t.Fatalf("refused dial returned no response: %v", err)
	}
	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("handshake status = 101: the socket was upgraded and then closed, not refused")
	}
	if resp.StatusCode != wantStatus {
		t.Errorf("handshake status = %d, want %d", resp.StatusCode, wantStatus)
	}
}

// TestGatewayClient_TicketConnectsAndBurns is the browser path at the unit level:
// a ticket minted for {lease, principal} opens the socket, is consumed by doing
// so, and cannot be replayed.
//
// The replay attempt is made only AFTER the first client has detached, so the
// lease is in the same state it was for the first connect and nothing about the
// attach differs; and a FRESH ticket is redeemed at the end, so the refusal in
// the middle means "that ticket is spent" rather than "this endpoint stopped
// working".
func TestGatewayClient_TicketConnectsAndBurns(t *testing.T) {
	env := newTestGatewayEnv(t, mustAuthChain(t, twoPrincipalAuthYAML), nil)
	leaseID := newOwnedLease(t, env.reg, ownerPrincipal)
	ticket := env.mintTicket(t, leaseID, ownerPrincipal)

	conn, resp, err := dialLeaseWith(t, env, leaseID, ticket, "")
	if err != nil {
		t.Fatalf("ticket-bearing client refused: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("handshake status = %d, want 101", resp.StatusCode)
	}
	waitFor(t, func() bool { return env.reg.ClientConn(leaseID) != nil })

	// Redemption consumed it: the store holds nothing, which is the burn observed
	// at the source rather than inferred from the second refusal.
	if got := env.tickets.outstanding(); got != 0 {
		t.Errorf("outstanding tickets after a ticket connect = %d, want 0 (the ticket did not burn)", got)
	}

	// Detach first, so the replay below is judged against a lease in exactly the
	// state the first connect found it in.
	conn.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return env.reg.ClientConn(leaseID) == nil })

	replayConn, replayResp, replayErr := dialLeaseWith(t, env, leaseID, ticket, "")
	assertRefusedBeforeUpgrade(t, http.StatusUnauthorized, replayConn, replayResp, replayErr)
	if got := env.reg.ClientConn(leaseID); got != nil {
		t.Error("a replayed ticket attached a client: the check ran after the upgrade")
	}

	// Non-vacuity: a freshly minted ticket for the same lease still connects.
	fresh := env.mintTicket(t, leaseID, ownerPrincipal)
	freshConn, _, err := dialLeaseWith(t, env, leaseID, fresh, "")
	if err != nil {
		t.Fatalf("a fresh ticket was refused, so the replay refusal proves nothing: %v", err)
	}
	freshConn.Close(websocket.StatusNormalClosure, "")
}

// TestGatewayClient_BadTicketRefusalsAreIndistinguishable pins the property that
// makes a ticket safe to probe against: every way a ticket can fail answers
// identically, so a holder of one value learns nothing about any other.
//
// The three cases reach the store by different routes — an unknown value misses
// the index, an expired one is found and dropped, a wrong-lease one is found and
// deliberately KEPT — and all three must be one response.
//
// The requests are plain GETs with no upgrade headers on purpose: the refusal is
// written before websocket.Accept is reached, so this compares exactly the bytes a
// dialing client receives.
func TestGatewayClient_BadTicketRefusalsAreIndistinguishable(t *testing.T) {
	env := newTestGatewayEnv(t, mustAuthChain(t, twoPrincipalAuthYAML), nil)
	leaseID := newOwnedLease(t, env.reg, ownerPrincipal)
	otherLeaseID := newOwnedLease(t, env.reg, ownerPrincipal)

	// Mint the doomed ticket, then advance past the TTL rather than sleeping (both
	// the store and the registry run on this clock), and only then mint the
	// wrong-lease one — so it is genuinely live and refused for its binding rather
	// than for having expired alongside.
	expired := env.mintTicket(t, leaseID, ownerPrincipal)
	env.clk.advance(ticketTTL)
	forOtherLease := env.mintTicket(t, otherLeaseID, ownerPrincipal)

	type refusal struct {
		name        string
		status      int
		contentType string
		body        []byte
	}
	var got []refusal
	for _, tc := range []struct{ name, ticket string }{
		{"expired ticket", expired},
		{"ticket minted for another lease", forOtherLease},
		{"garbage ticket", "0123456789abcdef"},
	} {
		resp := doAuthed(t, http.MethodGet, env.ticketURL(env.httpURL, leaseID, tc.ticket), "", "")
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("%s: read body: %v", tc.name, err)
		}
		got = append(got, refusal{
			name:        tc.name,
			status:      resp.StatusCode,
			contentType: resp.Header.Get("Content-Type"),
			body:        body,
		})
	}

	want := got[0]
	if want.status != http.StatusUnauthorized {
		t.Errorf("%s status = %d, want 401", want.name, want.status)
	}
	for _, r := range got[1:] {
		if r.status != want.status || r.contentType != want.contentType || !bytes.Equal(r.body, want.body) {
			t.Errorf("%q and %q are distinguishable — a ticket-probing oracle:\n %s: %d %q %q\n %s: %d %q %q",
				want.name, r.name,
				want.name, want.status, want.contentType, want.body,
				r.name, r.status, r.contentType, r.body)
		}
	}

	// A wrong-lease attempt is an authorization failure, not a use: the ticket must
	// still be good for the lease it was minted for. Preserving that is why the
	// store refuses without consuming — a stale reconnect must not be able to
	// destroy a credential the legitimate holder still needs.
	conn, _, err := dialLeaseWith(t, env, otherLeaseID, forOtherLease, "")
	if err != nil {
		t.Fatalf("a wrong-lease attempt consumed the ticket; its own lease can no longer redeem it: %v", err)
	}
	conn.Close(websocket.StatusNormalClosure, "")
}

// TestGatewayClient_TicketPrincipalMustStillOwnTheLease pins the deliberate
// belt-and-braces decision: a redeemed ticket does NOT bypass the ownership check.
//
// The ticket here is well-formed and redeems cleanly for this exact lease id — the
// store binds {lease, principal} and does not authorize — but it names a principal
// that does not own the lease, so the connection is refused with the same 404 an
// unknown lease gets. Without the second check this would connect.
func TestGatewayClient_TicketPrincipalMustStillOwnTheLease(t *testing.T) {
	env := newTestGatewayEnv(t, mustAuthChain(t, twoPrincipalAuthYAML), nil)
	leaseID := newOwnedLease(t, env.reg, ownerPrincipal)
	ticket := env.mintTicket(t, leaseID, otherPrincipal)

	conn, resp, err := dialLeaseWith(t, env, leaseID, ticket, "")
	assertRefusedBeforeUpgrade(t, http.StatusNotFound, conn, resp, err)
	if got := env.reg.ClientConn(leaseID); got != nil {
		t.Error("a non-owning ticket attached a client to the lease")
	}

	// ...and identically to a lease that never existed, so the ownership refusal is
	// not a lease-id oracle either.
	unknownTicket := env.mintTicket(t, "no-such-lease", otherPrincipal)
	unknown := doAuthed(t, http.MethodGet, env.ticketURL(env.httpURL, "no-such-lease", unknownTicket), "", "")
	unownedTicket := env.mintTicket(t, leaseID, otherPrincipal)
	unowned := doAuthed(t, http.MethodGet, env.ticketURL(env.httpURL, leaseID, unownedTicket), "", "")
	assertIdenticalRefusals(t, http.StatusNotFound, unknown, unowned)
}

// TestGatewayClient_TicketTakesPrecedenceOverBearer pins the documented
// precedence in BOTH directions, which is what makes it a rule rather than an
// accident of evaluation order.
//
// A good ticket alongside a good header is admitted BY THE TICKET (proven by the
// burn: the header path consumes nothing), and a bad ticket alongside a good
// header is REFUSED. The second half is the one that matters: falling back to the
// header would make "the ticket burns on connect" conditional on the caller not
// also holding a token.
func TestGatewayClient_TicketTakesPrecedenceOverBearer(t *testing.T) {
	t.Run("valid ticket wins and is consumed", func(t *testing.T) {
		env := newTestGatewayEnv(t, mustAuthChain(t, twoPrincipalAuthYAML), nil)
		leaseID := newOwnedLease(t, env.reg, ownerPrincipal)
		ticket := env.mintTicket(t, leaseID, ownerPrincipal)

		conn, _, err := dialLeaseWith(t, env, leaseID, ticket, ownerToken)
		if err != nil {
			t.Fatalf("a caller presenting both credentials was refused: %v", err)
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		waitFor(t, func() bool { return env.reg.ClientConn(leaseID) != nil })

		if got := env.tickets.outstanding(); got != 0 {
			t.Errorf("outstanding tickets = %d, want 0: the header was used and the ticket left unredeemed", got)
		}
	})

	t.Run("bad ticket is final even with a valid header", func(t *testing.T) {
		env := newTestGatewayEnv(t, mustAuthChain(t, twoPrincipalAuthYAML), nil)
		leaseID := newOwnedLease(t, env.reg, ownerPrincipal)
		spent := env.mintTicket(t, leaseID, ownerPrincipal)
		if _, ok := env.tickets.redeem(spent, leaseID); !ok {
			t.Fatal("precondition: could not spend the ticket")
		}

		conn, resp, err := dialLeaseWith(t, env, leaseID, spent, ownerToken)
		assertRefusedBeforeUpgrade(t, http.StatusUnauthorized, conn, resp, err)

		// Non-vacuity: the same header with NO ticket connects, so the refusal above
		// is the ticket being spent and not a broken credential.
		bearer, _, err := dialLeaseWith(t, env, leaseID, "", ownerToken)
		if err != nil {
			t.Fatalf("the bearer path is broken, so the precedence assertion proves nothing: %v", err)
		}
		bearer.Close(websocket.StatusNormalClosure, "")
	})
}

// TestGatewayClient_AuthDisabledIgnoresTicketParam is the backward-compatibility
// guarantee for the ticket path specifically: with no `auth:` block the route
// consults nothing and answers on `registry.Has(leaseID)` alone.
//
// The failure this prevents is real and easy to ship: an inert store issues
// nothing, so a client built against an authenticated broker and pointed at an
// open one would present a `?ticket=` that redeems nowhere and get a 401 from a
// deployment that used to accept it.
func TestGatewayClient_AuthDisabledIgnoresTicketParam(t *testing.T) {
	env := newTestGatewayEnv(t, nil, nil)
	if env.tickets.issuing() {
		t.Fatal("precondition: the store must be inert with auth disabled")
	}
	leaseID := newOwnedLease(t, env.reg, anonymousOwner().ID)

	conn, resp, err := dialLeaseWith(t, env, leaseID, "a-ticket-this-broker-never-issued", "")
	if err != nil {
		t.Fatalf("auth-disabled broker refused a ticket-bearing client: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("handshake status = %d, want 101", resp.StatusCode)
	}
	waitFor(t, func() bool { return env.reg.ClientConn(leaseID) != nil })

	// ...and an unknown lease is still a 404, i.e. the route is exactly
	// "does this lease exist?" and nothing more.
	unknownConn, unknownResp, unknownErr := dialLeaseWith(t, env, "no-such-lease", "any-ticket", "")
	assertRefusedBeforeUpgrade(t, http.StatusNotFound, unknownConn, unknownResp, unknownErr)
}

// TestGatewayClient_TicketValueIsNeverLogged pins the one exposure the broker
// actually controls. A ticket travels in a URL, so it already reaches proxy access
// logs and browser history; the broker's own records must not add to that.
//
// Both an admitted and a refused connect are exercised, at DEBUG level so even the
// store's issuance record is captured, and the assertion is on the raw log bytes
// rather than on any particular field — a value smuggled into a message string, an
// error, or a URL would fail it just the same.
func TestGatewayClient_TicketValueIsNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	env := newTestGatewayEnv(t, mustAuthChain(t, twoPrincipalAuthYAML), logger)
	leaseID := newOwnedLease(t, env.reg, ownerPrincipal)

	admitted := env.mintTicket(t, leaseID, ownerPrincipal)
	conn, _, err := dialLeaseWith(t, env, leaseID, admitted, "")
	if err != nil {
		t.Fatalf("ticket-bearing client refused: %v", err)
	}
	waitFor(t, func() bool { return env.reg.ClientConn(leaseID) != nil })
	conn.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return env.reg.ClientConn(leaseID) == nil })

	const refused = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	refusedConn, refusedResp, refusedErr := dialLeaseWith(t, env, leaseID, refused, "")
	assertRefusedBeforeUpgrade(t, http.StatusUnauthorized, refusedConn, refusedResp, refusedErr)

	out := buf.String()
	if out == "" {
		t.Fatal("no log output captured; the assertion below would be vacuous")
	}
	for name, value := range map[string]string{"admitted": admitted, "refused": refused} {
		if strings.Contains(out, value) {
			t.Errorf("the %s ticket VALUE appears in the log output:\n%s", name, out)
		}
	}
	// Non-vacuity: the connection WAS logged, so the absence above is the value
	// being withheld rather than nothing having been recorded.
	if !strings.Contains(out, "client connected") || !strings.Contains(out, leaseID) {
		t.Errorf("no client-connect record naming the lease; the value assertions prove nothing:\n%s", out)
	}
}
