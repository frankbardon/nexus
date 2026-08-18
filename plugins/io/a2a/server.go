package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// codeAuthDenied is the JSON-RPC error code this listener uses for
// authentication and authorization refusals.
//
// A2A defines no auth error in its taxonomy: specification section 3.3.2 lists
// "HTTP 401 Unauthorized, gRPC UNAUTHENTICATED, JSON-RPC custom error", leaving
// the JSON-RPC code to the implementation. -32000 is the one code in JSON-RPC
// 2.0's implementation-defined server-error range (-32000..-32099) that A2A does
// NOT claim for itself — A2A reserves -32001..-32099 — so it is the only value
// that can be used here without colliding with a protocol error a client
// already knows how to interpret.
//
// The HTTP status remains the authoritative signal, since that is what section
// 3.3.2 actually points clients at; the JSON-RPC body exists so a client that
// only ever parses a response envelope still gets a well-formed one.
const codeAuthDenied = -32000

// errorDomain is the google.rpc.ErrorInfo domain for refusals this plugin
// originates. It is deliberately NOT a2a.ErrorDomain: these are not A2A protocol
// errors, and stamping them with the protocol's domain would tell a client to
// look for a reason in a registry that does not define it.
const errorDomain = "nexus.io.a2a"

// ErrorInfo reasons for the refusals this listener originates.
const (
	reasonAuthRequired      = "AUTHENTICATION_REQUIRED"
	reasonInvalidCredential = "INVALID_CREDENTIAL"
	reasonInsufficientScope = "INSUFFICIENT_SCOPE"
	reasonAuthUnavailable   = "AUTHENTICATION_UNAVAILABLE"
	reasonNotImplemented    = "OPERATION_NOT_IMPLEMENTED"
)

// authRealm is the RFC 7235 realm this listener challenges with.
const authRealm = "nexus-a2a"

// agentBridge is the seam between the HTTP server and the plugin's bus wiring
// and task store. It is satisfied by *Plugin.
//
// Every read method takes the authenticated Principal as its FIRST parameter,
// and none of them offers an unscoped variant. That is not a style choice: it is
// how the "another principal's task is indistinguishable from unknown" rule is
// kept true at the seam as well as inside the store. A handler cannot ask this
// interface a question that spans principals, because no such question can be
// phrased.
type agentBridge interface {
	// startTurn binds the context, registers the active run, emits io.input and
	// returns the run, the requesting client's attached stream, and the task
	// snapshot that stream must open on.
	startTurn(in turnInput, caller nexusauth.Principal, opts streamOptions) (*run, *stream, a2a.Task, *a2a.Error)
	// resumeTurn routes a message naming a task onto the question that task is
	// parked on, and returns the SAME run with a fresh stream attached. It
	// starts no turn: a resumed task continues the one that asked. Its snapshot
	// is the state the task was in when the stream attached — INPUT_REQUIRED —
	// so the transitions the answer causes arrive as updates rather than being
	// folded into the opening frame.
	resumeTurn(in turnInput, caller nexusauth.Principal, opts streamOptions) (*run, *stream, a2a.Task, *a2a.Error)
	// cancelTask settles one of the caller's tasks at CANCELED and returns the
	// stored record. An already-terminal task is refused, not rewritten.
	cancelTask(caller nexusauth.Principal, taskID string) (a2a.Task, *a2a.Error)
	// lookupTask reads one of the caller's own tasks. A task belonging to
	// anybody else is reported absent.
	lookupTask(caller nexusauth.Principal, taskID string) (taskRecord, bool, error)
	// queryTasks reads one page of the caller's own tasks, with the total number
	// of matches and the cursor for the next page.
	queryTasks(caller nexusauth.Principal, q taskQuery) ([]taskRecord, listCursor, int, error)
	// liveRun returns the in-flight run for a task id, or nil. Callers must have
	// resolved the id through lookupTask first — see the method's own comment.
	liveRun(taskID string) *run
}

// serverConfig carries the resolved settings for the embedded HTTP server.
type serverConfig struct {
	cfg    *config
	card   *servedCard
	logger *slog.Logger
	// bridge drives the bus and reads the task store. A Server built without one
	// answers every operation that needs either with an internal error rather
	// than panicking, which is what a directly-constructed Server in a
	// transport-only test gets.
	bridge agentBridge
}

// Server is the embedded A2A HTTP server. It owns an *http.Server bound to a
// loopback address by default, authenticates requests through a nexusauth
// validator chain, and mounts the discovery document alongside both HTTP
// bindings.
type Server struct {
	cfg     serverConfig
	server  *http.Server
	corsSet map[string]struct{}
	corsAny bool

	// chain is the validator chain requests are authenticated against. A nil or
	// empty chain means auth is not configured and every request is permitted,
	// which is safe only because the listener binds loopback by default.
	chain *nexusauth.Chain
}

// NewServer builds a Server from cfg. The socket is not bound until Start.
func NewServer(cfg serverConfig) *Server {
	s := &Server{cfg: cfg, corsSet: make(map[string]struct{})}
	for _, o := range cfg.cfg.corsOrigins {
		if o == "*" {
			s.corsAny = true
		}
		s.corsSet[o] = struct{}{}
	}
	if cfg.cfg.chain.Enabled() {
		s.chain = cfg.cfg.chain
	}
	return s
}

// Handler builds the request multiplexer. It is exported through Start, and
// split out so tests can exercise routing without binding a socket.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Discovery. GET and HEAD only: the card is a document, not an operation.
	mux.HandleFunc("GET "+a2a.AgentCardPath, s.handleAgentCard)
	mux.HandleFunc("HEAD "+a2a.AgentCardPath, s.handleAgentCard)
	mux.HandleFunc("OPTIONS "+a2a.AgentCardPath, s.handlePreflight)

	// JSON-RPC binding: a single POST endpoint.
	mux.HandleFunc("POST "+s.cfg.cfg.jsonrpcPath, s.handleJSONRPC)
	mux.HandleFunc("OPTIONS "+s.cfg.cfg.jsonrpcPath, s.handlePreflight)

	// REST binding: a subtree, because the operation is encoded in the path and
	// A2A's custom verbs ("/tasks/{id}:cancel") cannot be expressed as ServeMux
	// wildcards. a2a.MatchRoute resolves the suffix.
	mux.HandleFunc(s.cfg.cfg.restPrefix+"/", s.handleREST)

	return mux
}

// Start binds the listener and serves in a background goroutine.
func (s *Server) Start() error {
	s.server = &http.Server{
		Addr:    s.cfg.cfg.bindAddr,
		Handler: s.Handler(),
	}

	ln, err := net.Listen("tcp", s.cfg.cfg.bindAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.cfg.cfg.bindAddr, err)
	}

	s.cfg.logger.Info("a2a server started",
		"agent_card", "http://"+s.cfg.cfg.bindAddr+a2a.AgentCardPath,
		"jsonrpc", "http://"+s.cfg.cfg.bindAddr+s.cfg.cfg.jsonrpcPath,
		"rest", "http://"+s.cfg.cfg.bindAddr+s.cfg.cfg.restPrefix,
	)

	go func() {
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.cfg.logger.Error("a2a server error", "error", err)
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

// ---- Discovery ----

// handleAgentCard serves the Agent Card.
//
// THE AUTH POSTURE, stated explicitly because the story requires a decision
// rather than an inherited default:
//
// The card is UNAUTHENTICATED by default. Section 8.2 makes the well-known URI a
// pre-authentication bootstrap step — a client fetches the card precisely to
// discover which credentials to obtain (section 7.3, step 1) — so gating it
// behind those same credentials is circular and breaks every conforming client.
// The specification's answer for a card that must stay private is a separate
// authenticated document behind GetExtendedAgentCard (section 6.9), which this
// plugin does not implement and honestly declares as false.
//
// The counter-argument is real and is why the key exists: this card names a
// private agent. Its description, skills and examples are hand-authored and may
// describe capability an operator would rather not publish. Two things answer
// it. First, the listener binds loopback by default, so the "public" document is
// not reachable from anywhere the operator did not deliberately open. Second,
// the card's contents are hand-authored for exactly this reason — nothing is
// derived from the tool catalog or the skills plugin, so what the card reveals
// is what an operator chose to reveal.
//
// An operator who moves bind off loopback and still needs the card private sets
// card_requires_auth: true and distributes the document out-of-band, which
// section 8.2 explicitly sanctions ("Direct Configuration"). That is a real
// trade — it makes the agent undiscoverable to clients that have not already
// been told about it — so it is opt-in, not the default.
func (s *Server) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)

	if s.cfg.cfg.cardRequiresAuth {
		if _, err := s.authorize(r); err != nil {
			// A plain HTTP refusal, not an A2A envelope: the card is a
			// discovery document fetched before any A2A negotiation, so there
			// is no binding whose error shape it should be consistent with.
			s.writeAuthChallenge(w, r, err)
			return
		}
	}

	card := s.cfg.card
	w.Header().Set("Content-Type", a2a.ContentTypeAgentCard)
	w.Header().Set("ETag", card.etag)
	// Section 8.6.1 asks for a Cache-Control max-age matching the expected
	// update frequency. The card is fixed at boot for the life of the process,
	// so five minutes is a compromise: long enough to spare a busy client, short
	// enough that a restart with new config propagates without operator action.
	w.Header().Set("Cache-Control", "public, max-age=300")

	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, card.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(card.body); err != nil {
		s.cfg.logger.Debug("a2a agent card write failed", "error", err)
	}
}

// etagMatches implements the If-None-Match comparison for the one validator
// this endpoint issues: a comma-separated list, "*", and the weak prefix.
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag {
			return true
		}
		if strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

// ---- JSON-RPC binding ----

// handleJSONRPC decodes a JSON-RPC 2.0 request for an A2A operation and answers
// it. Every stage that can refuse does so with a JSON-RPC error envelope
// carrying the id from the originating request wherever it could be recovered.
func (s *Server) handleJSONRPC(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)
	w.Header().Set(a2a.HeaderVersion, a2a.ProtocolVersion)

	// Auth first: a caller who cannot be identified gets no signal about what
	// the endpoint would have accepted. The resolved Principal is carried
	// forward rather than discarded: it is what a created task is filed under,
	// and what scopes every later read of it.
	caller, err := s.authorize(r)
	if err != nil {
		s.denyJSONRPC(w, r, err)
		return
	}

	// Version next, before the body is read: 1.0 and 0.3 disagree about the
	// shape of what is in it, so parsing before negotiating would be guessing.
	params, protoErr := s.serviceParams(r)
	if protoErr != nil {
		s.writeJSONRPCError(w, nil, protoErr)
		return
	}
	opts := s.activateExtensions(w, params)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeJSONRPCError(w, nil, a2a.Errorf(a2a.ErrorTypeInternal, "reading request body: %v", err))
		return
	}

	call, protoErr := a2a.DecodeCall(body)
	if protoErr != nil {
		// DecodeRequest returns the envelope even on failure so the id can be
		// echoed; recover it on a best-effort basis for the same reason.
		var id json.RawMessage
		if req, _ := a2a.DecodeRequest(body); req != nil {
			id = req.ID
		}
		s.writeJSONRPCError(w, id, protoErr)
		return
	}

	if !operationImplemented(call.Method) {
		s.writeJSONRPCError(w, call.ID(), notImplemented(call.Method))
		return
	}

	bind := binding{jsonrpc: true, id: call.ID()}
	switch call.Method {
	case a2a.MethodSendMessage, a2a.MethodSendStreamingMessage:
		req, ok := call.Params.(*a2a.SendMessageRequest)
		if !ok {
			s.writeError(w, bind, errWrongParams(call.Method, call.Params))
			return
		}
		s.handleSendMessage(w, r, caller, bind, req, call.Streaming(), opts)
		return

	case a2a.MethodGetTask:
		req, ok := call.Params.(*a2a.GetTaskRequest)
		if !ok {
			s.writeError(w, bind, errWrongParams(call.Method, call.Params))
			return
		}
		s.handleGetTask(w, caller, bind, req)
		return

	case a2a.MethodListTasks:
		req, ok := call.Params.(*a2a.ListTasksRequest)
		if !ok {
			s.writeError(w, bind, errWrongParams(call.Method, call.Params))
			return
		}
		s.handleListTasks(w, caller, bind, req)
		return

	case a2a.MethodSubscribeToTask:
		req, ok := call.Params.(*a2a.SubscribeToTaskRequest)
		if !ok {
			s.writeError(w, bind, errWrongParams(call.Method, call.Params))
			return
		}
		s.handleSubscribeToTask(r.Context(), w, bind, caller, req, opts)
		return

	case a2a.MethodCancelTask:
		req, ok := call.Params.(*a2a.CancelTaskRequest)
		if !ok {
			s.writeError(w, bind, errWrongParams(call.Method, call.Params))
			return
		}
		s.handleCancelTask(w, caller, bind, req)
		return
	}

	// Unreachable while implementedOperations and this switch agree. It is
	// written as an internal error rather than a panic so that adding an entry
	// to the map without adding a handler fails one request instead of the
	// process.
	s.writeJSONRPCError(w, call.ID(),
		a2a.Errorf(a2a.ErrorTypeInternal, "operation %q is marked implemented but has no handler", call.Method))
}

// writeJSONRPCError renders an A2A error as a JSON-RPC error response with the
// binding's HTTP status.
func (s *Server) writeJSONRPCError(w http.ResponseWriter, id json.RawMessage, protoErr *a2a.Error) {
	resp := a2a.NewErrorResponse(id, protoErr)
	data, err := resp.Encode()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", a2a.ContentTypeJSON)
	// The JSON-RPC binding carries its outcome in the body; section 9 does not
	// map A2A error codes onto HTTP statuses the way the REST binding does, and
	// a transport-level non-200 would tell an intermediary the request failed
	// when it was answered. 200 with an error object is the JSON-RPC contract.
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		s.cfg.logger.Debug("a2a jsonrpc write failed", "error", err)
	}
}

// ---- REST binding ----

// handleREST resolves an HTTP+JSON request to an A2A operation and answers it.
func (s *Server) handleREST(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)
	w.Header().Set(a2a.HeaderVersion, a2a.ProtocolVersion)

	if r.Method == http.MethodOptions {
		s.handlePreflight(w, r)
		return
	}

	caller, err := s.authorize(r)
	if err != nil {
		s.denyREST(w, r, err)
		return
	}

	params, protoErr := s.serviceParams(r)
	if protoErr != nil {
		s.writeRESTError(w, protoErr)
		return
	}
	opts := s.activateExtensions(w, params)

	suffix := strings.TrimPrefix(r.URL.Path, s.cfg.cfg.restPrefix)
	if suffix == "" {
		suffix = "/"
	}
	route, vars, found, methodMismatch := a2a.MatchRoute(r.Method, suffix)
	switch {
	case !found && methodMismatch:
		// The path names a real operation but the verb is wrong. 405 with the
		// standard Allow header, rather than a 404 that would send a client
		// looking for a typo in a URL that is correct.
		if allowed, ok := a2a.RouteFor(routeOperationForPath(suffix)); ok {
			w.Header().Set("Allow", allowed.HTTPMethod+", OPTIONS")
		}
		s.writeRESTErrorStatus(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
			fmt.Sprintf("%s is not allowed on %s", r.Method, r.URL.Path), "")
		return
	case !found:
		s.writeRESTError(w, a2a.Errorf(a2a.ErrorTypeMethodNotFound, "no A2A operation is mounted at %s", r.URL.Path))
		return
	}

	if !operationImplemented(route.Operation) {
		s.writeRESTError(w, notImplemented(route.Operation))
		return
	}

	switch route.Operation {
	case a2a.MethodSendMessage, a2a.MethodSendStreamingMessage:
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			s.writeRESTError(w, a2a.Errorf(a2a.ErrorTypeInternal, "reading request body: %v", readErr))
			return
		}
		req, decodeErr := a2a.DecodeSendMessageRequest(body)
		if decodeErr != nil {
			s.writeRESTError(w, decodeErr)
			return
		}
		s.handleSendMessage(w, r, caller, binding{}, req, route.Streaming, opts)
		return

	case a2a.MethodGetTask:
		// Section 11.5: a GET carries its parameters in the path and the query
		// string, named exactly as the JSON body would name them, so the two
		// bindings decode into the same request object.
		req, protoErr := a2a.ParseGetTaskQuery(vars["id"], r.URL.Query())
		if protoErr != nil {
			s.writeRESTError(w, protoErr)
			return
		}
		s.handleGetTask(w, caller, binding{}, req)
		return

	case a2a.MethodListTasks:
		req, protoErr := a2a.ParseListTasksQuery(r.URL.Query())
		if protoErr != nil {
			s.writeRESTError(w, protoErr)
			return
		}
		s.handleListTasks(w, caller, binding{}, req)
		return

	case a2a.MethodSubscribeToTask:
		req := &a2a.SubscribeToTaskRequest{ID: vars["id"], Tenant: r.URL.Query().Get("tenant")}
		if req.ID == "" {
			s.writeRESTError(w, a2a.ErrInvalidParams(a2a.FieldViolation{
				Field: "id", Description: "task id is required",
			}))
			return
		}
		s.handleSubscribeToTask(r.Context(), w, binding{}, caller, req, opts)
		return

	case a2a.MethodCancelTask:
		// Section 11.3 gives CancelTask a custom verb and an optional body; the
		// path id is authoritative over anything in it.
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			s.writeRESTError(w, a2a.Errorf(a2a.ErrorTypeInternal, "reading request body: %v", readErr))
			return
		}
		req, protoErr := a2a.DecodeCancelTaskRequest(vars["id"], body)
		if protoErr != nil {
			s.writeRESTError(w, protoErr)
			return
		}
		s.handleCancelTask(w, caller, binding{}, req)
		return
	}

	s.writeRESTError(w,
		a2a.Errorf(a2a.ErrorTypeInternal, "operation %q is marked implemented but has no handler", route.Operation))
}

// routeOperationForPath finds the operation a path template matches regardless
// of verb, so a 405 can name the method that would have worked.
func routeOperationForPath(suffix string) string {
	for _, m := range []string{
		http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
	} {
		if route, _, found, _ := a2a.MatchRoute(m, suffix); found {
			return route.Operation
		}
	}
	return ""
}

// writeRESTError renders an A2A error as the section 11.6 google.rpc.Status
// body with its mapped HTTP status.
func (s *Server) writeRESTError(w http.ResponseWriter, protoErr *a2a.Error) {
	status, body := protoErr.RESTError()
	s.writeRESTBody(w, status, body)
}

// writeRESTErrorStatus renders a refusal this plugin originates — one with no
// A2A error type — in the same google.rpc.Status shape, so a client parses one
// error format on this endpoint rather than two.
func (s *Server) writeRESTErrorStatus(w http.ResponseWriter, status int, grpcStatus, message, reason string) {
	body := a2a.RESTError{Error: a2a.RESTErrorStatus{
		Code:    status,
		Status:  grpcStatus,
		Message: message,
	}}
	if reason != "" {
		body.Error.Details = []a2a.ErrorDetail{{
			"@type":  a2a.TypeErrorInfo,
			"reason": reason,
			"domain": errorDomain,
		}}
	}
	s.writeRESTBody(w, status, body)
}

func (s *Server) writeRESTBody(w http.ResponseWriter, status int, body a2a.RESTError) {
	data, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", a2a.ContentTypeJSON)
	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		s.cfg.logger.Debug("a2a rest write failed", "error", err)
	}
}

// ---- Shared request handling ----

// serviceParams parses the A2A service parameters under this listener's
// absent-header policy. See config.assumedVersion for the decision.
func (s *Server) serviceParams(r *http.Request) (a2a.ServiceParams, *a2a.Error) {
	return a2a.ParseServiceParamsAssuming(r.Header, r.URL.Query(), s.cfg.cfg.assumedVersion())
}

// activateExtensions resolves the client's A2A-Extensions opt-in and echoes back
// the extensions this response actually activated.
//
// The echo is the half that matters for interop. Specification section 8.4 makes
// an extension a NEGOTIATION: a client lists what it can handle and the agent
// answers with what it will actually use, so a client asking for three
// extensions can tell which one it got. Echoing only what was activated — rather
// than reflecting the request header — is what makes that answer worth reading:
// an unknown URI in the request produces no echo, which is the agent saying "I
// do not speak that" without an error, exactly as an optional extension should.
//
// A client that asked for nothing gets no header and no telemetry, which is the
// whole of "must not be force-fed".
func (s *Server) activateExtensions(w http.ResponseWriter, params a2a.ServiceParams) streamOptions {
	opts := streamOptions{nexusExtension: params.SupportsExtension(a2a.NexusExtensionURI)}
	if opts.nexusExtension {
		w.Header().Set(a2a.HeaderExtensions, a2a.NexusExtensionURI)
	}
	return opts
}

// errWrongParams reports a decoded parameter object whose type does not match
// the operation it was decoded for. It is unreachable while pkg/a2a's decoder
// and this dispatch agree, and is written as an internal error rather than a
// type assertion panic so a disagreement costs one request instead of the
// process.
func errWrongParams(method string, params any) *a2a.Error {
	return a2a.Errorf(a2a.ErrorTypeInternal, "decoded %s params have type %T", method, params)
}

// notImplemented builds the answer an operation gets while it is wired but not
// yet driving a turn.
//
// UnsupportedOperationError is the right type rather than a placeholder:
// section 3.3.4 uses it for exactly this condition — an operation the
// specification defines that this agent does not support — and the Agent Card
// backs the claim up by advertising the matching capability as false. A client
// therefore gets a refusal it already knows how to handle, not a novel one.
func notImplemented(operation string) *a2a.Error {
	return a2a.Errorf(a2a.ErrorTypeUnsupportedOperation,
		"operation %q is not yet implemented by this agent", operation).
		WithMetadata("operation", operation).
		// A distinct key, not "reason": the ErrorInfo already carries
		// reason=UNSUPPORTED_OPERATION, and a second field by the same name
		// would read as a contradiction. This one narrows it — "not supported"
		// here means "not yet", which is what a partner integrating against
		// this agent needs to know.
		WithMetadata("detail", reasonNotImplemented)
}

// authorize resolves the caller's identity for r, or returns the chain's
// classified denial.
//
// With no chain configured it returns the zero Principal and no error: a
// listener with neither a bearer token nor an `auth:` block admits every caller,
// which is safe only because the bind address defaults to loopback. Note this is
// NOT chain.Validate's own behaviour — a disabled chain returns ErrAuthDisabled,
// because "not configured" is a deployment state the host must take a position
// on, and this host's position is "loopback-bound and open".
func (s *Server) authorize(r *http.Request) (nexusauth.Principal, error) {
	if !s.chain.Enabled() {
		return nexusauth.Principal{}, nil
	}
	return s.chain.Validate(r.Context(), r)
}

// denial classifies a nexusauth error into the transport-facing triple every
// binding renders in its own shape.
type denial struct {
	status     int
	grpcStatus string
	message    string
	reason     string
	challenge  string
}

// classifyDenial maps a nexusauth denial onto a status, a message and an RFC
// 6750 challenge.
//
// The classification comes from nexusauth.KindOf, never from matching an error
// string, so a reworded reason cannot silently reclassify a denial. The status
// mapping matches nexus.io.agui's and the broker's: the kinds ARE the package's
// transport contract, and one Nexus deployment should not answer the same
// refusal three different ways.
func classifyDenial(err error) denial {
	switch nexusauth.KindOf(err) {
	case nexusauth.KindInsufficientScope:
		return denial{
			status: http.StatusForbidden, grpcStatus: "PERMISSION_DENIED",
			message: "insufficient scope", reason: reasonInsufficientScope,
			challenge: `Bearer realm="` + authRealm + `", error="insufficient_scope"`,
		}
	case nexusauth.KindUnavailable:
		// Deliberately no challenge: a 503 is not an invitation to
		// re-authenticate, and emitting one would aim a re-auth storm at an
		// identity provider that is already down.
		return denial{
			status: http.StatusServiceUnavailable, grpcStatus: "UNAVAILABLE",
			message: "authentication temporarily unavailable", reason: reasonAuthUnavailable,
		}
	case nexusauth.KindNoCredential:
		return denial{
			status: http.StatusUnauthorized, grpcStatus: "UNAUTHENTICATED",
			message: "unauthorized", reason: reasonAuthRequired,
			challenge: `Bearer realm="` + authRealm + `"`,
		}
	default:
		// KindInvalidCredential and anything the package could not classify.
		// Failing closed on an unclassified denial is the only safe reading.
		return denial{
			status: http.StatusUnauthorized, grpcStatus: "UNAUTHENTICATED",
			message: "unauthorized", reason: reasonInvalidCredential,
			challenge: `Bearer realm="` + authRealm + `", error="invalid_token"`,
		}
	}
}

// applyDenialHeaders writes the status-independent parts of a refusal and logs
// it once, at the one place every binding funnels through.
func (s *Server) applyDenialHeaders(w http.ResponseWriter, r *http.Request, d denial, err error) {
	if d.challenge != "" {
		w.Header().Set("WWW-Authenticate", d.challenge)
	}
	if d.status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "5")
	}
	s.cfg.logger.Warn("a2a auth denied",
		"path", r.URL.Path,
		"status", d.status,
		"denial", err,
	)
}

// writeAuthChallenge answers a refusal on a non-A2A surface (the Agent Card)
// with a plain HTTP error.
func (s *Server) writeAuthChallenge(w http.ResponseWriter, r *http.Request, err error) {
	d := classifyDenial(err)
	s.applyDenialHeaders(w, r, d, err)
	http.Error(w, d.message, d.status)
}

// denyREST answers a refusal on the REST binding in the section 11.6 error
// shape, so a client parses one format for every failure on this endpoint.
func (s *Server) denyREST(w http.ResponseWriter, r *http.Request, err error) {
	d := classifyDenial(err)
	s.applyDenialHeaders(w, r, d, err)
	s.writeRESTErrorStatus(w, d.status, d.grpcStatus, d.message, d.reason)
}

// denyJSONRPC answers a refusal on the JSON-RPC binding.
//
// Unlike every other JSON-RPC response, this one carries a non-200 status. That
// is deliberate: section 3.3.2 names "HTTP 401 Unauthorized" as the expected
// signal for an authentication failure, and the RFC 6750 challenge is only
// meaningful alongside it. The body is still a well-formed JSON-RPC error
// object so a client that reads only the envelope is not left guessing.
func (s *Server) denyJSONRPC(w http.ResponseWriter, r *http.Request, err error) {
	d := classifyDenial(err)
	s.applyDenialHeaders(w, r, d, err)

	resp := a2a.Response{
		JSONRPC: a2a.JSONRPCVersion,
		ID:      json.RawMessage("null"),
		Error: &a2a.RPCError{
			Code:    codeAuthDenied,
			Message: d.message,
			Data: []a2a.ErrorDetail{{
				"@type":  a2a.TypeErrorInfo,
				"reason": d.reason,
				"domain": errorDomain,
			}},
		},
	}
	data, encErr := resp.Encode()
	if encErr != nil {
		http.Error(w, d.message, d.status)
		return
	}
	w.Header().Set("Content-Type", a2a.ContentTypeJSON)
	w.WriteHeader(d.status)
	if _, writeErr := w.Write(data); writeErr != nil {
		s.cfg.logger.Debug("a2a jsonrpc denial write failed", "error", writeErr)
	}
}

// ---- CORS ----

// handlePreflight answers CORS preflight requests.
//
// It is NOT authenticated, and must not become so: a browser never attaches
// Authorization to a preflight, so gating it would make every cross-origin
// client unable to reach an endpoint it is authorized for.
func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers",
		strings.Join([]string{"Authorization", "Content-Type", a2a.HeaderVersion, a2a.HeaderExtensions}, ", "))
	w.WriteHeader(http.StatusNoContent)
}

// applyCORS sets Access-Control-Allow-Origin when the request's Origin is
// allowed. A configured "*" echoes any origin; an explicit list only echoes
// matching origins. With no configured origins no CORS header is set at all
// (same-origin only), the safe default for a loopback listener.
func (s *Server) applyCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" || len(s.corsSet) == 0 {
		return
	}
	if _, ok := s.corsSet[origin]; !ok && !s.corsAny {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	// A2A clients read the version and any extension echo off the response, and
	// a browser cannot see either without an explicit expose list.
	w.Header().Set("Access-Control-Expose-Headers",
		strings.Join([]string{a2a.HeaderVersion, a2a.HeaderExtensions, "ETag"}, ", "))
}
