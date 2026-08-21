package broker

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Liveness timings for these tests. They keep the SHAPE of the shipped
// constants — the deadline is three intervals — while running three orders of
// magnitude faster, so what is exercised is the real policy rather than a
// degenerate one-strike variant of it. 50ms is generous for a loopback round
// trip even under -race, which is the margin that keeps the responsive test
// from flaking.
const (
	testPingInterval       = 50 * time.Millisecond
	testBrokerReadDeadline = 3 * testPingInterval
)

// livenessBroker is a stub broker gateway that counts dial-backs and can be
// told whether to READ from them.
//
// Not reading is the whole trick: coder/websocket answers a ping from inside a
// blocked Read and nowhere else, so a handler that accepts the connection and
// then parks is byte-for-byte what a half-open socket looks like to the
// instance — the TCP connection is up, writes are accepted, and no pong ever
// comes back.
type livenessBroker struct {
	srv *httptest.Server

	mu      sync.Mutex
	accepts int

	// park is closed on cleanup, releasing every silent handler. The handlers
	// must outlive the accept: returning would drop the hijacked connection and
	// hand the instance a clean disconnect, which is the fault it already
	// handles rather than the one under test.
	park chan struct{}
}

func newLivenessBroker(t *testing.T, answerPings bool) *livenessBroker {
	t.Helper()
	b := &livenessBroker{park: make(chan struct{})}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		b.mu.Lock()
		b.accepts++
		b.mu.Unlock()

		if !answerPings {
			<-b.park
			_ = conn.CloseNow()
			return
		}
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				_ = conn.CloseNow()
				return
			}
		}
	}))
	t.Cleanup(func() {
		close(b.park)
		b.srv.Close()
	})
	return b
}

func (b *livenessBroker) wsURL() string {
	return "ws" + strings.TrimPrefix(b.srv.URL, "http")
}

func (b *livenessBroker) acceptCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.accepts
}

// newLivenessClient starts a dial-back client against b with liveness timings
// and reconnect backoff shrunk to milliseconds.
func newLivenessClient(t *testing.T, b *livenessBroker) *client {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(logger, clientConfig{addr: b.wsURL(), leaseID: "lease-liveness"}, nil, nil)
	c.pingInterval = testPingInterval
	c.brokerReadDeadline = testBrokerReadDeadline
	c.minBackoff = 5 * time.Millisecond
	c.maxBackoff = 20 * time.Millisecond
	c.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		c.Stop(ctx)
	})
	return c
}

// TestClientLiveness_UnresponsiveBrokerIsRedialled is the instance half of the
// half-open socket. Detection matters here even though the broker probes too:
// this end holds the outbound buffer, so an instance that never notices keeps
// filling a megabyte against a socket nobody will ever drain, evicting its own
// oldest frames while the write pump sits happily in a Write the kernel accepts.
//
// A SECOND dial-back is the observable proof: the client can only redial after
// it has given up on the first socket.
func TestClientLiveness_UnresponsiveBrokerIsRedialled(t *testing.T) {
	b := newLivenessBroker(t, false)
	c := newLivenessClient(t, b)

	waitWithin(t, 10*testBrokerReadDeadline,
		"the instance never gave up on a broker that stopped answering pings",
		func() bool { return b.acceptCount() >= 2 })

	// And it is genuinely a fresh session rather than a client that quietly
	// wedged: the redial establishes a live connection.
	waitWithin(t, 10*testBrokerReadDeadline,
		"the instance dropped the dead socket but never brought a new one up",
		c.connected)
}

// TestClientLiveness_ResponsiveBrokerIsNotRedialled is the criterion that
// matters as much as detection: a liveness check that false-positives is worse
// than none.
//
// Nothing is sent in either direction for the whole test — this is an idle
// session with no user input, the state an instance spends most of its life in.
// It survives because the broker's WebSocket stack answers pings from inside its
// blocked read whether or not anyone is typing, which is exactly why the probe
// is a ping and not a timeout on Read.
func TestClientLiveness_ResponsiveBrokerIsNotRedialled(t *testing.T) {
	b := newLivenessBroker(t, true)
	c := newLivenessClient(t, b)

	waitWithin(t, 2*time.Second, "the instance never dialled the broker", c.connected)

	// Four deadlines — a dozen probes — of complete application-level silence.
	deadline := time.Now().Add(4 * testBrokerReadDeadline)
	for time.Now().Before(deadline) {
		if got := b.acceptCount(); got != 1 {
			t.Fatalf("an idle but responsive link was torn down and redialled (%d dial-backs); "+
				"an idle session with no user input must survive indefinitely", got)
		}
		if !c.connected() {
			t.Fatal("an idle but responsive link was dropped as dead")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestClientLiveness_DisabledWhenIntervalNonPositive pins the escape hatch the
// zero value implies: no interval, no liveness pump, and a silent broker keeps
// its socket. Nothing in the binary sets this — newClient always seeds the
// constants — but it is what makes the timings safely overridable.
func TestClientLiveness_DisabledWhenIntervalNonPositive(t *testing.T) {
	b := newLivenessBroker(t, false)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(logger, clientConfig{addr: b.wsURL(), leaseID: "lease-liveness"}, nil, nil)
	c.pingInterval = 0
	c.minBackoff = 5 * time.Millisecond
	c.maxBackoff = 20 * time.Millisecond
	c.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		c.Stop(ctx)
	})

	waitWithin(t, 2*time.Second, "the instance never dialled the broker", c.connected)
	time.Sleep(4 * testBrokerReadDeadline)
	if got := b.acceptCount(); got != 1 {
		t.Fatalf("a non-positive ping interval must disable liveness checking entirely, "+
			"but the link was redialled (%d dial-backs)", got)
	}
}

// waitWithin polls cond until it holds or the budget elapses, failing with msg.
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
