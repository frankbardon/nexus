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
//   - OpenAI Responses reasoning: under `store: false` — which is the only mode
//     Nexus uses, because it keeps its own history — each `reasoning` Item in a
//     turn's output carries `encrypted_content`, and every Item must be replayed
//     verbatim on the next request or the model loses its reasoning across a
//     tool round. This one degrades quietly rather than erroring: the request
//     still succeeds, it is just reasoning-blind from the second round on.
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
// NOTE: "gemini_thought_signatures" and "openai_reasoning_items" are duplicated
// here as string literals because each provider declares its key as an
// unexported constant in its own package (`thoughtSignatureMetaKey` in
// plugins/providers/gemini/plugin.go, `reasoningItemsMetaKey` in
// plugins/providers/openai/responses_reply.go). Nothing outside a provider may
// import one, so the spellings are coupled by convention only — change one and
// you must change the other.
//
// NOT on this list, deliberately: "openai_reasoning_summary". That key is the
// prose a model wrote about its own reasoning — display material a UI renders
// and the next request has no use for. Forwarding it would persist a second
// copy of every summary into history and replay it as nothing.
var forwardedKeys = []string{
	"thinking_blocks",
	"gemini_thought_signatures",
	"openai_reasoning_items",
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

// perCallKeys enumerates the forwarded keys whose value is a map keyed by
// tool-call ID, as opposed to a single per-message payload.
//
// The distinction exists because the providers issue their continuity token at
// different grains. Anthropic's `thinking_blocks` describes the whole assistant
// turn, and so does OpenAI's `openai_reasoning_items` — an ordered list of the
// turn's reasoning Items, which are not addressed by call ID at all; Gemini's
// `gemini_thought_signatures` is a call-ID -> signature map, and Gemini signs
// only the FIRST functionCall of a parallel batch, so the value is sparse
// across the calls of one turn.
//
// A transport that carries metadata per tool call rather than per message --
// AG-UI is one -- has to split a turn's metadata across its calls on the way
// out and reassemble it on the way back. Splitting is the only operation that
// needs to know which grain a key is at, so the knowledge lives here beside the
// allowlist rather than in the transport.
var perCallKeys = map[string]bool{
	"gemini_thought_signatures": true,
}

// ForCall returns the slice of an allowlisted metadata map that belongs to one
// tool call, suitable for attaching to that call on a transport whose metadata
// slot is per-call.
//
// A per-call key is narrowed to the single entry for callID, re-wrapped in a
// map of the same shape so the value a reader sees is indistinguishable from
// the unsplit one. A per-message key is carried whole on every call of the
// turn: MergeCall folds them back with last-write-wins, so repeating the value
// costs wire bytes and reassembles exactly.
//
// Returns nil when the call has nothing to carry, so a call needing no
// continuity state serialises without an empty metadata object.
func ForCall(src map[string]any, callID string) map[string]any {
	if src == nil || callID == "" {
		return nil
	}
	var out map[string]any
	for _, k := range forwardedKeys {
		v, ok := src[k]
		if !ok {
			continue
		}
		if perCallKeys[k] {
			sig, found := lookupCall(v, callID)
			if !found {
				continue
			}
			v = map[string]any{callID: sig}
		}
		if out == nil {
			out = make(map[string]any, len(forwardedKeys))
		}
		out[k] = v
	}
	return out
}

// MergeCall folds one tool call's metadata back into the message-level map
// ForCall split it out of, and returns the result.
//
// A per-call key is merged ENTRY BY ENTRY, because each call carries only its
// own and the turn needs all of them; a per-message key is assigned whole,
// which is the spec's last-write-wins rule and is why ForCall may repeat it.
// Keys outside the allowlist are dropped: the map arrives from a client on the
// inbound path, and a replayed message must not be able to introduce
// engine-internal routing hints by naming them.
func MergeCall(dst map[string]any, callMeta map[string]any) map[string]any {
	if len(callMeta) == 0 {
		return dst
	}
	for _, k := range forwardedKeys {
		v, ok := callMeta[k]
		if !ok {
			continue
		}
		if dst == nil {
			dst = make(map[string]any, len(forwardedKeys))
		}
		if !perCallKeys[k] {
			dst[k] = v
			continue
		}
		entries, ok := asStringMap(v)
		if !ok {
			continue
		}
		existing, _ := dst[k].(map[string]any)
		if existing == nil {
			existing = make(map[string]any, len(entries))
		}
		for id, sig := range entries {
			existing[id] = sig
		}
		dst[k] = existing
	}
	return dst
}

// Allowed returns the allowlisted subset of a metadata map that arrived from
// outside the engine. It is ForwardMessageMetadata's inbound twin: same
// allowlist, applied to a map a client authored rather than one a provider
// published.
func Allowed(src map[string]any) map[string]any {
	return ForwardMessageMetadata(src)
}

// lookupCall reads one call's entry out of a per-call metadata value.
//
// The dual-shape switch is load-bearing rather than defensive: the capture path
// publishes map[string]string, and the same value comes back as map[string]any
// with any-boxed strings once it has been through JSON -- which is every
// replayed and every persisted turn. A single-shape assertion would work live
// and silently return nothing on exactly the path the 400 appears on.
func lookupCall(v any, callID string) (string, bool) {
	switch m := v.(type) {
	case map[string]string:
		s, ok := m[callID]
		return s, ok && s != ""
	case map[string]any:
		s, ok := m[callID].(string)
		return s, ok && s != ""
	default:
		return "", false
	}
}

// asStringMap normalises either shape of a per-call metadata value into a
// string map, for the same reason lookupCall switches on both.
func asStringMap(v any) (map[string]string, bool) {
	switch m := v.(type) {
	case map[string]string:
		return m, true
	case map[string]any:
		out := make(map[string]string, len(m))
		for k, raw := range m {
			s, ok := raw.(string)
			if !ok || s == "" {
				continue
			}
			out[k] = s
		}
		return out, true
	default:
		return nil, false
	}
}
