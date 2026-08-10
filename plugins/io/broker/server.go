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
	ChoiceID  string `json:"choice_id,omitempty"`
	FreeText  string `json:"free_text,omitempty"`

	// cancel.complete (server -> client). Pointer so we can distinguish
	// "not set" from "explicit false".
	Resumable *bool `json:"resumable,omitempty"`

	// cancel (client -> server)
	Source string `json:"source,omitempty"`
}

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

	mu   sync.Mutex
	conn *websocket.Conn

	runCtx    context.Context
	runCancel context.CancelFunc
	done      chan struct{}
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
		logger:     logger,
		cfg:        cfg,
		onIO:       onIO,
		onShutdown: onShutdown,
		minBackoff: 250 * time.Millisecond,
		maxBackoff: 5 * time.Second,
		done:       make(chan struct{}),
	}
}

// Start launches the reconnect loop on a background goroutine and returns
// immediately. The loop runs until Stop is called.
func (c *client) Start() {
	c.runCtx, c.runCancel = context.WithCancel(context.Background())
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
	c.runCancel()
	c.closeConn(websocket.StatusNormalClosure, "shutting down")
	select {
	case <-c.done:
	case <-ctx.Done():
	}
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
	return c.readPump(conn)
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

// SendIO marshals an ioMessage into a SignalIO frame and writes it to the
// broker. It is a no-op (logged at debug) when not currently connected so
// bus handlers never block on a downed link.
func (c *client) SendIO(msg ioMessage) {
	payload, err := json.Marshal(msg)
	if err != nil {
		c.logger.Error("broker io marshal failed", "type", msg.Type, "error", err)
		return
	}
	frame := brokerframe.Frame{
		LeaseID: c.cfg.leaseID,
		Signal:  brokerframe.SignalIO,
		Payload: payload,
	}
	if err := c.send(frame); err != nil {
		c.logger.Debug("dropping broker frame", "type", msg.Type, "error", err)
	}
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
