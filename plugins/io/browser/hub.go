package browser

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/ui"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Hub manages WebSocket client connections and broadcasts messages.
type Hub struct {
	mu      sync.RWMutex
	clients map[string]*Client
	logger  *slog.Logger

	onMessage func(clientID string, env ui.Envelope)
}

// Client represents a single WebSocket connection.
type Client struct {
	id        string
	conn      *websocket.Conn
	send      chan []byte
	hub       *Hub
	done      chan struct{}
	userAgent string
	connectedAt time.Time
}

// NewHub creates a new connection hub.
func NewHub(logger *slog.Logger) *Hub {
	return &Hub{
		clients: make(map[string]*Client),
		logger:  logger,
	}
}

// OnMessage registers a callback for inbound client messages.
func (h *Hub) OnMessage(fn func(clientID string, env ui.Envelope)) {
	h.onMessage = fn
}

// Register adds a client to the hub and starts its read/write pumps.
func (h *Hub) Register(client *Client) {
	h.mu.Lock()
	h.clients[client.id] = client
	h.mu.Unlock()
	h.logger.Info("client connected", "client_id", client.id)
}

// Unregister removes a client from the hub and closes its send channel.
func (h *Hub) Unregister(id string) {
	h.mu.Lock()
	if c, ok := h.clients[id]; ok {
		close(c.send)
		delete(h.clients, id)
	}
	h.mu.Unlock()
	h.logger.Info("client disconnected", "client_id", id)
}

// Broadcast sends raw JSON bytes to all connected clients.
func (h *Hub) Broadcast(data []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.clients {
		select {
		case c.send <- data:
		default:
			h.logger.Warn("client send buffer full, dropping message", "client_id", c.id)
		}
	}
}

// Close closes all client connections and removes them from the hub.
func (h *Hub) Close() {
	h.mu.Lock()
	for id, c := range h.clients {
		c.conn.Close(websocket.StatusGoingAway, "server shutting down")
		close(c.send)
		delete(h.clients, id)
	}
	h.mu.Unlock()
}

// ClientCount returns the number of connected clients.
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Sessions returns session info for all connected clients.
func (h *Hub) Sessions() []ui.SessionInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	sessions := make([]ui.SessionInfo, 0, len(h.clients))
	for _, c := range h.clients {
		sessions = append(sessions, ui.SessionInfo{
			ID:          c.id,
			Transport:   "browser",
			ConnectedAt: c.connectedAt,
			UserAgent:   c.userAgent,
		})
	}
	return sessions
}

// ServeClient starts the read and write pumps for a client.
func (h *Hub) ServeClient(ctx context.Context, client *Client) {
	go client.writePump(ctx)
	client.readPump(ctx)
}

func (c *Client) readPump(ctx context.Context) {
	defer func() {
		c.hub.Unregister(c.id)
		c.conn.Close(websocket.StatusNormalClosure, "")
	}()

	for {
		var env ui.Envelope
		err := wsjson.Read(ctx, c.conn, &env)
		if err != nil {
			if ctx.Err() == nil {
				c.hub.logger.Debug("client read error", "client_id", c.id, "error", err)
			}
			return
		}

		if c.hub.onMessage != nil {
			c.hub.onMessage(c.id, env)
		}
	}
}

func (c *Client) writePump(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case msg, ok := <-c.send:
			if !ok {
				return
			}
			err := c.conn.Write(ctx, websocket.MessageText, msg)
			if err != nil {
				c.hub.logger.Debug("client write error", "client_id", c.id, "error", err)
				return
			}
		}
	}
}

// BroadcastEnvelope marshals an envelope and broadcasts it.
func (h *Hub) BroadcastEnvelope(env ui.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	h.Broadcast(data)
	return nil
}
