package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/frankbardon/nexus/pkg/brokerframe"
)

// ioMessage is the opaque IO payload carried inside a brokerframe.Frame on
// SignalIO frames. The broker forwards it untouched between the connected
// client and this instance; only the client at the far end and this plugin
// interpret its fields. It is a flat union — only the fields relevant to a
// given Type are populated, and omitempty keeps frames compact.
type ioMessage struct {
	Type string `json:"type"`

	// Common output/streaming fields.
	TurnID  string `json:"turn_id,omitempty"`
	Content string `json:"content,omitempty"`
	Role    string `json:"role,omitempty"`

	// stream.end
	FinishReason string `json:"finish_reason,omitempty"`

	// status
	State  string `json:"state,omitempty"`
	Detail string `json:"detail,omitempty"`

	// approval.request / approval.response
	PromptID    string `json:"prompt_id,omitempty"`
	Description string `json:"description,omitempty"`
	ToolCall    string `json:"tool_call,omitempty"`
	Risk        string `json:"risk,omitempty"`
	Approved    bool   `json:"approved,omitempty"`
	Always      bool   `json:"always,omitempty"`

	// hitl.request / hitl.response
	RequestID string `json:"request_id,omitempty"`
	Prompt    string `json:"prompt,omitempty"`
	// Mode and Choices carry a multiple-choice question's shape, spelled
	// exactly as ui.HITLRequestMessage spells them so every transport renders
	// one question the same way.
	//
	// They exist because a prompt ALONE is not an answerable question: the
	// responder replies with a choice_id, and without the option list it has no
	// way to learn what the ids are. nexus.io.browser has always forwarded both
	// (plugins/io/browser/plugin.go, handleHITLRequest); this transport dropped
	// them, so a multiple-choice ask_user reached a broker client as bare prose
	// it could only answer as free text. Adding them is a parity fix, not a new
	// capability.
	//
	// Both are omitempty and both are additive: an older broker forwards the
	// SignalIO payload verbatim and a client that does not read them behaves
	// exactly as before, so no brokerframe.Version bump is implied.
	Mode     string     `json:"mode,omitempty"`
	Choices  []ioChoice `json:"choices,omitempty"`
	ChoiceID string     `json:"choice_id,omitempty"`
	FreeText string     `json:"free_text,omitempty"`

	// cancel.complete (server -> client). Pointer so we can distinguish
	// "not set" from "explicit false".
	Resumable *bool `json:"resumable,omitempty"`

	// cancel (client -> server)
	Source string `json:"source,omitempty"`
}

// ioChoice is one option of a multiple-choice hitl.request, mirroring
// ui.HITLChoiceMessage. The ID is what a responder echoes back in ChoiceID; the
// Label is display text and may be empty, in which case a renderer shows the id.
type ioChoice struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

const (
	// defaultOutboundBufferBytes bounds how much un-sent instance output is
	// held while the dial-back socket is down.
	//
	// The number is sized against the reconnect loop it exists to cover:
	// backoff runs 250 ms doubling to a 5 s ceiling, so a broker restart can
	// leave an instance disconnected for several seconds while its agent keeps
	// producing. 1 MiB holds many seconds of token deltas for a chatty agent
	// and still holds several whole tool results for one that only emits large
	// frames. It matches the broker's own per-lease replay bound
	// (client_replay_buffer_bytes), so the two halves of the same stream pin
	// comparable memory.
	defaultOutboundBufferBytes = 1 << 20

	// outboundDropLogEvery rate-limits the overflow warning. Sustained
	// overflow drops about one frame per frame produced, which for token
	// deltas is thousands a second.
	outboundDropLogEvery = time.Second

	// defaultOutboundFlushTimeout caps the best-effort drain in Stop. It is
	// well inside the plugin's own 5 s shutdown budget, so a stuck socket
	// cannot turn a graceful shutdown into a hang.
	defaultOutboundFlushTimeout = 2 * time.Second
)

// client is the dial-back WebSocket client. Unlike the listener-style
// transports (io/browser, io/realtime), this plugin DIALS OUT to the broker
// gateway. The broker is the only listening socket; the client establishes
// the connection, registers its lease, announces readiness, and then pumps
// IO frames in both directions. It reconnects with backoff until its context
// is cancelled.
type client struct {
	logger     *slog.Logger
	cfg        clientConfig
	onIO       func(ioMessage)
	onShutdown func()

	// minBackoff/maxBackoff bound the reconnect loop. Broken out as fields
	// so tests can shrink them.
	minBackoff time.Duration
	maxBackoff time.Duration

	// shutdownRequested is set once the broker sends a SignalShutdown frame.
	// It tells runLoop to stop instead of reconnecting after the session
	// ends, so a graceful teardown is not undone by the reconnect backoff.
	shutdownRequested atomic.Bool

	// started reports whether Start has ever been called. It gates the
	// outbound buffer: a plugin left dormant (no broker_addr / no lease_id)
	// never dials, so buffering its output would pin memory and warn about
	// overflow for a transport nobody is listening to. Dormant SendIO keeps
	// its original behaviour — drop, at debug.
	started atomic.Bool

	mu   sync.Mutex
	conn *websocket.Conn

	// --- outbound buffer (see enqueueOutbound) ---

	outMu sync.Mutex

	// outBuf holds encoded SignalIO frames oldest-first, awaiting the wire.
	outBuf []outboundFrame

	// outBytes is the sum of len(data) over outBuf, tracked incrementally so
	// eviction stays O(evicted) rather than O(buffered).
	outBytes int

	// outLimit is the byte bound. Always positive in practice — it is not
	// configurable and newClient seeds it from defaultOutboundBufferBytes;
	// tests shrink it to exercise eviction.
	outLimit int

	// outNextID labels each buffered frame so the write pump can pop exactly
	// the frame it wrote, even if an overflow evicted it meanwhile.
	outNextID uint64

	// outDroppedFrames/outDroppedBytes accumulate what overflow has discarded
	// since the last warning, and outLastDropLog rate-limits that warning.
	outDroppedFrames int
	outDroppedBytes  int
	outLastDropLog   time.Time

	// outSignal wakes the write pump when a frame is enqueued. Buffered with
	// capacity 1 and signalled non-blockingly, so a producer never waits on a
	// consumer and a wakeup is never lost.
	outSignal chan struct{}

	// dropLogEvery bounds how often an overflow warning is emitted; the
	// counters above make each one cumulative. Broken out so tests can
	// disable the rate limit.
	dropLogEvery time.Duration

	// flushTimeout bounds the best-effort drain in Stop. It is a CAP, not a
	// wait: Stop returns as soon as the buffer empties, the link drops, or
	// the caller's context expires — whichever comes first.
	flushTimeout time.Duration

	runCtx    context.Context
	runCancel context.CancelFunc
	done      chan struct{}
}

// outboundFrame is one encoded SignalIO frame held in the outbound buffer.
//
// The id exists because the write pump peeks a frame, releases the lock to
// write it, and only then pops it: an overflow landing in that window may have
// already evicted it, and popping "the front" blindly would then discard a
// frame that was never sent.
type outboundFrame struct {
	id   uint64
	data []byte
}

// clientConfig is the set of spawn-time coordinates a dial-back client needs.
//
// It is a struct rather than four positional string parameters because they are
// all strings and one of them is a credential: a transposed pair would compile
// cleanly and send the spawn secret where the lease id belongs, putting a secret
// on a surface that is logged and echoed to clients.
type clientConfig struct {
	// addr is the ws:// URL of the broker's instance dial-back endpoint.
	addr string

	// leaseID identifies the lease and is echoed in every frame.
	leaseID string

	// spawnSecret is the per-spawn second factor echoed in the register frame.
	// Empty is valid — an unauthenticated broker does not check it — and the
	// frame then simply omits the field.
	spawnSecret string

	// sessionID is the engine session id reported after ready, when non-empty.
	sessionID string
}

// newClient constructs a dial-back client. It does not dial until Start.
// onShutdown is invoked (once) when the broker sends a SignalShutdown frame so
// the plugin can trigger a graceful engine shutdown; it may be nil.
func newClient(logger *slog.Logger, cfg clientConfig, onIO func(ioMessage), onShutdown func()) *client {
	if logger == nil {
		logger = slog.Default()
	}
	return &client{
		logger:       logger,
		cfg:          cfg,
		onIO:         onIO,
		onShutdown:   onShutdown,
		minBackoff:   250 * time.Millisecond,
		maxBackoff:   5 * time.Second,
		outLimit:     defaultOutboundBufferBytes,
		outSignal:    make(chan struct{}, 1),
		dropLogEvery: outboundDropLogEvery,
		flushTimeout: defaultOutboundFlushTimeout,
		done:         make(chan struct{}),
	}
}

// Start launches the reconnect loop on a background goroutine and returns
// immediately. The loop runs until Stop is called.
func (c *client) Start() {
	c.runCtx, c.runCancel = context.WithCancel(context.Background())
	c.started.Store(true)
	go c.runLoop()
}

// Stop cancels the reconnect loop and closes the active connection, then
// waits for the loop goroutine to exit (bounded by the supplied context).
func (c *client) Stop(ctx context.Context) {
	if c.runCancel == nil {
		// Start was never called — the plugin stayed dormant because no
		// broker_addr was configured. There is no loop goroutine, so `done` will
		// never close and the wait below would burn the caller's entire timeout
		// (5s) on every shutdown of a dormant instance.
		return
	}
	// Best-effort flush BEFORE the link is torn down, so the last output of a
	// locally-initiated shutdown is not left in the buffer. It is bounded three
	// ways — buffer empty, flushTimeout, or the caller's context — and returns
	// immediately when nothing is connected, which is the case on the
	// broker-initiated path (the shutdown frame ends the session first).
	c.drainOutbound(ctx)
	c.runCancel()
	c.closeConn(websocket.StatusNormalClosure, "shutting down")
	select {
	case <-c.done:
	case <-ctx.Done():
	}
}

// drainOutbound waits, bounded, for the write pump to empty the outbound
// buffer. It NEVER waits on a downed link: with no connection there is nothing
// draining the buffer, so waiting could only burn the caller's whole timeout.
func (c *client) drainOutbound(ctx context.Context) {
	if c.flushTimeout <= 0 || !c.connected() {
		return
	}
	deadline := time.NewTimer(c.flushTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		frames, bytes := c.pendingOutbound()
		if frames == 0 || !c.connected() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			c.logger.Warn("broker outbound buffer not flushed before shutdown",
				"pending_frames", frames, "pending_bytes", bytes)
			return
		case <-tick.C:
		}
	}
}

// connected reports whether a dial-back socket is currently established.
func (c *client) connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// runLoop dials the broker, registers, and pumps until the connection drops,
// then backs off and retries until the run context is cancelled.
func (c *client) runLoop() {
	defer close(c.done)
	backoff := c.minBackoff
	for {
		if c.runCtx.Err() != nil || c.shutdownRequested.Load() {
			return
		}
		if err := c.session(); err != nil && c.runCtx.Err() == nil {
			c.logger.Warn("broker connection lost", "error", err, "retry_in", backoff)
			select {
			case <-c.runCtx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < c.maxBackoff {
				backoff *= 2
				if backoff > c.maxBackoff {
					backoff = c.maxBackoff
				}
			}
			continue
		}
		// Clean exit (context cancelled) — leave.
		if c.runCtx.Err() != nil {
			return
		}
		backoff = c.minBackoff
	}
}

// session dials the broker, performs the register/ready/session-id handshake,
// then reads frames until the connection closes or the context is cancelled.
func (c *client) session() error {
	dialCtx, cancel := context.WithTimeout(c.runCtx, 10*time.Second)
	conn, _, err := websocket.Dial(dialCtx, c.cfg.addr, nil)
	cancel()
	if err != nil {
		return fmt.Errorf("dial broker %s: %w", c.cfg.addr, err)
	}

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer c.closeConn(websocket.StatusNormalClosure, "")

	// First frame MUST be register so the broker can bind this socket to
	// the lease (E1-S2 contract). It carries the spawn secret alongside the
	// lease id: the lease id says WHICH lease, the secret proves this process is
	// the one the broker spawned for it. The secret is sent on this frame only —
	// it is meaningless on the others and every later frame is forwarded to a
	// client verbatim.
	if err := c.send(brokerframe.Frame{
		LeaseID: c.cfg.leaseID,
		Signal:  brokerframe.SignalRegister,
		Secret:  c.cfg.spawnSecret,
	}); err != nil {
		return fmt.Errorf("send register: %w", err)
	}
	// Announce readiness to accept IO.
	if err := c.send(brokerframe.Frame{LeaseID: c.cfg.leaseID, Signal: brokerframe.SignalReady}); err != nil {
		return fmt.Errorf("send ready: %w", err)
	}
	// Report the engine session id so the broker can persist it for -recall.
	if c.cfg.sessionID != "" {
		if err := c.send(brokerframe.Frame{
			LeaseID:   c.cfg.leaseID,
			Signal:    brokerframe.SignalSessionIDReport,
			SessionID: c.cfg.sessionID,
		}); err != nil {
			return fmt.Errorf("send session-id-report: %w", err)
		}
	}

	c.logger.Info("registered with broker", "addr", c.cfg.addr, "lease_id", c.cfg.leaseID)

	// The write pump starts HERE and not a line earlier. Everything buffered
	// while the link was down is flushed by it, so starting it before the three
	// frames above would let a reconnecting instance's output reach the broker
	// ahead of the register frame that binds this socket to the lease.
	pumpCtx, pumpCancel := context.WithCancel(c.runCtx)
	defer pumpCancel()
	go c.writePump(pumpCtx, conn)

	return c.readPump(conn)
}

// writePump drains the outbound buffer onto conn for the life of one session.
//
// It is the ONLY writer of SignalIO frames, which is what makes the buffer's
// order the wire's order: a single consumer taking frames oldest-first cannot
// interleave them however many bus handlers are producing.
//
// A frame is popped only AFTER it has been written, so a write that fails on a
// dying socket leaves it at the head of the buffer for the next session rather
// than consuming it.
func (c *client) writePump(ctx context.Context, conn *websocket.Conn) {
	for {
		frame, ok := c.peekOutbound()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-c.outSignal:
			}
			continue
		}
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := conn.Write(writeCtx, websocket.MessageText, frame.data)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				c.logger.Debug("broker outbound write failed; frame retained", "error", err)
				// Close the socket so the read pump unwinds and runLoop
				// reconnects; the retained frame goes out after the next
				// handshake. Guarded on identity so a pump that outlived its
				// session can never close its successor's connection.
				c.closeConnIf(conn, websocket.StatusInternalError, "write failed")
			}
			return
		}
		c.popOutbound(frame.id)
	}
}

// readPump reads frames until an error or context cancellation. SignalIO
// frames are handed to onIO; SignalShutdown ends the session cleanly.
func (c *client) readPump(conn *websocket.Conn) error {
	for {
		_, data, err := conn.Read(c.runCtx)
		if err != nil {
			if c.runCtx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read frame: %w", err)
		}
		frame, err := brokerframe.Decode(data)
		if err != nil {
			c.logger.Debug("broker frame decode failed", "error", err)
			continue
		}
		switch frame.Signal {
		case brokerframe.SignalIO:
			if c.onIO == nil || len(frame.Payload) == 0 {
				continue
			}
			var msg ioMessage
			if err := json.Unmarshal(frame.Payload, &msg); err != nil {
				c.logger.Debug("broker io payload decode failed", "error", err)
				continue
			}
			c.onIO(msg)
		case brokerframe.SignalShutdown:
			c.logger.Info("broker requested shutdown")
			// Latch shutdown so runLoop does not reconnect, then trigger the
			// plugin's graceful engine shutdown. Returning ends the read pump
			// cleanly; the engine flushes/persists the session before exit.
			c.shutdownRequested.Store(true)
			if c.onShutdown != nil {
				c.onShutdown()
			}
			return nil
		default:
			c.logger.Debug("ignoring inbound broker signal", "signal", frame.Signal)
		}
	}
}

// SendIO marshals an ioMessage into a SignalIO frame and hands it to the
// outbound buffer. It never touches the socket, so it never blocks: the engine
// dispatches bus events synchronously, so this runs on the goroutine that is
// producing the agent's output.
//
// It used to write the frame inline and DROP it when the socket was down. That
// is exactly the state an instance is in while it backs off through a broker
// restart (250 ms doubling to 5 s), so the output produced during the very
// window reattach is meant to make seamless was lost before the broker could
// ever see it — and, being lost on the instance side, out of reach of the
// broker's own replay buffer. Buffering here closes that path.
func (c *client) SendIO(msg ioMessage) {
	payload, err := json.Marshal(msg)
	if err != nil {
		c.logger.Error("broker io marshal failed", "type", msg.Type, "error", err)
		return
	}
	data, err := brokerframe.Encode(brokerframe.Frame{
		LeaseID: c.cfg.leaseID,
		Signal:  brokerframe.SignalIO,
		Payload: payload,
	})
	if err != nil {
		c.logger.Error("broker io encode failed", "type", msg.Type, "error", err)
		return
	}
	if !c.started.Load() {
		// Dormant transport: no broker_addr or no lease_id, so Ready never
		// dialled and never will. Keep the original drop-at-debug behaviour
		// rather than pinning memory for a link that is not coming up.
		c.logger.Debug("dropping broker frame", "type", msg.Type, "reason", "transport not started")
		return
	}
	c.enqueueOutbound(data)
}

// enqueueOutbound appends one encoded frame and evicts oldest-first until the
// byte bound holds, then wakes the write pump.
//
// The bound is in BYTES rather than frames because outbound payloads span
// orders of magnitude — a token delta is a few bytes, a tool result is tens of
// kilobytes — so a frame count says nothing about how much memory an instance
// can pin while it is disconnected, and a byte bound says exactly.
//
// Oldest-first is the right eviction end for the same reason it is on the
// broker: what a client needs after a gap is the most recent tail.
func (c *client) enqueueOutbound(data []byte) {
	c.outMu.Lock()
	c.outNextID++
	c.outBuf = append(c.outBuf, outboundFrame{id: c.outNextID, data: data})
	c.outBytes += len(data)

	// A single frame larger than the whole bound is appended and then evicted
	// by this same loop, leaving the buffer empty rather than over its bound.
	// That is the honest outcome: the alternative is a bound one big tool
	// result can breach.
	for c.outBytes > c.outLimit && len(c.outBuf) > 0 {
		evicted := c.outBuf[0]
		c.outBytes -= len(evicted.data)
		c.outDroppedFrames++
		c.outDroppedBytes += len(evicted.data)
		// Release the payload before the reslice; the backing array outlives
		// the reslice and would otherwise keep the bytes alive.
		c.outBuf[0].data = nil
		c.outBuf = c.outBuf[1:]
	}

	// Report overflow with CUMULATIVE counts on a rate limit. A sustained
	// overflow drops roughly one frame per new frame, so logging each one
	// would bury the fact it is happening under the noise of it happening.
	var (
		logFrames, logBytes int
		shouldLog           bool
	)
	if c.outDroppedFrames > 0 {
		now := time.Now()
		if c.outLastDropLog.IsZero() || c.dropLogEvery <= 0 || now.Sub(c.outLastDropLog) >= c.dropLogEvery {
			shouldLog = true
			logFrames, logBytes = c.outDroppedFrames, c.outDroppedBytes
			c.outDroppedFrames, c.outDroppedBytes = 0, 0
			c.outLastDropLog = now
		}
	}
	c.outMu.Unlock()

	if shouldLog {
		c.logger.Warn("broker outbound buffer overflow; dropped oldest frames",
			"dropped_frames", logFrames, "dropped_bytes", logBytes,
			"limit_bytes", c.outLimit, "lease_id", c.cfg.leaseID)
	}

	select {
	case c.outSignal <- struct{}{}:
	default:
	}
}

// peekOutbound returns the oldest buffered frame without removing it.
func (c *client) peekOutbound() (outboundFrame, bool) {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	if len(c.outBuf) == 0 {
		return outboundFrame{}, false
	}
	return c.outBuf[0], true
}

// popOutbound removes the oldest buffered frame if it is still the one with
// the given id. A mismatch means an overflow evicted it while it was being
// written, in which case there is nothing to remove.
func (c *client) popOutbound(id uint64) {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	if len(c.outBuf) == 0 || c.outBuf[0].id != id {
		return
	}
	c.outBytes -= len(c.outBuf[0].data)
	c.outBuf[0].data = nil
	c.outBuf = c.outBuf[1:]
}

// pendingOutbound reports how much is still waiting on the wire.
func (c *client) pendingOutbound() (frames, bytes int) {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	return len(c.outBuf), c.outBytes
}

// send encodes and writes a frame under the write/connection lock.
func (c *client) send(frame brokerframe.Frame) error {
	data, err := brokerframe.Encode(frame)
	if err != nil {
		return fmt.Errorf("encode frame: %w", err)
	}
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("not connected")
	}
	writeCtx := c.runCtx
	if writeCtx == nil {
		writeCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(writeCtx, 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}
	return nil
}

// closeConnIf closes and clears the active connection only when it is still
// target. The guard matters because a write pump can outlive the session that
// spawned it by a moment; without it, a stale pump could close the connection
// the NEXT session just established.
func (c *client) closeConnIf(target *websocket.Conn, status websocket.StatusCode, reason string) {
	c.mu.Lock()
	if c.conn != target {
		c.mu.Unlock()
		return
	}
	c.conn = nil
	c.mu.Unlock()
	_ = target.Close(status, reason)
}

// closeConn closes and clears the active connection if any.
func (c *client) closeConn(status websocket.StatusCode, reason string) {
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close(status, reason)
	}
}
