package broker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/frankbardon/nexus/pkg/brokerframe"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// stubBroker is a minimal in-process stand-in for the broker's instance
// gateway. It accepts a single dial-back connection, records inbound frames,
// and exposes a channel to push frames at the instance.
type stubBroker struct {
	srv *httptest.Server

	mu       sync.Mutex
	frames   []brokerframe.Frame
	connOnce sync.Once
	connCh   chan *websocket.Conn
}

func newStubBroker(t *testing.T) *stubBroker {
	t.Helper()
	s := &stubBroker{connCh: make(chan *websocket.Conn, 1)}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		s.connOnce.Do(func() { s.connCh <- conn })
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			frame, err := brokerframe.Decode(data)
			if err != nil {
				continue
			}
			s.mu.Lock()
			s.frames = append(s.frames, frame)
			s.mu.Unlock()
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubBroker) wsURL() string {
	return "ws" + strings.TrimPrefix(s.srv.URL, "http")
}

func (s *stubBroker) snapshot() []brokerframe.Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]brokerframe.Frame, len(s.frames))
	copy(out, s.frames)
	return out
}

// newTestPlugin wires a Plugin against a real bus and a stub broker, mirroring
// what Init/Ready do without a full PluginContext.
func newTestPlugin(t *testing.T, addr, leaseID, sessionID string) (*Plugin, engine.EventBus) {
	t.Helper()
	bus := engine.NewEventBus()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	p := New().(*Plugin)
	p.bus = bus
	p.logger = logger
	p.brokerAddr = addr
	p.leaseID = leaseID
	p.sessionID = sessionID
	p.client = newClient(logger, clientConfig{
		addr:      addr,
		leaseID:   leaseID,
		sessionID: sessionID,
	}, p.handleInbound, p.handleShutdown)

	p.unsubs = append(p.unsubs,
		bus.Subscribe("io.output", p.handleOutput),
		bus.Subscribe("llm.stream.chunk", p.handleStreamChunk),
		bus.Subscribe("llm.stream.end", p.handleStreamEnd),
		bus.Subscribe("io.status", p.handleStatus),
		bus.Subscribe("io.approval.request", p.handleApprovalRequest),
		bus.Subscribe("hitl.requested", p.handleHITLRequest),
		bus.Subscribe("cancel.complete", p.handleCancelComplete),
	)

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return p, bus
}

func waitConn(t *testing.T, s *stubBroker) *websocket.Conn {
	t.Helper()
	select {
	case conn := <-s.connCh:
		return conn
	case <-time.After(3 * time.Second):
		t.Fatal("stub broker never accepted a dial-back")
		return nil
	}
}

func TestDialRegisterHandshake(t *testing.T) {
	stub := newStubBroker(t)
	p, _ := newTestPlugin(t, stub.wsURL(), "lease-xyz", "sess-123")

	p.client.Start()
	waitConn(t, stub)

	// Expect register, ready, session-id-report in order.
	deadline := time.Now().Add(2 * time.Second)
	var frames []brokerframe.Frame
	for time.Now().Before(deadline) {
		frames = stub.snapshot()
		if len(frames) >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(frames) < 3 {
		t.Fatalf("expected >=3 handshake frames, got %d: %+v", len(frames), frames)
	}
	if frames[0].Signal != brokerframe.SignalRegister || frames[0].LeaseID != "lease-xyz" {
		t.Fatalf("first frame want register/lease-xyz, got %+v", frames[0])
	}
	if frames[1].Signal != brokerframe.SignalReady {
		t.Fatalf("second frame want ready, got %+v", frames[1])
	}
	if frames[2].Signal != brokerframe.SignalSessionIDReport || frames[2].SessionID != "sess-123" {
		t.Fatalf("third frame want session-id-report/sess-123, got %+v", frames[2])
	}
}

// TestRegisterFrameCarriesSpawnSecret proves the instance half of E5-S1: the
// spawn secret rides on the REGISTER frame and on that frame only.
//
// The "only" half is load-bearing. The broker forwards SignalIO frames verbatim
// to the connected client, so a secret leaking onto any later frame would be
// handed straight to whoever is on the other end of the session socket.
func TestRegisterFrameCarriesSpawnSecret(t *testing.T) {
	const secret = "3a7c91e0d5b6482f10ac2b3d4e5f6071"

	stub := newStubBroker(t)
	p, _ := newTestPlugin(t, stub.wsURL(), "lease-sec", "sess-sec")
	p.spawnSecret = secret
	p.client.cfg.spawnSecret = secret

	p.client.Start()
	waitConn(t, stub)

	deadline := time.Now().Add(2 * time.Second)
	var frames []brokerframe.Frame
	for time.Now().Before(deadline) {
		frames = stub.snapshot()
		if len(frames) >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(frames) < 3 {
		t.Fatalf("expected >=3 handshake frames, got %d: %+v", len(frames), frames)
	}
	if frames[0].Signal != brokerframe.SignalRegister {
		t.Fatalf("first frame want register, got %+v", frames[0])
	}
	if frames[0].Secret != secret {
		t.Errorf("register frame Secret = %q, want the configured spawn secret", frames[0].Secret)
	}
	for _, f := range frames[1:] {
		if f.Secret != "" {
			t.Errorf("a %s frame carries the spawn secret; only register may: %+v", f.Signal, f)
		}
	}
}

// TestRegisterFrameOmitsAbsentSpawnSecret is the unauthenticated-broker path:
// with no secret resolved the register frame must still be sent, carrying no
// secret at all rather than failing or stalling the handshake.
func TestRegisterFrameOmitsAbsentSpawnSecret(t *testing.T) {
	stub := newStubBroker(t)
	p, _ := newTestPlugin(t, stub.wsURL(), "lease-nosec", "")

	p.client.Start()
	waitConn(t, stub)

	deadline := time.Now().Add(2 * time.Second)
	var frames []brokerframe.Frame
	for time.Now().Before(deadline) {
		frames = stub.snapshot()
		if len(frames) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(frames) < 2 {
		t.Fatalf("expected >=2 handshake frames, got %d: %+v", len(frames), frames)
	}
	if frames[0].Signal != brokerframe.SignalRegister || frames[0].Secret != "" {
		t.Errorf("first frame want register with no secret, got %+v", frames[0])
	}
}

func TestOutboundStreamChunkBridged(t *testing.T) {
	stub := newStubBroker(t)
	p, bus := newTestPlugin(t, stub.wsURL(), "lease-1", "")

	p.client.Start()
	waitConn(t, stub)
	// Let the handshake settle.
	time.Sleep(50 * time.Millisecond)

	if err := bus.Emit("llm.stream.chunk", events.StreamChunk{
		SchemaVersion: events.StreamChunkVersion,
		Content:       "hello",
		TurnID:        "t-1",
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range stub.snapshot() {
			if f.Signal != brokerframe.SignalIO {
				continue
			}
			var msg ioMessage
			if json.Unmarshal(f.Payload, &msg) == nil && msg.Type == "stream.delta" {
				if msg.Content == "hello" && msg.TurnID == "t-1" {
					return
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("stub broker never received stream.delta IO frame")
}

func TestInboundInputEmitsUserInput(t *testing.T) {
	stub := newStubBroker(t)
	p, bus := newTestPlugin(t, stub.wsURL(), "lease-1", "")

	var (
		mu   sync.Mutex
		seen events.UserInput
		got  atomic.Bool
	)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		ui, ok := e.Payload.(events.UserInput)
		if !ok {
			return
		}
		mu.Lock()
		seen = ui
		mu.Unlock()
		got.Store(true)
	})

	p.client.Start()
	conn := waitConn(t, stub)

	payload, _ := json.Marshal(ioMessage{Type: "input", Content: "hi there"})
	data, _ := brokerframe.Encode(brokerframe.Frame{
		LeaseID: "lease-1",
		Signal:  brokerframe.SignalIO,
		Payload: payload,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write inbound frame: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got.Load() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !got.Load() {
		t.Fatal("io.input never emitted from inbound frame")
	}
	mu.Lock()
	defer mu.Unlock()
	if seen.Content != "hi there" {
		t.Fatalf("want content 'hi there', got %q", seen.Content)
	}
}

func TestInboundShutdownEmitsSessionEnd(t *testing.T) {
	stub := newStubBroker(t)
	p, bus := newTestPlugin(t, stub.wsURL(), "lease-1", "sess-shut")

	ended := make(chan events.SessionInfo, 1)
	bus.Subscribe("io.session.end", func(e engine.Event[any]) {
		if si, ok := e.Payload.(events.SessionInfo); ok {
			select {
			case ended <- si:
			default:
			}
		}
	})

	p.client.Start()
	conn := waitConn(t, stub)

	data, _ := brokerframe.Encode(brokerframe.Frame{
		LeaseID: "lease-1",
		Signal:  brokerframe.SignalShutdown,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write shutdown frame: %v", err)
	}

	select {
	case si := <-ended:
		if si.Transport != "broker" || si.ID != "sess-shut" {
			t.Fatalf("unexpected session.end payload: %+v", si)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown frame did not trigger io.session.end")
	}

	// The shutdown latch must stop the reconnect loop rather than re-dialing.
	if !p.client.shutdownRequested.Load() {
		t.Error("shutdownRequested not latched after shutdown frame")
	}
}

// TestOutboundHITLRequestCarriesModeAndChoices is the parity assertion for the
// question payload.
//
// nexus.io.browser has always forwarded a HITL question's mode and options (via
// ui.HITLRequestMessage); this transport forwarded only the prompt, so a
// multiple-choice ask_user reached a broker client as bare prose it could not
// answer by id — and a broker-side A2A mapping could not render the options
// either. The ids are what a responder echoes back in choice_id, so a payload
// without them is not an answerable question.
func TestOutboundHITLRequestCarriesModeAndChoices(t *testing.T) {
	stub := newStubBroker(t)
	p, bus := newTestPlugin(t, stub.wsURL(), "lease-1", "")

	p.client.Start()
	waitConn(t, stub)
	time.Sleep(50 * time.Millisecond)

	if err := bus.Emit("hitl.requested", events.HITLRequest{
		SchemaVersion: events.HITLRequestVersion,
		ID:            "q-1",
		TurnID:        "t-1",
		Prompt:        "Which environment?",
		Mode:          events.HITLModeChoices,
		Choices: []events.HITLChoice{
			{ID: "staging", Label: "Staging"},
			{ID: "production", Label: "Production"},
		},
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range stub.snapshot() {
			if f.Signal != brokerframe.SignalIO {
				continue
			}
			var msg ioMessage
			if json.Unmarshal(f.Payload, &msg) != nil || msg.Type != "hitl.request" {
				continue
			}
			if msg.RequestID != "q-1" || msg.Prompt != "Which environment?" {
				t.Fatalf("hitl.request payload = %+v", msg)
			}
			if msg.Mode != string(events.HITLModeChoices) {
				t.Errorf("mode = %q, want %q", msg.Mode, events.HITLModeChoices)
			}
			if len(msg.Choices) != 2 {
				t.Fatalf("choices = %+v, want 2", msg.Choices)
			}
			if msg.Choices[0].ID != "staging" || msg.Choices[0].Label != "Staging" {
				t.Errorf("choices[0] = %+v, want the staging option", msg.Choices[0])
			}
			if msg.Choices[1].ID != "production" || msg.Choices[1].Label != "Production" {
				t.Errorf("choices[1] = %+v, want the production option", msg.Choices[1])
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("stub broker never received the hitl.request IO frame")
}

// TestOutboundFreeTextHITLRequestOmitsChoices keeps the additive claim honest:
// a question with no options must encode no `choices` key at all, so a
// free-text question's frame is byte-identical to what it was before choices
// existed and an older client sees no change.
func TestOutboundFreeTextHITLRequestOmitsChoices(t *testing.T) {
	stub := newStubBroker(t)
	p, bus := newTestPlugin(t, stub.wsURL(), "lease-1", "")

	p.client.Start()
	waitConn(t, stub)
	time.Sleep(50 * time.Millisecond)

	if err := bus.Emit("hitl.requested", events.HITLRequest{
		SchemaVersion: events.HITLRequestVersion,
		ID:            "q-1",
		Prompt:        "Approve the destructive migration?",
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range stub.snapshot() {
			if f.Signal != brokerframe.SignalIO {
				continue
			}
			var decoded map[string]any
			if json.Unmarshal(f.Payload, &decoded) != nil || decoded["type"] != "hitl.request" {
				continue
			}
			if _, present := decoded["choices"]; present {
				t.Errorf("a free-text question encoded a choices key: %v", decoded)
			}
			if decoded["mode"] != string(events.HITLModeFreeText) {
				t.Errorf("mode = %v, want %q", decoded["mode"], events.HITLModeFreeText)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("stub broker never received the hitl.request IO frame")
}
