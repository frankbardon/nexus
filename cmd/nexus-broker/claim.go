package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// defaultReadyTimeout bounds how long POST /claim waits for a freshly spawned
// instance to dial back and signal ready before giving up, killing it, and
// returning an error. Kept as a constant (not a config key) for this story.
const defaultReadyTimeout = 30 * time.Second

// defaultSessionReportGrace bounds how long POST /claim waits, after the
// instance signals ready, for its session-id-report frame to arrive. The
// plugin sends the report immediately after ready, so this is a short grace
// window: if it elapses the claim still succeeds, just without a session id in
// the response. Kept as a constant (not a config key) for this story.
const defaultSessionReportGrace = 5 * time.Second

// maxClaimBody caps the size of a claim request body to avoid unbounded reads.
const maxClaimBody = 1 << 20 // 1 MiB

// claimRequest is the JSON body of POST /claim. The caller supplies the full
// nexus config inline (YAML text) for the instance to boot with.
type claimRequest struct {
	// Config is the full nexus config as YAML text. It is written verbatim to
	// a temp file the spawned instance reads via -config.
	Config string `json:"config"`

	// SessionID, when set, resumes an existing persisted session: the broker
	// spawns the instance with -recall <id> so the engine reloads that
	// session and replays its history. When empty the broker starts a fresh
	// session and returns the engine-generated id in the response.
	SessionID string `json:"session_id,omitempty"`

	// Binary names which entry of the broker's `binaries:` registry to spawn.
	//
	// OMITTED MEANS THE RESERVED `nexus` ENTRY, which every load guarantees
	// exists — so adding this field is additive and every client written before
	// the registry existed keeps getting exactly what it got before. There is no
	// operator-settable default for the same reason: an operator must not be
	// able to silently change what an existing client ends up spawning.
	//
	// An unknown name is a 400, not a fallback to `nexus`. Quietly spawning the
	// base binary for a caller that asked for a vision build would produce a
	// session that merely behaves oddly, which is far harder to diagnose than a
	// rejected claim.
	Binary string `json:"binary,omitempty"`
}

// claimResponse is the JSON body returned once the instance is ready.
type claimResponse struct {
	LeaseID string `json:"lease_id"`
	WSURL   string `json:"ws_url"`

	// SessionID is the engine session id the instance is running. For a new
	// session it is the engine's generated id (capture it to -recall later);
	// for a resume it echoes the requested id. It may be empty if the
	// instance did not report one within the grace window.
	SessionID string `json:"session_id,omitempty"`

	// Ticket is a single-use, lease-and-principal-scoped credential the client
	// presents on the WebSocket handshake, valid for ticketTTL. It exists because
	// browser JavaScript cannot put an Authorization header on a WebSocket
	// handshake, so the claim's bearer credential can never reach WS
	// /lease/{lease_id} from a browser.
	//
	// It is OMITTED — the same omitempty convention as SessionID — when the broker
	// runs with no `auth:` block, because there is then nothing to authenticate
	// and the WebSocket accepts a connection with no ticket at all. Omitted rather
	// than empty so a client can tell "this broker issues no tickets" from "a
	// ticket was issued and it is the empty string", which would be a bug. It is
	// also omitted if minting failed; POST /ticket/{lease_id} mints a replacement.
	//
	// Adding this field is additive: a client that ignores unknown JSON keys is
	// unaffected.
	Ticket string `json:"ticket,omitempty"`
}

// ClaimServer handles POST /claim: it mints a lease, spawns a nexus instance
// with the per-claim config, waits for the instance to dial back and signal
// ready, then returns the lease id and the broker's client WebSocket URL.
type ClaimServer struct {
	logger             *slog.Logger
	registry           *Registry
	cfg                Config
	runner             commandRunner
	readyTimeout       time.Duration
	sessionReportGrace time.Duration

	// tickets mints the single-use client WebSocket ticket returned with a
	// successful claim. A nil store (or one built inert because auth is disabled)
	// simply issues nothing, and the response omits the `ticket` field.
	tickets *ticketStore

	// queueWaitTimeout bounds how long an over-capacity claim parks in the FIFO
	// capacity queue before returning a timeout error. It is sourced from the
	// broker config's queue_wait_timeout. A non-positive value disables waiting:
	// an at-capacity claim is rejected immediately with "no capacity".
	queueWaitTimeout time.Duration

	// spawnKey derives each lease's spawn secret from a broker-held key instead of
	// minting a random one, so a restarted broker can recompute the value a
	// surviving instance is still holding without that value ever reaching disk
	// (see spawnKey). It is nil when no state_dir is configured, and secretFor
	// then falls back to a per-spawn random secret — the pre-existing behaviour.
	spawnKey spawnKey
}

// useSpawnKey binds the derivation key spawn secrets are computed from.
//
// It is a setter rather than a NewClaimServer parameter for the same reason
// Registry.useLeaseStore is: the key is optional (it exists only when state_dir
// is set) and threading an optional dependency through the constructor would
// churn every existing claim test for something none of them exercise. Call it
// once at wiring time, before the broker serves.
func (s *ClaimServer) useSpawnKey(k spawnKey) { s.spawnKey = k }

// NewClaimServer constructs a claim handler. A nil runner defaults to the
// production execRunner; tests inject a fake to avoid booting a real engine. A
// nil tickets store means no `ticket` is returned with a claim, which is also
// what an auth-disabled broker produces.
func NewClaimServer(logger *slog.Logger, registry *Registry, cfg Config, runner commandRunner, tickets *ticketStore) *ClaimServer {
	if logger == nil {
		logger = slog.Default()
	}
	if runner == nil {
		runner = execRunner{}
	}
	return &ClaimServer{
		logger:             logger,
		registry:           registry,
		cfg:                cfg,
		runner:             runner,
		readyTimeout:       defaultReadyTimeout,
		sessionReportGrace: defaultSessionReportGrace,
		tickets:            tickets,
		queueWaitTimeout:   cfg.QueueWaitTimeout,
	}
}

// Register wires the claim endpoint onto a mux. It takes a routeMux so main can
// register it behind the auth guard.
func (s *ClaimServer) Register(mux routeMux) {
	mux.HandleFunc("POST /claim", s.handleClaim)
}

// handleClaim implements the new-session claim spine: validate, mint lease,
// write temp config, spawn, wait for ready (bounded), respond. Every error
// path cleans up the temp config, kills/reaps the process, and drops the lease
// so nothing leaks.
func (s *ClaimServer) handleClaim(w http.ResponseWriter, r *http.Request) {
	var req claimRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxClaimBody))
	if err := dec.Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, "invalid claim body", err)
		return
	}
	if req.Config == "" {
		s.fail(w, http.StatusBadRequest, "claim requires a non-empty config", nil)
		return
	}

	// Resolve the requested variant BEFORE anything is allocated. Everything
	// below this point acquires something that has to be given back — a capacity
	// slot, a lease id, a temp config file, a child process — and a caller's
	// typo must not be able to consume any of it. Doing it here also means the
	// rejection is instant rather than arriving after a queue wait.
	binaryName, entry, err := resolveClaimBinary(s.cfg.Binaries, req.Binary)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err.Error(), nil)
		return
	}

	// The lease is stamped with whoever the auth guard authenticated. PrincipalFrom
	// reports absent when the broker runs with no `auth:` block (or when /claim is
	// registered outside the guard, as some tests do), and that is a supported
	// state, not an error — the lease then records the anonymous owner so no code
	// path downstream has to cope with an absent one.
	owner, authenticated := PrincipalFrom(r.Context())
	if !authenticated {
		owner = anonymousOwner()
	}

	// NewLeaseQueued acquires a capacity slot before the lease exists, so a
	// claim can never spawn an instance past max_concurrent. Ownership is only
	// carried through it — it does not participate in capacity accounting. At
	// capacity the claim parks in a FIFO queue (E3-S2) and proceeds when a slot
	// frees:
	//
	//   - errQueueTimeout: the claim waited past queue_wait_timeout → a distinct
	//     503 "capacity wait timed out" (told apart from the immediate
	//     "no capacity" below by its message).
	//   - errNoCapacity: the cap is full AND queue_wait_timeout <= 0 (waiting
	//     disabled) → immediate 503 "no capacity".
	//   - context cancelled: the client hung up while queued → no response (the
	//     connection is gone); the waiter is already dropped from the queue.
	leaseID, err := s.registry.NewLeaseQueued(r.Context(), s.queueWaitTimeout, owner)
	if err != nil {
		switch {
		case errors.Is(err, errQueueTimeout):
			s.fail(w, http.StatusServiceUnavailable, "capacity wait timed out", nil)
		case errors.Is(err, errNoCapacity):
			s.fail(w, http.StatusServiceUnavailable, "no capacity", nil)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			s.logger.Info("claim cancelled while queued for capacity", "error", err)
		default:
			s.fail(w, http.StatusInternalServerError, "minting lease", err)
		}
		return
	}

	configPath, err := writeTempConfig(req.Config)
	if err != nil {
		s.registry.Remove(leaseID)
		s.fail(w, http.StatusInternalServerError, "writing temp config", err)
		return
	}
	// The instance reads the config synchronously at boot (before it dials
	// back and signals ready), so the file is safe to remove once this
	// handler returns — on success and on every failure path alike.
	defer func() { _ = os.Remove(configPath) }()

	// Mint the dial-back second factor and record it on the lease BEFORE any
	// process exists. Ordering is the whole point: an instance can dial back the
	// microsecond it is exec()d, so a secret stored after the spawn would leave a
	// window in which a legitimate instance is refused for presenting a value the
	// registry does not yet expect.
	//
	// It is minted here rather than inside the runner so it holds for EVERY
	// commandRunner — a fake runner in a test injects no environment, and must
	// not thereby be able to skip the check the production path enforces.
	//
	// With a state_dir configured the value is DERIVED from the broker's key
	// rather than drawn from crypto/rand, so a restarted broker can recompute it
	// for a surviving instance; with no state_dir it is random exactly as before.
	// Either way it is minted here, held only in memory, and never journaled — see
	// spawnKey for why a derivation key on disk is not the same thing as a
	// persisted credential.
	//
	// A generation failure fails the claim. crypto/rand not producing 16 bytes
	// means the machine is in a state where nothing security-relevant should
	// proceed, and spawning an instance whose dial-back cannot be authenticated
	// is precisely the outcome this story exists to prevent.
	spawnSecret, err := s.spawnKey.secretFor(leaseID)
	if err != nil {
		s.registry.Remove(leaseID)
		s.fail(w, http.StatusInternalServerError, "minting spawn secret", err)
		return
	}
	s.registry.SetSpawnSecret(leaseID, spawnSecret)

	brokerAddr := "ws://" + instanceDialHost(s.cfg.ListenAddr) + instanceWSPath
	// The entry's RESOLVED path is what is exec()d — LoadConfig verified it at
	// boot, so no claim performs a stat or a PATH lookup, and a PATH that
	// changes under a running broker cannot make one claim spawn a different
	// build than the next.
	handle, err := s.runner.start(r.Context(), spawnSpec{
		binaryName:      binaryName,
		binaryPath:      entry.ResolvedPath,
		binaryArgs:      entry.Args,
		binaryEnv:       entry.Env,
		configPath:      configPath,
		leaseID:         leaseID,
		brokerAddr:      brokerAddr,
		spawnSecret:     spawnSecret,
		recallSessionID: req.SessionID,
	})
	if err != nil {
		s.registry.Remove(leaseID)
		s.fail(w, http.StatusInternalServerError, "spawning instance", err)
		return
	}
	// SetProcess starts the single reaper that wait()s the process and closes
	// the lease's exited channel; both this handler and a later release observe
	// it, so the process is wait()ed in exactly one place.
	s.registry.SetProcess(leaseID, handle)

	select {
	case <-s.registry.ReadyChan(leaseID):
		// Instance booted and signalled ready.
	case <-s.registry.ExitedChan(leaseID):
		exitErr := s.registry.ExitErr(leaseID)
		s.registry.Remove(leaseID)
		s.fail(w, http.StatusBadGateway, "instance exited before signalling ready", exitErr)
		return
	case <-time.After(s.readyTimeout):
		_ = handle.kill()
		<-s.registry.ExitedChan(leaseID) // reap the killed process so nothing leaks
		s.registry.Remove(leaseID)
		s.fail(w, http.StatusGatewayTimeout, "instance did not become ready in time", nil)
		return
	}

	// Resolve the session id to return. The instance reports it via a
	// session-id-report frame just after ready; wait a bounded grace for it.
	// For a resume, the requested id is authoritative for the response (and
	// is what the caller already holds); a mismatching report is logged but
	// not fatal. For a new session, the reported id is the only way the
	// caller learns the engine-generated id to -recall later.
	sessionID := req.SessionID
	select {
	case <-s.registry.SessionReportedChan(leaseID):
		reported := s.registry.SessionID(leaseID)
		if req.SessionID == "" {
			sessionID = reported
		} else if reported != "" && reported != req.SessionID {
			s.logger.Warn("instance reported a different session id than requested",
				"lease_id", leaseID, "requested", req.SessionID, "reported", reported)
		}
	case <-time.After(s.sessionReportGrace):
		if req.SessionID == "" {
			s.logger.Warn("instance did not report a session id within grace window",
				"lease_id", leaseID)
		}
	}

	// The instance is live: from here an unexpected exit is a crash, not a
	// pre-ready spawn failure (which this handler's earlier paths own). Start
	// the crash watcher. It latches the lease's releasing flag only if no
	// deliberate teardown beats it, so a later POST /release is never
	// misclassified as a crash.
	go s.registry.watchExit(leaseID)

	// Mint the client's WebSocket ticket LAST, immediately before responding, so
	// its short TTL starts when the caller receives it rather than when the
	// instance began booting — a boot may take up to readyTimeout (30s), which
	// would otherwise hand back an already-expired ticket.
	//
	// A mint failure does NOT fail the claim. The instance is live and the lease is
	// usable (a non-browser client authenticates the socket with its bearer
	// credential, and any client can mint a replacement via POST
	// /ticket/{lease_id}), so tearing down a working session over a recoverable
	// gap would be the worse outcome. The response then omits `ticket`.
	ticket, mintErr := s.tickets.mint(leaseID, owner.ID)
	if mintErr != nil {
		s.logger.Error("minting claim ticket failed; claim returns no ticket",
			"lease_id", leaseID, "principal_id", owner.ID, "error", mintErr)
	}

	wsURL := clientWSBaseURL(s.cfg, r.Host) + ClientWSPath(leaseID)
	// principal_id ties the new lease id back to the identity that claimed it. The
	// guard's allow record cannot carry it: /claim has no lease id in its path, so
	// this is the only line that joins the two halves of the audit trail. Empty
	// when auth is disabled.
	//
	// `ticket_issued` records WHETHER a ticket went out, never its value: a ticket
	// is a bearer credential and the broker's logs must not add to the exposure it
	// already has from travelling in a URL.
	//
	// `binary` records WHICH registry entry was spawned. With several variants
	// behind one broker, "which build is this lease running" stops being
	// inferable from the broker's config alone, and this line is the only place
	// the answer is joined to a lease id.
	s.logger.Info("claim ready", "lease_id", leaseID, "pid", handle.pid(), "ws_url", wsURL,
		"session_id", sessionID, "principal_id", owner.ID, "ticket_issued", ticket != "",
		"binary", binaryName)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(claimResponse{
		LeaseID:   leaseID,
		WSURL:     wsURL,
		SessionID: sessionID,
		Ticket:    ticket,
	})
}

// resolveClaimBinary picks the registry entry a claim selects, returning the
// resolved name alongside the entry so the caller never has to re-derive
// "which name did the empty string mean".
//
// The name is trimmed before lookup for the same reason foldBinaryRegistry
// trims entry names: the two have to agree, or a config an operator wrote with a
// stray space would be unselectable by the obvious name.
//
// The error names both the rejected value and the registry's actual contents.
// Listing them is not a leak — /claim is behind the same auth guard as every
// other control-plane route, and a caller that may spawn a variant may
// certainly know it exists — and without the list the only way to discover a
// typo is to ask the operator for their broker.yaml.
func resolveClaimBinary(binaries map[string]BinaryEntry, requested string) (string, BinaryEntry, error) {
	name := strings.TrimSpace(requested)
	if name == "" {
		name = reservedBinaryName
	}
	entry, ok := binaries[name]
	if !ok {
		return "", BinaryEntry{}, fmt.Errorf("unknown binary %q; this broker spawns: %s",
			name, strings.Join(sortedBinaryNames(binaries), ", "))
	}
	return name, entry, nil
}

// fail writes a JSON error response and logs the cause.
func (s *ClaimServer) fail(w http.ResponseWriter, code int, msg string, err error) {
	if err != nil {
		s.logger.Warn("claim failed", "status", code, "reason", msg, "error", err)
	} else {
		s.logger.Warn("claim failed", "status", code, "reason", msg)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// writeTempConfig writes the claim's config to a temp YAML file and returns its
// path. The caller is responsible for removing it.
func writeTempConfig(config string) (string, error) {
	f, err := os.CreateTemp("", "nexus-broker-claim-*.yaml")
	if err != nil {
		return "", fmt.Errorf("creating temp config: %w", err)
	}
	if _, err := f.WriteString(config); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("writing temp config: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("closing temp config: %w", err)
	}
	return f.Name(), nil
}

// instanceDialHost resolves the host:port a spawned instance dials back to.
// Instances run on the same machine, so a wildcard/empty bind host collapses
// to loopback to guarantee reachability.
func instanceDialHost(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil || port == "" {
		return "127.0.0.1:8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// listenAddrNamesHost reports whether listenAddr carries an explicit,
// non-wildcard host — i.e. whether it names an address a remote client could
// dial. A wildcard or empty bind host names none.
//
// It is shared by clientWSHost and warnIfAdvertiseAddrMissing on purpose: the
// warning must fire on exactly the configuration shape that makes clientWSHost
// fall through to the request Host, and one predicate is the only way to keep
// those two in step.
func listenAddrNamesHost(listenAddr string) bool {
	host, _, err := net.SplitHostPort(listenAddr)
	return err == nil && host != "" && host != "0.0.0.0" && host != "::"
}

// clientWSHost resolves the host:port a remote client uses to reach the broker,
// in strict precedence order:
//
//  1. advertiseHost — the parsed advertise_addr. Explicit operator intent about
//     how THIS broker is reached, so nothing may override it.
//  2. an explicit host in listen_addr — unambiguous, and already correct.
//  3. the Host header the claim arrived on — a guess. Right for a direct client,
//     WRONG behind a proxy or load balancer, where it names the intermediary and
//     can send a reconnect to a broker that does not hold the lease.
//  4. loopback — the last resort when even the request Host is absent.
//
// With advertiseHost empty this is byte-identical to the pre-advertise_addr
// behavior, which is what keeps existing deployments unchanged.
func clientWSHost(advertiseHost, listenAddr, requestHost string) string {
	if advertiseHost != "" {
		return advertiseHost
	}
	if listenAddrNamesHost(listenAddr) {
		return listenAddr
	}
	if requestHost != "" {
		return requestHost
	}
	return instanceDialHost(listenAddr)
}

// clientWSScheme resolves the URL scheme of a returned ws_url. It defaults to
// plain ws:// so that every configuration that predates advertise_addr — and
// every bare `host:port` advertise_addr — keeps producing the exact same URLs.
// Only a scheme-qualified advertise_addr (typically wss:// for a
// TLS-terminating proxy) changes it.
func clientWSScheme(advertiseScheme string) string {
	if advertiseScheme == "" {
		return "ws"
	}
	return advertiseScheme
}

// clientWSBaseURL assembles the scheme://host prefix of a client WebSocket URL.
// It is the single place the two halves are combined, so the claim response and
// any future caller cannot disagree about precedence.
func clientWSBaseURL(cfg Config, requestHost string) string {
	return clientWSScheme(cfg.AdvertiseScheme) + "://" + clientWSHost(cfg.AdvertiseHost, cfg.ListenAddr, requestHost)
}

// warnIfAdvertiseAddrMissing logs one WARN at boot when the broker is configured
// in the precise shape that makes every returned ws_url a guess: no
// advertise_addr, and a listen_addr with no explicit host for clientWSHost to
// fall back on.
//
// It is loud and specific because the failure is silent otherwise — the broker
// works perfectly for a directly-connected client and breaks only once a proxy is
// in front of it, at which point the returned ws_url names the proxy and a
// reconnect can be routed to a broker that does not hold the lease.
func warnIfAdvertiseAddrMissing(logger *slog.Logger, cfg Config) {
	if cfg.AdvertiseHost != "" || listenAddrNamesHost(cfg.ListenAddr) {
		return
	}
	logger.Warn("advertise_addr is not set and listen_addr names no host: "+
		"the ws_url returned by POST /claim will be derived from each claim request's Host header, "+
		"which behind a reverse proxy or load balancer names the proxy rather than this broker "+
		"and can send a client to a broker that does not hold its lease; "+
		"set advertise_addr to the address clients use to reach this broker",
		"listen_addr", cfg.ListenAddr)
}
