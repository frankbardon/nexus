package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/frankbardon/nexus/pkg/brokerframe"
)

// restartableGateway is a stand-in for the broker's instance gateway that can
// be stopped and started again ON THE SAME ADDRESS. That is what makes it able
// to model the case this story is about: a broker restart, during which the
// instance's dial-back socket is down and its agent keeps producing output.
//
// The stub in dial_test.go accepts exactly one connection from an
// httptest-allocated port, so it cannot express "gone, then back".
type restartableGateway struct {
	addr string

	mu     sync.Mutex
	frames []brokerframe.Frame
	srv    *http.Server
	conns  []*websocket.Conn
}

// newRestartableGateway reserves a loopback address and returns a gateway that
// is NOT yet listening, so the first dials fail fast with ECONNREFUSED rather
// than hanging in the 10s dial timeout.
func newRestartableGateway(t *testing.T) *restartableGateway {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release reserved address: %v", err)
	}
	g := &restartableGateway{addr: addr}
	t.Cleanup(g.stop)
	return g
}

func (g *restartableGateway) wsURL() string { return "ws://" + g.addr }

func (g *restartableGateway) start(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", g.addr)
	if err != nil {
		t.Fatalf("listen on %s: %v", g.addr, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(g.handle)}
	g.mu.Lock()
	g.srv = srv
	g.mu.Unlock()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return
		}
	}()
}

func (g *restartableGateway) handle(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	g.mu.Lock()
	g.conns = append(g.conns, conn)
	g.mu.Unlock()
	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		frame, err := brokerframe.Decode(data)
		if err != nil {
			continue
		}
		g.mu.Lock()
		g.frames = append(g.frames, frame)
		g.mu.Unlock()
	}
}

func (g *restartableGateway) stop() {
	g.mu.Lock()
	srv, conns := g.srv, g.conns
	g.srv, g.conns = nil, nil
	g.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close(websocket.StatusGoingAway, "gateway down")
	}
	if srv != nil {
		_ = srv.Close()
	}
}

func (g *restartableGateway) snapshot() []brokerframe.Frame {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]brokerframe.Frame, len(g.frames))
	copy(out, g.frames)
	return out
}

// ioContents decodes the SignalIO frames a gateway received, oldest first,
// returning each payload's content field.
func ioContents(frames []brokerframe.Frame) []string {
	var out []string
	for _, f := range frames {
		if f.Signal != brokerframe.SignalIO {
			continue
		}
		var msg ioMessage
		if json.Unmarshal(f.Payload, &msg) != nil {
			continue
		}
		out = append(out, msg.Content)
	}
	return out
}

// recordingHandler is a slog handler that keeps every record it is given, so a
// test can assert on what was logged rather than only on state.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) find(level slog.Level, msgSubstr string) (slog.Record, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level == level && strings.Contains(r.Message, msgSubstr) {
			return r, true
		}
	}
	return slog.Record{}, false
}

func recordAttr(r slog.Record, key string) (slog.Value, bool) {
	var (
		val   slog.Value
		found bool
	)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			val, found = a.Value, true
			return false
		}
		return true
	})
	return val, found
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestOutboundFramesBufferedWhileDisconnectedFlushAfterHandshake is the story's
// central claim.
//
// The instance emits while the gateway is DOWN — exactly the state a broker
// restart puts it in — and every frame must still reach the gateway once the
// socket comes back, in emission order, and only AFTER the register/ready/
// session-id handshake that binds the socket to the lease.
func TestOutboundFramesBufferedWhileDisconnectedFlushAfterHandshake(t *testing.T) {
	gw := newRestartableGateway(t)

	c := newClient(discardLogger(), clientConfig{
		addr:      gw.wsURL(),
		leaseID:   "lease-reconnect",
		sessionID: "sess-reconnect",
	}, nil, nil)
	c.minBackoff = 5 * time.Millisecond
	c.maxBackoff = 20 * time.Millisecond
	c.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		c.Stop(ctx)
	})

	// Emit while nothing is listening. Before this story every one of these was
	// dropped at debug and never reached the broker at all.
	want := []string{"alpha", "bravo", "charlie", "delta", "echo"}
	for _, content := range want {
		c.SendIO(ioMessage{Type: "stream.delta", Content: content, TurnID: "t-1"})
	}
	if frames, bytes := c.pendingOutbound(); frames != len(want) || bytes == 0 {
		t.Fatalf("pendingOutbound() = (%d, %d), want (%d, >0)", frames, bytes, len(want))
	}

	// The broker comes back.
	gw.start(t)

	deadline := time.Now().Add(5 * time.Second)
	var frames []brokerframe.Frame
	for time.Now().Before(deadline) {
		frames = gw.snapshot()
		if len(ioContents(frames)) >= len(want) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	got := ioContents(frames)
	if len(got) != len(want) {
		t.Fatalf("gateway saw %d IO frames, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("IO frame order = %v, want %v", got, want)
		}
	}

	// Ordering against the handshake: the first three frames on the wire must
	// be register, ready and session-id-report. A buffered frame arriving
	// before register would reach a broker that has not yet bound the lease.
	if len(frames) < 4 {
		t.Fatalf("expected handshake + IO frames, got %d", len(frames))
	}
	wantSignals := []brokerframe.Signal{brokerframe.SignalRegister, brokerframe.SignalReady, brokerframe.SignalSessionIDReport}
	for i, sig := range wantSignals {
		if frames[i].Signal != sig {
			t.Fatalf("frame %d signal = %q, want %q (handshake must precede the flush)", i, frames[i].Signal, sig)
		}
	}
	for _, f := range frames[len(wantSignals):] {
		if f.Signal != brokerframe.SignalIO {
			t.Fatalf("unexpected %q frame after the handshake", f.Signal)
		}
	}

	// The buffer is emptied by the flush, not merely copied out of.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := c.pendingOutbound(); n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	n, b := c.pendingOutbound()
	t.Fatalf("buffer still holds %d frames / %d bytes after the flush", n, b)
}

// TestOutboundBufferEvictsOldestOnOverflow pins the bound's shape: it is in
// BYTES, the oldest frames go first, and the overflow is named in the log with
// a count rather than being silent.
func TestOutboundBufferEvictsOldestOnOverflow(t *testing.T) {
	rec := &recordingHandler{}
	c := newClient(slog.New(rec), clientConfig{leaseID: "lease-overflow"}, nil, nil)
	c.started.Store(true)

	// Size the bound from a real encoded frame so the test asserts on bytes,
	// not on a guessed frame count.
	probe, err := brokerframe.Encode(brokerframe.Frame{
		LeaseID: "lease-overflow",
		Signal:  brokerframe.SignalIO,
		Payload: mustPayload(t, ioMessage{Type: "stream.delta", Content: "msg-00"}),
	})
	if err != nil {
		t.Fatalf("encode probe: %v", err)
	}
	const keep = 3
	c.outLimit = len(probe) * keep

	const total = 10
	for i := 0; i < total; i++ {
		c.SendIO(ioMessage{Type: "stream.delta", Content: fmt.Sprintf("msg-%02d", i)})
	}

	frames, bytes := c.pendingOutbound()
	if frames != keep {
		t.Fatalf("buffer holds %d frames, want %d (bound = %d bytes)", frames, keep, c.outLimit)
	}
	if bytes > c.outLimit {
		t.Fatalf("buffer holds %d bytes, over its %d byte bound", bytes, c.outLimit)
	}

	// What survives is the most recent tail — the part a broker actually needs.
	var got []string
	c.outMu.Lock()
	for _, f := range c.outBuf {
		decoded, err := brokerframe.Decode(f.data)
		if err != nil {
			c.outMu.Unlock()
			t.Fatalf("decode buffered frame: %v", err)
		}
		var msg ioMessage
		if err := json.Unmarshal(decoded.Payload, &msg); err != nil {
			c.outMu.Unlock()
			t.Fatalf("decode buffered payload: %v", err)
		}
		got = append(got, msg.Content)
	}
	c.outMu.Unlock()

	want := []string{"msg-07", "msg-08", "msg-09"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("retained tail = %v, want %v", got, want)
		}
	}

	r, ok := rec.find(slog.LevelWarn, "outbound buffer overflow")
	if !ok {
		t.Fatal("overflow was not logged at WARN")
	}
	dropped, ok := recordAttr(r, "dropped_frames")
	if !ok {
		t.Fatalf("overflow warning carries no dropped_frames attr: %v", r)
	}
	if dropped.Int64() < 1 {
		t.Fatalf("dropped_frames = %d, want >= 1", dropped.Int64())
	}
	if _, ok := recordAttr(r, "dropped_bytes"); !ok {
		t.Fatalf("overflow warning carries no dropped_bytes attr: %v", r)
	}
	if limit, ok := recordAttr(r, "limit_bytes"); !ok || limit.Int64() != int64(c.outLimit) {
		t.Fatalf("overflow warning limit_bytes = %v, want %d", limit, c.outLimit)
	}
}

// TestOutboundOverflowWarningIsRateLimited keeps the log honest under sustained
// overflow: the counts accumulate into the next warning instead of one line per
// dropped token delta.
func TestOutboundOverflowWarningIsRateLimited(t *testing.T) {
	rec := &recordingHandler{}
	c := newClient(slog.New(rec), clientConfig{leaseID: "lease-spam"}, nil, nil)
	c.started.Store(true)
	c.outLimit = 1 // every frame overflows

	for i := 0; i < 50; i++ {
		c.SendIO(ioMessage{Type: "stream.delta", Content: fmt.Sprintf("%d", i)})
	}

	rec.mu.Lock()
	var warns int
	for _, r := range rec.records {
		if r.Level == slog.LevelWarn && strings.Contains(r.Message, "outbound buffer overflow") {
			warns++
		}
	}
	rec.mu.Unlock()
	if warns != 1 {
		t.Fatalf("logged %d overflow warnings for 50 dropped frames, want 1 (rate limit = %s)", warns, c.dropLogEvery)
	}
}

// TestShutdownWithBufferedFramesTerminates is the liveness half of the story: a
// buffer that cannot be flushed must not turn a graceful shutdown into a hang.
//
// The gateway is never started, so the client is in reconnect backoff with a
// full buffer — the worst case — and Stop must still return promptly rather
// than burning its whole context waiting for a link that is not coming back.
func TestShutdownWithBufferedFramesTerminates(t *testing.T) {
	gw := newRestartableGateway(t)

	c := newClient(discardLogger(), clientConfig{addr: gw.wsURL(), leaseID: "lease-stop"}, nil, nil)
	c.minBackoff = 5 * time.Millisecond
	c.maxBackoff = 20 * time.Millisecond
	c.flushTimeout = time.Second
	c.Start()

	for i := 0; i < 500; i++ {
		c.SendIO(ioMessage{Type: "stream.delta", Content: strings.Repeat("x", 512)})
	}
	if n, _ := c.pendingOutbound(); n == 0 {
		t.Fatal("nothing buffered; the test is not exercising the mid-buffer path")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		c.Stop(ctx)
		done <- time.Since(start)
	}()

	select {
	case took := <-done:
		// Nothing is connected, so the drain must be skipped entirely rather
		// than waited out.
		if took > 2*time.Second {
			t.Fatalf("Stop took %s with a full buffer and no link; it should not wait", took)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Stop did not return with a non-empty outbound buffer")
	}
}

// TestShutdownFlushesBufferWhenConnected is the other side of the same
// criterion: bounded does not mean pointless. With a live socket the drain
// gives the write pump its chance, so the last output before a locally
// initiated shutdown still reaches the broker.
func TestShutdownFlushesBufferWhenConnected(t *testing.T) {
	gw := newRestartableGateway(t)
	gw.start(t)

	c := newClient(discardLogger(), clientConfig{addr: gw.wsURL(), leaseID: "lease-flush", sessionID: "sess-flush"}, nil, nil)
	c.minBackoff = 5 * time.Millisecond
	c.maxBackoff = 20 * time.Millisecond
	c.Start()

	// Wait for the handshake so the drain has a live link to work with.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !c.connected() {
		time.Sleep(5 * time.Millisecond)
	}
	if !c.connected() {
		t.Fatal("client never connected to the gateway")
	}

	c.SendIO(ioMessage{Type: "output", Content: "final word"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c.Stop(ctx)

	for _, content := range ioContents(gw.snapshot()) {
		if content == "final word" {
			return
		}
	}
	t.Fatal("the frame buffered at shutdown never reached the gateway")
}

// TestDormantClientDoesNotBuffer guards the plugin's dormant mode: with no
// broker_addr or lease_id Ready never calls Start, and buffering output for a
// transport that will never dial would pin memory and warn about an overflow
// nobody can act on.
func TestDormantClientDoesNotBuffer(t *testing.T) {
	c := newClient(discardLogger(), clientConfig{leaseID: "lease-dormant"}, nil, nil)
	for i := 0; i < 100; i++ {
		c.SendIO(ioMessage{Type: "stream.delta", Content: "ignored"})
	}
	if frames, bytes := c.pendingOutbound(); frames != 0 || bytes != 0 {
		t.Fatalf("dormant client buffered %d frames / %d bytes, want nothing", frames, bytes)
	}
}

func mustPayload(t *testing.T, msg ioMessage) []byte {
	t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal io message: %v", err)
	}
	return data
}
