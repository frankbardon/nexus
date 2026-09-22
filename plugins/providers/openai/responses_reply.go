package openai

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file is the non-streaming reply parser for OpenAI's Responses API. It is
// a second parser rather than a branch inside convertAPIResponse, for the same
// reason responses.go is a second serializer: the reply is not a renamed Chat
// Completions one.
//
//	Chat Completions                     Responses
//	-----------------------------------  ------------------------------------
//	choices[0].message.content           `message` Items whose content parts
//	                                     are `output_text`
//	choices[0].message.tool_calls        separate `function_call` Items, keyed
//	                                     by call_id
//	—                                    `reasoning` Items carrying
//	                                     encrypted_content
//	choices[0].finish_reason             top-level `status` plus
//	                                     incomplete_details.reason
//	usage.prompt_tokens/completion_tokens usage.input_tokens/output_tokens
//
// The SSE reader is responses_stream.go's; everything here stops at a complete
// JSON body. The two share this file's Item types, its usage mapping and its
// finish-reason mapping, so a streamed turn and a non-streamed one of the same
// run produce the same LLMResponse.

// reasoningItemsMetaKey is where a turn's `reasoning` Items are stashed on
// events.LLMResponse.Metadata, as the decoded Items verbatim.
//
// They are captured rather than rendered because they are continuity state, not
// output: under `store: false` — which buildResponsesBody always sends — each
// one carries `encrypted_content`, and the NEXT request must replay every Item
// of the previous turn verbatim or the model loses its reasoning across a tool
// round. That is structurally the Anthropic `thinking_blocks` and Gemini
// `gemini_thought_signatures` problem, so this key belongs in the
// pkg/roundtrip allowlist that carries those onto the stored assistant
// Message — E5-S2 adds it there and writes the replay half. Until then the
// data survives the parser and goes no further, which is why dropping it here
// would be the expensive mistake: nothing downstream can reconstruct it.
//
// The shape is []map[string]any — the decoded Item, not a narrowed struct — so
// that every field the API puts on a reasoning Item (`id`, `encrypted_content`,
// `summary`, and anything added later) survives a JSON round trip through
// persisted history without this file needing to know about it.
//
// NOTE: like the Gemini key, this constant is duplicated as a bare string
// literal in pkg/roundtrip once E5-S2 lands — nothing outside a provider may
// import one. Change one spelling and you must change the other.
const reasoningItemsMetaKey = "openai_reasoning_items"

// Responses API reply types.

type responsesReply struct {
	ID                string               `json:"id"`
	Object            string               `json:"object"`
	Model             string               `json:"model"`
	Status            string               `json:"status"`
	IncompleteDetails *responsesIncomplete `json:"incomplete_details"`
	Error             *responsesError      `json:"error"`
	// Output Items stay raw until their type is known: a `reasoning` Item is
	// kept whole, and decoding into a struct first would throw away the
	// fields that struct does not name.
	Output []json.RawMessage `json:"output"`
	Usage  responsesUsage    `json:"usage"`
}

type responsesIncomplete struct {
	Reason string `json:"reason"`
}

type responsesError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// responsesOutputItem names the fields shared across the Item types this parser
// acts on. Items are a tagged union on `type`, so only the fields belonging to
// the tag are populated on any one Item.
type responsesOutputItem struct {
	Type string `json:"type"`

	// message
	Role    string                 `json:"role"`
	Content []responsesContentPart `json:"content"`

	// function_call. `id` is the Item's own id; `call_id` is the handle a
	// later function_call_output references, and is the one that must become
	// ToolCallRequest.ID — buildResponsesInput writes it back out as call_id.
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type responsesContentPart struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
		AudioTokens  int `json:"audio_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
		AudioTokens     int `json:"audio_tokens"`
	} `json:"output_tokens_details"`
}

// handleResponsesSyncResponse is handleSyncResponse's counterpart on the
// Responses surface. handleRequest picks between them on the surface it already
// resolved, exactly as it picks between the two body builders.
//
// reasoning is the resolved reasoning configuration for this turn, carried in
// from handleRequest for the same reason the streaming reader takes one: it is
// what decides whether reasoning summaries were asked for, and therefore
// whether the summaries riding the reasoning Items may become thinking.step
// events.
func (p *Plugin) handleResponsesSyncResponse(body io.Reader, requestID string, meta map[string]any, tags map[string]string, reasoning reasoningConfig) {
	var reply responsesReply
	if err := json.NewDecoder(body).Decode(&reply); err != nil {
		p.emitError(fmt.Errorf("openai: failed to decode responses reply: %w", err))
		return
	}

	// A Responses turn can fail inside an HTTP 200: the request was accepted,
	// the run was not completed. Surfacing it as an error rather than as an
	// empty response is what lets the fallback coordinator see it, since it
	// watches core.error and never llm.response.
	if reply.Error != nil {
		p.emitErrorInfo(events.ErrorInfo{
			SchemaVersion: events.ErrorInfoVersion,
			Err:           fmt.Errorf("openai: responses run failed (%s): %s", reply.Error.Code, reply.Error.Message),
			Retryable:     false,
			RequestMeta:   meta,
			RequestID:     requestID,
		})
		return
	}

	resp := p.convertResponsesReply(reply)
	resp.RequestID = requestID

	// Reasoning summaries are emitted here rather than inside
	// convertResponsesReply because that function is shared with the streaming
	// path, which settles its turn from the same terminal snapshot after
	// already having emitted every delta — emitting there would double every
	// step on a streamed turn.
	if reasoning.summaryRequested() {
		p.emitReasoningItemSummaries(reply.ID, resp.Metadata)
	}

	// Same merge order as the chat path: request-passthrough metadata (e.g.
	// _structured_output) wins over anything the parser attached.
	resp.Metadata = mergeMetadata(resp.Metadata, meta)
	resp.Tags = tags

	engine.PublishLLMResponse(p.bus, resp)
}

// emitReasoningItemSummaries publishes a thinking.step per summary part of a
// non-streamed turn.
//
// The two surfaces carry the same text in two places. A streamed turn gets it
// as its own event family (response.reasoning_summary_text.delta and friends),
// which is why responses_stream.go accumulates it under
// reasoningSummaryMetaKey; a non-streamed one never sees those events, and the
// text arrives only inside each reasoning Item's `summary` array:
//
//	{"type":"reasoning","id":"rs_1","encrypted_content":"…",
//	 "summary":[{"type":"summary_text","text":"Checking the units first."}]}
//
// So this reads back out of the Items the parser already captured rather than
// asking convertResponsesReply to fork. The Items themselves are left exactly
// as they were — they are replay state that must survive verbatim (E5-S2), and
// this function only looks.
//
// Index is the part's position in its Item's summary array, which is the
// non-streaming counterpart of summary_index. A turn carrying several reasoning
// Items therefore restarts the numbering per Item, exactly as the streamed
// summary_index does.
//
// Everything here is a type assertion away from a nil map: the Items come from
// JSON the provider does not control, so a shape this does not recognise is
// skipped rather than panicking a live turn.
func (p *Plugin) emitReasoningItemSummaries(turnID string, meta map[string]any) {
	items, ok := meta[reasoningItemsMetaKey].([]map[string]any)
	if !ok {
		return
	}
	for _, item := range items {
		parts, ok := item["summary"].([]any)
		if !ok {
			continue
		}
		for i, raw := range parts {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			text, _ := part["text"].(string)
			p.emitReasoningStep(turnID, i, text)
		}
	}
}

// convertResponsesReply turns a decoded Responses reply into an LLMResponse.
//
// The output array is walked once, in order, because order is meaningful on
// this surface in a way it is not on the chat one: a turn can interleave
// reasoning, text and several function calls, and the reasoning Items must be
// replayed in the sequence they arrived.
func (p *Plugin) convertResponsesReply(reply responsesReply) events.LLMResponse {
	usage := responsesUsageToEvents(reply.Usage)

	resp := events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Model:         reply.Model,
		Usage:         usage,
		CostUSD:       p.costForModel(reply.Model, usage),
	}

	var content []byte
	var reasoningItems []map[string]any

	for _, raw := range reply.Output {
		var item responsesOutputItem
		if err := json.Unmarshal(raw, &item); err != nil {
			p.logDebug("openai: skipping undecodable responses output item", "error", err)
			continue
		}

		switch item.Type {
		case "message":
			for _, part := range item.Content {
				switch part.Type {
				case "output_text":
					content = append(content, part.Text...)
				case "refusal":
					// The chat path drops refusals too (its content is simply
					// null), so surfacing one here would make the same model
					// behave differently on the two surfaces.
					p.logDebug("openai: responses reply carried a refusal part", "refusal", part.Refusal)
				}
			}

		case "function_call":
			// call_id is the pairing handle; the Item id is the fallback only
			// because a call with no id at all cannot be answered.
			id := item.CallID
			if id == "" {
				id = item.ID
			}
			args := item.Arguments
			if args == "" {
				args = "{}"
			}
			resp.ToolCalls = append(resp.ToolCalls, events.ToolCallRequest{
				ID:        id,
				Name:      item.Name,
				Arguments: args,
			})

		case "reasoning":
			// Kept whole and undecoded-into-a-struct: see reasoningItemsMetaKey.
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				p.logDebug("openai: skipping undecodable reasoning item", "error", err)
				continue
			}
			reasoningItems = append(reasoningItems, decoded)

		default:
			// Server-side tool Items (web_search_call, image_generation_call,
			// …) are not something this provider asks for, so an unknown type
			// is noted and stepped over rather than failing the turn.
			p.logDebug("openai: ignoring unhandled responses output item", "type", item.Type)
		}
	}

	resp.Content = string(content)
	resp.FinishReason = responsesFinishReason(reply, len(resp.ToolCalls) > 0)

	if len(reasoningItems) > 0 {
		resp.Metadata = map[string]any{reasoningItemsMetaKey: reasoningItems}
	}

	return resp
}

// responsesUsageToEvents maps the Responses usage block onto events.Usage.
//
// The names differ from the chat block (input/output rather than
// prompt/completion) but the meaning does not: reasoning tokens are billed as
// output and are already counted inside output_tokens, so they need no separate
// pricing field here either.
func responsesUsageToEvents(u responsesUsage) events.Usage {
	usage := events.Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens,
		CachedTokens:     u.InputTokensDetails.CachedTokens,
		ReasoningTokens:  u.OutputTokensDetails.ReasoningTokens,
	}
	// Modality detail is the same set of counters under different names, so it
	// is translated onto the chat block and handed to the one helper that owns
	// the modality key vocabulary, rather than restating that vocabulary here.
	var shim apiUsage
	shim.PromptTokensDetails.AudioTokens = u.InputTokensDetails.AudioTokens
	shim.CompletionTokensDetails.AudioTokens = u.OutputTokensDetails.AudioTokens
	if mb := openaiModalityBreakdown(shim); len(mb) > 0 {
		usage.ModalityBreakdown = mb
	}
	return usage
}

// responsesFinishReason maps the Responses lifecycle onto the finish-reason
// vocabulary the chat path already publishes.
//
// Responses reports completion as a run `status` plus, when the run stopped
// early, an `incomplete_details.reason`; Chat Completions reports it as one
// per-choice string. They are translated rather than passed through because
// both surfaces are the same provider: an operator flipping `api:` should not
// find `llm.response.FinishReason` suddenly saying "completed" where it used to
// say "stop", or "max_output_tokens" where it used to say "length".
//
// A status this doesn't know is passed through verbatim, which is more useful
// than flattening it to "stop".
func responsesFinishReason(reply responsesReply, hasToolCalls bool) string {
	switch reply.Status {
	case "completed", "":
		if hasToolCalls {
			return "tool_calls"
		}
		return "stop"
	case "incomplete":
		reason := ""
		if reply.IncompleteDetails != nil {
			reason = reply.IncompleteDetails.Reason
		}
		switch reason {
		case "max_output_tokens":
			return "length"
		case "":
			return "incomplete"
		default:
			// content_filter and anything added later already read as a
			// finish reason.
			return reason
		}
	default:
		return reply.Status
	}
}

// logDebug is a nil-safe logger shim: the parser is exercised directly by tests
// that construct a Plugin without one.
func (p *Plugin) logDebug(msg string, args ...any) {
	if p.logger != nil {
		p.logger.Debug(msg, args...)
	}
}

// logWarn is logDebug's counterpart for the one thing the Responses path warns
// about on a live turn: an SSE frame it could not decode.
func (p *Plugin) logWarn(msg string, args ...any) {
	if p.logger != nil {
		p.logger.Warn(msg, args...)
	}
}
