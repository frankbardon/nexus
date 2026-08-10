package agui

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// agentPath is the POST route that accepts a RunAgentInput and responds with an
// AG-UI SSE stream.
const agentPath = "/agui"

// legacyBearerPrincipal is the identity a request authenticated by the legacy
// `bearer_token` / `bearer_token_env` config acts as.
//
// The legacy keys carry no notion of who the holder is — they are one shared
// secret — but nexusauth requires a non-empty Principal.ID for a validation to
// count as a success (an empty id would compare equal to every other empty id).
// Naming the credential source rather than inventing an identity keeps the audit
// record honest: "this caller presented the configured shared token".
const legacyBearerPrincipal = "bearer_token"

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
	addr string

	// bearerToken is the legacy single-shared-secret form. Init no longer sets it
	// (it resolves a chain instead), but NewServer still desugars it so a
	// directly-constructed Server — tests, and any embedder building a Server
	// without going through the plugin — keeps behaving exactly as before.
	bearerToken string

	// chain is the resolved validator chain. When enabled it wins over
	// bearerToken; the plugin's Init makes the two mutually exclusive at config
	// level, so in production exactly one of them is ever set.
	chain *nexusauth.Chain

	corsOrigins []string
	logger      *slog.Logger
	bridge      bridge
}

// Server is the embedded AG-UI HTTP server. It owns an *http.Server bound to a
// loopback address by default, authenticates requests through a nexusauth
// validator chain, and answers CORS preflight for browser AG-UI clients.
type Server struct {
	cfg     serverConfig
	server  *http.Server
	corsSet map[string]struct{}
	corsAny bool

	// chain is the validator chain requests are authenticated against. A nil or
	// empty chain means auth is not configured and every request is permitted,
	// which is what an AG-UI listener with no token and no `auth:` block has
	// always done.
	chain *nexusauth.Chain

	// authBroken latches a chain that could not be built. It exists so the one
	// construction path that cannot return an error (NewServer desugaring
	// bearerToken) fails CLOSED rather than silently open. Unreachable in
	// practice — the only failures NewStaticValidator has are an empty token and
	// an empty principal, and both are excluded here — but "unreachable" is not a
	// reason to discard an error on an authentication path.
	authBroken bool
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

	switch {
	case cfg.chain.Enabled():
		s.chain = cfg.chain
	case cfg.bearerToken != "":
		chain, err := staticChainFromToken(cfg.bearerToken)
		if err != nil {
			s.authBroken = true
			if cfg.logger != nil {
				cfg.logger.Error("agui bearer token could not be compiled into a validator; refusing every request", "error", err)
			}
			break
		}
		s.chain = chain
	}
	return s
}

// staticChainFromToken desugars the legacy `bearer_token` / `bearer_token_env`
// configuration into a one-entry static validator chain.
//
// This is the whole of the backward-compatibility story: the two keys keep their
// exact meaning and precedence, and are simply expressed in terms of the shared
// identity layer instead of a bespoke string comparison. Routing them through
// StaticValidator also upgrades the comparison to constant-time, which the
// hand-rolled `==` was not.
func staticChainFromToken(token string) (*nexusauth.Chain, error) {
	v, err := nexusauth.NewStaticValidator([]nexusauth.StaticToken{{
		Token:     token,
		Principal: nexusauth.Principal{ID: legacyBearerPrincipal},
	}})
	if err != nil {
		return nil, fmt.Errorf("desugaring bearer_token into a static validator: %w", err)
	}
	return nexusauth.NewChain(nexusauth.NamedValidator{
		Name:      nexusauth.ValidatorTypeStatic,
		Validator: v,
	}), nil
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
//
// It is NOT authenticated, and must not become so: a browser never attaches
// Authorization to a preflight, so gating it would make every cross-origin
// AG-UI client unable to reach the endpoint it is authorized for. POST /agui
// remains the one authenticated surface, exactly as before.
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

	// CORS headers are applied BEFORE the auth check on purpose: a browser can
	// only read a 401 that carries them, so denying without CORS would surface to
	// a browser AG-UI client as an opaque network error instead of "your token
	// was refused".
	principal, err := s.authorize(r)
	if err != nil {
		s.denyAuth(w, r, err)
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
		state:    input.State,
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

	// principal_id is empty when auth is disabled. It is recorded here and
	// NOWHERE ELSE, because AG-UI has no per-identity behaviour to key on yet:
	// this transport serves a single engine/session per listener and admits one
	// run at a time, so there is no second principal for an authorization
	// decision to distinguish. The identity is deliberately carried this far and
	// logged rather than dropped at the guard — the run record is what an
	// operator correlates a turn back to a caller with, and it is the seam a
	// future per-principal policy (thread ownership, per-tenant sessions) would
	// extend rather than re-derive.
	s.cfg.logger.Debug("agui run started",
		"thread_id", input.ThreadID,
		"run_id", input.RunID,
		"messages", len(input.Messages),
		"principal_id", principal.ID,
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

// authorize resolves the caller's identity for r, or returns the chain's
// classified denial.
//
// With no chain configured it returns the zero Principal and no error: an AG-UI
// listener with neither a bearer token nor an `auth:` block has always admitted
// every caller, and the disabled path must keep doing exactly that. Note that
// this is NOT chain.Validate's own behaviour — a disabled chain returns
// ErrAuthDisabled, because "not configured" is a deployment state the host has
// to take a position on, and this host's position is "loopback-bound and open".
func (s *Server) authorize(r *http.Request) (nexusauth.Principal, error) {
	if s.authBroken {
		return nexusauth.Principal{}, nexusauth.NewError(nexusauth.KindUnavailable,
			"authentication is misconfigured", nil)
	}
	if !s.chain.Enabled() {
		return nexusauth.Principal{}, nil
	}
	return s.chain.Validate(r.Context(), r)
}

// denyAuth maps a nexusauth denial onto a status code and writes the refusal.
//
// The classification comes from nexusauth.KindOf, never from matching the error
// string, so a reworded reason cannot silently reclassify a denial.
//
// The mapping matches the broker's — the kinds ARE the package's transport
// contract, and one Nexus deployment should not answer the same refusal two
// different ways — but the response body deliberately does not. The broker
// speaks JSON on every route; this endpoint's success body is an SSE stream, so
// there is no JSON envelope for an error to be consistent with, and the 401 body
// stays the plain "unauthorized" it has always been.
//
// The RFC 6750 challenge is kept, and matters MORE here than on the broker: an
// AG-UI client is frequently a browser front-end whose only structured signal is
// the status plus the challenge, and `error="invalid_token"` is what lets it
// tell "sign in" from "your session expired, refresh the token" without parsing
// prose. Retry-After is kept for the same reason it exists on the broker — a 503
// from an unreachable identity provider must not read as "re-authenticate", or
// every connected client aims a re-auth storm at a provider that is already down.
func (s *Server) denyAuth(w http.ResponseWriter, r *http.Request, err error) {
	kind := nexusauth.KindOf(err)

	status := http.StatusUnauthorized
	// Unchanged from the hand-rolled check, so a client string-matching the 401
	// body sees what it always saw.
	message := "unauthorized"
	switch kind {
	case nexusauth.KindInsufficientScope:
		status = http.StatusForbidden
		message = "insufficient scope"
	case nexusauth.KindUnavailable:
		status = http.StatusServiceUnavailable
		message = "authentication temporarily unavailable"
	default:
		// KindNoCredential, KindInvalidCredential, and anything the package could
		// not classify. Failing closed on an unclassified denial is the only safe
		// reading.
	}

	s.cfg.logger.Warn("agui auth denied",
		"path", r.URL.Path,
		"status", status,
		"denial", err,
	)

	switch status {
	case http.StatusUnauthorized:
		if kind == nexusauth.KindNoCredential {
			w.Header().Set("WWW-Authenticate", `Bearer realm="nexus-agui"`)
		} else {
			w.Header().Set("WWW-Authenticate", `Bearer realm="nexus-agui", error="invalid_token"`)
		}
	case http.StatusForbidden:
		w.Header().Set("WWW-Authenticate", `Bearer realm="nexus-agui", error="insufficient_scope"`)
	case http.StatusServiceUnavailable:
		// Deliberately no WWW-Authenticate: a 503 is not a challenge, and emitting
		// one would invite exactly the re-authentication this status exists to
		// avoid.
		w.Header().Set("Retry-After", "5")
	}

	http.Error(w, message, status)
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
