package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := NewRegistry(logger, 0)
	gateway := NewGateway(logger, registry, newAuthGuard(logger, chain))

	mux := http.NewServeMux()
	gateway.Register(mux)
	srv := httptest.NewServer(mux)

	t.Cleanup(func() {
		gateway.Shutdown()
		srv.Close()
	})

	return "ws" + strings.TrimPrefix(srv.URL, "http"), srv.URL, registry
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

	// Instance dials back and registers.
	instance := dial(t, wsURL+instanceWSPath)
	defer instance.Close(websocket.StatusNormalClosure, "")
	writeFrame(t, instance, brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalRegister,
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
