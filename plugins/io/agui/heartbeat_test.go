package agui

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

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// recordingSocket is a ResponseWriter that accumulates what was written and can
// be read safely while the handler is still writing to it.
type recordingSocket struct {
	mu  sync.Mutex
	buf strings.Builder
	hdr http.Header
}

func newRecordingSocket() *recordingSocket {
	return &recordingSocket{hdr: make(http.Header)}
}

func (r *recordingSocket) Header() http.Header { return r.hdr }
func (r *recordingSocket) WriteHeader(int)     {}
func (r *recordingSocket) Flush()              {}

func (r *recordingSocket) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(b)
}

func (r *recordingSocket) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// TestAnIdleTurnKeepsTheStreamAlive is the property the keepalive exists for.
//
// An AG-UI stream is silent between events, and a turn that spends its time
// inside one LLM call writes NOTHING for as long as that takes. An intermediary
// measures exactly that silence: a proxy, load balancer or service mesh with an
// idle timeout below the gap closes the connection under a live turn, and the
// client sees a 200 with a truncated body and no answer — the status having
// been decided with the first byte, long before.
//
// Measured against a real deployment: a one-delegation Arc turn ran 32.7s wall
// with a largest inter-event gap of 6.5s, so the gaps are real and so is the
// total.
func TestAnIdleTurnKeepsTheStreamAlive(t *testing.T) {
	p, bus, _ := newTestPlugin(t)

	s := NewServer(serverConfig{
		logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		bridge:            p,
		heartbeatInterval: 20 * time.Millisecond,
	})

	inputSeen := make(chan events.UserInput, 1)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		if in, ok := e.Payload.(events.UserInput); ok {
			select {
			case inputSeen <- in:
			default:
			}
		}
	})

	body := `{"threadId":"thread-hb","runId":"run-hb",` +
		`"messages":[{"id":"m1","role":"user","content":"a long quiet turn"}]}`
	req := httptest.NewRequest(http.MethodPost, agentPath, strings.NewReader(body))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req = req.WithContext(ctx)
	w := newRecordingSocket()

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		s.handleRunAgent(w, req)
	}()

	<-inputSeen
	if err := bus.Emit("agent.turn.start", events.TurnInfo{
		SchemaVersion: events.TurnInfoVersion, TurnID: "turn-hb",
	}); err != nil {
		t.Fatalf("emit agent.turn.start: %v", err)
	}

	// The turn now goes quiet, exactly as it does inside an LLM call. Nothing
	// else is emitted: every byte that appears from here is a keepalive.
	deadline := time.After(3 * time.Second)
	for {
		if strings.Count(w.String(), ": heartbeat") >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("an idle turn wrote no keepalives in 3s; the stream was silent.\ngot:\n%s", w.String())
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the request context was cancelled")
	}
}

// TestKeepalivesCanBeTurnedOff is the control: a deployment with its own
// keepalive must be able to decline a second one, and "off" must actually be
// silent rather than merely slower.
func TestKeepalivesCanBeTurnedOff(t *testing.T) {
	p, bus, _ := newTestPlugin(t)

	s := NewServer(serverConfig{
		logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		bridge:            p,
		heartbeatInterval: -1,
	})

	inputSeen := make(chan events.UserInput, 1)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		if in, ok := e.Payload.(events.UserInput); ok {
			select {
			case inputSeen <- in:
			default:
			}
		}
	})

	body := `{"threadId":"thread-off","runId":"run-off",` +
		`"messages":[{"id":"m1","role":"user","content":"quiet"}]}`
	req := httptest.NewRequest(http.MethodPost, agentPath, strings.NewReader(body))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req = req.WithContext(ctx)
	w := newRecordingSocket()

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		s.handleRunAgent(w, req)
	}()

	<-inputSeen
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(w.String(), ": heartbeat") {
		t.Errorf("keepalives are disabled but the stream carried one:\n%s", w.String())
	}

	cancel()
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the request context was cancelled")
	}
}
