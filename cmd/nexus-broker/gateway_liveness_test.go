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
)

// Liveness timings for these tests. They keep the SHAPE of the shipped
// constants — the deadline is three intervals — while running three orders of
// magnitude faster, so what the tests exercise is the real policy and not a
// degenerate one-strike variant of it.
//
// 50ms is generous for a loopback round trip even under -race: a healthy peer
// would have to miss three consecutive 50ms probes to be detached, which is the
// margin that keeps the responsive tests from flaking.
const (
	testPingInterval     = 50 * time.Millisecond
	testPeerReadDeadline = 3 * testPingInterval
)

// newLivenessGateway serves a gateway with liveness timings shrunk to
// milliseconds and authentication disabled, and returns its ws:// base URL plus
// the registry to inspect attachment state through.
func newLivenessGateway(t *testing.T) (string, *Registry) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := NewRegistry(logger, 0)
	gateway := NewGateway(logger, registry, newAuthGuard(logger, nil), nil)
	gateway.pingInterval = testPingInterval
	gateway.peerReadDeadline = testPeerReadDeadline

	mux := http.NewServeMux()
	gateway.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		gateway.Shutdown()
		srv.Close()
	})
	return "ws" + strings.TrimPrefix(srv.URL, "http"), registry
}

// livenessLease mints a lease and dials an instance onto it, returning the lease
// id and the instance socket. Every liveness test needs an attached instance:
// the client route refuses a lease that has none.
func livenessLease(t *testing.T, wsURL string, registry *Registry) (string, *websocket.Conn) {
	t.Helper()
	leaseID, err := registry.NewLease(anonymousOwner())
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}
	registry.SetSpawnSecret(leaseID, testSpawnSecret)
	instance := dial(t, wsURL+instanceWSPath)
	t.Cleanup(func() { instance.Close(websocket.StatusNormalClosure, "") })
	writeFrame(t, instance, brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalRegister,
		Secret:  testSpawnSecret,
	})
	waitFor(t, func() bool { return registry.InstanceConn(leaseID) != nil })
	return leaseID, instance
}

// answerPings keeps conn reading for the life of the test, which is what makes a
// peer "responsive": coder/websocket answers a ping from inside a blocked Read
// and nowhere else, so a peer that never reads never pongs. That asymmetry is
// what the silent tests below exploit and what this helper undoes.
func answerPings(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()
}

// waitWithin polls cond until it holds or the given budget elapses, failing with
// msg. It is waitFor with a caller-chosen deadline, because a liveness detection
// is expected within a few multiples of the read deadline rather than within the
// two seconds waitFor allows.
func waitWithin(t *testing.T, budget time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestGatewayLiveness_SilentInstanceIsDetached is the half-open socket the whole
// story exists for: an instance that is still TCP-connected but answers nothing.
// Before the liveness pump this looked perfectly healthy to the broker until the
// OS keepalive noticed, which is minutes.
//
// The peer here is a real WebSocket client that simply never reads, which is
// exactly the observable behaviour of a host that slept or moved network: the
// socket is open, writes are accepted into a send queue, and no pong ever comes
// back.
func TestGatewayLiveness_SilentInstanceIsDetached(t *testing.T) {
	wsURL, registry := newLivenessGateway(t)
	leaseID, _ := livenessLease(t, wsURL, registry)

	// Deliberately no answerPings: this instance never reads, so it never pongs.
	waitWithin(t, 10*testPeerReadDeadline,
		"an instance that stopped answering pings was still attached after the read deadline",
		func() bool { return registry.InstanceConn(leaseID) == nil })
}

// TestGatewayLiveness_ResponsiveInstanceSurvives is the criterion that matters
// as much as detection: a liveness check that false-positives is worse than
// none.
//
// The instance here sends NOTHING for the whole test and receives nothing — it
// is an idle session with no user input, the case an operator must be able to
// leave running overnight. It answers pings only because its WebSocket stack
// does so from inside a blocked read, which is the entire point of probing with
// a ping instead of timing out the read.
func TestGatewayLiveness_ResponsiveInstanceSurvives(t *testing.T) {
	wsURL, registry := newLivenessGateway(t)
	leaseID, instance := livenessLease(t, wsURL, registry)
	answerPings(t, instance)

	// Four deadlines of complete application-level silence — a dozen probes —
	// with no frame in either direction.
	deadline := time.Now().Add(4 * testPeerReadDeadline)
	for time.Now().Before(deadline) {
		if registry.InstanceConn(leaseID) == nil {
			t.Fatal("an idle but responsive instance was detached as dead; an idle session " +
				"with no user input must survive indefinitely (reaping is idle_timeout's job)")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestGatewayLiveness_SilentClientIsDetached is the client half. A browser tab
// on a slept laptop leaves exactly this socket behind, and until it is detached
// the lease keeps buffering frames for a peer that is never going to read them.
func TestGatewayLiveness_SilentClientIsDetached(t *testing.T) {
	wsURL, registry := newLivenessGateway(t)
	leaseID, instance := livenessLease(t, wsURL, registry)
	answerPings(t, instance)

	client := dial(t, wsURL+ClientWSPath(leaseID))
	defer client.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return registry.ClientConn(leaseID) != nil })

	// The client never reads, so it never pongs.
	waitWithin(t, 10*testPeerReadDeadline,
		"a client that stopped answering pings was still attached after the read deadline",
		func() bool { return registry.ClientConn(leaseID) == nil })

	// Detaching the client must not have taken the instance with it: the lease
	// survives so a reconnecting client can resume it.
	if registry.InstanceConn(leaseID) == nil {
		t.Error("detaching a dead client also detached the live instance")
	}
}

// TestGatewayLiveness_ResponsiveClientSurvives mirrors the instance case: a
// connected client that sends no input for many deadlines is a user reading the
// last answer, not a dead socket.
func TestGatewayLiveness_ResponsiveClientSurvives(t *testing.T) {
	wsURL, registry := newLivenessGateway(t)
	leaseID, instance := livenessLease(t, wsURL, registry)
	answerPings(t, instance)

	client := dial(t, wsURL+ClientWSPath(leaseID))
	defer client.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return registry.ClientConn(leaseID) != nil })
	answerPings(t, client)

	deadline := time.Now().Add(4 * testPeerReadDeadline)
	for time.Now().Before(deadline) {
		if registry.ClientConn(leaseID) == nil {
			t.Fatal("an idle but responsive client was detached as dead")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestGatewayLiveness_DisabledWhenIntervalNonPositive pins the escape hatch the
// zero value implies: a gateway constructed with no interval runs no liveness
// pump at all, and a peer that answers nothing stays attached. Nothing in the
// binary sets this — NewGateway always seeds the constants — but tests that
// deliberately park a silent socket depend on being able to turn it off.
func TestGatewayLiveness_DisabledWhenIntervalNonPositive(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := NewRegistry(logger, 0)
	gateway := NewGateway(logger, registry, newAuthGuard(logger, nil), nil)
	gateway.pingInterval = 0

	mux := http.NewServeMux()
	gateway.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		gateway.Shutdown()
		srv.Close()
	})
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	leaseID, _ := livenessLease(t, wsURL, registry)
	time.Sleep(4 * testPeerReadDeadline)
	if registry.InstanceConn(leaseID) == nil {
		t.Fatal("a non-positive ping interval must disable liveness checking entirely")
	}
}
