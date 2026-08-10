package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/frankbardon/nexus/pkg/brokerframe"
	"github.com/frankbardon/nexus/pkg/nexusauth"
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

	// ticketQueryParam is the query parameter a client presents its single-use
	// lease ticket in on the client WebSocket handshake.
	//
	// A query parameter rather than a header because it is the ONLY channel a
	// browser has: JavaScript cannot set headers on a WebSocket upgrade. Every
	// other property of a ticket — the 30s TTL, the single use, the
	// never-log-the-value rule — exists to contain the exposure this carrier
	// implies (URLs reach proxy access logs, browser history and referrers).
	ticketQueryParam = "ticket"
)

// errTicketRejected is THE refusal for every way a presented ticket can fail:
// an unknown value, an expired one, one already redeemed, and one minted for a
// different lease.
//
// A single error for all four is the point. The handler cannot tell a legitimate
// client with a stale ticket from someone probing values, so any distinction it
// drew here would become an oracle — "that value existed but was for another
// lease" is a far richer hint than "no such value". The store already refuses all
// four through one `ok == false`; this keeps the response side just as flat.
//
// It is classified KindInvalidCredential so the guard answers it with the same
// 401 envelope a rejected bearer token gets. That is the honest reading (the
// caller presented something and it was refused) and it tells a client the
// actionable thing: mint a fresh ticket via POST /ticket/{lease_id}. A 404 would
// instead suggest the lease is gone, for which refreshing is pointless.
//
// The reason is operator-facing prose and deliberately carries no ticket value.
var errTicketRejected = nexusauth.NewError(nexusauth.KindInvalidCredential, "lease ticket rejected", nil)

// clientCredential names WHICH channel admitted (or was refused for) a client
// WebSocket, for the audit record. It never carries the credential itself: a
// ticket value must not reach the log, and a bearer token has never been logged
// either.
type clientCredential string

const (
	// credentialAnonymous is the identity every caller resolves to while
	// authentication is disabled.
	credentialAnonymous clientCredential = "anonymous"

	// credentialTicket is a single-use ticket presented as a query parameter.
	credentialTicket clientCredential = "ticket"

	// credentialBearer is an Authorization: Bearer header resolved through the
	// validator chain.
	credentialBearer clientCredential = "bearer"
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

	// tickets redeems the single-use `?ticket=` credential on the client
	// WebSocket. It is the browser path: a browser cannot present the bearer
	// header above, so this is the only credential it can carry.
	//
	// A nil store redeems nothing, so a ticket-bearing caller is refused — which
	// is the correct answer for a gateway wired without an issuer, since no ticket
	// it could present was ever minted here.
	tickets *ticketStore

	// rootCtx is cancelled on Shutdown so all read/write pumps exit.
	rootCtx    context.Context
	rootCancel context.CancelFunc
}

// NewGateway constructs a gateway over the given registry. auth may be nil (or a
// guard over an empty chain), which disables credential resolution and makes
// every client caller anonymous; tickets may be nil, which refuses every
// presented ticket.
func NewGateway(logger *slog.Logger, registry *Registry, auth *authGuard, tickets *ticketStore) *Gateway {
	if logger == nil {
		logger = slog.Default()
	}
	rootCtx, rootCancel := context.WithCancel(context.Background())
	return &Gateway{
		logger:     logger,
		registry:   registry,
		auth:       auth,
		tickets:    tickets,
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
	// The register frame must carry BOTH the lease id and the per-spawn secret
	// the broker handed this child through its environment. Enforcement is gated
	// on authentication being configured at all: with no `auth:` block the
	// secret is ignored, so an older nexus binary at nexus_binary_path keeps
	// working in an unauthenticated deployment exactly as it did.
	//
	// Every refusal — unknown lease, absent secret, wrong secret — gets the SAME
	// policy-violation close and the same reason text. The distinctions live in
	// the log below and nowhere else: a dialer that could tell "no such lease"
	// from "wrong secret" could enumerate live lease ids by differencing the two.
	if err := g.registry.AttachInstance(leaseID, wc, frame.Secret, g.auth.enabled()); err != nil {
		g.logInstanceRegistrationRefused(leaseID, err)
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

// logInstanceRegistrationRefused emits the audit record for a rejected
// dial-back. The wire response is identical for every cause; this is the ONLY
// place they are told apart.
//
// The absent-secret case gets its own message, and says out loud that the
// instance binary may predate the protocol, because that is the failure an
// operator will otherwise misdiagnose. The symptom of a version-skewed
// nexus_binary_path is indistinguishable from a network fault at the claim
// layer: every claim times out with "instance did not become ready in time"
// while the child process is alive, connecting fine, and being silently hung up
// on. Naming the cause here is what turns a day of packet captures into a
// binary upgrade.
//
// No record carries the presented secret. Refusing to log a credential is not
// optional just because this one was rejected — a near-miss value is exactly
// what makes a log worth stealing.
func (g *Gateway) logInstanceRegistrationRefused(leaseID string, err error) {
	switch {
	case errors.Is(err, errSpawnSecretAbsent):
		g.logger.Warn("rejecting instance registration: its register frame carried NO spawn secret, "+
			"but this broker has an auth block and therefore requires one. "+
			"The most likely cause is that the instance binary at nexus_binary_path predates the "+
			"spawn-secret protocol: upgrade it, or remove the auth block to run this broker unauthenticated",
			"lease_id", leaseID, "error", err)
	case errors.Is(err, errSpawnSecretMismatch):
		g.logger.Warn("rejecting instance registration: its register frame carried the WRONG spawn secret. "+
			"This broker did not spawn the process that dialed back",
			"lease_id", leaseID, "error", err)
	default:
		g.logger.Warn("rejecting instance registration", "lease_id", leaseID, "error", err)
	}
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

	// Authenticate first — from EITHER a `?ticket=` query parameter or an
	// Authorization header — and answer a bad or missing credential with the
	// guard's usual 401/403 envelope. That branch never touches the registry, so it
	// leaks nothing about whether the lease exists. With authentication disabled
	// this resolves to the anonymous identity with no error, so deny is unreachable
	// and the route behaves as it always did.
	caller, credential, err := g.resolveClientPrincipal(r, leaseID)
	if err != nil {
		g.auth.deny(w, r, err)
		return
	}

	// Then ownership, for a ticket exactly as for a bearer token. ownsLease folds
	// "no such lease" and "someone else's lease" into one false, and this writes the
	// byte-identical response the pre-ownership unknown-lease miss wrote, so the two
	// remain indistinguishable.
	//
	// A redeemed ticket is ALREADY bound to this lease id (the store refuses a
	// mismatch), so this second check is belt and braces — one map lookup. It is
	// kept because it is what makes "the lease still exists and is still owned by
	// this principal" true at CONNECT time rather than at mint time, and because it
	// leaves exactly one authorization predicate on this route: any future path that
	// hands out or resurrects a ticket (persisted leases, restart reattach) cannot
	// bypass ownership by virtue of having skipped a check the bearer path makes.
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

	// The audit record names the identity and WHICH credential admitted it, never
	// the credential itself — a ticket value must not reach the log, which is the
	// one exposure the broker actually controls.
	g.logger.Info("client connected", "lease_id", leaseID,
		"principal_id", caller.ID, "credential", string(credential))

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

// resolveClientPrincipal resolves the identity behind a client WebSocket
// handshake from whichever credential it presented, and reports which one that
// was for the audit record.
//
// PRECEDENCE: a non-empty `?ticket=` wins, and wins EXCLUSIVELY — the
// Authorization header is not consulted at all, and a ticket failure is final.
// Two reasons, and the first is the load-bearing one:
//
//  1. Falling back to the header would soften single use into "single use unless
//     you also hold a token": a replayed, burned ticket accompanied by a valid
//     bearer credential would connect, and "the ticket burns on connect" would
//     become an order-dependent claim rather than a property.
//  2. A ticket is the NARROWER credential (one lease, one principal, 30 seconds,
//     one use). When a caller presents both, honouring the narrower one is the
//     conservative reading of its intent.
//
// The cost is a client that sends both and lets its ticket expire being refused
// despite a good header. That is a deliberate, documented trade: the remedy is to
// send one credential, or to mint a fresh ticket.
//
// With authentication DISABLED nothing is consulted — not the header, not the
// ticket — and every caller is anonymous. That is the backward-compatibility
// guarantee stated as code: a client built for an authenticated broker can point
// `?ticket=…` at an open one and see exactly the behaviour that predates tickets,
// rather than a 401 from an inert store that never issued anything.
func (g *Gateway) resolveClientPrincipal(r *http.Request, leaseID string) (nexusauth.Principal, clientCredential, error) {
	if !g.auth.enabled() {
		return anonymousOwner(), credentialAnonymous, nil
	}

	if value := r.URL.Query().Get(ticketQueryParam); value != "" {
		// Redemption is the whole check: the store atomically consumes the ticket
		// only if it is live AND was minted for this lease, so unknown, expired,
		// already-used and wrong-lease all arrive here as one false. A wrong-lease
		// value is deliberately NOT consumed by the store (a failed authorization is
		// not a use), yet the refusal below is identical either way.
		principalID, ok := g.tickets.redeem(value, leaseID)
		if !ok {
			return nexusauth.Principal{}, credentialTicket, errTicketRejected
		}
		// Only the ID is reconstituted, because only the ID is ever compared (see
		// ticketRecord). Synthesising a Tenant or Scopes here would resurrect a
		// snapshot frozen at mint time and invite a future check to trust it.
		return nexusauth.Principal{ID: principalID}, credentialTicket, nil
	}

	p, err := g.auth.resolvePrincipal(r)
	if err != nil {
		return nexusauth.Principal{}, credentialBearer, err
	}
	return p, credentialBearer, nil
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
