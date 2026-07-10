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

// bridge is the seam between the HTTP server and the plugin's bus wiring. The
// server hands a decoded RunAgentInput to startRun (which registers the active
// run and emits io.input) and calls endRun when the SSE stream terminates. For
// a continuation RunAgentInput carrying resume[], it calls resumeRun instead,
// which validates the resume against the pending interrupts, emits the matching
// hitl.responded events to unblock the in-process agent, and registers a fresh
// run for the continuation stream. It is satisfied by *Plugin.
type bridge interface {
	startRun(input runInput) (*run, bool)
	resumeRun(input runInput) (*run, error)
	endRun(r *run)
}

// serverConfig carries the resolved settings for the embedded HTTP server.
type serverConfig struct {
	addr        string
	bearerToken string
	corsOrigins []string
	logger      *slog.Logger
	bridge      bridge
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

// handleRunAgent decodes a RunAgentInput, drives one Nexus agent turn via the
// bus, and streams the resulting bus events back as canonical AG-UI SSE. It
// registers the active run (emitting io.input), then drains the run's channel —
// the single source of translated AG-UI events — writing each to the SSE
// stream and flushing incrementally until RunFinished/RunError terminates it.
//
// This goroutine is the sole writer to the SSE stream; bus handlers only push
// onto the run channel, so the SSEWriter is never touched concurrently.
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

	if s.cfg.bridge == nil {
		// Defensive: a server built without a bridge cannot drive the bus.
		_ = sse.Write(agui.NewRunStarted(input.ThreadID, input.RunID))
		_ = sse.Write(agui.NewRunError("agui serve transport not wired"))
		return
	}

	in := runInput{
		threadID: input.ThreadID,
		runID:    input.RunID,
		messages: input.Messages,
		resume:   input.Resume,
		tools:    input.Tools,
	}

	var run *run
	if len(in.resume) > 0 {
		// Continuation of an interrupted run: resolve the pending interrupts
		// (emitting hitl.responded to unblock the in-process agent) and register
		// a fresh run for the continuation stream. A validation failure (unknown/
		// expired interrupt, or a partial resume) yields a clean terminal stream.
		var err error
		run, err = s.cfg.bridge.resumeRun(in)
		if err != nil {
			_ = sse.Write(agui.NewRunStarted(input.ThreadID, input.RunID))
			_ = sse.Write(agui.NewRunError(err.Error()))
			return
		}
	} else {
		var ok bool
		run, ok = s.cfg.bridge.startRun(in)
		if !ok {
			// Another run is already in flight for this listener (one run per
			// listener for this scope). Return a well-formed terminal stream.
			_ = sse.Write(agui.NewRunStarted(input.ThreadID, input.RunID))
			_ = sse.Write(agui.NewRunError("a run is already in flight on this listener"))
			return
		}
	}
	defer s.cfg.bridge.endRun(run)

	s.cfg.logger.Debug("agui run started",
		"thread_id", input.ThreadID,
		"run_id", input.RunID,
		"messages", len(input.Messages),
	)

	// Client disconnect: fail the run so the drain loop stops promptly and the
	// active-run slot is released for the next request.
	ctx := r.Context()
	go func() {
		<-ctx.Done()
		run.fail("client disconnected")
	}()

	// Drain translated AG-UI events until the terminal event closes the run.
	for {
		select {
		case ev := <-run.out:
			if err := sse.Write(ev); err != nil {
				s.cfg.logger.Debug("agui sse write failed; ending run", "error", err)
				run.fail("sse write failed")
				return
			}
			if isTerminal(ev) {
				// Drain any events queued before the terminal one, then stop.
				drain(sse, run)
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// drain flushes any events already queued after a terminal event was observed,
// then returns. It never blocks: it stops as soon as the channel is empty.
func drain(sse *agui.SSEWriter, run *run) {
	for {
		select {
		case ev := <-run.out:
			_ = sse.Write(ev)
		default:
			return
		}
	}
}

// isTerminal reports whether an AG-UI event ends the SSE stream.
func isTerminal(e agui.Event) bool {
	switch e.EventType() {
	case agui.EventRunFinished, agui.EventRunError:
		return true
	default:
		return false
	}
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
