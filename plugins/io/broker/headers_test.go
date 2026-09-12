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

// newHeaderTestPlugin wires a plugin against a real bus and session without
// dialling anything, and returns a func that runs one inbound message and
// hands back the UserInput it produced.
func newHeaderTestPlugin(t *testing.T) (*Plugin, func(ioMessage) events.UserInput) {
	t.Helper()
	bus := engine.NewEventBus()
	session, err := engine.NewSessionWorkspace(filepath.Join(t.TempDir(), "sessions"), bus)
	if err != nil {
		t.Fatalf("new session workspace: %v", err)
	}
	p := New().(*Plugin)
	p.bus = bus
	p.logger = slog.Default()
	p.session = session

	inputs := make(chan events.UserInput, 4)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		if in, ok := e.Payload.(events.UserInput); ok {
			inputs <- in
		}
	})

	return p, func(msg ioMessage) events.UserInput {
		t.Helper()
		p.handleInbound(msg)
		if msg.Type != "input" {
			return events.UserInput{}
		}
		select {
		case in := <-inputs:
			return in
		case <-time.After(2 * time.Second):
			t.Fatal("io.input never emitted")
			return events.UserInput{}
		}
	}
}

// The broker announces a client connection's headers ahead of the frame they
// belong to; the input that follows must run under them.
func TestClientHeadersAnnouncementAppliesToTheNextInput(t *testing.T) {
	_, run := newHeaderTestPlugin(t)

	run(ioMessage{Type: "client.headers", Headers: map[string]string{"tenant": "acme"}})
	in := run(ioMessage{Type: "input", Content: "hello"})

	if in.Headers["tenant"] != "acme" {
		t.Errorf("io.input Headers = %v, want tenant=acme", in.Headers)
	}
}

// The pipe is opaque, so a client CAN set headers on its own envelope and the
// broker cannot strip it. The broker's announcement — derived from the HTTP
// request an operator's proxy controls — must win, or a caller could overwrite
// what that proxy asserted about it.
func TestClientHeadersAnnouncementBeatsAClientSuppliedField(t *testing.T) {
	_, run := newHeaderTestPlugin(t)

	run(ioMessage{Type: "client.headers", Headers: map[string]string{"subject": "alice"}})
	in := run(ioMessage{
		Type:    "input",
		Content: "hello",
		Headers: map[string]string{"subject": "root", "extra": "smuggled"},
	})

	if in.Headers["subject"] != "alice" {
		t.Errorf("io.input Headers[subject] = %q, want the announced alice", in.Headers["subject"])
	}
	if _, present := in.Headers["extra"]; present {
		t.Errorf("io.input Headers = %v; an announcement replaces the message field wholesale", in.Headers)
	}
}

// An empty announcement clears, so a second client connection that sends no
// header does not inherit the first one's values.
func TestClientHeadersAnnouncementClears(t *testing.T) {
	_, run := newHeaderTestPlugin(t)

	run(ioMessage{Type: "client.headers", Headers: map[string]string{"tenant": "acme"}})
	run(ioMessage{Type: "client.headers"})
	in := run(ioMessage{Type: "input", Content: "hello"})

	if len(in.Headers) != 0 {
		t.Errorf("io.input Headers = %v, want none after a clearing announcement", in.Headers)
	}
}

// With no announcement ever made — the A2A path, where the broker built the
// message from the request itself — the message's own field stands.
func TestMessageHeadersStandWithoutAnAnnouncement(t *testing.T) {
	_, run := newHeaderTestPlugin(t)

	in := run(ioMessage{Type: "input", Content: "hello", Headers: map[string]string{"tenant": "acme"}})
	if in.Headers["tenant"] != "acme" {
		t.Errorf("io.input Headers = %v, want the message's own tenant=acme", in.Headers)
	}
}
