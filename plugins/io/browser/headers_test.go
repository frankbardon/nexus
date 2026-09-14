package browser

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/ui"
)

// The upgrade's X-Nexus-* headers are connection-scoped, and the adapter must
// be able to resolve them from the client id a message arrived on — that id is
// the only thing distinguishing two browsers attached to one session.
func TestHubHeadersAreConnectionScoped(t *testing.T) {
	hub := NewHub(slog.Default())

	a := &Client{
		id:          "client-a",
		send:        make(chan []byte, 1),
		hub:         hub,
		done:        make(chan struct{}),
		connectedAt: time.Now(),
		headers:     map[string]string{"tenant": "acme"},
	}
	b := &Client{
		id:          "client-b",
		send:        make(chan []byte, 1),
		hub:         hub,
		done:        make(chan struct{}),
		connectedAt: time.Now(),
	}
	hub.Register(a)
	hub.Register(b)

	if got := hub.Headers("client-a")["tenant"]; got != "acme" {
		t.Errorf("Headers(client-a)[tenant] = %q, want acme", got)
	}
	if got := hub.Headers("client-b"); len(got) != 0 {
		t.Errorf("Headers(client-b) = %v, want none", got)
	}
	if got := hub.Headers("client-unknown"); got != nil {
		t.Errorf("Headers(client-unknown) = %v, want nil", got)
	}
}

// The input callback must be told WHICH connection typed, or the headers it
// needs cannot be looked up.
func TestAdapterInputCarriesOriginatingClientID(t *testing.T) {
	hub := NewHub(slog.Default())
	adapter := NewAdapter(hub, "session-1")

	type seen struct {
		clientID string
		content  string
	}
	got := make(chan seen, 1)
	adapter.OnInput(func(clientID string, msg ui.InputMessage) {
		got <- seen{clientID: clientID, content: msg.Content}
	})

	payload, err := json.Marshal(ui.InputMessage{Content: "hello"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	adapter.handleInbound("client-a", ui.Envelope{Type: ui.TypeInput, Payload: payload})

	select {
	case s := <-got:
		if s.clientID != "client-a" || s.content != "hello" {
			t.Errorf("input handler saw %+v, want client-a/hello", s)
		}
	case <-time.After(time.Second):
		t.Fatal("input handler was never called")
	}
}
