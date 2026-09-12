package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/frankbardon/nexus/pkg/brokerframe"
)

// dialWithHeaders is dial() with extra request headers on the handshake, which
// is the whole subject of this file.
func dialWithHeaders(t *testing.T, url string, headers map[string]string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	return conn
}

// attachedLease registers an instance and returns its connection plus the
// lease id, so a test can watch what the broker actually sends it.
func attachedLease(t *testing.T, wsURL string, registry *Registry) (*websocket.Conn, string) {
	t.Helper()
	leaseID, err := registry.NewLease(anonymousOwner())
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}
	registry.SetSpawnSecret(leaseID, testSpawnSecret)
	instance := dial(t, wsURL+instanceWSPath)
	t.Cleanup(func() { _ = instance.Close(websocket.StatusNormalClosure, "") })
	writeFrame(t, instance, brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalRegister,
		Secret:  testSpawnSecret,
	})
	waitFor(t, func() bool { return registry.InstanceConn(leaseID) != nil })
	return instance, leaseID
}

// decodeAnnouncement reads one frame and asserts it is a client.headers payload,
// returning the headers it carries.
func decodeAnnouncement(t *testing.T, conn *websocket.Conn) map[string]string {
	t.Helper()
	f := readFrame(t, conn)
	if f.Signal != brokerframe.SignalIO {
		t.Fatalf("expected an io frame, got %+v", f)
	}
	var msg brokerIOMessage
	if err := json.Unmarshal(f.Payload, &msg); err != nil {
		t.Fatalf("decoding payload %s: %v", f.Payload, err)
	}
	if msg.Type != ioTypeClientHeaders {
		t.Fatalf("expected a %q payload, got %q (%s)", ioTypeClientHeaders, msg.Type, f.Payload)
	}
	return msg.Headers
}

// The broker terminates the client request, so this hop is the only place the
// headers exist: an X-Nexus-* header on the client handshake must reach the
// instance ahead of the frame it belongs to, with nothing outside the namespace
// riding along.
func TestGateway_ClientHeadersPrecedeTheFrameTheyBelongTo(t *testing.T) {
	wsURL, registry := newTestGateway(t)
	instance, leaseID := attachedLease(t, wsURL, registry)

	client := dialWithHeaders(t, wsURL+ClientWSPath(leaseID), map[string]string{
		"X-Nexus-Tenant-ID": "acme",
		"X-Nexus-Timezone":  "Europe/Amsterdam",
		"X-Trace-Id":        "should-not-appear",
	})
	defer client.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return registry.ClientConn(leaseID) != nil })

	writeFrame(t, client, brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalIO,
		Payload: []byte(`{"type":"input","content":"hello"}`),
	})

	headers := decodeAnnouncement(t, instance)
	if headers["tenant-id"] != "acme" || headers["timezone"] != "Europe/Amsterdam" {
		t.Errorf("announced headers = %v, want the two X-Nexus-* headers", headers)
	}
	if len(headers) != 2 {
		t.Errorf("announced headers = %v, want nothing outside the X-Nexus-* namespace", headers)
	}

	// And the client's own frame follows it, byte for byte.
	got := readFrame(t, instance)
	if string(got.Payload) != `{"type":"input","content":"hello"}` {
		t.Errorf("client frame reached the instance as %s; it must be forwarded verbatim", got.Payload)
	}
}

// A second connection that sends no header must not leave the first
// connection's values standing, which is why an empty set is announced too.
func TestGateway_ClientHeadersClearOnAConnectionThatSendsNone(t *testing.T) {
	wsURL, registry := newTestGateway(t)
	instance, leaseID := attachedLease(t, wsURL, registry)

	first := dialWithHeaders(t, wsURL+ClientWSPath(leaseID), map[string]string{"X-Nexus-Tenant": "acme"})
	waitFor(t, func() bool { return registry.ClientConn(leaseID) != nil })
	writeFrame(t, first, brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalIO,
		Payload: []byte(`{"type":"input","content":"one"}`),
	})
	if headers := decodeAnnouncement(t, instance); headers["tenant"] != "acme" {
		t.Fatalf("first connection announced %v, want tenant=acme", headers)
	}
	readFrame(t, instance) // the input itself
	_ = first.Close(websocket.StatusNormalClosure, "")

	second := dial(t, wsURL+ClientWSPath(leaseID))
	defer second.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return registry.ClientConn(leaseID) != nil })
	writeFrame(t, second, brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalIO,
		Payload: []byte(`{"type":"input","content":"two"}`),
	})
	if headers := decodeAnnouncement(t, instance); len(headers) != 0 {
		t.Errorf("second connection announced %v, want an empty set clearing the first's", headers)
	}
}

// Only IO frames carry user input, so only they are preceded by an
// announcement — a ping must reach the instance with nothing in front of it.
func TestGateway_NonIOFramesAreNotAnnounced(t *testing.T) {
	wsURL, registry := newTestGateway(t)
	instance, leaseID := attachedLease(t, wsURL, registry)

	client := dialWithHeaders(t, wsURL+ClientWSPath(leaseID), map[string]string{"X-Nexus-Tenant": "acme"})
	defer client.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return registry.ClientConn(leaseID) != nil })

	writeFrame(t, client, brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalStreamGap,
	})
	if got := readFrame(t, instance); got.Signal != brokerframe.SignalStreamGap {
		t.Errorf("instance got %q first, want the non-IO frame forwarded with nothing in front", got.Signal)
	}
}
