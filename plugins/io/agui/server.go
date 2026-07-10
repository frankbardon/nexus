package agui

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"

	"github.com/frankbardon/nexus/pkg/agui"
)

// agentPath is the POST route that accepts a RunAgentInput and responds with an
// AG-UI SSE stream.
const agentPath = "/agui"

// serverConfig carries the resolved settings for the embedded HTTP server.
type serverConfig struct {
	addr        string
	bearerToken string
	corsOrigins []string
	logger      *slog.Logger
}

// Server is the embedded AG-UI HTTP server. It owns an *http.Server bound to a
// loopback address by default, enforces optional bearer auth, and answers CORS
// preflight for browser AG-UI clients.
type Server struct {
	cfg     serverConfig
	server  *http.Server
	corsSet map[string]struct{}
	corsAny bool
}

// NewServer builds a Server from cfg. The socket is not bound until Start.
func NewServer(cfg serverConfig) *Server {
	s := &Server{cfg: cfg, corsSet: make(map[string]struct{})}
	for _, o := range cfg.corsOrigins {
		if o == "*" {
			s.corsAny = true
		}
		s.corsSet[o] = struct{}{}
	}
	return s
}

// Start binds the listener and serves in a background goroutine.
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+agentPath, s.handleRunAgent)
	mux.HandleFunc("OPTIONS "+agentPath, s.handlePreflight)

	s.server = &http.Server{
		Addr:    s.cfg.addr,
		Handler: mux,
	}

	ln, err := net.Listen("tcp", s.cfg.addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.cfg.addr, err)
	}

	s.cfg.logger.Info("agui server started", "addr", "http://"+s.cfg.addr+agentPath)

	go func() {
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.cfg.logger.Error("agui server error", "error", err)
		}
	}()
	return nil
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

// handlePreflight answers CORS preflight requests for browser AG-UI clients.
func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.WriteHeader(http.StatusNoContent)
}

// handleRunAgent parses a RunAgentInput and (for now) streams a not-yet-wired
// RunError. The bus <-> SSE mapping is wired in a later story; this handler is
// the seam for that work.
func (s *Server) handleRunAgent(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)

	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "reading request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	input, err := agui.DecodeRunAgentInput(body)
	if err != nil {
		http.Error(w, "invalid RunAgentInput: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Begin the SSE response.
	agui.WriteHeaders(w.Header())
	w.WriteHeader(http.StatusOK)
	sse := agui.NewSSEWriter(w)

	// TODO(E1-S3): translate the RunAgentInput into an io.input on the bus,
	// bridge the resulting bus events (io.output, llm.stream.chunk/end,
	// tool.call/result, thinking.step, hitl.*, agent.turn.*) into AG-UI events
	// on this SSE stream, and end with RunFinished. Until then, acknowledge the
	// run start and emit a RunError so conformance clients get a well-formed,
	// terminal stream rather than a hung connection.
	_ = sse.Write(agui.NewRunStarted(input.ThreadID, input.RunID))
	_ = sse.Write(agui.NewRunError("agui serve transport not yet wired (E1-S3)"))

	s.cfg.logger.Debug("agui run received (transport not yet wired)",
		"thread_id", input.ThreadID,
		"run_id", input.RunID,
		"messages", len(input.Messages),
	)
}

// authorized reports whether the request satisfies bearer auth. When no token
// is configured, all requests are permitted.
func (s *Server) authorized(r *http.Request) bool {
	if s.cfg.bearerToken == "" {
		return true
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return false
	}
	return h[len(prefix):] == s.cfg.bearerToken
}

// applyCORS sets Access-Control-Allow-Origin when the request's Origin is
// allowed. A configured "*" echoes any origin; an explicit list only echoes
// matching origins. With no configured origins, no CORS header is set (same-
// origin only), which is the safe default for a loopback listener.
func (s *Server) applyCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" || len(s.corsSet) == 0 {
		return
	}
	if s.corsAny {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		return
	}
	if _, ok := s.corsSet[origin]; ok {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
}
