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

// The two sources merge: the connection carries what is fixed for the
// conversation, the turn carries what varies, and a key the connection set is
// NOT overridable — the pipe is opaque, so a client can put anything on its own
// envelope, and a proxy-injected value must survive that.
func TestPerTurnHeadersMergeUnderTheAnnouncement(t *testing.T) {
	_, run := newHeaderTestPlugin(t)

	run(ioMessage{Type: "client.headers", Headers: map[string]string{
		"subject": "alice",
		"tenant":  "acme",
	}})
	in := run(ioMessage{
		Type:    "input",
		Content: "hello",
		Headers: map[string]string{
			"subject":    "root",    // collides: the connection wins
			"request-id": "req-123", // per-turn only: comes through
		},
	})

	if in.Headers["subject"] != "alice" {
		t.Errorf("Headers[subject] = %q, want the connection's alice; a caller must not overwrite it",
			in.Headers["subject"])
	}
	if in.Headers["tenant"] != "acme" {
		t.Errorf("Headers[tenant] = %q, want the connection's acme", in.Headers["tenant"])
	}
	if in.Headers["request-id"] != "req-123" {
		t.Errorf("Headers[request-id] = %q, want the per-turn value", in.Headers["request-id"])
	}
}

// Per-turn values genuinely vary turn to turn on one connection, which is the
// whole reason the merge exists.
func TestPerTurnHeadersVaryAcrossTurns(t *testing.T) {
	_, run := newHeaderTestPlugin(t)

	run(ioMessage{Type: "client.headers", Headers: map[string]string{"tenant": "acme"}})

	for _, want := range []string{"req-1", "req-2", "req-3"} {
		in := run(ioMessage{
			Type:    "input",
			Content: "hello",
			Headers: map[string]string{"request-id": want},
		})
		if in.Headers["request-id"] != want {
			t.Errorf("Headers[request-id] = %q, want %q", in.Headers["request-id"], want)
		}
		if in.Headers["tenant"] != "acme" {
			t.Errorf("the fixed header was lost on a per-turn update: %v", in.Headers)
		}
	}
}

// The per-turn half bypasses nexusheaders.Extract, so it must be normalized and
// bounded on the way in rather than trusted as already-clean.
func TestPerTurnHeadersAreNormalizedAndBounded(t *testing.T) {
	_, run := newHeaderTestPlugin(t)

	run(ioMessage{Type: "client.headers", Headers: map[string]string{"tenant": "acme"}})
	in := run(ioMessage{
		Type:    "input",
		Content: "hello",
		Headers: map[string]string{
			"X-Nexus-Request-ID": "req-1",         // prefixed + cased: normalized
			"bad key":            "dropped",       // not a usable label key
			"note":               "line\r\nbreak", // control characters stripped
		},
	})

	if in.Headers["request-id"] != "req-1" {
		t.Errorf("a prefixed per-turn name was not normalized: %v", in.Headers)
	}
	if _, present := in.Headers["bad key"]; present {
		t.Errorf("an unusable name survived: %v", in.Headers)
	}
	if got := in.Headers["note"]; got != "linebreak" {
		t.Errorf("Headers[note] = %q, want control characters stripped", got)
	}
}

// Identity is deliberately NOT part of the merge.
func TestPerTurnHeadersCannotContributeIdentity(t *testing.T) {
	p, session, bus := newPrincipalTestPlugin(t)

	id, ok := principalDuring(t, p, session, bus,
		ioMessage{Type: "client.headers", PrincipalID: "alice"},
		ioMessage{Type: "input", Content: "hello", PrincipalID: "root",
			Headers: map[string]string{"principal_id": "root"}},
	)
	if !ok || id != "alice" {
		t.Errorf("_principal_id = (%q, %v), want the announced alice", id, ok)
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

// principalDuring runs one input and reports what _principal_id was bound to
// while the turn was in flight.
func principalDuring(t *testing.T, p *Plugin, session *engine.SessionWorkspace, bus engine.EventBus, msgs ...ioMessage) (string, bool) {
	t.Helper()
	type seen struct {
		id string
		ok bool
	}
	got := make(chan seen, 1)
	unsub := bus.Subscribe("io.input", func(engine.Event[any]) {
		id, ok := session.PrincipalID()
		select {
		case got <- seen{id: id, ok: ok}:
		default:
		}
	})
	defer unsub()

	for _, msg := range msgs {
		p.handleInbound(msg)
	}
	select {
	case s := <-got:
		return s.id, s.ok
	case <-time.After(2 * time.Second):
		t.Fatal("io.input never emitted")
		return "", false
	}
}

// newPrincipalTestPlugin is the header harness plus the handles the principal
// assertions need.
func newPrincipalTestPlugin(t *testing.T) (*Plugin, *engine.SessionWorkspace, engine.EventBus) {
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
	return p, session, bus
}

// The gap this closes: the broker verifies an identity to gate lease ownership,
// and until now dropped it. A turn must be able to read it.
func TestAnnouncedPrincipalIsBoundForTheTurn(t *testing.T) {
	p, session, bus := newPrincipalTestPlugin(t)

	id, ok := principalDuring(t, p, session, bus,
		ioMessage{Type: "client.headers", Headers: map[string]string{"tenant": "acme"}, PrincipalID: "alice"},
		ioMessage{Type: "input", Content: "hello"},
	)
	if !ok || id != "alice" {
		t.Errorf("_principal_id during the turn = (%q, %v), want alice", id, ok)
	}
}

// The whole point of carrying identity separately from headers: a client
// authoring its own input payload on the opaque pipe must not be able to name
// itself anyone.
func TestAnnouncedPrincipalBeatsAClientSuppliedField(t *testing.T) {
	p, session, bus := newPrincipalTestPlugin(t)

	id, ok := principalDuring(t, p, session, bus,
		ioMessage{Type: "client.headers", PrincipalID: "alice"},
		ioMessage{Type: "input", Content: "hello", PrincipalID: "root"},
	)
	if !ok || id != "alice" {
		t.Errorf("_principal_id = (%q, %v), want the announced alice, not the client's claim", id, ok)
	}
}

// A turn under an unauthenticated connection must clear the previous turn's
// identity rather than inherit it.
func TestAnnouncedPrincipalClearsWhenAbsent(t *testing.T) {
	p, session, bus := newPrincipalTestPlugin(t)

	if _, ok := principalDuring(t, p, session, bus,
		ioMessage{Type: "client.headers", PrincipalID: "alice"},
		ioMessage{Type: "input", Content: "one"},
	); !ok {
		t.Fatal("the first turn bound no principal")
	}

	id, ok := principalDuring(t, p, session, bus,
		ioMessage{Type: "client.headers"},
		ioMessage{Type: "input", Content: "two"},
	)
	if ok {
		t.Errorf("_principal_id = %q after an announcement carrying none; it must be cleared", id)
	}
}

// With no announcement — the A2A path — the message's field is the broker's own
// and stands.
func TestMessagePrincipalStandsWithoutAnAnnouncement(t *testing.T) {
	p, session, bus := newPrincipalTestPlugin(t)

	id, ok := principalDuring(t, p, session, bus,
		ioMessage{Type: "input", Content: "hello", PrincipalID: "alice"},
	)
	if !ok || id != "alice" {
		t.Errorf("_principal_id = (%q, %v), want the message's own alice", id, ok)
	}
}

// Identity and attributes must never be readable from different turns.
func TestPrincipalAndHeadersAreBoundTogether(t *testing.T) {
	p, session, bus := newPrincipalTestPlugin(t)

	var (
		gotID      string
		gotHeaders map[string]string
	)
	done := make(chan struct{})
	unsub := bus.Subscribe("io.input", func(engine.Event[any]) {
		gotID, _ = session.PrincipalID()
		gotHeaders, _ = session.RequestHeaders()
		close(done)
	})
	defer unsub()

	p.handleInbound(ioMessage{
		Type:        "client.headers",
		Headers:     map[string]string{"tenant": "acme"},
		PrincipalID: "alice",
	})
	p.handleInbound(ioMessage{Type: "input", Content: "hello"})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("io.input never emitted")
	}
	if gotID != "alice" || gotHeaders["tenant"] != "acme" {
		t.Errorf("bound identity/attributes = (%q, %v), want alice/acme together", gotID, gotHeaders)
	}
}
