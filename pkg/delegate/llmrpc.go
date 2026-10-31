package delegate

import (
	"context"
	"errors"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// SyncLLM emits an llm.request and synchronously returns the matching
// llm.response. It is the shared implementation behind every blocking sub-agent
// LLM call (delegate runtime, subagent worker loop, and any future blocking
// caller) — the pattern was previously copy-pasted across both call sites.
//
// Semantics:
//
//   - Stream is forced to false. The helper relies on the bus dispatching
//     synchronously: a non-streaming provider emits llm.response inside the
//     Emit("llm.request") call, the subscriber pushes onto respCh, and the
//     select below picks it up. A streaming response would arrive after Emit
//     returns and miss the select, surfacing as ErrNoResponse.
//
//   - RequestID is generated when empty. Subscribers match the response
//     primarily by req.RequestID; a metadata "_source" fallback keeps
//     compatibility with callers that haven't migrated their downstream
//     filters yet.
//
//   - A block veto on before:llm.request surfaces as an error wrapping
//     engine.ErrLLMRequestBlocked rather than waiting for a response that
//     will never come.
//
//   - An interception (engine.InterceptLLMRequest — a fanout role, the test
//     IO mock) is answered asynchronously, so the helper waits on ctx for the
//     correlated llm.response and returns ctx.Err() if ctx ends first. Fanout
//     legs, which share the RequestID, are skipped for the merged response.
//
//   - If a published request gets no synchronous response (no provider wired
//     for the request's role), engine.ErrNoLLMResponse is returned — same
//     diagnostic behavior the call sites had before extraction. See
//     engine.AwaitLLMResponse.
func SyncLLM(ctx context.Context, bus engine.EventBus, req events.LLMRequest) (events.LLMResponse, error) {
	if bus == nil {
		return events.LLMResponse{}, errors.New("delegate.SyncLLM: bus is nil")
	}

	if req.SchemaVersion == 0 {
		req.SchemaVersion = events.LLMRequestVersion
	}
	if req.RequestID == "" {
		req.RequestID = engine.GenerateID()
	}
	req.Stream = false

	source, _ := req.Metadata["_source"].(string)

	respCh := make(chan events.LLMResponse, 1)
	unsub := bus.Subscribe("llm.response", func(ev engine.Event[any]) {
		resp, ok := ev.Payload.(events.LLMResponse)
		if !ok {
			return
		}
		if !responseMatches(resp, req.RequestID, source) {
			return
		}
		select {
		case respCh <- resp:
		default:
		}
	}, engine.WithPriority(1))
	defer unsub()

	// A block veto fails at once; an interception (fanout, test mock) is
	// answered asynchronously, so AwaitLLMResponse waits on ctx for it.
	res := engine.PublishLLMRequest(bus, req)
	return engine.AwaitLLMResponse(ctx, res, respCh)
}

func responseMatches(resp events.LLMResponse, requestID, source string) bool {
	// A fanout leg shares the request's RequestID (and carries the fanout's
	// own _source); only the merged response, which restores the request's
	// _source, answers the request.
	if engine.IsFanoutLegResponse(resp.Metadata) {
		return false
	}
	if requestID != "" && resp.RequestID == requestID {
		return true
	}
	if source != "" {
		if s, _ := resp.Metadata["_source"].(string); s == source {
			return true
		}
	}
	return false
}
