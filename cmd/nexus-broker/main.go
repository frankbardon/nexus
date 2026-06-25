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
		"nexus_binary_path", cfg.NexusBinaryPath,
		"max_concurrent", cfg.MaxConcurrent,
		"idle_timeout", cfg.IdleTimeout,
		"queue_wait_timeout", cfg.QueueWaitTimeout,
		"release_grace", cfg.ReleaseGrace,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	registry := NewRegistry(logger, cfg.MaxConcurrent)
	gateway := NewGateway(logger, registry)
	claims := NewClaimServer(logger, registry, cfg, execRunner{})
	releases := NewReleaseServer(logger, registry, cfg.ReleaseGrace)

	// The idle sweeper releases leases with no real client input for
	// idle_timeout, reusing the shared release teardown. idle_timeout <= 0
	// disables it. It runs until sweepCtx is cancelled on shutdown.
	sweeper := newIdleSweeper(logger, registry, cfg.IdleTimeout, cfg.ReleaseGrace)
	sweepCtx, stopSweep := context.WithCancel(context.Background())
	defer stopSweep()
	go sweeper.Run(sweepCtx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	gateway.Register(mux)
	claims.Register(mux)
	releases.Register(mux)

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
