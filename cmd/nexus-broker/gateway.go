package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/frankbardon/nexus/pkg/brokerframe"
)

const (
	// instanceWSPath is where spawned instances dial back to register.
	instanceWSPath = "/instance"

	// clientWSPathPrefix is the prefix for the per-lease client endpoint.
	// The full pattern is clientWSPathPrefix + "{lease_id}". See ClientWSPath.
	clientWSPathPrefix = "/lease/"

	// registerTimeout bounds how long the gateway waits for an instance's
	// first (register) frame before rejecting the dial-back.
	registerTimeout = 10 * time.Second
)

// ClientWSPath returns the WebSocket path a client uses to reach the instance
// claimed under the given lease id. E1-S4's POST /claim returns this to the
// caller so it knows where to connect.
func ClientWSPath(leaseID string) string {
	return clientWSPathPrefix + leaseID
}

// Gateway owns the WebSocket endpoints and routes brokerframe.Frame messages
// between each lease's client and instance connections. It is protocol-aware:
// it decodes every frame and routes by signal/lease rather than blind-piping
// bytes, so later stories can observe turns and idleness.
type Gateway struct {
	logger   *slog.Logger
	registry *Registry

	// auth resolves the credential on an inbound CLIENT WebSocket request so
	// handleClient can check lease ownership. The gateway holds the guard rather
	// than reading a Principal out of the request context because its routes stay
	// on the raw mux: the client socket gains a query-parameter ticket credential
	// that bearer-header middleware cannot express, so it must resolve its own.
	//
	// A nil guard means authentication is disabled, which resolves every caller to
	// the anonymous identity — the owner every lease carries when the broker runs
	// with no `auth:` block.
	auth *authGuard

	// rootCtx is cancelled on Shutdown so all read/write pumps exit.
	rootCtx    context.Context
	rootCancel context.CancelFunc
}

// NewGateway constructs a gateway over the given registry. auth may be nil (or a
// guard over an empty chain), which disables credential resolution and makes
// every client caller anonymous.
func NewGateway(logger *slog.Logger, registry *Registry, auth *authGuard) *Gateway {
	if logger == nil {
		logger = slog.Default()
	}
	rootCtx, rootCancel := context.WithCancel(context.Background())
	return &Gateway{
		logger:     logger,
		registry:   registry,
		auth:       auth,
		rootCtx:    rootCtx,
		rootCancel: rootCancel,
	}
}

// Register wires the gateway's WebSocket endpoints onto a mux. It takes a
// routeMux for symmetry with the HTTP servers, though main deliberately
// registers these two routes unguarded — see run(). The client route names its
// wildcard {lease_id} so the shared audit helpers find the lease id on it just as
// they do on POST /release/{lease_id}.
func (g *Gateway) Register(mux routeMux) {
	mux.HandleFunc("GET "+instanceWSPath, g.handleInstance)
	mux.HandleFunc("GET "+clientWSPathPrefix+"{lease_id}", g.handleClient)
}

// Shutdown cancels all in-flight pumps so connections close cleanly.
func (g *Gateway) Shutdown() {
	g.rootCancel()
}

// handleInstance accepts an inbound dial-back from a spawned instance. The
// first frame MUST be a register frame carrying a known lease id; otherwise
// the connection is rejected and closed.
func (g *Gateway) handleInstance(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		g.logger.Error("instance websocket accept failed", "error", err)
		return
	}

	// Read the mandatory register frame under a bounded timeout.
	readCtx, cancel := context.WithTimeout(g.rootCtx, registerTimeout)
	_, data, err := conn.Read(readCtx)
	cancel()
	if err != nil {
		g.logger.Warn("instance closed before register frame", "error", err)
		_ = conn.Close(websocket.StatusPolicyViolation, "no register frame")
		return
	}

	frame, err := brokerframe.Decode(data)
	if err != nil {
		g.logger.Warn("instance sent undecodable first frame", "error", err)
		_ = conn.Close(websocket.StatusPolicyViolation, "invalid register frame")
		return
	}
	if frame.Signal != brokerframe.SignalRegister {
		g.logger.Warn("instance first frame was not register", "signal", frame.Signal)
		_ = conn.Close(websocket.StatusPolicyViolation, "expected register frame")
		return
	}

	leaseID := frame.LeaseID
	wc := newWSConn(conn)
	if err := g.registry.AttachInstance(leaseID, wc); err != nil {
		g.logger.Warn("rejecting instance registration", "lease_id", leaseID, "error", err)
		_ = conn.Close(websocket.StatusPolicyViolation, "unknown lease")
		return
	}

	g.logger.Info("instance registered", "lease_id", leaseID)

	ctx, cancelPumps := context.WithCancel(g.rootCtx)
	defer cancelPumps()

	go g.writePump(ctx, wc)
	// Instance read pump: forward decoded frames to the lease's client conn.
	// Lifecycle signals from the instance are observed here so the gateway can
	// unblock POST /claim when the engine reports ready.
	g.readPump(ctx, leaseID, wc, func(id string) *wsConn {
		return g.registry.ClientConn(id)
	}, func(f brokerframe.Frame) {
		switch f.Signal {
		case brokerframe.SignalReady:
			g.registry.MarkReady(leaseID)
		case brokerframe.SignalSessionIDReport:
			g.registry.MarkSessionID(leaseID, f.SessionID)
		}
	})

	g.registry.DetachInstance(leaseID, wc)
	wc.shutdown(websocket.StatusNormalClosure, "")
	g.logger.Info("instance disconnected", "lease_id", leaseID)
}

// handleClient accepts a client connection for a specific lease and routes its
// frames to the lease's instance connection.
//
// BOTH credential resolution and the ownership check happen before
// websocket.Accept, so a refused caller gets a plain HTTP refusal and never an
// upgraded socket that is immediately closed. An accept-then-close is observably
// different from a clean refusal — it confirms the lease id reached a live
// handler — which would defeat the point of returning an indistinguishable 404.
func (g *Gateway) handleClient(w http.ResponseWriter, r *http.Request) {
	leaseID := r.PathValue("lease_id")
	if leaseID == "" {
		http.Error(w, unknownLeaseError, http.StatusNotFound)
		return
	}

	// Authenticate first, and answer a bad or missing credential with the guard's
	// usual 401/403 envelope. That branch never touches the registry, so it leaks
	// nothing about whether the lease exists. A nil guard is a DISABLED guard:
	// resolvePrincipal then short-circuits to the anonymous identity with no error,
	// so deny is unreachable and this route behaves as it always did.
	caller, err := g.auth.resolvePrincipal(r)
	if err != nil {
		g.auth.deny(w, r, err)
		return
	}

	// Then ownership. ownsLease folds "no such lease" and "someone else's lease"
	// into one false, and this writes the byte-identical response the pre-ownership
	// unknown-lease miss wrote, so the two remain indistinguishable.
	if !ownsLease(g.registry, leaseID, caller) {
		logLeaseDenied(g.logger, r, caller, leaseID)
		http.Error(w, unknownLeaseError, http.StatusNotFound)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		g.logger.Error("client websocket accept failed", "error", err)
		return
	}

	wc := newWSConn(conn)
	if err := g.registry.AttachClient(leaseID, wc); err != nil {
		g.logger.Warn("rejecting client connection", "lease_id", leaseID, "error", err)
		_ = conn.Close(websocket.StatusPolicyViolation, "lease unavailable")
		return
	}

	g.logger.Info("client connected", "lease_id", leaseID)

	ctx, cancelPumps := context.WithCancel(g.rootCtx)
	defer cancelPumps()

	go g.writePump(ctx, wc)
	// Client read pump: forward decoded frames to the lease's instance conn.
	// Stamp last-activity ONLY for real user input (io frames flowing client →
	// instance) so the idle sweeper resets on genuine activity and not on
	// instance output, pings, or control frames.
	g.readPump(ctx, leaseID, wc, func(id string) *wsConn {
		return g.registry.InstanceConn(id)
	}, func(f brokerframe.Frame) {
		if f.Signal == brokerframe.SignalIO {
			g.registry.markActivity(leaseID)
		}
	})

	g.registry.DetachClient(leaseID, wc)
	wc.shutdown(websocket.StatusNormalClosure, "")
	g.logger.Info("client disconnected", "lease_id", leaseID)
}

// readPump reads frames from wc, decodes them (protocol-aware), and forwards
// each to the peer connection resolved by peerFor. The optional observe
// callback is invoked for every decoded frame before forwarding, letting the
// caller react to lifecycle signals (e.g. ready) without disturbing routing.
// It returns when the connection closes or its context is cancelled.
func (g *Gateway) readPump(ctx context.Context, leaseID string, wc *wsConn, peerFor func(string) *wsConn, observe func(brokerframe.Frame)) {
	for {
		_, data, err := wc.conn.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				g.logger.Debug("read pump ended", "lease_id", leaseID, "error", err)
			}
			return
		}

		frame, err := brokerframe.Decode(data)
		if err != nil {
			g.logger.Warn("dropping undecodable frame", "lease_id", leaseID, "error", err)
			continue
		}
		if observe != nil {
			observe(frame)
		}
		if frame.LeaseID != "" && frame.LeaseID != leaseID {
			g.logger.Warn("dropping frame with mismatched lease",
				"bound_lease", leaseID, "frame_lease", frame.LeaseID)
			continue
		}

		peer := peerFor(leaseID)
		if peer == nil {
			g.logger.Debug("no peer attached, dropping frame",
				"lease_id", leaseID, "signal", frame.Signal)
			continue
		}
		if !peer.queue(data) {
			g.logger.Warn("peer send buffer full, dropping frame",
				"lease_id", leaseID, "signal", frame.Signal)
		}
	}
}

// writePump drains wc.send and writes each frame to the WebSocket until the
// context is cancelled or the connection is closed.
func (g *Gateway) writePump(ctx context.Context, wc *wsConn) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-wc.closed:
			return
		case data, ok := <-wc.send:
			if !ok {
				return
			}
			if err := wc.conn.Write(ctx, websocket.MessageText, data); err != nil {
				if ctx.Err() == nil {
					g.logger.Debug("write pump ended", "error", err)
				}
				return
			}
		}
	}
}
