// Command nexus-broker is a standalone service that fronts OS-isolated Nexus
// instances. It exposes an HTTP/WebSocket gateway: clients claim a lease, the
// broker spawns (or recalls) a nexus instance, and bridges IO frames between
// them.
//
// This binary is the foundation scaffold (E1-S1): config loading, signal
// handling, an slog handler, and an HTTP server with a health route. The
// gateway, claim handling, and spawn logic land in later stories.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		// Logging already happened at the failure site; this is the
		// non-zero exit path.
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "broker.yaml", "path to broker config file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		logger.Error("failed to load config", "path", *configPath, "error", err)
		return err
	}

	// Non-fatal config complaints (deprecated keys, folded aliases) are collected
	// during the load rather than logged there, because config is parsed before
	// anything else exists and the parser has no logger. Drained here so they
	// land in the same boot output as everything else, before the startup line
	// that reports the values they affected.
	for _, warning := range cfg.Warnings {
		logger.Warn(warning)
	}

	logger.Info("nexus-broker starting",
		"listen_addr", cfg.ListenAddr,
		// Logged because it decides what clients are told to connect back to; an
		// operator debugging a bad ws_url needs to see the value in effect.
		"advertise_addr", cfg.AdvertiseAddr,
		// nexus_binary_path is deliberately NOT logged here any more. It is a
		// deprecated INPUT that is folded into the registry at load and read
		// nowhere afterwards, so printing it would name a value that no longer
		// decides anything — the per-entry lines below are the truth.
		// Logged because the registry decides what a claim can spawn at all, and
		// the count is the cheapest way for an operator to confirm the `binaries:`
		// block they wrote was actually picked up (it is never below 1 — the
		// reserved `nexus` entry always exists).
		"binaries", len(cfg.Binaries),
		// Logged because an empty state_dir means lease state is lost on restart,
		// and that is a decision an operator should be able to confirm from the
		// boot line rather than infer from a missing directory.
		"state_dir", cfg.StateDir,
		"max_concurrent", cfg.MaxConcurrent,
		"idle_timeout", cfg.IdleTimeout,
		"queue_wait_timeout", cfg.QueueWaitTimeout,
		"release_grace", cfg.ReleaseGrace,
		// Logged because it decides how long a restored lease holds a capacity slot
		// waiting for an instance that may never come back.
		"reattach_window", cfg.ReattachWindow,
		// Logged because a mistyped admin scope fails SILENTLY — the operator view
		// of GET /leases simply never engages — so the value in effect has to be
		// visible somewhere at boot.
		"admin_scope", cfg.AdminScope,
	)

	// One line per registry entry, carrying the RESOLVED path and not just the
	// configured one. A bare `path` goes through the broker process's own PATH, so
	// `nexus` can silently be a different build than the operator has in mind —
	// a stale copy in ~/go/bin ahead of /usr/local/bin, say. That surprise has to
	// be visible in the boot log, where it can be compared against a deploy, rather
	// than inferred later from an instance behaving oddly.
	for _, name := range sortedBinaryNames(cfg.Binaries) {
		entry := cfg.Binaries[name]
		logger.Info("binary registry entry",
			"name", name,
			"path", entry.Path,
			"resolved_path", entry.ResolvedPath,
		)
	}

	// A wildcard bind with no advertise_addr makes every returned ws_url depend on
	// the claim request's Host header. That is fine for a directly-reachable
	// broker and wrong behind a proxy, and nothing later in the process can tell
	// the two apart — so the only place to say so is here, at boot.
	warnIfAdvertiseAddrMissing(logger, cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Authentication is opt-in. A broker.yaml with no `auth:` block keeps every
	// route exactly as it was before authentication existed — that backward
	// compatibility is a release requirement, not a convenience — so the disabled
	// path does nothing but warn, once and loudly, that this broker will serve
	// anyone who can reach it.
	//
	// The guard is built before the gateway because the gateway needs it: the
	// client WebSocket route resolves its own credential (it is not registered
	// through Guard) so it can refuse a caller that does not own the lease.
	guard := newAuthGuard(logger, cfg.AuthChain)
	guard.logStartupState()

	// Single-use WebSocket tickets exist ONLY because a browser cannot put a bearer
	// header on a WebSocket handshake. With authentication disabled there is
	// nothing to authenticate, so the store is built INERT: it issues nothing, the
	// claim response omits `ticket`, and WS /lease/{lease_id} keeps accepting a
	// connection with no ticket exactly as it did before tickets existed.
	tickets := newTicketStore(logger, guard.enabled())

	// Lease durability. An unset state_dir returns a nil store and persistence
	// stays off; a state_dir that is set but unusable fails the boot, because an
	// operator who asked for durability and silently did not get it would only
	// find out from a restart that lost the state they configured it to keep.
	leaseStore, brokerID, err := openLeaseStore(logger, cfg)
	if err != nil {
		logger.Error("failed to open lease state", "state_dir", cfg.StateDir, "error", err)
		return err
	}
	if leaseStore != nil {
		defer func() { _ = leaseStore.Close() }()
		logger.Info("lease durability enabled", "state_dir", cfg.StateDir, "broker_id", brokerID)
	} else {
		logger.Warn("state_dir is not set: lease state is in-memory only and a restart " +
			"will lose track of every instance this broker spawned")
	}

	// The durable session → binary index. It is separate from the lease journal on
	// purpose: the journal is compacted down to live leases, and a resume always
	// arrives after the original lease was released, so a binding kept only there
	// would be gone exactly when it is wanted.
	//
	// It shares state_dir's fate — unset means no index and no file — but NOT the
	// journal's boot-failure policy. The index backs an advisory check whose answer
	// for an unknown session is "no opinion, proceed", so a broker that cannot open
	// it degrades to the behaviour it had before the index existed. Refusing to boot
	// over that would trade a weaker resume check for a total outage. Loud WARN,
	// nil index, carry on.
	sessionBinaries, err := openSessionBinaryIndex(logger, cfg)
	if err != nil {
		logger.Warn("failed to open the session binary index; resume will not know which binary "+
			"served a session whose lease has already been released",
			"state_dir", cfg.StateDir, "error", err)
		sessionBinaries = nil
	}
	if sessionBinaries != nil {
		defer func() { _ = sessionBinaries.Close() }()
	}

	// The spawn-secret derivation key. It shares state_dir's fate: present and
	// stable when durability is on, absent (and secrets random per spawn, as they
	// always were) when it is off. Loaded AFTER openLeaseStore because that is
	// what creates the directory.
	spawnSecretKey, err := loadSpawnKey(logger, cfg.StateDir)
	if err != nil {
		logger.Error("failed to load the broker spawn key", "state_dir", cfg.StateDir, "error", err)
		return err
	}

	registry := NewRegistry(logger, cfg.MaxConcurrent)
	// The registry invalidates a lease's tickets when the lease goes away, through
	// the single teardown convergence point (Remove) so manual release, the idle
	// sweeper and crash detection all invalidate.
	registry.useTicketStore(tickets)
	// Records carry the RAW advertise_addr, verbatim as configured, so a record
	// round-trips what is in broker.yaml rather than a derived host.
	registry.useLeaseStore(leaseStore, brokerID, cfg.AdvertiseAddr)
	// Wired BEFORE recovery, so a lease restored from the journal can still have its
	// pairing consulted, and before anything is served, since the setter is not safe
	// to call alongside a live lease transition.
	registry.useSessionBinaryIndex(sessionBinaries)

	// Restart recovery. It runs BEFORE anything is served, because a surviving
	// instance is already dialing /instance on its reconnect backoff and every
	// attempt made before its lease is back in the registry is refused as unknown.
	// A restored lease re-holds its capacity slot immediately, so the cap is
	// honest from the first claim this process accepts.
	restoredLeases := recoverLeases(logger, registry, leaseStore, brokerID, spawnSecretKey)

	// The gateway holds the ticket store as well as the guard: the client WebSocket
	// accepts a single-use `?ticket=` as an alternative to a bearer header, and it
	// is the only route that redeems one.
	gateway := NewGateway(logger, registry, guard, tickets)
	claims := NewClaimServer(logger, registry, cfg, execRunner{}, tickets)
	// Spawn secrets are derived from the broker's key when there is one, so a
	// restart can recompute what a surviving instance is holding.
	claims.useSpawnKey(spawnSecretKey)
	releases := NewReleaseServer(logger, registry, cfg.ReleaseGrace)
	// Ticket refresh: ticketTTL is deliberately tight, so a reconnect needs a fresh
	// ticket and re-claiming (which would spawn a new instance) is not an answer.
	ticketsServer := NewTicketServer(logger, registry, tickets)
	// The leases listing is scoped to the caller unless it holds cfg.AdminScope,
	// so it needs both the guard (to know whether auth is on at all) and the
	// configured operator scope.
	leases := NewLeasesServer(logger, registry, guard, cfg.AdminScope)
	// The binary listing is projected from the config ONCE, here, because the
	// registry is immutable after load — there is no reload path — so the handler
	// holds a finished response rather than the Config it came from.
	binaries := NewBinariesServer(logger, cfg.Binaries)

	// The A2A ingress. Each `agents:` profile publishes an Agent Card and the two
	// HTTP bindings under its own path namespace, so a third-party A2A client can
	// address an agent by URL instead of supplying a full nexus config the way
	// POST /claim demands.
	//
	// Cards are rendered HERE, at boot, so a card that is not servable — a
	// missing skill, a security scheme that cannot be derived — fails the start
	// rather than the first client that fetches it. With no `agents:` block this
	// builds an empty server that registers no routes at all.
	agents, err := NewA2AServer(logger, cfg)
	if err != nil {
		logger.Error("failed to build the a2a agent profiles", "error", err)
		return err
	}
	agents.logStartupState(cfg)

	// The idle sweeper releases leases with no real client input for
	// idle_timeout, reusing the shared release teardown. idle_timeout <= 0
	// disables it. It runs until sweepCtx is cancelled on shutdown.
	sweeper := newIdleSweeper(logger, registry, cfg.IdleTimeout, cfg.ReleaseGrace)
	sweepCtx, stopSweep := context.WithCancel(context.Background())
	defer stopSweep()
	go sweeper.Run(sweepCtx)

	// Bound the restored leases: anything no instance reattaches to within
	// reattach_window is torn down through the shared release path. It shares the
	// sweeper's context so a shutdown cancels the wait instead of reaping leases on
	// the way out. A no-op when nothing was restored.
	go reapUnreattached(sweepCtx, logger, registry, restoredLeases, cfg.ReattachWindow, cfg.ReleaseGrace)

	mux := http.NewServeMux()

	// Health stays outside the guard on purpose: a load balancer or container
	// probe has no credential to present, and liveness leaks nothing.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// The WebSocket routes are registered on the raw mux: the client WS will also
	// accept a single-use ticket and the instance dial-back a spawn secret, both of
	// which are their own stories. Bearer-header middleware is the wrong instrument
	// for either, so the client route resolves its own credential and enforces
	// lease ownership inside handleClient (it holds the guard for exactly that).
	gateway.Register(mux)

	// Client-facing control-plane routes go through the guard. Registering via
	// `guarded` (rather than wrapping handlers individually) means any route
	// added here later is authenticated by construction.
	guarded := guard.Guard(mux)
	claims.Register(guarded)
	releases.Register(guarded)
	leases.Register(guarded)
	ticketsServer.Register(guarded)
	// Behind the SAME guard as /claim: enumerating what this broker can spawn is
	// a control-plane read, and a caller that cannot claim has no business
	// learning which variants an operator deploys.
	binaries.Register(guarded)
	// Behind the SAME guard again, card included. An A2A caller is refused by
	// exactly the middleware that refuses a /claim caller, so there is one
	// authentication policy on this binary rather than two — see the auth-posture
	// comment on handleAgentCard for why the discovery document is inside it
	// here and outside it in nexus.io.a2a.
	agents.Register(guarded)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("http gateway listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining")
	case err := <-serveErr:
		if err != nil {
			logger.Error("http gateway failed", "error", err)
			return err
		}
		return nil
	}

	stopSweep()
	gateway.Shutdown()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		return err
	}
	logger.Info("nexus-broker stopped")
	return nil
}
