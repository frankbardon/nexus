package engine

import (
	"github.com/frankbardon/nexus/pkg/events"
)

// Metadata keys stamped onto a substituted llm.response. Gates read
// MetaVetoed to recognize a response they (or another handler) already
// replaced; MetaVetoReason carries the reason verbatim for logging and for
// consumers that want the machine-readable cause rather than the prose in
// Content.
const (
	// MetaVetoed marks an llm.response that was substituted after a
	// before:llm.response veto. Always the bool true when present.
	MetaVetoed = "_vetoed"
	// MetaVetoReason carries the VetoResult.Reason of the handler that
	// vetoed. Present whenever MetaVetoed is.
	MetaVetoReason = "_veto_reason"
)

// FinishReasonVetoed is the LLMResponse.FinishReason of a substituted
// response. Agent loops treat it as a terminal, non-tool-calling turn.
const FinishReasonVetoed = "vetoed"

// PublishLLMResponse runs resp through the vetoable "before:llm.response"
// hook and then emits "llm.response" exactly once. It returns the response
// that was actually published, and whether that response was substituted for
// the one passed in.
//
// Every producer of an llm.response — every provider plugin, the fanout
// coordinator's merged re-emit, the test IO plugin's mock responses — must
// publish through this function rather than calling bus.Emit directly, so
// that the hook fires uniformly and the invariants below hold everywhere.
//
// The contract differs from every other before:* event in the system, in one
// way that matters:
//
// A veto on before:llm.response means SUBSTITUTE, not DROP. Agent loops
// (react, planexec, orchestrator) subscribe to llm.response and block their
// turn waiting for it; suppressing the event entirely would hang the turn
// rather than block an action. So a vetoing handler does not prevent an
// llm.response — it changes which one is published.
//
// Handlers have two ways to shape that response:
//
//   - Mutate without vetoing. The payload is a *events.LLMResponse, so a
//     handler that rewrites Content (or strips individual ToolCalls) in place
//     and returns without setting Veto has its edits published as the model's
//     response. This is the same mutate-then-veto convention the shipped gates
//     already use on before:io.output.
//
//   - Veto. The response is replaced by a substitute derived from it. A
//     handler that wants to dictate the replacement text mutates Content and
//     then vetoes; the substitute keeps the mutated Content. A handler that
//     vetoes without touching Content gets a substitute whose Content is the
//     veto reason, so the agent loop can see why it was blocked.
//
// In both veto cases ToolCalls is cleared. A vetoed response must never drive
// tool execution — that invariant is the entire security value of the hook,
// and it lives here rather than in each provider so it cannot be forgotten at
// one of the emit sites.
//
// Correlation and accounting fields survive substitution: RequestID (so the
// blocking sync-RPC helper and provider in-flight tracking still match the
// response to its request), Model, Tags, Usage and CostUSD. The tokens were
// spent whether or not the answer ships, so nexus.gate.token_budget must
// still see them.
//
// Streaming caveat: providers emit llm.stream.chunk as tokens arrive and
// llm.response only at the end of the stream. A veto here cannot unsay text
// already streamed to a UI. Gates that must prevent disclosure require
// stream: false; gates that only steer the loop (blocking tool calls) work
// unchanged under streaming.
func PublishLLMResponse(bus EventBus, resp events.LLMResponse) (events.LLMResponse, bool) {
	if bus == nil {
		return resp, false
	}

	// Re-entrancy guard. A gate that vetoes and then drives a corrective LLM
	// request produces a second response that flows back through here; a
	// substitute that is itself re-offered to the hook would let a gate veto
	// its own replacement indefinitely. An already-substituted response is
	// emitted directly.
	if vetoed, _ := resp.Metadata[MetaVetoed].(bool); vetoed {
		_ = bus.Emit("llm.response", resp)
		return resp, false
	}

	// Snapshot before dispatch: handlers mutate the payload in place, so
	// this is the only way to tell "handler dictated the replacement text"
	// from "handler vetoed and left Content as the model wrote it".
	modelContent := resp.Content

	veto, err := bus.EmitVetoable("before:llm.response", &resp)
	if err != nil || !veto.Vetoed {
		// Handlers may have mutated resp in place; publish what they left.
		_ = bus.Emit("llm.response", resp)
		return resp, false
	}

	sub := substituteLLMResponse(resp, modelContent, veto.Reason)
	_ = bus.Emit("llm.response", sub)
	return sub, true
}

// substituteLLMResponse derives the response published in place of a vetoed
// one. modelContent is resp.Content as it stood before handlers ran.
// Exported behavior is documented on PublishLLMResponse.
func substituteLLMResponse(resp events.LLMResponse, modelContent, reason string) events.LLMResponse {
	sub := resp

	// The invariant: a vetoed response never carries tool calls.
	sub.ToolCalls = nil
	sub.Alternatives = nil
	sub.Citations = nil
	sub.FinishReason = FinishReasonVetoed

	// A handler that rewrote Content before vetoing is dictating the
	// replacement text; only fall back to the reason when Content is still
	// what the model produced.
	if sub.Content == modelContent && reason != "" {
		sub.Content = reason
	}

	// Copy rather than mutate: resp.Metadata is the map the provider built
	// and may be shared with the request's passthrough metadata.
	meta := make(map[string]any, len(resp.Metadata)+2)
	for k, v := range resp.Metadata {
		meta[k] = v
	}
	meta[MetaVetoed] = true
	meta[MetaVetoReason] = reason
	sub.Metadata = meta

	if sub.SchemaVersion == 0 {
		sub.SchemaVersion = events.LLMResponseVersion
	}
	return sub
}
