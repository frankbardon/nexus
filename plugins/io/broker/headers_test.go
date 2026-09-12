package broker

import (
	"encoding/json"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This process never sees the client's HTTP request — the broker terminates it
// — so the forwarded `headers` field is the ONLY way per-request caller context
// reaches a plugin here. It must reach both the turn's io.input and the
// session's reserved labels.
func TestInboundInputCarriesForwardedHeaders(t *testing.T) {
	bus := engine.NewEventBus()
	session, err := engine.NewSessionWorkspace(filepath.Join(t.TempDir(), "sessions"), bus)
	if err != nil {
		t.Fatalf("new session workspace: %v", err)
	}

	p := New().(*Plugin)
	p.bus = bus
	p.logger = slog.Default()
	p.session = session

	var mu sync.Mutex
	var seen events.UserInput
	var boundDuringTurn map[string]string
	done := make(chan struct{})
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		in, ok := e.Payload.(events.UserInput)
		if !ok {
			return
		}
		hdrs, err := session.RequestHeaders()
		if err != nil {
			t.Errorf("RequestHeaders: %v", err)
		}
		mu.Lock()
		seen = in
		boundDuringTurn = hdrs
		mu.Unlock()
		close(done)
	})

	p.handleInbound(ioMessage{
		Type:    "input",
		Content: "hello",
		Headers: map[string]string{"tenant": "acme"},
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("io.input never emitted")
	}

	mu.Lock()
	defer mu.Unlock()
	if seen.Headers["tenant"] != "acme" {
		t.Errorf("io.input Headers = %v, want tenant=acme", seen.Headers)
	}
	if boundDuringTurn["tenant"] != "acme" {
		t.Errorf("session request headers during the turn = %v, want tenant=acme", boundDuringTurn)
	}
}

// The field is additive and omitempty, so an older broker that never sets it
// must decode into a message the instance handles exactly as before.
func TestIOMessageHeadersAreOptional(t *testing.T) {
	var msg ioMessage
	if err := json.Unmarshal([]byte(`{"type":"input","content":"hi"}`), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Headers != nil {
		t.Errorf("Headers = %v, want nil for a payload that omits it", msg.Headers)
	}

	raw, err := json.Marshal(ioMessage{Type: "input", Content: "hi"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := keys["headers"]; present {
		t.Errorf("an input message with no headers encodes %q; it must stay omitempty", "headers")
	}
}
