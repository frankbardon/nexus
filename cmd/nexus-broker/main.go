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

	logger.Info("nexus-broker starting",
		"listen_addr", cfg.ListenAddr,
		// Logged because it decides what clients are told to connect back to; an
		// operator debugging a bad ws_url needs to see the value in effect.
		"advertise_addr", cfg.AdvertiseAddr,
		"nexus_binary_path", cfg.NexusBinaryPath,
		"max_concurrent", cfg.MaxConcurrent,
		"idle_timeout", cfg.IdleTimeout,
		"queue_wait_timeout", cfg.QueueWaitTimeout,
		"release_grace", cfg.ReleaseGrace,
		// Logged because a mistyped admin scope fails SILENTLY — the operator view
		// of GET /leases simply never engages — so the value in effect has to be
		// visible somewhere at boot.
		"admin_scope", cfg.AdminScope,
	)

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

	registry := NewRegistry(logger, cfg.MaxConcurrent)
	// The registry invalidates a lease's tickets when the lease goes away, through
	// the single teardown convergence point (Remove) so manual release, the idle
	// sweeper and crash detection all invalidate.
	registry.useTicketStore(tickets)
	// The gateway holds the ticket store as well as the guard: the client WebSocket
	// accepts a single-use `?ticket=` as an alternative to a bearer header, and it
	// is the only route that redeems one.
	gateway := NewGateway(logger, registry, guard, tickets)
	claims := NewClaimServer(logger, registry, cfg, execRunner{}, tickets)
	releases := NewReleaseServer(logger, registry, cfg.ReleaseGrace)
	// Ticket refresh: ticketTTL is deliberately tight, so a reconnect needs a fresh
	// ticket and re-claiming (which would spawn a new instance) is not an answer.
	ticketsServer := NewTicketServer(logger, registry, tickets)
	// The leases listing is scoped to the caller unless it holds cfg.AdminScope,
	// so it needs both the guard (to know whether auth is on at all) and the
	// configured operator scope.
	leases := NewLeasesServer(logger, registry, guard, cfg.AdminScope)

	// The idle sweeper releases leases with no real client input for
	// idle_timeout, reusing the shared release teardown. idle_timeout <= 0
	// disables it. It runs until sweepCtx is cancelled on shutdown.
	sweeper := newIdleSweeper(logger, registry, cfg.IdleTimeout, cfg.ReleaseGrace)
	sweepCtx, stopSweep := context.WithCancel(context.Background())
	defer stopSweep()
	go sweeper.Run(sweepCtx)

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
