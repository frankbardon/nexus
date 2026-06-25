package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
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
}

// NewClaimServer constructs a claim handler. A nil runner defaults to the
// production execRunner; tests inject a fake to avoid booting a real engine.
func NewClaimServer(logger *slog.Logger, registry *Registry, cfg Config, runner commandRunner) *ClaimServer {
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
	}
}

// Register wires the claim endpoint onto a mux.
func (s *ClaimServer) Register(mux *http.ServeMux) {
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

	leaseID, err := s.registry.NewLease()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "minting lease", err)
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

	brokerAddr := "ws://" + instanceDialHost(s.cfg.ListenAddr) + instanceWSPath
	handle, err := s.runner.start(r.Context(), spawnSpec{
		binaryPath:      s.cfg.NexusBinaryPath,
		configPath:      configPath,
		leaseID:         leaseID,
		brokerAddr:      brokerAddr,
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

	wsURL := "ws://" + clientWSHost(s.cfg.ListenAddr, r.Host) + ClientWSPath(leaseID)
	s.logger.Info("claim ready", "lease_id", leaseID, "pid", handle.pid(), "ws_url", wsURL, "session_id", sessionID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(claimResponse{LeaseID: leaseID, WSURL: wsURL, SessionID: sessionID})
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

// clientWSHost resolves the host:port a remote client uses to reach the broker.
// It prefers an explicit host in listen_addr; otherwise it falls back to the
// host the claim request arrived on, then to loopback.
func clientWSHost(listenAddr, requestHost string) string {
	host, _, err := net.SplitHostPort(listenAddr)
	if err == nil && host != "" && host != "0.0.0.0" && host != "::" {
		return listenAddr
	}
	if requestHost != "" {
		return requestHost
	}
	return instanceDialHost(listenAddr)
}
