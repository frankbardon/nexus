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
	p.client = newClient(logger, addr, leaseID, sessionID, p.handleInbound)

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
