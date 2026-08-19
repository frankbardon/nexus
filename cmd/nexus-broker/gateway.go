package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
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

	// defaultPingInterval is how often each frame pump asks its peer for a
	// pong, and defaultPeerReadDeadline is how long a peer may answer NOTHING
	// before the gateway declares its socket dead and closes it.
	//
	// THE RELATIONSHIP IS THE POINT: the deadline is three whole intervals, so
	// a peer has to miss three consecutive pings before it is detached. A
	// single unanswered ping proves very little — a stop-the-world GC pause, a
	// saturated uplink, a laptop that slept for four seconds, a process the
	// scheduler starved — and a deadline set at or just above one interval
	// would turn every one of those into a dropped session. A deadline that
	// comfortably exceeds the interval buys the redundancy that makes the
	// signal trustworthy: silence sustained across 45 seconds and three
	// independent probes is a dead socket, not a hiccup.
	//
	// The absolute numbers are chosen against the failure they exist to catch.
	// A half-open TCP connection — a slept laptop, a moved network, a NAT
	// table that dropped the flow — is invisible to both ends until something
	// writes, and the OS keepalive that would eventually notice runs on a
	// two-hour default. 45 seconds is fast enough that a client reconnects
	// while its replay buffer still holds the stream (E3-S1/S2) and slow
	// enough to cost one control frame per peer per 15 seconds.
	//
	// This is emphatically NOT idle reaping. A ping is answered by the
	// WebSocket stack itself, so a session sitting untouched for an hour with
	// no user input answers every one of them and survives indefinitely.
	// Releasing genuinely idle leases is a separate policy owned by
	// `idle_timeout`, which is stamped only by real client → instance IO.
	defaultPingInterval = 15 * time.Second

	// defaultPeerReadDeadline is documented with defaultPingInterval above:
	// three intervals of total silence, not one.
	defaultPeerReadDeadline = 3 * defaultPingInterval

	// peerUnresponsiveReason is the close reason a socket detached for
	// unanswered pings carries. The peer that earned it is by definition not
	// reading, so this is for the log and for a slow-but-alive peer that does
	// eventually drain its socket.
	peerUnresponsiveReason = "peer unresponsive"
)

// peerRole names which end of a lease a frame pump is pumping, for log records
// only. It never reaches the wire.
type peerRole string

const (
	peerInstance peerRole = "instance"
	peerClient   peerRole = "client"
)

const (
	// ticketQueryParam is the query parameter a client presents its single-use
	// lease ticket in on the client WebSocket handshake.
	//
	// A query parameter rather than a header because it is the ONLY channel a
	// browser has: JavaScript cannot set headers on a WebSocket upgrade. Every
	// other property of a ticket — the 30s TTL, the single use, the
	// never-log-the-value rule — exists to contain the exposure this carrier
	// implies (URLs reach proxy access logs, browser history and referrers).
	ticketQueryParam = "ticket"

	// fromSeqQueryParam is the query parameter a reconnecting client states the
	// highest sequence it received on in, so the broker can replay what it
	// still holds beyond that point before resuming the live stream.
	//
	// A query parameter for exactly the reason `?ticket=` is one: a browser
	// cannot set headers on a WebSocket upgrade, and there is no client →
	// broker control frame to carry it in — the first frame a client sends is
	// already routed straight through to the instance. Unlike a ticket it is
	// not a credential and carries no secret, so putting it in a URL costs
	// nothing.
	fromSeqQueryParam = "from_seq"
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

	// pingInterval and peerReadDeadline configure the liveness pump — see
	// defaultPingInterval for what they mean and why the second is three times
	// the first. They are fields rather than bare constants so tests can shrink
	// them to milliseconds; nothing else writes them, and a non-positive
	// pingInterval disables liveness checking entirely.
	pingInterval     time.Duration
	peerReadDeadline time.Duration

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
		logger:           logger,
		registry:         registry,
		auth:             auth,
		tickets:          tickets,
		pingInterval:     defaultPingInterval,
		peerReadDeadline: defaultPeerReadDeadline,
		rootCtx:          rootCtx,
		rootCancel:       rootCancel,
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

	// Every refusal from here down — skewed frame version, unknown lease, absent
	// secret, wrong secret — gets the SAME policy-violation close and the SAME
	// reason text. The distinctions live in the log and nowhere else: a dialer
	// that could tell "no such lease" from "wrong secret" could enumerate live
	// lease ids by differencing the two, and lease ids are the thing the secret
	// exists to stop being sufficient.

	// The register frame must declare the schema version this broker speaks.
	// brokerframe.Encode stamps it on every frame and Decode deliberately
	// tolerates any value "so callers can decide how to react"; this is the
	// caller deciding. It is checked BEFORE the secret so a mismatched build is
	// diagnosed as skew rather than as whatever its frame happened to omit.
	if frame.Version != brokerframe.Version {
		g.logInstanceRegistrationRefused(leaseID, versionSkewError{presented: frame.Version})
		_ = conn.Close(websocket.StatusPolicyViolation, instanceRefusedReason)
		return
	}

	wc := newWSConn(conn)
	// The register frame must carry BOTH the lease id and the per-spawn secret
	// the broker handed this child through its environment. The secret is
	// required unconditionally — see Registry.AttachInstance for why gating it on
	// `auth:` was authenticating the dial-back socket with a value that travels
	// in ws_urls and logs.
	if err := g.registry.AttachInstance(leaseID, wc, frame.Secret); err != nil {
		g.logInstanceRegistrationRefused(leaseID, err)
		_ = conn.Close(websocket.StatusPolicyViolation, instanceRefusedReason)
		return
	}

	g.logger.Info("instance registered", "lease_id", leaseID)

	ctx, cancelPumps := context.WithCancel(g.rootCtx)
	defer cancelPumps()

	go g.writePump(ctx, wc)
	go g.livenessPump(ctx, leaseID, peerInstance, wc)
	// Instance read pump: forward decoded frames to the lease's client conn.
	// Lifecycle signals from the instance are observed here so the gateway can
	// unblock POST /claim when the engine reports ready.
	g.readPump(ctx, leaseID, wc, g.forwardToClient, func(f brokerframe.Frame) {
		switch f.Signal {
		case brokerframe.SignalReady:
			g.registry.MarkReady(leaseID)
		case brokerframe.SignalSessionIDReport:
			g.registry.MarkSessionID(leaseID, f.SessionID)
		case brokerframe.SignalIO:
			g.deliverIO(leaseID, f)
		}
	})

	g.registry.DetachInstance(leaseID, wc)
	wc.shutdown(websocket.StatusNormalClosure, "")
	g.logger.Info("instance disconnected", "lease_id", leaseID)
}

// deliverIO hands one instance IO payload to the lease's in-process observer,
// if it has one.
//
// It runs INSIDE the instance read pump's observe callback, before the frame is
// forwarded to the client conn, and it is additive: an A2A-observed lease with a
// client attached still relays every frame to that client untouched. The A2A
// ingress needs this hook because it has no socket to be relayed to — its caller
// is an HTTP request, not a WebSocket peer.
//
// The decode happens only when a sink exists, so a lease with no observer pays
// one map lookup per frame and nothing else. An undecodable payload is logged
// and dropped rather than failing anything: a broker must keep relaying frames
// it cannot itself interpret, which is what it did before it interpreted any.
func (g *Gateway) deliverIO(leaseID string, f brokerframe.Frame) {
	if f.LeaseID != "" && f.LeaseID != leaseID {
		// The same refusal the forwarding path below makes, applied before the
		// payload reaches an in-process observer. A frame naming another lease is
		// dropped there; an observer must not be the one place it gets through.
		return
	}
	sink := g.registry.ioSink(leaseID)
	if sink == nil {
		return
	}
	msg, err := decodeIOPayload(f.Payload)
	if err != nil {
		g.logger.Warn("dropping an instance io payload the broker could not decode",
			"lease_id", leaseID, "error", err)
		return
	}
	sink(msg)
}

// instanceRefusedReason is the WebSocket close reason every refused dial-back
// gets, whatever the cause. It deliberately reads as the unknown-lease case, so
// a dialer holding a guessed lease id learns nothing from the difference between
// "there is no such lease" and "there is, and you failed its secret".
const instanceRefusedReason = "unknown lease"

// errRegisterVersionSkew means the register frame declared a brokerframe schema
// version this broker does not speak. It is a distinct sentinel, alongside
// errSpawnSecretAbsent and errSpawnSecretMismatch, purely so
// logInstanceRegistrationRefused can name the cause; on the wire it is the same
// refusal as all the others.
var errRegisterVersionSkew = errors.New("register frame schema version does not match this broker")

// versionSkewError carries the version the instance declared, so the audit
// record can state the actual skew rather than "a mismatch". It unwraps to
// errRegisterVersionSkew so errors.Is keeps working for anything that only cares
// about the class.
type versionSkewError struct{ presented int }

func (e versionSkewError) Error() string {
	return fmt.Sprintf("%s: instance sent version %d, this broker speaks version %d",
		errRegisterVersionSkew, e.presented, brokerframe.Version)
}

func (e versionSkewError) Unwrap() error { return errRegisterVersionSkew }

// logInstanceRegistrationRefused emits the audit record for a rejected
// dial-back. The wire response is identical for every cause; this is the ONLY
// place they are told apart.
//
// The version-skew and absent-secret cases get their own messages because they
// are the failures an operator will otherwise misdiagnose. A mismatched
// nexus_binary_path is indistinguishable from a network fault at the claim
// layer: every claim times out with "instance did not become ready in time"
// while the child process is alive, connecting fine, and being silently hung up
// on. Naming the cause here is what turns a day of packet captures into a binary
// upgrade — and since the spawn secret became mandatory, "upgrade the binary" is
// the answer to both of them rather than "drop the auth block".
//
// No record carries the presented secret. Refusing to log a credential is not
// optional just because this one was rejected — a near-miss value is exactly
// what makes a log worth stealing.
func (g *Gateway) logInstanceRegistrationRefused(leaseID string, err error) {
	var skew versionSkewError
	switch {
	case errors.As(err, &skew):
		g.logger.Warn("rejecting instance registration: its register frame declares a broker frame "+
			"schema version this broker does not speak. The binary the binaries registry entry for "+
			"this lease points at is a different build from this broker: upgrade whichever is older "+
			"so both speak the same version",
			"lease_id", leaseID,
			"instance_frame_version", skew.presented,
			"broker_frame_version", brokerframe.Version,
			"error", err)
	case errors.Is(err, errSpawnSecretAbsent):
		g.logger.Warn("rejecting instance registration: its register frame carried NO spawn secret, "+
			"and this broker requires one for every registration — with or without an auth block. "+
			"The most likely cause is that the instance binary the binaries registry entry points at "+
			"predates the spawn-secret protocol: upgrade it. Removing the auth block no longer makes "+
			"this work",
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
	// A resuming client stages its replay INSIDE the attach, under the lease's
	// stream lock, so no live frame can be queued in front of it. Without
	// `?from_seq=` the attach is the one that predates resumption, byte for byte.
	//
	// The attach also DISPLACES any connection already on the lease, which is
	// safe only because ownership was checked above, before the upgrade: the
	// principal doing the displacing is by then known to own the lease. A
	// stranger is refused with the indistinguishable 404 and evicts nothing.
	var (
		attach    clientAttach
		attachErr error
	)
	if fromSeq, resuming := parseFromSeq(r.URL.Query()); resuming {
		attach, attachErr = g.registry.AttachClientFrom(leaseID, wc, fromSeq)
	} else {
		attach, attachErr = g.registry.AttachClient(leaseID, wc)
	}
	if attachErr != nil {
		// Reachable now only for a lease that vanished between the ownership
		// check and the attach — a release or a crash landing in that window. The
		// close reason is unchanged, because from the client's side the outcome
		// is unchanged: the lease it asked for is not available to stream.
		g.logger.Warn("rejecting client connection", "lease_id", leaseID, "error", attachErr)
		_ = conn.Close(websocket.StatusPolicyViolation, "lease unavailable")
		return
	}

	// The audit record names the identity and WHICH credential admitted it, never
	// the credential itself — a ticket value must not reach the log, which is the
	// one exposure the broker actually controls.
	g.logger.Info("client connected", "lease_id", leaseID,
		"principal_id", caller.ID, "credential", string(credential),
		"evicted_previous_client", attach.evicted)
	if attach.evicted {
		// Logged separately, and at Warn, because it is the one connect outcome
		// that took something away from somebody: whichever socket was streaming
		// this lease has just been closed on this principal's authority.
		g.logger.Warn("this connection displaced the lease's previous client, which has been "+
			"closed. A client must not keep two connections open to one lease — they will "+
			"evict each other",
			"lease_id", leaseID, "principal_id", caller.ID,
			"credential", string(credential), "reason", evictedCloseReason,
			"close_status", int(evictedCloseStatus))
	}
	g.logResume(leaseID, attach.resume)

	ctx, cancelPumps := context.WithCancel(g.rootCtx)
	defer cancelPumps()

	go g.writePump(ctx, wc)
	go g.livenessPump(ctx, leaseID, peerClient, wc)
	// Client read pump: forward decoded frames to the lease's instance conn.
	// Stamp last-activity ONLY for real user input (io frames flowing client →
	// instance) so the idle sweeper resets on genuine activity and not on
	// instance output, pings, or control frames.
	g.readPump(ctx, leaseID, wc, g.forwardToInstance, func(f brokerframe.Frame) {
		if f.Signal == brokerframe.SignalIO {
			g.registry.markActivity(leaseID)
		}
	})

	g.registry.DetachClient(leaseID, wc)
	wc.shutdown(websocket.StatusNormalClosure, "")
	g.logger.Info("client disconnected", "lease_id", leaseID)
}

// parseFromSeq reads the resume point off a client handshake's query string.
// It reports whether the client asked to resume at all, which is NOT the same
// as a zero sequence: `?from_seq=0` is a client saying it has seen nothing and
// wants everything the buffer still holds, while an absent parameter is a
// client asking for the live stream only.
//
// A MALFORMED value — not a number, negative, out of range, or repeated with a
// bad first value — is treated as ABSENT rather than refused. A resume is an
// optimisation on top of a connection that works without it, so a client bug
// in building the URL should cost the resume, not the session: refusing the
// handshake would turn a cosmetic defect into an outage. The value is not a
// credential, so nothing is being authorised leniently here.
func parseFromSeq(query url.Values) (uint64, bool) {
	raw := query.Get(fromSeqQueryParam)
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

// logResume records what a resuming client was handed. A resume with nothing
// to replay and no gap is silent: it is the steady state of a client that
// reconnects promptly, and logging it would drown the record that matters.
func (g *Gateway) logResume(leaseID string, resume clientResume) {
	if resume.gap == nil {
		if len(resume.frames) > 0 {
			g.logger.Info("replaying buffered frames to a resuming client",
				"lease_id", leaseID, "frames", len(resume.frames), "last_seq", resume.lastSeq)
		}
		return
	}
	g.logger.Warn("a resuming client could not be fully replayed; it has been told which "+
		"frames are gone. Raise client_replay_buffer_bytes if this is routine rather than "+
		"the tail of a long disconnection",
		"lease_id", leaseID,
		"reason", resume.gap.Reason,
		"requested_from_seq", resume.gap.RequestedFromSeq,
		"missing_from_seq", resume.gap.MissingFromSeq,
		"missing_through_seq", resume.gap.MissingThroughSeq,
		"replayed_frames", len(resume.frames),
		"last_seq", resume.lastSeq)
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

// livenessPump probes one peer with a WebSocket ping every pingInterval and
// closes its socket once the peer has failed to answer for peerReadDeadline.
// Closing is what enforces the deadline: it makes the blocked read in readPump
// return an error, which runs the ordinary detach path — the same one a peer
// that hung up cleanly takes.
//
// WHY A PING RATHER THAN A READ TIMEOUT. The obvious implementation — wrap each
// conn.Read in a context.WithTimeout — is wrong here, and dangerously so. Read
// returns on a DATA message and on nothing else; the library answers pings and
// consumes pongs inside that blocked call without ever surfacing them. So a
// timeout on Read measures how long since the peer last said something, which is
// exactly what a healthy idle session does not do. It would reap every session
// whose user stepped away, which is precisely the bug idle_timeout's own policy
// (and E4-S1) exists to get right. A ping, by contrast, is answered by the
// peer's WebSocket stack whether or not there is a human at the far end, so
// silence in the face of one means the socket is gone and nothing else.
//
// conn.Ping is used rather than a hand-rolled brokerframe signal deliberately.
// It waits for the matching pong, so one call is both halves of the probe; it
// works against a peer that speaks no Nexus protocol at all; and a ping is a
// transport concern that has no business in the IO envelope. It requires a
// concurrent reader to collect the pong — see its doc comment — which both
// pumps satisfy, since each is started alongside a readPump that immediately
// blocks in Read.
//
// A ping failure alone does NOT detach. Only the accumulated silence since the
// last successful pong is compared against the deadline, so the three-interval
// redundancy described on defaultPingInterval is real rather than nominal.
func (g *Gateway) livenessPump(ctx context.Context, leaseID string, role peerRole, wc *wsConn) {
	if g.pingInterval <= 0 || wc == nil || wc.conn == nil {
		return
	}
	ticker := time.NewTicker(g.pingInterval)
	defer ticker.Stop()

	lastPong := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wc.closed:
			return
		case <-ticker.C:
		}

		// The probe is bounded by one interval so a wedged socket cannot park
		// this goroutine indefinitely and so probes never overlap; the deadline
		// is enforced by the accumulated silence below, not by this timeout.
		pingCtx, cancel := context.WithTimeout(ctx, g.pingInterval)
		err := wc.conn.Ping(pingCtx)
		cancel()
		if err == nil {
			lastPong = time.Now()
			continue
		}
		if ctx.Err() != nil {
			return
		}

		silent := time.Since(lastPong)
		if silent < g.peerReadDeadline {
			g.logger.Debug("peer did not answer a ping",
				"lease_id", leaseID, "peer", string(role),
				"silent_for", silent, "error", err)
			continue
		}
		g.logger.Warn("peer stopped answering pings; closing its socket so the lease detaches. "+
			"The connection is half-open — the peer's host slept, moved network, or had its flow "+
			"dropped by a NAT — and would otherwise have looked healthy until the OS keepalive noticed",
			"lease_id", leaseID, "peer", string(role),
			"silent_for", silent, "deadline", g.peerReadDeadline, "error", err)
		wc.abort(websocket.StatusGoingAway, peerUnresponsiveReason)
		return
	}
}

// readPump reads frames from wc, decodes them (protocol-aware), and hands each
// to forward, which is direction-specific: forwardToInstance pipes the raw
// bytes through untouched, forwardToClient sequences and buffers them first.
// The optional observe callback is invoked for every decoded frame before
// forwarding, letting the caller react to lifecycle signals (e.g. ready)
// without disturbing routing. It returns when the connection closes or its
// context is cancelled.
func (g *Gateway) readPump(ctx context.Context, leaseID string, wc *wsConn, forward func(string, brokerframe.Frame, []byte), observe func(brokerframe.Frame)) {
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

		forward(leaseID, frame, data)
	}
}

// forwardToInstance relays one client-originated frame to the lease's instance
// connection, verbatim.
//
// Instance-bound frames are deliberately NOT sequenced and NOT buffered: the
// broker assigns sequences on the client-bound side only, so an instance needs
// no protocol awareness and nothing on the dial-back side changed. Both loss
// paths here — no instance attached, and an instance whose send queue is full —
// still log and continue, exactly as they always did.
func (g *Gateway) forwardToInstance(leaseID string, frame brokerframe.Frame, data []byte) {
	peer := g.registry.InstanceConn(leaseID)
	if peer == nil {
		g.logger.Debug("no peer attached, dropping frame",
			"lease_id", leaseID, "signal", frame.Signal)
		return
	}
	if !peer.queue(data) {
		g.logger.Warn("peer send buffer full, dropping frame",
			"lease_id", leaseID, "signal", frame.Signal)
	}
}

// forwardToClient relays one instance-originated frame to the lease's client,
// through the lease's sequencer and replay buffer.
//
// The raw bytes are deliberately discarded: SendClientFrame re-encodes the
// decoded frame so it can stamp the sequence, which is also what strips any
// Secret an instance might have set on a frame headed for a client.
//
// Neither loss path is fatal any more. With no client attached the frame is
// retained rather than dropped, so a client that attaches later can be caught
// up; with a full send queue it is retained too, and the existing warning still
// fires because a client that is not draining its socket is still an operator
// problem worth naming.
func (g *Gateway) forwardToClient(leaseID string, frame brokerframe.Frame, _ []byte) {
	seq, outcome, err := g.registry.SendClientFrame(leaseID, frame)
	if err != nil {
		g.logger.Debug("dropping client-bound frame for a lease that is gone",
			"lease_id", leaseID, "signal", frame.Signal, "error", err)
		return
	}
	switch outcome {
	case clientFrameBuffered:
		g.logger.Debug("no client attached; frame retained for replay",
			"lease_id", leaseID, "signal", frame.Signal, "seq", seq)
	case clientFrameDropped:
		g.logger.Warn("peer send buffer full, dropping frame",
			"lease_id", leaseID, "signal", frame.Signal, "seq", seq)
	}
}

// writePump writes wc's staged replay, then drains wc.send and writes each
// frame to the WebSocket until the context is cancelled or the connection is
// closed.
//
// The staged replay goes first and goes out UNCONDITIONALLY — it is not
// select-ed against the send channel — which is what makes "replayed frames
// precede live ones" a property of the pump rather than a race between two
// producers. It is also why a replay is staged rather than queued: the send
// channel holds 256 frames and a replay is bounded in bytes, so a megabyte of
// token deltas would otherwise be truncated on its way out of the very buffer
// that exists to stop frames being lost.
func (g *Gateway) writePump(ctx context.Context, wc *wsConn) {
	for _, data := range wc.takeReplay() {
		if err := wc.conn.Write(ctx, websocket.MessageText, data); err != nil {
			if ctx.Err() == nil {
				g.logger.Debug("write pump ended during replay", "error", err)
			}
			return
		}
	}
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
