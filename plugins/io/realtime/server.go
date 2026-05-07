package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// envelope is the JSON message carried over the WebSocket in either
// direction. Fields are a flat union — only the ones relevant to a given
// envelope Type are populated. JSON tags use omitempty so each frame
// stays compact.
type envelope struct {
	Type string `json:"type"`

	// Common fields used by multiple envelope types.
	TurnID  string `json:"turn_id,omitempty"`
	Content string `json:"content,omitempty"`

	// stream.end
	FinishReason string `json:"finish_reason,omitempty"`

	// tool.preview
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name,omitempty"`
	Args map[string]any `json:"args,omitempty"`

	// audio.chunk (both directions)
	Sequence    int    `json:"sequence,omitempty"`
	AudioBase64 string `json:"audio_base64,omitempty"`
	MimeType    string `json:"mime_type,omitempty"`
	Final       bool   `json:"final,omitempty"`

	// cancel.complete (server -> client). Pointer so we can distinguish
	// "not set" from "explicit false" — Resumable=false matters to clients.
	Resumable *bool `json:"resumable,omitempty"`

	// hitl.request (server -> client) and approval (client -> server).
	Prompt    string `json:"prompt,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Decision  string `json:"decision,omitempty"`
}

// Server is the HTTP/WebSocket front-end for the realtime IO plugin. It
// owns a connection set, a single mux registered at the configured path,
// and a handler that the plugin calls into for each inbound envelope.
type Server struct {
	logger     *slog.Logger
	addr       string
	path       string
	maxClients int
	onInbound  func(envelope)

	mu       sync.RWMutex
	clients  map[*client]struct{}
	listener net.Listener
	httpSrv  *http.Server

	// rootCtx is cancelled on Shutdown so all read/write pumps exit. New
	// connections accepted after Shutdown see an immediately-cancelled
	// context and close cleanly.
	rootCtx    context.Context
	rootCancel context.CancelFunc
}

// client is a single connected websocket peer.
type client struct {
	conn *websocket.Conn
	send chan envelope
	done chan struct{}
}

// NewServer constructs a server but does not listen yet — call Start.
// Mostly broken out from the plugin so tests can construct a server
// against an ephemeral port without touching plugin lifecycle.
func NewServer(logger *slog.Logger, addr, path string, maxClients int, onInbound func(envelope)) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	rootCtx, rootCancel := context.WithCancel(context.Background())
	return &Server{
		logger:     logger,
		addr:       addr,
		path:       path,
		maxClients: maxClients,
		onInbound:  onInbound,
		clients:    make(map[*client]struct{}),
		rootCtx:    rootCtx,
		rootCancel: rootCancel,
	}
}

// Handler returns the http.Handler the server uses to upgrade incoming
// WebSocket connections. Exposed so tests can mount it on httptest.Server
// without going through Start().
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(s.path, s.handleWebSocket)
	return mux
}

// Start binds to the configured address and serves WebSocket upgrades.
// Returns once the listener is bound; the actual Serve runs in a
// background goroutine.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.addr, err)
	}
	s.listener = ln

	s.httpSrv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	s.logger.Info("realtime IO server started", "addr", ln.Addr().String(), "path", s.path)

	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logger.Error("realtime server error", "error", err)
		}
	}()

	return nil
}

// Addr returns the listener's bound address (useful for tests where
// addr=":0" picks an ephemeral port).
func (s *Server) Addr() string {
	if s.listener == nil {
		return s.addr
	}
	return s.listener.Addr().String()
}

// Shutdown closes all client connections and stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.rootCancel()

	s.mu.Lock()
	for c := range s.clients {
		_ = c.conn.Close(websocket.StatusGoingAway, "server shutting down")
		select {
		case <-c.done:
		default:
			close(c.done)
		}
	}
	s.clients = make(map[*client]struct{})
	s.mu.Unlock()

	if s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Shutdown(ctx)
}

// Broadcast queues an envelope for every connected client. A client whose
// send buffer is full is logged and dropped on this frame — we do not
// block on slow consumers.
func (s *Server) Broadcast(env envelope) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for c := range s.clients {
		select {
		case c.send <- env:
		default:
			s.logger.Warn("realtime client send buffer full, dropping frame",
				"type", env.Type)
		}
	}
}

// ClientCount returns the number of currently-connected clients.
func (s *Server) ClientCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.clients)
}

// handleWebSocket upgrades the request and runs read/write pumps until
// the connection closes. **No auth in v1** — operators must front this
// with a reverse proxy when exposed beyond localhost. See the package
// godoc and docs/src/configuration/reference.md for the follow-up.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Cap at maxClients before upgrading so an abusive caller can't
	// exhaust descriptors. Race window between this check and Add is
	// acceptable for an alpha — exact enforcement is a follow-up if it
	// matters.
	if s.ClientCount() >= s.maxClients {
		http.Error(w, "max clients reached", http.StatusServiceUnavailable)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Permissive origin policy — same as io/browser. Tighter origin
		// checks land alongside auth in a future revision.
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		s.logger.Error("websocket accept failed", "error", err)
		return
	}

	c := &client{
		conn: conn,
		send: make(chan envelope, 256),
		done: make(chan struct{}),
	}

	s.mu.Lock()
	s.clients[c] = struct{}{}
	s.mu.Unlock()

	s.logger.Debug("realtime client connected", "remote", r.RemoteAddr)

	// Bind the per-connection lifetime to rootCtx so Shutdown propagates.
	ctx, cancel := context.WithCancel(s.rootCtx)
	defer cancel()

	go s.writePump(ctx, c)
	s.readPump(ctx, c)

	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()

	_ = conn.Close(websocket.StatusNormalClosure, "")
	s.logger.Debug("realtime client disconnected", "remote", r.RemoteAddr)
}

func (s *Server) readPump(ctx context.Context, c *client) {
	defer func() {
		select {
		case <-c.done:
		default:
			close(c.done)
		}
	}()
	for {
		var env envelope
		if err := wsjson.Read(ctx, c.conn, &env); err != nil {
			if ctx.Err() == nil {
				s.logger.Debug("realtime read error", "error", err)
			}
			return
		}
		if s.onInbound != nil {
			s.onInbound(env)
		}
	}
}

func (s *Server) writePump(ctx context.Context, c *client) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case env, ok := <-c.send:
			if !ok {
				return
			}
			data, err := json.Marshal(env)
			if err != nil {
				s.logger.Error("realtime marshal failed", "error", err)
				continue
			}
			if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
				if ctx.Err() == nil {
					s.logger.Debug("realtime write error", "error", err)
				}
				return
			}
		}
	}
}
