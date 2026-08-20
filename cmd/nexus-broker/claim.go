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

	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// defaultReadyTimeout bounds how long POST /claim waits for a freshly spawned
// instance to dial back and signal ready before giving up, killing it, and
// returning an error. It is the value the `ready_timeout` config key takes when
// an operator does not write it.
//
// Thirty seconds is a boot budget, and it is the one most likely to be wrong for
// a given deployment: it has to cover process start, engine construction and
// every plugin's Init/Ready, so a config that pulls a large model list, warms a
// vector store or dials several MCP servers can legitimately exceed it. Before
// the key existed that surfaced as `504 instance did not become ready in time`
// with nothing an operator could turn.
const defaultReadyTimeout = 30 * time.Second

// defaultSessionReportGrace bounds how long POST /claim waits, after the
// instance signals ready, for its session-id-report frame to arrive. The
// plugin sends the report immediately after ready, so this is a short grace
// window: if it elapses the claim still succeeds, just without a session id in
// the response. It is the value the `session_report_grace` config key takes when
// an operator does not write it.
const defaultSessionReportGrace = 5 * time.Second

// defaultMaxClaimBody caps the size of a claim request body to avoid unbounded
// reads. It is the value the `max_claim_body` config key takes when an operator
// does not write it.
//
// A claim body carries the WHOLE nexus config inline, so the ceiling scales with
// how large an operator's profiles are: a deployment shipping a long skills
// block, many MCP servers or an inlined system prompt can outgrow a megabyte,
// and the refusal (a 400 from http.MaxBytesReader) says nothing about which
// bound was hit.
const defaultMaxClaimBody int64 = 1 << 20 // 1 MiB

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
	//
	// ON A RESUME the recorded binary takes part: omitted then means the entry
	// that CREATED the session (not the reserved one), and a name that disagrees
	// with the record is a 409. Both are best-effort — an unrecorded session
	// resolves exactly as a fresh claim does. See resolveSpawnBinary.
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
	logger   *slog.Logger
	registry *Registry
	runner   commandRunner

	// live is the atomic configuration snapshot every claim reads.
	//
	// It replaces the by-value Config copy and the four bounds this server used
	// to latch at construction (ready_timeout, session_report_grace,
	// max_claim_body, queue_wait_timeout). Latching them meant a SIGHUP could
	// never reach a live claim server: swapping a Config behind it would have
	// changed nothing an in-flight or future claim actually read. Reading them
	// through the holder at USE time is what makes them reloadable, and it is one
	// seam rather than four setters.
	//
	// A claim reads the snapshot ONCE, at the top of the request, so one claim
	// cannot straddle a reload — it either spawns entirely under the old
	// configuration or entirely under the new one.
	live *configHolder

	// tickets mints the single-use client WebSocket ticket returned with a
	// successful claim. A nil store (or one built inert because auth is disabled)
	// simply issues nothing, and the response omits the `ticket` field.
	tickets *ticketStore

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
//
// The Config is held as a PRIVATE configuration snapshot (see
// newLocalConfigHolder). run() calls useConfigHolder afterwards to replace it
// with the process-wide one, which is what makes the claim path follow a SIGHUP.
//
// A non-positive ready_timeout, session_report_grace, max_claim_body or
// release_grace in that Config falls back to the corresponding default — see
// settleConfig, which is where that fold now lives so every reader of a snapshot
// sees one settled number. That is not a second, silent validation path:
// LoadConfigFromBytes already refuses a non-positive value for each of them,
// naming the key, so the only way to reach the fallback is a Config built as a
// struct literal rather than loaded (every caller in the test suite, and any
// future in-process embedder), where "unset" must keep meaning "the default"
// rather than "reject every claim instantly".
func NewClaimServer(logger *slog.Logger, registry *Registry, cfg Config, runner commandRunner, tickets *ticketStore) *ClaimServer {
	if logger == nil {
		logger = slog.Default()
	}
	if runner == nil {
		runner = execRunner{}
	}
	return &ClaimServer{
		logger:   logger,
		registry: registry,
		runner:   runner,
		live:     newLocalConfigHolder(cfg),
		tickets:  tickets,
	}
}

// useConfigHolder points this server at the process-wide configuration snapshot,
// so a SIGHUP changes what the next claim spawns and how long it will wait.
//
// It is a setter rather than a constructor parameter for the same reason
// useSpawnKey is: every existing caller hands the constructor a bare Config, and
// only run() has a shared holder to give. A nil holder is ignored, leaving the
// private snapshot the constructor built — which is exactly a broker that never
// reloads.
func (s *ClaimServer) useConfigHolder(h *configHolder) {
	if h == nil {
		return
	}
	s.live = h
}

// config returns the configuration snapshot in force. The returned pointer aims
// into an immutable value: read it, never write through it.
//
// A caller that needs more than one field from it must take the snapshot ONCE
// into a local and read every field from that local, so its answers cannot
// straddle a reload — spawnInstance is the worked example.
func (s *ClaimServer) config() *Config { return s.live.config() }

// Register wires the claim endpoint onto a mux. It takes a routeMux so main can
// register it behind the auth guard.
func (s *ClaimServer) Register(mux routeMux) {
	mux.HandleFunc("POST /claim", s.handleClaim)
}

// instanceSpawn is a successfully booted instance: the lease it runs under, the
// engine session it is serving, and the identifying details a caller records or
// logs.
//
// It exists because POST /claim is no longer the only thing that spawns an
// instance. The broker's A2A ingress starts one too — from a profile rather than
// from a request body — and the two MUST NOT be separate spawn paths: capacity
// accounting, the spawn secret, the recorded-binary reconciliation, the ready
// wait and the crash watcher are each load-bearing, and a second implementation
// would eventually differ in one of them. So the spine below is a method, this
// is what it returns, and handleClaim is a thin HTTP shell over it.
type instanceSpawn struct {
	// leaseID is the lease the instance registered against.
	leaseID string

	// sessionID is the engine session the instance is running: the id it
	// reported for a fresh session, or the requested id on a resume. It may be
	// EMPTY when a fresh instance did not report one within the grace window —
	// the spawn still succeeded, and a caller that needs an id to resume later
	// has to treat the absence as "unknown", never as an error.
	sessionID string

	// binary is the `binaries:` registry entry name that was exec()d.
	binary string

	// pid is the spawned process id, for logging.
	pid int
}

// claimFailure is a spawn that produced no live instance, carrying the HTTP
// status POST /claim answers it with.
//
// The status is part of the value rather than re-derived by each caller because
// classifying a failure is exactly the part that must not drift: an unknown
// binary is a 400, a session bound to a binary this broker no longer offers is a
// 409, capacity exhaustion is a 503, and a boot that never became ready is a
// 504. A second caller — the A2A ingress — maps the SAME classification onto A2A
// task states, and it can only do that faithfully if the classification is made
// once, here.
type claimFailure struct {
	status int
	msg    string
	err    error
}

func (f *claimFailure) Error() string {
	if f.err != nil {
		return f.msg + ": " + f.err.Error()
	}
	return f.msg
}

func (f *claimFailure) Unwrap() error { return f.err }

// silent reports a failure that must not be answered at all, because the caller
// is already gone: a claim whose context was cancelled while it waited in the
// capacity queue. It is spelled as a zero status rather than a bool so a caller
// that forgets to check it writes an obviously-wrong response code instead of a
// plausible one.
func (f *claimFailure) silent() bool { return f.status == 0 }

// refusedByCaller reports whether the REQUEST was at fault (a 4xx) rather than
// the spawn. It is the distinction the A2A ingress needs: a request the broker
// refused is a REJECTED task, and a boot that failed is a FAILED one.
func (f *claimFailure) refusedByCaller() bool { return f.status >= 400 && f.status < 500 }

// spawnInstance is THE instance spawn spine: resolve the binary, acquire a
// capacity slot, mint the lease and its spawn secret, write the temp config,
// exec the instance, wait (bounded) for its dial-back and ready signal, wait a
// short grace for its session-id report, and arm the crash watcher.
//
// Every error path cleans up after itself — the temp config is removed, a
// started process is killed and reaped, and the lease (with its capacity slot)
// is dropped — so a failed spawn leaks nothing.
//
// It performs NO authorization and writes NO response. Ownership is decided by
// the caller: handleClaim passes the principal the auth guard resolved, and the
// A2A ingress passes the principal that authenticated the A2A request. Both are
// real callers; neither is the broker acting on itself, so there is nothing here
// that has to cope with an absent one.
func (s *ClaimServer) spawnInstance(ctx context.Context, req claimRequest, owner nexusauth.Principal) (instanceSpawn, *claimFailure) {
	// Resolve the variant this spawn produces BEFORE anything is allocated.
	// Everything below this point acquires something that has to be given back —
	// a capacity slot, a lease id, a temp config file, a child process — and
	// neither a caller's typo nor a refused resume must be able to consume any of
	// it. Doing it here also means the rejection is instant rather than arriving
	// after a queue wait.
	//
	// For a resume the answer is not the requested name alone: the binary
	// recorded for the session takes part, so a persisted session directory is
	// not handed to a build that never created it. BinaryForSession is
	// best-effort and reports unknown for a fresh claim, so this one call covers
	// both shapes without a branch here — see resolveSpawnBinary.
	//
	// The configuration snapshot is taken ONCE, here, and every value this spawn
	// needs is read from it — the registry entry, the environment it inherits,
	// the address it dials back on and all three of its bounds. A SIGHUP that
	// lands mid-spawn therefore cannot pair a new registry entry with an old
	// inherit_env, or a new ready_timeout with an entry that is no longer
	// offered: this spawn runs entirely under the configuration it started with,
	// and the next one runs entirely under the reloaded one.
	cfg := s.config()
	recorded, recordedKnown := s.registry.BinaryForSession(req.SessionID)
	binaryName, entry, status, err := resolveSpawnBinary(cfg.Binaries, req.SessionID, req.Binary, recorded, recordedKnown)
	if err != nil {
		return instanceSpawn{}, &claimFailure{status: status, msg: err.Error()}
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
	//   - errQueueFull: the cap is full and the queue already holds
	//     max_queue_depth waiters → immediate 503 "capacity queue full". A THIRD
	//     distinct message, so the three capacity refusals are told apart in the
	//     claim-failure log without correlating timings.
	//   - errPrincipalLeaseLimit / errPrincipalQueueLimit: this PRINCIPAL is over
	//     one of its per-principal caps → 429, not 503: the broker may be
	//     entirely idle, and the refusal is about the caller's quota rather than
	//     about capacity. Both are inert unless `auth:` is configured (see
	//     Registry.principalCaps), so an unauthenticated broker can never reach
	//     these arms.
	//   - context cancelled: the caller hung up while queued → a silent failure,
	//     because there is nobody left to answer; the waiter is already dropped
	//     from the queue.
	leaseID, err := s.registry.NewLeaseQueued(ctx, cfg.QueueWaitTimeout, owner)
	if err != nil {
		switch {
		case errors.Is(err, errQueueTimeout):
			return instanceSpawn{}, &claimFailure{status: http.StatusServiceUnavailable, msg: "capacity wait timed out"}
		case errors.Is(err, errNoCapacity):
			return instanceSpawn{}, &claimFailure{status: http.StatusServiceUnavailable, msg: "no capacity"}
		case errors.Is(err, errQueueFull):
			return instanceSpawn{}, &claimFailure{status: http.StatusServiceUnavailable, msg: "capacity queue full"}
		case errors.Is(err, errPrincipalLeaseLimit):
			return instanceSpawn{}, &claimFailure{status: http.StatusTooManyRequests, msg: "lease limit reached for this principal"}
		case errors.Is(err, errPrincipalQueueLimit):
			return instanceSpawn{}, &claimFailure{status: http.StatusTooManyRequests, msg: "queued claim limit reached for this principal"}
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return instanceSpawn{}, &claimFailure{status: 0, msg: "claim cancelled while queued for capacity", err: err}
		default:
			return instanceSpawn{}, &claimFailure{status: http.StatusInternalServerError, msg: "minting lease", err: err}
		}
	}

	// Stamp the resolved entry name on the lease at once, while nothing else can
	// yet have happened to it. Every record the lease writes from here on carries
	// which variant it is running — in particular the one the session-id report
	// produces, which is where the session id and the binary first exist together
	// and therefore where the pairing becomes durable. Doing it any later would
	// mean the mapping depended on how far a claim got before something failed.
	s.registry.SetBinary(leaseID, binaryName)

	// A RESUME already names its session, so the pairing is complete here — before
	// anything is spawned and before the instance reports anything. Record it now
	// rather than waiting for the session-id report: a resume that never gets that
	// far (the instance dies booting) would otherwise leave the binding for a
	// session that DOES exist on disk unrecorded, and the next attempt would be
	// just as blind. For a new session req.SessionID is empty, RecordSessionBinary
	// no-ops, and MarkSessionID does the recording when the id arrives.
	//
	// The requested id is used rather than a reported one because it is what the
	// caller holds and what -recall was handed; a mismatching report is already
	// treated as advisory further down.
	s.registry.RecordSessionBinary(req.SessionID, binaryName)

	configPath, err := writeTempConfig(req.Config)
	if err != nil {
		s.registry.Remove(leaseID)
		return instanceSpawn{}, &claimFailure{status: http.StatusInternalServerError, msg: "writing temp config", err: err}
	}
	// The instance reads the config synchronously at boot (before it dials
	// back and signals ready), so the file is safe to remove once this
	// call returns — on success and on every failure path alike.
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
	// A generation failure fails the spawn. crypto/rand not producing 16 bytes
	// means the machine is in a state where nothing security-relevant should
	// proceed, and spawning an instance whose dial-back cannot be authenticated
	// is precisely the outcome this check exists to prevent.
	spawnSecret, err := s.spawnKey.secretFor(leaseID)
	if err != nil {
		s.registry.Remove(leaseID)
		return instanceSpawn{}, &claimFailure{status: http.StatusInternalServerError, msg: "minting spawn secret", err: err}
	}
	s.registry.SetSpawnSecret(leaseID, spawnSecret)

	brokerAddr := "ws://" + instanceDialHost(cfg.ListenAddr) + instanceWSPath
	// The entry's RESOLVED path is what is exec()d — LoadConfig verified it at
	// boot, so no claim performs a stat or a PATH lookup, and a PATH that
	// changes under a running broker cannot make one claim spawn a different
	// build than the next.
	handle, err := s.runner.start(ctx, spawnSpec{
		binaryName: binaryName,
		binaryPath: entry.ResolvedPath,
		binaryArgs: entry.Args,
		binaryEnv:  entry.Env,
		inheritEnv: cfg.InheritEnv,
		// The entry's EFFECTIVE credential: its own `run_as`, or the broker-level
		// default folded onto it at boot. Nil for every entry that declared
		// neither, which is the spawn this broker has always performed.
		runAs:           entry.RunAs,
		configPath:      configPath,
		leaseID:         leaseID,
		brokerAddr:      brokerAddr,
		spawnSecret:     spawnSecret,
		recallSessionID: req.SessionID,
	})
	if err != nil {
		s.registry.Remove(leaseID)
		return instanceSpawn{}, &claimFailure{status: http.StatusInternalServerError, msg: "spawning instance", err: err}
	}
	// SetProcess starts the single reaper that wait()s the process and closes
	// the lease's exited channel; both this path and a later release observe
	// it, so the process is wait()ed in exactly one place.
	s.registry.SetProcess(leaseID, handle)

	select {
	case <-s.registry.ReadyChan(leaseID):
		// Instance booted and signalled ready.
	case <-s.registry.ExitedChan(leaseID):
		exitErr := s.registry.ExitErr(leaseID)
		s.registry.Remove(leaseID)
		return instanceSpawn{}, &claimFailure{status: http.StatusBadGateway, msg: "instance exited before signalling ready", err: exitErr}
	case <-time.After(cfg.ReadyTimeout):
		_ = handle.kill()
		<-s.registry.ExitedChan(leaseID) // reap the killed process so nothing leaks
		s.registry.Remove(leaseID)
		return instanceSpawn{}, &claimFailure{status: http.StatusGatewayTimeout, msg: "instance did not become ready in time"}
	}

	// Resolve the session id to report. The instance reports it via a
	// session-id-report frame just after ready; wait a bounded grace for it.
	// For a resume, the requested id is authoritative (and is what the caller
	// already holds); a mismatching report is logged but not fatal. For a new
	// session, the reported id is the only way the caller learns the
	// engine-generated id to -recall later.
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
	case <-time.After(cfg.SessionReportGrace):
		if req.SessionID == "" {
			s.logger.Warn("instance did not report a session id within grace window",
				"lease_id", leaseID)
		}
	}

	// The instance is live: from here an unexpected exit is a crash, not a
	// pre-ready spawn failure (which the paths above own). Start the crash
	// watcher. It latches the lease's releasing flag only if no deliberate
	// teardown beats it, so a later POST /release is never misclassified as a
	// crash.
	go s.registry.watchExit(leaseID)

	return instanceSpawn{
		leaseID:   leaseID,
		sessionID: sessionID,
		binary:    binaryName,
		pid:       handle.pid(),
	}, nil
}

// handleClaim implements POST /claim: validate the body, run the shared spawn
// spine, mint the client's WebSocket ticket, respond.
//
// It is deliberately thin. Everything that has to be true of ANY spawn — the
// capacity slot, the spawn secret, the recorded-binary reconciliation, the
// bounded ready wait, the crash watcher — lives in spawnInstance, so the A2A
// ingress boots an instance through exactly this machinery rather than beside
// it.
func (s *ClaimServer) handleClaim(w http.ResponseWriter, r *http.Request) {
	var req claimRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.config().MaxClaimBody))
	if err := dec.Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, "invalid claim body", err)
		return
	}
	if req.Config == "" {
		s.fail(w, http.StatusBadRequest, "claim requires a non-empty config", nil)
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

	spawn, failure := s.spawnInstance(r.Context(), req, owner)
	if failure != nil {
		if failure.silent() {
			// The client hung up while queued; the connection is gone and there is
			// nothing to write to.
			s.logger.Info("claim cancelled while queued for capacity", "error", failure.err)
			return
		}
		s.fail(w, failure.status, failure.msg, failure.err)
		return
	}

	// Mint the client's WebSocket ticket LAST, immediately before responding, so
	// its short TTL starts when the caller receives it rather than when the
	// instance began booting — a boot may take up to ready_timeout (30s by
	// default), which
	// would otherwise hand back an already-expired ticket.
	//
	// A mint failure does NOT fail the claim. The instance is live and the lease is
	// usable (a non-browser client authenticates the socket with its bearer
	// credential, and any client can mint a replacement via POST
	// /ticket/{lease_id}), so tearing down a working session over a recoverable
	// gap would be the worse outcome. The response then omits `ticket`.
	ticket, mintErr := s.tickets.mint(spawn.leaseID, owner.ID)
	if mintErr != nil {
		s.logger.Error("minting claim ticket failed; claim returns no ticket",
			"lease_id", spawn.leaseID, "principal_id", owner.ID, "error", mintErr)
	}

	wsURL := clientWSBaseURL(*s.config(), r.Host) + ClientWSPath(spawn.leaseID)
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
	s.logger.Info("claim ready", "lease_id", spawn.leaseID, "pid", spawn.pid, "ws_url", wsURL,
		"session_id", spawn.sessionID, "principal_id", owner.ID, "ticket_issued", ticket != "",
		"binary", spawn.binary)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(claimResponse{
		LeaseID:   spawn.leaseID,
		WSURL:     wsURL,
		SessionID: spawn.sessionID,
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

// resolveSpawnBinary picks the registry entry a claim actually spawns,
// reconciling the name the caller asked for with the one recorded for the
// session it is resuming. It returns the resolved name and entry, or an error
// together with the HTTP status the caller should report it as (0 on success),
// so handleClaim never has to re-classify a failure it did not diagnose.
//
// WHY A MISMATCH IS REFUSED RATHER THAN HONOURED. A session directory is engine
// state written by one particular build: its history, its tool results, its
// per-plugin data dirs and storage all assume the plugin set that produced them.
// Replaying it under a different variant does not fail loudly — the engine boots,
// the transcript loads, and the session simply behaves as if capabilities it once
// had have vanished, or gains ones its history was never written against. That
// failure surfaces to a user as "my session got worse", days after the claim that
// caused it, with nothing in any log tying the two together. Refusing at the
// claim is the only point where the mistake is still attributable, so the
// requested and the recorded name are BOTH named in the error: the caller can
// then either resume correctly or start a fresh session deliberately.
//
// recorded/recordedKnown come straight from Registry.BinaryForSession, and
// recordedKnown == false MEANS "NO OPINION, PROCEED" — it is never a mismatch.
// It is the answer for a fresh claim (no session id at all), an unknown session,
// a session created before bindings were recorded, a broker with no state_dir,
// and a binding the index evicted under its entry cap. Every one of those is a
// legitimate resume, so the unknown branch must fall through to exactly the
// pre-binding behaviour: spawn what was asked for, or the reserved entry when
// nothing was. Treating any of them as a conflict would reject real work to
// enforce a check the broker was explicitly built not to guarantee.
func resolveSpawnBinary(binaries map[string]BinaryEntry, sessionID, requested, recorded string, recordedKnown bool) (string, BinaryEntry, int, error) {
	// The REQUESTED name is validated first, so a misspelling is still the 400
	// that E1-S3 defined even on a resume. Reporting a typo as a mismatch would
	// tell the caller its session is bound to some other build when the real
	// problem is a name this broker has never had — and only the 400 lists the
	// names that do exist.
	name, entry, err := resolveClaimBinary(binaries, requested)
	if err != nil {
		return "", BinaryEntry{}, http.StatusBadRequest, err
	}
	if !recordedKnown {
		return name, entry, 0, nil
	}

	// An omitted binary on a resume INHERITS the recorded one instead of falling
	// back to the reserved `nexus` entry. This is the case the whole story exists
	// for: a client that captured a session id from a claim it made against a
	// variant, and reconnects to it later without restating which variant that
	// was, gets its own session back rather than a base build replaying it.
	if strings.TrimSpace(requested) == "" {
		recordedEntry, ok := binaries[recorded]
		if !ok {
			// The recorded entry was removed from `binaries:` (or renamed) since
			// the session was created. 409 rather than 400 or 500, deliberately:
			// the request is well-formed and the caller did nothing wrong — it
			// named a real session and asked for nothing this broker could
			// dispute — so blaming the client with a 400 would point at a value
			// it never sent and cannot correct. Nor did anything fail, so a 500
			// would report a healthy broker as broken. What conflicts is the
			// session's recorded state against this broker's current
			// configuration, which is precisely 409's meaning, and it keeps ONE
			// status for "this resume cannot proceed as recorded".
			//
			// A silent fallback to `nexus` is the outcome this branch exists to
			// prevent — it is the same foreign-build replay a mismatch is refused
			// for, arrived at by an operator's config change instead of a
			// caller's parameter, and it would be even harder to trace. The
			// message names the missing entry so the fix (restore it, or start a
			// new session) is obvious from the response alone.
			return "", BinaryEntry{}, http.StatusConflict, fmt.Errorf(
				"session %q was created by binary %q, which this broker no longer offers; "+
					"restore that entry, or start a new session. This broker spawns: %s",
				sessionID, recorded, strings.Join(sortedBinaryNames(binaries), ", "))
		}
		return recorded, recordedEntry, 0, nil
	}

	if name != recorded {
		return "", BinaryEntry{}, http.StatusConflict, fmt.Errorf(
			"session %q was created by binary %q but this claim requests %q; "+
				"resume it with %q, or omit `binary` to inherit it, or start a new session",
			sessionID, recorded, name, recorded)
	}
	return name, entry, 0, nil
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

// warnIfAdvertiseSchemeUnserved logs one WARN at boot when advertise_addr names
// a TLS scheme (wss:// or https://, both of which normalize to "wss") while this
// broker serves cleartext — which it always does, since it has no TLS listener
// and terminates nothing itself.
//
// It is a WARNING and deliberately not a boot refusal: a wss:// advertise_addr
// in front of a TLS-terminating proxy is the documented, supported deployment
// (see "Behind a proxy" in docs/src/guides/session-broker.md), and this process
// cannot tell whether such a proxy exists. Refusing to boot would break the
// intended configuration; saying nothing leaves an operator who forgot the proxy
// with no signal at all beyond clients failing to connect.
//
// It sits beside warnIfAdvertiseAddrMissing because it is the same kind of
// statement: a claim about how clients reach this broker that only the operator
// can confirm, and that nothing later in the process gets a chance to check.
func warnIfAdvertiseSchemeUnserved(logger *slog.Logger, cfg Config) {
	if cfg.AdvertiseScheme != "wss" {
		return
	}
	logger.Warn("advertise_addr names a TLS scheme but this broker serves cleartext: "+
		"it has no TLS listener, so the wss:// ws_url returned by POST /claim is correct "+
		"ONLY if a TLS-terminating proxy fronts this broker; "+
		"expected behind such a proxy, and a misconfiguration otherwise",
		"advertise_addr", cfg.AdvertiseAddr,
		"listen_addr", cfg.ListenAddr)
}
