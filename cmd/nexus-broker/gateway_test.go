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

// newTestGateway spins up an httptest server hosting the gateway endpoints and
// returns the ws:// base URL plus the registry and a cleanup func.
func newTestGateway(t *testing.T) (string, *Registry) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := NewRegistry(logger, 0)
	gateway := NewGateway(logger, registry)

	mux := http.NewServeMux()
	gateway.Register(mux)
	srv := httptest.NewServer(mux)

	t.Cleanup(func() {
		gateway.Shutdown()
		srv.Close()
	})

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	return wsURL, registry
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

	leaseID, err := registry.NewLease()
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
	leaseID, err := registry.NewLease()
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
