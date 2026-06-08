package delegate

import (
	"context"
	"errors"
	"fmt"

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
//   - The before:llm.request veto is honored; a vetoed request surfaces as
//     an ErrVetoed error rather than waiting for a response that will never
//     come.
//
//   - If ctx cancels before a response arrives, ctx.Err() is returned. If
//     Emit completes without a synchronous response (no provider wired for
//     the request's role), ErrNoResponse is returned — same diagnostic
//     behavior the call sites had before extraction.
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

	if veto, err := bus.EmitVetoable("before:llm.request", &req); err == nil && veto.Vetoed {
		return events.LLMResponse{}, fmt.Errorf("llm.request vetoed: %s", veto.Reason)
	}
	if err := bus.Emit("llm.request", req); err != nil {
		return events.LLMResponse{}, err
	}

	select {
	case resp := <-respCh:
		return resp, nil
	case <-ctx.Done():
		return events.LLMResponse{}, ctx.Err()
	default:
		return events.LLMResponse{}, errors.New("no LLM response (provider not active for this role?)")
	}
}

func responseMatches(resp events.LLMResponse, requestID, source string) bool {
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
