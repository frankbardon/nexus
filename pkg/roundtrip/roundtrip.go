// Package roundtrip centralises the allowlist of LLMResponse.Metadata keys
// that must travel with a conversation Message so the NEXT request can echo
// them back to the provider verbatim.
//
// Conversation history is provider-neutral, but some providers hand back
// opaque continuity tokens on an assistant turn and then reject the following
// request if those tokens are missing:
//
//   - Anthropic extended thinking: `thinking_blocks` must be echoed on the
//     assistant turn that precedes a tool_result, or the API returns HTTP 400.
//   - Gemini thinking: a `thoughtSignature` rides alongside each functionCall
//     part and must be re-emitted with that call, or the API returns HTTP 400.
//
// The forwarder deliberately does NOT copy the whole metadata map. Engine-
// internal flags like `_source`, `_structured_output` and `_target_*` are
// request-scoped routing hints, not message-scoped state, and leaking them
// into persisted history would make a replayed message look like it came from
// an internal sub-flow. Only keys a provider identifies as required for
// round-trip continuity are preserved.
//
// It applies to EVERY history a response is replayed from, not only a
// persisted one. It was extracted because the same allowlist had been
// copy-pasted into memory/capped and memory/simple and forgotten entirely in
// memory/summary_buffer — which silently dropped Anthropic thinking blocks —
// and it lives under pkg/ because the agent loops that keep their own
// in-process history (pkg/delegate, plugins/agents/subagent,
// plugins/agents/planexec, plugins/agents/orchestrator) had forgotten it too:
// a delegated posture that called one tool and then asked again sent Gemini a
// functionCall with no thoughtSignature, and every such turn 400'd.
package roundtrip

// forwardedKeys enumerates the LLMResponse.Metadata keys copied onto a stored
// Message. Keep this list minimal: every entry is persisted to history JSONL
// and replayed on later requests.
//
// NOTE: "gemini_thought_signatures" is duplicated here as a string literal
// because the Gemini provider declares it as an unexported constant in its own
// package (`thoughtSignatureMetaKey` in plugins/providers/gemini/plugin.go).
// Nothing outside a provider may import one, so the two spellings are coupled
// by convention only — change one and you must change the other.
var forwardedKeys = []string{
	"thinking_blocks",
	"gemini_thought_signatures",
}

// ForwardMessageMetadata returns the allowlisted subset of an LLMResponse
// metadata map, suitable for assigning to events.Message.Metadata. Call it
// wherever an llm.response becomes a history message a later request replays.
//
// Returns nil — not an empty map — when src is nil or carries none of the
// allowlisted keys, so a message that needs no continuity state serialises
// without an empty metadata object.
func ForwardMessageMetadata(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	var out map[string]any
	for _, k := range forwardedKeys {
		v, ok := src[k]
		if !ok {
			continue
		}
		if out == nil {
			out = make(map[string]any, len(forwardedKeys))
		}
		out[k] = v
	}
	return out
}
