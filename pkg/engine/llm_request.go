package engine

import (
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
