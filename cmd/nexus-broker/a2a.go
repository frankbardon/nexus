package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
)

// The per-profile route shape. Every A2A route this broker serves lives under
// /agents/<profile>/, which is what makes profiles unable to collide: two
// profiles never share a path, and none of them can shadow an existing broker
// route, because no control-plane route starts with /agents/.
//
// The suffixes mirror nexus.io.a2a's defaults (`/a2a` for JSON-RPC, `/a2a/v1`
// for REST) so a client — or a person reading a card — sees one Nexus URL shape
// whether it is talking to a standalone instance or to the broker.
//
// The card sits at the profile's own /.well-known/agent-card.json rather than
// the origin's. Specification section 8.2 scopes the well-known URI to an
// origin, which can name exactly one agent; a broker fronts several, so the
// canonical filename is published per profile and each card advertises its own
// absolute URLs. A client that was handed a profile's card URL — section 8.2's
// "Direct Configuration" — needs nothing else.
const (
	agentRoutePrefix    = "/agents/"
	agentJSONRPCSuffix  = "/a2a"
	agentRESTSuffix     = agentJSONRPCSuffix + "/v1"
	agentProfileWildEnc = "{profile}"
)

// The registered ServeMux patterns. There is one registration per route
// REGARDLESS of how many profiles exist: the profile is a path wildcard the
// handler resolves, not a literal baked into a pattern.
//
// That matters for two reasons beyond tidiness. An unknown profile becomes a
// refusal this package writes — in the binding's own error shape — instead of
// the mux's bare 404 with an HTML body, which an A2A client cannot parse. And
// the auth guard's audit records label a route by its matched PATTERN
// (routeLabel), so the label stays bounded no matter how many profiles an
// operator configures.
const (
	agentCardPattern    = agentRoutePrefix + agentProfileWildEnc + a2a.AgentCardPath
	agentJSONRPCPattern = agentRoutePrefix + agentProfileWildEnc + agentJSONRPCSuffix
	agentRESTPattern    = agentRoutePrefix + agentProfileWildEnc + agentRESTSuffix + "/{rest...}"
)

// maxA2ABody caps an A2A request body, matching the `max_claim_body` default. A
// JSON-RPC envelope carrying a message is far smaller than a claim's inline
// config, so this is a backstop against an unbounded read rather than a working
// limit — which is why it stays a constant while max_claim_body, the bound on a
// body that carries a whole nexus config, became a config key.
const maxA2ABody = 1 << 20 // 1 MiB

// a2aErrorDomain is the google.rpc.ErrorInfo domain for refusals the broker's
// A2A ingress originates. It is deliberately NOT a2a.ErrorDomain: these are not
// A2A protocol errors, and stamping them with the protocol's domain would tell
// a client to look for a reason in a registry that does not define it.
const a2aErrorDomain = "nexus.broker.a2a"

// a2aReasonNotImplemented narrows the protocol's UNSUPPORTED_OPERATION reason to
// what it actually means here: not yet, rather than never.
const a2aReasonNotImplemented = "OPERATION_NOT_IMPLEMENTED"

// brokerImplementedOperations is the set of A2A operations this ingress drives
// end to end.
//
// It is a map rather than a scattering of `if` statements so that the story
// which implements an operation flips exactly one place and the Agent Card
// follows on its own — the card and the dispatch cannot drift, because they
// read the same value (see buildAgentCard).
//
// WHAT IS IN IT. The three message operations: a client's message becomes the
// `input` payload a leased instance's nexus.io.broker plugin turns into
// io.input, everything the instance sends back is translated into A2A frames
// (see a2atask.go), and CancelTask settles a live task and tells the instance.
// Both the blocking and the streaming shapes of SendMessage are driven by the
// same translation, so they cannot report different outcomes for one turn.
//
// The three task reads: GetTask, ListTasks and SubscribeToTask are served from
// the broker's durable task store (a2ataskstore.go), which is why they can be
// answered at all — the record outlives the instance that produced it and the
// broker process that started it, which is precisely when a client needs them.
// Every one of them is scoped to the authenticated principal.
//
// WHAT IS NOT, and why the absences are honest rather than arbitrary:
//
//   - The push-notification operations are unbuilt: the broker has no outbound
//     delivery path, and the card declares capabilities.pushNotifications false.
//   - GetExtendedAgentCard is unbuilt: a profile publishes one card, and there is
//     no second, authenticated document to hand out.
var brokerImplementedOperations = map[string]bool{
	a2a.MethodSendMessage:          true,
	a2a.MethodSendStreamingMessage: true,
	a2a.MethodCancelTask:           true,
	a2a.MethodGetTask:              true,
	a2a.MethodListTasks:            true,
	a2a.MethodSubscribeToTask:      true,
}

// brokerOperationImplemented reports whether the ingress drives an operation.
func brokerOperationImplemented(operation string) bool {
	return brokerImplementedOperations[operation]
}

// agentBasePath is the route namespace one profile owns.
func agentBasePath(profile string) string { return agentRoutePrefix + profile }

// agentCardPath is where one profile's Agent Card is served.
func agentCardPath(profile string) string { return agentBasePath(profile) + a2a.AgentCardPath }

// agentJSONRPCPath is one profile's JSON-RPC endpoint.
func agentJSONRPCPath(profile string) string { return agentBasePath(profile) + agentJSONRPCSuffix }

// agentRESTPrefix is the root of one profile's HTTP+JSON (REST) binding.
func agentRESTPrefix(profile string) string { return agentBasePath(profile) + agentRESTSuffix }

// A2AServer serves the broker's A2A ingress: one Agent Card, one JSON-RPC
// endpoint and one REST binding per configured `agents:` profile.
//
// It holds no per-request state and is safe for concurrent use. The cards are
// rendered once at construction, because the profile map is immutable after
// LoadConfig — there is no reload path — so there is nothing to recompute.
//
// It does NOT authenticate. Every route is registered through the broker's
// existing guard (see run()), which means an A2A caller is refused by exactly
// the middleware that refuses a /claim caller, with the broker's standard
// {"error": "..."} envelope and the same WWW-Authenticate challenge. Putting a
// second authentication path here would be a second place for the policy to
// drift.
type A2AServer struct {
	logger *slog.Logger

	// cards is the rendered card per profile name, and doubles as the routing
	// table: a path whose {profile} is not a key here names no agent.
	cards map[string]*servedAgentCard

	// names is the profile list in a stable order, for the boot log.
	names []string

	// profiles is the resolved `agents:` block, so a dispatched turn can hand
	// the lease provider the config and binary its profile names.
	profiles map[string]AgentProfile

	// tasks is the live-task registry: the tasks this process is driving right
	// now. A task is forgotten the moment it settles, because everything a client
	// can ask about a finished task is answered from store instead.
	tasks *a2aTasks

	// store is the DURABLE task record. It is what makes GetTask, ListTasks and
	// SubscribeToTask answerable after the instance — or the broker — that ran a
	// task has gone, and it is never nil: with no state_dir configured it is a
	// memory-only store, so the operations still answer for the life of the
	// process rather than refusing.
	store *a2aTaskStore

	// queues serializes concurrent tasks on one conversation. See a2aqueue.go.
	queues *a2aContextQueues

	// inputTimeout is how long a task may sit at INPUT_REQUIRED before it is
	// abandoned, which is also what stops a queue deadlocking behind an
	// unanswered question. Zero disables it.
	inputTimeout time.Duration

	// leases produces the instance a turn runs on. It defaults to a provider
	// that refuses everything, so an ingress built without a lifecycle answers
	// with a clear error instead of panicking on a nil interface.
	leases a2aLeaseProvider
}

// NewA2AServer renders every configured profile's Agent Card and returns the
// ingress that serves them.
//
// A Config with no `agents:` block yields a server with no profiles, which
// registers no routes at all — see Register. Building it unconditionally (rather
// than branching in run()) keeps the wiring in main uniform; the emptiness is
// handled once, here.
func NewA2AServer(logger *slog.Logger, cfg Config) (*A2AServer, error) {
	if logger == nil {
		logger = slog.Default()
	}
	s := &A2AServer{
		logger:   logger,
		profiles: cfg.Agents,
		tasks:    newA2ATasks(),
		// A memory-only store by default. run() replaces it with the durable one
		// when a state_dir is configured; building one here unconditionally means
		// the read operations never have to branch on whether a store exists.
		store:        newA2ATaskStore(logger, cfg.A2ATaskRetention),
		queues:       newA2AContextQueues(logger),
		inputTimeout: cfg.A2AInputTimeout,
		leases:       unwiredLeaseProvider{},
	}
	if len(cfg.Agents) == 0 {
		return s, nil
	}

	s.cards = make(map[string]*servedAgentCard, len(cfg.Agents))
	// Sorted rather than a map walk so a config with two unservable cards fails
	// on the same one every boot.
	s.names = sortedAgentNames(cfg.Agents)
	for _, name := range s.names {
		card, err := buildAgentCard(name, cfg.Agents[name], cfg.A2ABaseURL, cfg.AuthValidators)
		if err != nil {
			return nil, err
		}
		s.cards[name] = card
	}
	return s, nil
}

// enabled reports whether any profile is configured.
func (s *A2AServer) enabled() bool { return s != nil && len(s.cards) > 0 }

// useLeaseProvider installs the machinery that produces an agent instance for a
// turn.
//
// It is a setter rather than a constructor parameter because the provider needs
// the registry, the spawner and the gateway — all built after the cards are
// rendered — and because a test supplies a provider that never starts a
// process. Calling it more than once replaces the provider; the ingress does
// not reload, so there is no live task to reconcile.
// useTaskStore installs the durable task store.
//
// It is a setter for the same reason useLeaseProvider is: opening the store
// touches the filesystem, and run() owns the policy for what to do when that
// fails — warn and carry on with the memory-only store the constructor already
// installed, rather than refusing to boot. A nil store is ignored, so a failed
// open cannot leave the ingress without one.
func (s *A2AServer) useTaskStore(store *a2aTaskStore) {
	if store == nil {
		return
	}
	s.store = store
}

func (s *A2AServer) useLeaseProvider(p a2aLeaseProvider) {
	if p == nil {
		p = unwiredLeaseProvider{}
	}
	s.leases = p
}

// Shutdown settles every live task so no client is left holding a stream the
// broker will never write to again. A task settled this way reports FAILED with
// the reason, exactly as an instance dying mid-turn does.
func (s *A2AServer) Shutdown() {
	if s == nil || s.tasks == nil {
		return
	}
	// The queues are closed FIRST, and the order is load-bearing: settling a task
	// makes it terminal, which is the event the queue advances on, so settling
	// without this would spawn an instance for every message queued behind a task
	// the broker is in the middle of cancelling.
	if s.queues != nil {
		s.queues.stop()
	}
	s.tasks.shutdown("the broker is shutting down, so the turn was ended before it finished")
}

// Register wires the A2A routes onto a mux. It takes a routeMux so run() can
// register it behind the auth guard, exactly as POST /claim and GET /binaries
// are.
//
// With NO profiles configured it registers NOTHING. That is the mechanism
// behind "a broker with no `agents:` block behaves exactly as before": not a
// handler that answers 404, but an absence of any pattern at all, so the mux is
// byte-for-byte the mux it was before this file existed.
func (s *A2AServer) Register(mux routeMux) {
	if !s.enabled() {
		return
	}
	// GET and HEAD only for the card: it is a document, not an operation.
	mux.HandleFunc("GET "+agentCardPattern, s.handleAgentCard)
	mux.HandleFunc("HEAD "+agentCardPattern, s.handleAgentCard)

	mux.HandleFunc("POST "+agentJSONRPCPattern, s.handleJSONRPC)

	// The REST binding is a subtree because the operation is encoded in the
	// path, and A2A's custom verbs ("/tasks/{id}:cancel") cannot be expressed as
	// ServeMux wildcards — a "{id}" wildcard would swallow the verb into the id.
	// a2a.MatchRoute resolves the suffix instead. Registered for every method,
	// since the verb is part of what MatchRoute decides.
	mux.HandleFunc(agentRESTPattern, s.handleREST)
}

// logStartupState emits the boot record for the A2A ingress: one line for the
// ingress and one per profile, naming the URLs a client will actually be given.
//
// The per-profile line carries the resolved config path and binary for the same
// reason the binary registry's does: a profile is a promise about what a request
// will boot, and an operator debugging the wrong agent answering needs to see
// which file and which build this broker bound that name to.
func (s *A2AServer) logStartupState(cfg Config) {
	if !s.enabled() {
		// Silence is the point: a broker with no `agents:` block must not gain a
		// line saying it has no A2A ingress, because it never had one.
		return
	}
	s.logger.Info("a2a ingress enabled", "profiles", len(s.cards), "base_url", cfg.A2ABaseURL)
	// The retention policy is stated at boot rather than left to be discovered:
	// it decides how long a client can read a task back for, and an operator who
	// tuned it needs to see the value this process actually resolved. A state_dir
	// is called out because without one the whole store is memory-only, which is
	// the difference between "tasks survive a restart" and "they do not".
	s.logger.Info("a2a task retention",
		"state_dir", cfg.StateDir,
		"durable", cfg.StateDir != "",
		"ttl", cfg.A2ATaskRetention.ttl,
		"max_per_context", cfg.A2ATaskRetention.maxPerContext,
		"max_tasks", maxA2ATaskRecords,
		"input_timeout", cfg.A2AInputTimeout)
	// An ingress with no way to produce an instance answers every operation with
	// errLeaseUnavailable. That is a confusing failure to meet at runtime and a
	// trivial one to read at boot, so it is stated here rather than left to be
	// discovered one refused request at a time.
	if _, unwired := s.leases.(unwiredLeaseProvider); unwired {
		s.logger.Warn("a2a ingress has no agent instance provider wired: " +
			"the routes answer and the cards are served, but every message will be refused " +
			"until the broker can start a Nexus instance to run a turn on")
	}
	for _, name := range s.names {
		profile := cfg.Agents[name]
		s.logger.Info("a2a agent profile",
			"name", name,
			"binary", profile.Binary,
			"config", profile.ResolvedConfig,
			"agent_card", cfg.A2ABaseURL+agentCardPath(name),
			"jsonrpc", cfg.A2ABaseURL+agentJSONRPCPath(name),
			"rest", cfg.A2ABaseURL+agentRESTPrefix(name),
		)
	}
}

// ---- Discovery ----

// handleAgentCard serves one profile's Agent Card.
//
// THE AUTH POSTURE, stated because it deliberately differs from
// nexus.io.a2a's: this card is BEHIND the broker's auth guard, like every other
// route on this binary.
//
// The specification's argument for an unauthenticated card is real — section 8.2
// makes the well-known URI a pre-authentication bootstrap step, and a client
// fetches the card precisely to discover which credential to obtain, so gating
// it is circular. nexus.io.a2a follows that, and can afford to: it binds
// loopback by default and serves one agent an operator already chose to expose.
//
// The broker is the opposite deployment. It is an INGRESS — its whole purpose is
// to be reachable — and its standing policy is that even GET /binaries, a
// listing of nothing but names, requires a credential, because enumerating what
// a broker can run is a control-plane read. A card is strictly more than that: it
// names an agent, its provider and its skills. Publishing that to anyone who can
// reach the port would be a wider disclosure than the broker makes anywhere
// else, decided by inheritance rather than by an operator.
//
// The cost is honest and bounded: a client must be given a credential
// out-of-band before it can fetch the card, which is section 8.2's "Direct
// Configuration" path and is explicitly sanctioned. A broker with no `auth:`
// block serves the card to everyone, exactly as it serves every other route.
func (s *A2AServer) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	card, ok := s.cardFor(r)
	if !ok {
		// The card is a document fetched before any A2A negotiation, so the
		// broker's own error envelope is the right shape here — there is no
		// binding whose error format it should be consistent with.
		s.writeUnknownProfileEnvelope(w, r)
		return
	}

	w.Header().Set("Content-Type", a2a.ContentTypeAgentCard)
	w.Header().Set("ETag", card.etag)
	// Section 8.6.1 asks for a Cache-Control max-age matching the expected
	// update frequency. A card is fixed at boot for the life of the process, so
	// five minutes is the compromise: long enough to spare a busy client, short
	// enough that a restart with new config propagates without operator action.
	w.Header().Set("Cache-Control", "public, max-age=300")

	if match := r.Header.Get("If-None-Match"); match != "" && a2aETagMatches(match, card.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(card.body); err != nil {
		s.logger.Debug("a2a agent card write failed", "profile", card.profile, "error", err)
	}
}

// a2aETagMatches implements the If-None-Match comparison for the one validator
// this endpoint issues: a comma-separated list, "*", and the weak prefix.
func a2aETagMatches(header, etag string) bool {
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
// it.
//
// Authentication has already happened — this handler is only ever reached
// through the guard — so the stages here are profile, version, body, method.
// Each stage that can refuse does so with a JSON-RPC error envelope carrying the
// id from the originating request wherever it could be recovered, because a
// client correlating responses by id has no other way to attribute a failure.
func (s *A2AServer) handleJSONRPC(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(a2a.HeaderVersion, a2a.ProtocolVersion)

	card, ok := s.cardFor(r)
	if !ok {
		s.writeUnknownProfileJSONRPC(w, r)
		return
	}

	// Version before the body is read: 1.0 and 0.3 disagree about the shape of
	// what is in it, so parsing before negotiating would be guessing. The
	// assumed version for an absent header is 1.0, matching nexus.io.a2a — this
	// ingress has never served 0.3, so a header-less request is a 1.0 client
	// whose HTTP layer omitted a header, not a 0.3 client to protect.
	if _, protoErr := a2a.ParseServiceParamsAssuming(r.Header, r.URL.Query(), a2a.ProtocolVersion); protoErr != nil {
		s.writeJSONRPCError(w, card.profile, nil, protoErr)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxA2ABody))
	if err != nil {
		s.writeJSONRPCError(w, card.profile, nil, a2a.Errorf(a2a.ErrorTypeInternal, "reading request body: %v", err))
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
		s.writeJSONRPCError(w, card.profile, id, protoErr)
		return
	}

	// Decoding happens before the implemented check so a malformed request is
	// still told it is malformed, rather than being answered with a refusal
	// about the operation it was trying to name.
	if !brokerOperationImplemented(call.Method) {
		s.writeJSONRPCError(w, card.profile, call.ID(), a2aNotImplemented(call.Method))
		return
	}

	b := a2aBinding{jsonrpc: true, id: call.ID()}
	switch call.Method {
	case a2a.MethodSendMessage, a2a.MethodSendStreamingMessage:
		req, ok := call.Params.(*a2a.SendMessageRequest)
		if !ok {
			s.writeJSONRPCError(w, card.profile, call.ID(),
				a2a.Errorf(a2a.ErrorTypeInternal, "operation %q decoded to an unexpected parameter type", call.Method))
			return
		}
		s.handleSendMessage(w, r, card, b, req, call.Streaming())
		return

	case a2a.MethodCancelTask:
		req, ok := call.Params.(*a2a.CancelTaskRequest)
		if !ok {
			s.writeJSONRPCError(w, card.profile, call.ID(),
				a2a.Errorf(a2a.ErrorTypeInternal, "operation %q decoded to an unexpected parameter type", call.Method))
			return
		}
		s.handleCancelTask(w, r, card, b, req)
		return

	case a2a.MethodGetTask:
		req, ok := call.Params.(*a2a.GetTaskRequest)
		if !ok {
			s.writeJSONRPCError(w, card.profile, call.ID(),
				a2a.Errorf(a2a.ErrorTypeInternal, "operation %q decoded to an unexpected parameter type", call.Method))
			return
		}
		s.handleGetTask(w, r, card, b, req)
		return

	case a2a.MethodListTasks:
		req, ok := call.Params.(*a2a.ListTasksRequest)
		if !ok {
			s.writeJSONRPCError(w, card.profile, call.ID(),
				a2a.Errorf(a2a.ErrorTypeInternal, "operation %q decoded to an unexpected parameter type", call.Method))
			return
		}
		s.handleListTasks(w, r, card, b, req)
		return

	case a2a.MethodSubscribeToTask:
		req, ok := call.Params.(*a2a.SubscribeToTaskRequest)
		if !ok {
			s.writeJSONRPCError(w, card.profile, call.ID(),
				a2a.Errorf(a2a.ErrorTypeInternal, "operation %q decoded to an unexpected parameter type", call.Method))
			return
		}
		s.handleSubscribeToTask(r.Context(), w, r, card, b, req)
		return
	}

	// Unreachable: every entry in brokerImplementedOperations has a case above.
	// Written as an internal error rather than a panic so that adding an entry
	// to the map without adding a handler costs one request instead of the
	// process.
	s.writeJSONRPCError(w, card.profile, call.ID(),
		a2a.Errorf(a2a.ErrorTypeInternal, "operation %q is marked implemented but has no handler", call.Method))
}

// writeJSONRPCError renders an A2A error as a JSON-RPC error response.
//
// The status is 200: section 9 does not map A2A error codes onto HTTP statuses
// the way the REST binding does, and a transport-level non-200 would tell an
// intermediary the request failed when it was in fact answered. The exceptions
// are transport-level conditions — an unknown profile (404, below) and an auth
// denial (401/403, written by the guard) — where the URL or the credential, not
// the operation, is what was wrong.
func (s *A2AServer) writeJSONRPCError(w http.ResponseWriter, profile string, id json.RawMessage, protoErr *a2a.Error) {
	resp := a2a.NewErrorResponse(id, protoErr)
	data, err := resp.Encode()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", a2a.ContentTypeJSON)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		s.logger.Debug("a2a jsonrpc write failed", "profile", profile, "error", err)
	}
}

// ---- REST binding ----

// handleREST resolves an HTTP+JSON request to an A2A operation and answers it.
func (s *A2AServer) handleREST(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(a2a.HeaderVersion, a2a.ProtocolVersion)

	card, ok := s.cardFor(r)
	if !ok {
		s.writeUnknownProfileREST(w, r)
		return
	}

	if _, protoErr := a2a.ParseServiceParamsAssuming(r.Header, r.URL.Query(), a2a.ProtocolVersion); protoErr != nil {
		s.writeRESTError(w, card.profile, protoErr)
		return
	}

	suffix := strings.TrimPrefix(r.URL.Path, agentRESTPrefix(card.profile))
	if suffix == "" {
		suffix = "/"
	}
	route, vars, found, methodMismatch := a2a.MatchRoute(r.Method, suffix)
	switch {
	case !found && methodMismatch:
		// The path names a real operation but the verb is wrong. 405 with the
		// standard Allow header, rather than a 404 that would send a client
		// looking for a typo in a URL that is correct.
		if allowed, ok := a2a.RouteFor(a2aOperationForPath(suffix)); ok {
			w.Header().Set("Allow", allowed.HTTPMethod+", OPTIONS")
		}
		s.writeRESTStatus(w, card.profile, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
			fmt.Sprintf("%s is not allowed on %s", r.Method, r.URL.Path), "")
		return
	case !found:
		s.writeRESTError(w, card.profile,
			a2a.Errorf(a2a.ErrorTypeMethodNotFound, "no A2A operation is mounted at %s", r.URL.Path))
		return
	}

	if !brokerOperationImplemented(route.Operation) {
		s.writeRESTError(w, card.profile, a2aNotImplemented(route.Operation))
		return
	}

	switch route.Operation {
	case a2a.MethodSendMessage, a2a.MethodSendStreamingMessage:
		body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, maxA2ABody))
		if readErr != nil {
			s.writeRESTError(w, card.profile, a2a.Errorf(a2a.ErrorTypeInternal, "reading request body: %v", readErr))
			return
		}
		req, decodeErr := a2a.DecodeSendMessageRequest(body)
		if decodeErr != nil {
			s.writeRESTError(w, card.profile, decodeErr)
			return
		}
		s.handleSendMessage(w, r, card, a2aBinding{}, req, route.Streaming)
		return

	case a2a.MethodCancelTask:
		// Section 11.3 gives CancelTask a custom verb and an optional body; the
		// path id is authoritative over anything in it.
		body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, maxA2ABody))
		if readErr != nil {
			s.writeRESTError(w, card.profile, a2a.Errorf(a2a.ErrorTypeInternal, "reading request body: %v", readErr))
			return
		}
		req, protoErr := a2a.DecodeCancelTaskRequest(vars["id"], body)
		if protoErr != nil {
			s.writeRESTError(w, card.profile, protoErr)
			return
		}
		s.handleCancelTask(w, r, card, a2aBinding{}, req)
		return

	case a2a.MethodGetTask:
		// Section 11.5: a GET carries its parameters in the path and the query
		// string, named exactly as the JSON body would name them, so the two
		// bindings decode into the same request object.
		req, protoErr := a2a.ParseGetTaskQuery(vars["id"], r.URL.Query())
		if protoErr != nil {
			s.writeRESTError(w, card.profile, protoErr)
			return
		}
		s.handleGetTask(w, r, card, a2aBinding{}, req)
		return

	case a2a.MethodListTasks:
		req, protoErr := a2a.ParseListTasksQuery(r.URL.Query())
		if protoErr != nil {
			s.writeRESTError(w, card.profile, protoErr)
			return
		}
		s.handleListTasks(w, r, card, a2aBinding{}, req)
		return

	case a2a.MethodSubscribeToTask:
		// Section 11.3's custom-verb shape again: an optional body, with the path
		// id authoritative over anything in it.
		body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, maxA2ABody))
		if readErr != nil {
			s.writeRESTError(w, card.profile, a2a.Errorf(a2a.ErrorTypeInternal, "reading request body: %v", readErr))
			return
		}
		req, protoErr := a2a.DecodeSubscribeToTaskRequest(vars["id"], body)
		if protoErr != nil {
			s.writeRESTError(w, card.profile, protoErr)
			return
		}
		s.handleSubscribeToTask(r.Context(), w, r, card, a2aBinding{}, req)
		return
	}

	// Unreachable; see handleJSONRPC.
	s.writeRESTError(w, card.profile,
		a2a.Errorf(a2a.ErrorTypeInternal, "operation %q is marked implemented but has no handler", route.Operation))
}

// a2aOperationForPath finds the operation a path template matches regardless of
// verb, so a 405 can name the method that would have worked.
func a2aOperationForPath(suffix string) string {
	for _, m := range []string{
		http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
	} {
		if route, _, found, _ := a2a.MatchRoute(m, suffix); found {
			return route.Operation
		}
	}
	return ""
}

// writeRESTError renders an A2A error as the section 11.6 google.rpc.Status body
// with its mapped HTTP status.
func (s *A2AServer) writeRESTError(w http.ResponseWriter, profile string, protoErr *a2a.Error) {
	status, body := protoErr.RESTError()
	s.writeRESTBody(w, profile, status, body)
}

// writeRESTStatus renders a refusal the broker originates — one with no A2A
// error type — in the same google.rpc.Status shape, so a client parses one error
// format on this endpoint rather than two.
func (s *A2AServer) writeRESTStatus(w http.ResponseWriter, profile string, status int, grpcStatus, message, reason string) {
	body := a2a.RESTError{Error: a2a.RESTErrorStatus{
		Code:    status,
		Status:  grpcStatus,
		Message: message,
	}}
	if reason != "" {
		body.Error.Details = []a2a.ErrorDetail{{
			"@type":  a2a.TypeErrorInfo,
			"reason": reason,
			"domain": a2aErrorDomain,
		}}
	}
	s.writeRESTBody(w, profile, status, body)
}

func (s *A2AServer) writeRESTBody(w http.ResponseWriter, profile string, status int, body a2a.RESTError) {
	data, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", a2a.ContentTypeJSON)
	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		s.logger.Debug("a2a rest write failed", "profile", profile, "error", err)
	}
}

// ---- Shared ----

// a2aNotImplemented builds the answer an operation gets while it is routed but
// not yet driving a turn.
//
// UnsupportedOperationError is the right type rather than a placeholder: section
// 3.3.4 uses it for exactly this condition — an operation the specification
// defines that this agent does not support — and the Agent Card backs the claim
// up by advertising the matching capability as false. A client therefore gets a
// refusal it already knows how to handle, not a novel one.
func a2aNotImplemented(operation string) *a2a.Error {
	return a2a.Errorf(a2a.ErrorTypeUnsupportedOperation,
		"operation %q is not yet implemented by this broker's A2A ingress", operation).
		WithMetadata("operation", operation).
		// A distinct key, not "reason": the ErrorInfo already carries
		// reason=UNSUPPORTED_OPERATION, and a second field by that name would
		// read as a contradiction. This one narrows it — "not supported" here
		// means "not yet", which is what a partner integrating against this
		// broker needs to know.
		WithMetadata("detail", a2aReasonNotImplemented)
}

// cardFor resolves the profile named in the request path.
//
// It reports absent for an unknown name rather than falling back to a default
// profile, for the same reason POST /claim refuses an unknown binary: quietly
// serving a different agent than the one addressed produces a conversation that
// merely behaves oddly, which is far harder to diagnose than a refusal.
func (s *A2AServer) cardFor(r *http.Request) (*servedAgentCard, bool) {
	card, ok := s.cards[r.PathValue("profile")]
	return card, ok
}

// unknownProfileMessage is THE refusal for a path naming no configured profile.
// One string, three renderings, so the three bindings cannot drift apart.
const unknownProfileMessage = "unknown agent profile"

// writeUnknownProfileEnvelope answers an unknown profile on the card route in
// the broker's standard error envelope.
func (s *A2AServer) writeUnknownProfileEnvelope(w http.ResponseWriter, r *http.Request) {
	s.logUnknownProfile(r)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": unknownProfileMessage})
}

// writeUnknownProfileJSONRPC answers an unknown profile on the JSON-RPC binding.
//
// Unlike every other JSON-RPC response this one carries a non-200 status, for
// the same reason an auth denial does: the URL was wrong, not the operation, and
// 404 is the answer an intermediary and a human both read correctly. The body is
// still a well-formed JSON-RPC error object so a client that only ever parses an
// envelope is not left guessing.
func (s *A2AServer) writeUnknownProfileJSONRPC(w http.ResponseWriter, r *http.Request) {
	s.logUnknownProfile(r)
	protoErr := a2a.Errorf(a2a.ErrorTypeMethodNotFound, "%s: no A2A agent is mounted at %s", unknownProfileMessage, r.URL.Path)
	resp := a2a.NewErrorResponse(nil, protoErr)
	data, err := resp.Encode()
	if err != nil {
		http.Error(w, unknownProfileMessage, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", a2a.ContentTypeJSON)
	w.WriteHeader(http.StatusNotFound)
	if _, err := w.Write(data); err != nil {
		s.logger.Debug("a2a jsonrpc write failed", "error", err)
	}
}

// writeUnknownProfileREST answers an unknown profile on the REST binding.
// MethodNotFoundError already maps to 404, so the binding's own rendering is
// exactly right here.
func (s *A2AServer) writeUnknownProfileREST(w http.ResponseWriter, r *http.Request) {
	s.logUnknownProfile(r)
	s.writeRESTError(w, "",
		a2a.Errorf(a2a.ErrorTypeMethodNotFound, "%s: no A2A agent is mounted at %s", unknownProfileMessage, r.URL.Path))
}

// logUnknownProfile records a request for a profile this broker does not serve.
//
// It logs the REQUESTED name, which is safe to record and is the whole value of
// the line: the common cause is a client configured against a broker that has
// since dropped (or never had) that profile, and the operator's first question
// is which name it is asking for.
func (s *A2AServer) logUnknownProfile(r *http.Request) {
	s.logger.Warn("a2a request for an unknown agent profile",
		"route", routeLabel(r),
		"profile", r.PathValue("profile"),
		"path", r.URL.Path)
}
