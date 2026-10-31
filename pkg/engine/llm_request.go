package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/frankbardon/nexus/pkg/events"
)

// MetaInterceptedBy is the LLMRequest.Metadata key a before:llm.request
// handler stamps when it vetoes a request because it has taken ownership of
// answering it — it will publish the correlated llm.response itself, rather
// than blocking the request outright. Its value is the intercepting handler's
// plugin ID. Set it with InterceptLLMRequest; read it through
// LLMRequestResult.Intercepted.
//
// The distinction matters to a producer that waits for its response: a
// blocked request will never be answered and the producer should give up at
// once, while an intercepted one will be answered (typically asynchronously)
// and the producer should keep waiting exactly as if it had been published.
// The fanout coordinator (which re-dispatches the request to several
// providers and publishes a merged response) and the test IO plugin's mock
// mode (which publishes a scripted response) are the two interceptors today.
const MetaInterceptedBy = "_intercepted_by"

// LLMRequestResult is the outcome of PublishLLMRequest.
type LLMRequestResult struct {
	// Request is the request as the before:llm.request handlers left it —
	// what was published, or what was vetoed. Handlers mutate in place
	// (tags, tool filters, routing, decorated messages), so a producer that
	// keeps its own copy of the request should take this one.
	Request events.LLMRequest
	// Published reports whether llm.request was emitted.
	Published bool
	// Veto is the veto that stopped the request, when one did.
	Veto VetoResult
	// Err is the error from emitting llm.request, when it was emitted.
	Err error
}

// Intercepted reports whether the request was vetoed by a handler that will
// publish its llm.response itself (see MetaInterceptedBy).
func (r LLMRequestResult) Intercepted() bool {
	if !r.Veto.Vetoed {
		return false
	}
	by, _ := r.Request.Metadata[MetaInterceptedBy].(string)
	return by != ""
}

// Blocked reports whether the request was vetoed outright: nothing will ever
// answer it, so a producer waiting on its response should stop waiting.
func (r LLMRequestResult) Blocked() bool {
	return r.Veto.Vetoed && !r.Intercepted()
}

// AwaitResponse reports whether an llm.response for this request is still
// expected: it was published, or a handler intercepted it.
func (r LLMRequestResult) AwaitResponse() bool {
	return r.Published || r.Intercepted()
}

// InterceptLLMRequest vetoes a before:llm.request on behalf of a handler that
// takes ownership of answering it, stamping MetaInterceptedBy so the producer
// keeps waiting for the response instead of treating the veto as a block.
// The handler is then responsible for publishing a correlated llm.response
// (via PublishLLMResponse, carrying the request's RequestID and Metadata).
func InterceptLLMRequest(vp *VetoablePayload, req *events.LLMRequest, by, reason string) {
	if req.Metadata == nil {
		req.Metadata = make(map[string]any)
	}
	req.Metadata[MetaInterceptedBy] = by
	vp.Veto = VetoResult{Vetoed: true, Reason: reason}
}

// MetaFanoutProvider is the LLMResponse.Metadata key nexus.provider.fanout
// stamps on each per-provider leg of a fanout (its value is the leg's
// provider). Legs are copies of the intercepted request and keep its
// RequestID, so a producer correlating by RequestID must skip them and wait
// for the merged response, which carries the same RequestID without this key.
// Read it through IsFanoutLegResponse.
const MetaFanoutProvider = "_fanout_provider"

// IsFanoutLegResponse reports whether a response is one per-provider leg of a
// fanout rather than the answer to the request (see MetaFanoutProvider).
func IsFanoutLegResponse(meta map[string]any) bool {
	p, _ := meta[MetaFanoutProvider].(string)
	return p != ""
}

// ErrLLMRequestBlocked is returned by AwaitLLMResponse when a
// before:llm.request handler vetoed the request outright: nothing will
// answer it.
var ErrLLMRequestBlocked = errors.New("llm.request vetoed")

// ErrNoLLMResponse is returned by AwaitLLMResponse when the request was
// published but no response was delivered during the synchronous emit (no
// provider active for the request's role, or the provider failed).
var ErrNoLLMResponse = errors.New("no LLM response (provider not active for this role?)")

// AwaitLLMResponse collects the response to a request published with
// PublishLLMRequest by a producer that blocks on it. ch is the producer's
// own correlation channel, fed by its llm.response subscription (match on
// RequestID, skipping IsFanoutLegResponse legs); it must be subscribed
// before the publish so a response delivered inside the emit is not lost.
//
//   - Emit error: returned as is.
//   - Blocked: ErrLLMRequestBlocked wrapping the veto reason, at once.
//   - Intercepted: a handler owns the answer and typically publishes it
//     asynchronously (fanout merge, test IO mock), so this waits on ch until
//     ctx is done and then returns ctx.Err(). ctx is the only bound.
//   - Published: the bus dispatches synchronously and a non-streaming
//     provider answers inside the emit, so the response is either already
//     on ch or not coming — ErrNoLLMResponse (ctx.Err() if ctx is done).
func AwaitLLMResponse[T any](ctx context.Context, res LLMRequestResult, ch <-chan T) (T, error) {
	var zero T
	if res.Err != nil {
		return zero, res.Err
	}
	if res.Blocked() {
		return zero, fmt.Errorf("%w: %s", ErrLLMRequestBlocked, res.Veto.Reason)
	}
	select {
	case v := <-ch:
		return v, nil
	default:
	}
	if res.Intercepted() {
		select {
		case v := <-ch:
			return v, nil
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	return zero, ErrNoLLMResponse
}

// PublishLLMRequest runs req through the vetoable "before:llm.request" hook
// and, unless a handler vetoes it, emits "llm.request" with whatever the
// handlers left in the payload.
//
// Every producer of an llm.request — agent loops, planners, routers, memory
// summarisers, gate retries, provider failover, RAG contextualisation, HITL
// synthesis, embedder code — must publish through this function (or
// PublishLLMRequestAsync) rather than calling bus.Emit("llm.request")
// directly. The before:llm.request hop is where the engine seeds the
// cost-attribution tags (session_id, tenant, project, user) and where the
// request-side gates and curators run (rate limiting, token budget, tool
// filtering, dynamic variables, routing, stream policy). A producer that
// skips it is invisible to all of them.
//
// Veto semantics: a vetoed request is not emitted. Producers should branch on
// the result — Blocked means give up now (nothing will answer), Intercepted
// means a handler owns the answer and the producer should keep waiting for
// the llm.response as though the request had been published.
//
// Recursion: a before:llm.request handler that itself produces an
// llm.request (the classifier router's probe, compaction triggered by the
// context-window gate) re-enters this hook. Such handlers must recognise and
// skip their own follow-ups; the convention is that every internal request
// carries a non-empty Metadata["_source"] (and the classifier also
// "_routed_by"), which every emitting handler already skips on. The fanout
// coordinator uses "_fanout_id" the same way.
func PublishLLMRequest(bus EventBus, req events.LLMRequest) LLMRequestResult {
	res, ok := offerLLMRequest(bus, req)
	if !ok {
		return res
	}
	res.Err = bus.Emit("llm.request", res.Request)
	res.Published = true
	return res
}

// PublishLLMRequestAsync is PublishLLMRequest for a producer that must not
// block on the provider round-trip (it waits for the response under its own
// deadline instead). The before:llm.request hop still runs synchronously, so
// a veto is known on return; only the llm.request emission is dispatched
// with EmitAsync. Err is always nil — an emission error surfaces on the
// async channel, which this helper does not wait for.
func PublishLLMRequestAsync(bus EventBus, req events.LLMRequest) LLMRequestResult {
	res, ok := offerLLMRequest(bus, req)
	if !ok {
		return res
	}
	bus.EmitAsync("llm.request", res.Request)
	res.Published = true
	return res
}

// offerLLMRequest runs the before:llm.request hop. ok is false when the
// request must not be emitted.
func offerLLMRequest(bus EventBus, req events.LLMRequest) (LLMRequestResult, bool) {
	if req.SchemaVersion == 0 {
		req.SchemaVersion = events.LLMRequestVersion
	}
	res := LLMRequestResult{Request: req}
	if bus == nil {
		return res, false
	}
	veto, err := bus.EmitVetoable("before:llm.request", &res.Request)
	if err == nil && veto.Vetoed {
		res.Veto = veto
		return res, false
	}
	// An EmitVetoable error (no handler could veto) falls through to the
	// emit, the same as every producer did before this helper existed.
	return res, true
}

// MetaSource is the LLMRequest.Metadata key a producer stamps with its own
// identity ("_source"). A non-empty value is the recursion guard documented in
// docs/src/architecture/event-bus.md, and it is also the cost-attribution tag:
// since #83 the agent loops stamp it on their own main turns too, so a
// non-empty "_source" alone no longer means "internal request".
const MetaSource = "_source"

// MetaTaskKind is the LLMRequest.Metadata key naming what kind of call a
// request is ("react_main", "summarise", "gate_retry", ...). It is the
// discriminator IsInternalLLMRequest reads to tell an agent-loop turn from an
// internal sub-flow.
const MetaTaskKind = "task_kind"

// MetaTurnID is the LLMRequest.Metadata key naming the agent turn a request
// serves. LLMRequest has no TurnID field, so request-time consumers read it
// here: nexus.gate.approval_policy copies it onto the HITLRequest an
// llm.request rule raises, which is what lets a turn-scoped transport
// (nexus.io.agui) resume and finish the right run (#218). Providers echo
// request metadata onto the response, so rag.citations and io.voice read it
// back off llm.response.
//
// Every agent loop stamps it on every llm.request it publishes. A nested loop
// (subagent, delegate) stamps the PARENT turn it is serving — the one whose
// run is waiting on it — not its private tool-correlation turn id, since that
// id names no turn a transport ever saw start.
const MetaTurnID = "turn_id"

// MetaLoopTurnID is the LLMRequest.Metadata key a nested loop (subagent,
// delegate) sets to its private "subagent_<id>" / "delegate_<id>" turn —
// the TurnID its tool calls carry — beside MetaTurnID's parent turn.
// Providers echo it onto llm.response, so a consumer that correlates a
// response with that loop's own tool activity (nexus.rag.citations matching
// knowledge_search's rag.retrieved) reads it first and falls back to
// MetaTurnID. It is NOT a turn any transport saw start: bind HITL and other
// transport-visible emissions to MetaTurnID, never to this.
const MetaLoopTurnID = "loop_turn_id"

// TaskKindCompaction is the task_kind nexus.memory.compaction stamps on its
// summarisation request. It is a named constant because two sides must agree
// on its spelling: the producer, and every consumer that recognises a
// compaction call by kind (the memory plugins' internalflow skip list, router
// rules matching metadata.task_kind). Those drifted once ("compact" vs
// "compaction") and the memory plugins recorded the compaction summary into
// the conversation history.
const TaskKindCompaction = "compaction"

// Task kinds of the nested agent loops: they run inside a parent turn, keep
// their own in-process history, and offer a tool set scoped by their caller.
const (
	TaskKindSubagent = "subagent"
	TaskKindDelegate = "delegate"
)

// Task kinds of the top-level agent-loop turns. Their responses drive the
// conversation and tool execution, so IsInternalLLMRequest treats them as
// turns even though the loops stamp "_source" for cost attribution.
const (
	TaskKindReactMain              = "react_main"
	TaskKindPlanexecStep           = "planexec_step"
	TaskKindPlanexecSynthesize     = "planexec_synthesize"
	TaskKindOrchestratorDecompose  = "orchestrator_decompose"
	TaskKindOrchestratorSynthesize = "orchestrator_synthesize"
)

// Task kinds of internal sub-flows: requests Nexus makes on its own behalf
// whose response is not a conversation turn. Every value is a wire value —
// it lands in cost records (`nexus cost --group-by task_kind`) and router
// rules matching metadata.task_kind — so the spellings are fixed.
const (
	// TaskKindPlan is nexus.planner.dynamic's plan-generation request.
	TaskKindPlan = "plan"
	// TaskKindClassify is nexus.router.classifier's routing probe.
	TaskKindClassify = "classify"
	// TaskKindSummarise is nexus.memory.summary_buffer's summary request.
	TaskKindSummarise = "summarise"
	// TaskKindGateRetry is the shared gate retry-with-LLM helper's request
	// (plugins/gates/internal/retry).
	TaskKindGateRetry = "gate_retry"
	// TaskKindFanoutJudge is nexus.provider.fanout's judge request.
	TaskKindFanoutJudge = "fanout_judge"
	// TaskKindRAGContextualize is nexus.rag.ingest's contextual-chunk request.
	TaskKindRAGContextualize = "rag_contextualize"
	// TaskKindHITLSynthesize is nexus.control.hitl_synthesizer's answer
	// synthesis request.
	TaskKindHITLSynthesize = "hitl_synthesize"
)

// agentTurnTaskKinds are the task_kind values of agent-loop turns: requests
// whose response drives the conversation and tool execution. Every other
// _source-tagged request — planner, classifier probe, summariser, compaction,
// gate retry, fanout judge, RAG contextualisation, HITL synthesis, and any
// tagged request whose kind is not listed here — is internal.
//
// subagent and delegate are listed because they are agent loops that run
// tools; their prompts are authored by a parent model rather than echoing
// content the user already sent through io.input.
var agentTurnTaskKinds = map[string]bool{
	TaskKindReactMain:              true,
	TaskKindPlanexecStep:           true,
	TaskKindPlanexecSynthesize:     true,
	TaskKindOrchestratorDecompose:  true,
	TaskKindOrchestratorSynthesize: true,
	TaskKindSubagent:               true,
	TaskKindDelegate:               true,
}

// IsAgentTurnTaskKind reports whether kind is the task_kind of an agent-loop
// turn (see IsInternalLLMRequest).
func IsAgentTurnTaskKind(kind string) bool {
	return agentTurnTaskKinds[kind]
}

// IsNestedAgentTaskKind reports whether kind is the task_kind of a nested
// agent loop (subagent, delegate) — an agent turn that runs inside a parent
// turn rather than being the conversation itself.
func IsNestedAgentTaskKind(kind string) bool {
	return kind == TaskKindSubagent || kind == TaskKindDelegate
}

// IsConversationLLMRequest reports whether a request is a turn of the
// top-level conversation: not internal (see IsInternalLLMRequest) and not a
// nested agent loop (see IsNestedAgentTaskKind). Handlers that act on the
// conversation's own history or tool surface — the context-window gate
// (which compacts the main history) and progressive discovery (which
// replaces the tool list from the global catalog) — use this, because doing
// either to a subagent's or delegate's request would compact the wrong
// history or widen a caller-scoped tool set. An untagged request is a
// conversation turn.
func IsConversationLLMRequest(meta map[string]any) bool {
	if IsInternalLLMRequest(meta) {
		return false
	}
	kind, _ := meta[MetaTaskKind].(string)
	return !IsNestedAgentTaskKind(kind)
}

// IsInternalLLMRequest reports whether a request's metadata marks it as one of
// Nexus's own internal requests rather than an agent-loop turn: it carries a
// non-empty "_source" and its "task_kind" is not an agent-turn kind. A request
// with no "_source" is never internal — an untagged request is treated as a
// turn, so a handler that skips internal requests fails closed on it.
//
// Fallback retries copy the original request's metadata, so a retried agent
// turn stays a turn and a retried summary stays internal.
//
// Content gates whose input was already checked at io.input
// (nexus.gate.prompt_injection, nexus.gate.stop_words), the iteration counter
// (nexus.gate.endless_loop) and the approval-policy gate skip internal
// requests on before:llm.request; budget gates (rate_limiter, token_budget)
// do not.
func IsInternalLLMRequest(meta map[string]any) bool {
	if meta == nil {
		return false
	}
	src, _ := meta[MetaSource].(string)
	if src == "" {
		return false
	}
	kind, _ := meta[MetaTaskKind].(string)
	return !agentTurnTaskKinds[kind]
}
