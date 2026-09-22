package openai

import (
	"log/slog"

	"github.com/frankbardon/nexus/pkg/events"
)

// This file is the request serializer for OpenAI's Responses API
// (`/v1/responses`). It is a second serializer rather than a branch inside
// buildRequestBody, because the Responses request is not a renamed Chat
// Completions one:
//
//	Chat Completions                     Responses
//	-----------------------------------  ------------------------------------
//	messages: [...]                      input: [Item, ...]
//	assistant .tool_calls                separate `function_call` Items
//	role: tool message                   `function_call_output` Item, linked
//	                                     by call_id
//	{"type":"function","function":{..}}  {"type":"function","name":...} — flat
//	strict defaults off                  omitting strict ATTEMPTS strict mode
//	response_format                      text.format
//	max_tokens                           max_output_tokens
//	—                                    store, include
//
// Only the request half lives here. The reply parser (responses_reply.go), the
// SSE reader (responses_stream.go), the multimodal Item shapes and the endpoint
// builder are their own files; this one deliberately stops at the body.

// buildResponsesBody constructs a `/v1/responses` request body.
//
// Two choices worth stating outright, because both are places the Responses API
// differs from the chat one in a direction that would otherwise be inherited by
// accident:
//
//   - `store: false`, always. Nexus keeps its own conversation history — the
//     memory plugins and the in-process agent loops — so server-side state would
//     be a second source of truth for the same turn, and a divergent one the
//     moment a gate rewrites a response. It is also what makes reasoning Items
//     come back carrying `encrypted_content`, which is the only way reasoning
//     survives a tool round on this API.
//   - `strict` is written explicitly wherever the API has one, never omitted.
//     On Responses an absent `strict` *attempts* strict mode, the reverse of
//     Chat Completions — see responsesTools.
//
// `model` is always on the body here, including in Azure modes, where the chat
// path strips it because the deployment is in the URL path. Azure's Responses
// route is `/openai/v1/responses` with the deployment name carried in the body's
// `model` field instead, so stripping it there would send a request with no
// model at all. (That URL is E4-S5's; this is the body half of the same fact.)
func (p *Plugin) buildResponsesBody(model string, maxTokens int, req events.LLMRequest) map[string]any {
	return p.buildResponsesBodyWith(model, maxTokens, req, p.resolveReasoning(req))
}

// buildResponsesBodyWith is buildResponsesBody with the reasoning resolution
// already done.
//
// The split exists because handleRequest needs the resolved configuration for
// itself — whether summaries were asked for is what gates thinking.step
// emission on the reply — and resolving it twice would both duplicate the
// warning an invalid hand-set override raises and leave the two halves free to
// disagree. buildResponsesBody stays the whole-job entry point for everything
// that only wants a body.
func (p *Plugin) buildResponsesBodyWith(model string, maxTokens int, req events.LLMRequest, reasoning reasoningConfig) map[string]any {
	body := map[string]any{
		"model":             model,
		"max_output_tokens": maxTokens,
		"stream":            req.Stream,
		"store":             false,
	}

	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}

	body["input"] = p.buildResponsesInput(req.Messages)

	// Tool filtering is shared with the chat path — it operates on
	// events.ToolDef, before any wire shape exists.
	filteredTools := applyToolFilter(req.Tools, req.ToolFilter)
	if req.ToolChoice != nil && req.ToolChoice.Mode == "none" {
		filteredTools = nil
	}
	if tools := responsesTools(filteredTools); len(tools) > 0 {
		body["tools"] = tools
	}
	if tc := resolveResponsesToolChoice(req.ToolChoice, filteredTools); tc != nil {
		body["tool_choice"] = tc
	}

	if text := responsesText(req.ResponseFormat); text != nil {
		body["text"] = text
	}

	// Predicted outputs are a Chat Completions feature with no counterpart on
	// this surface, so the field is dropped rather than sent and rejected. The
	// agent that set it loses only the latency optimisation.
	if req.Prediction != "" && p.logger != nil {
		p.logger.Debug("openai: dropping prediction — the Responses API has no predicted-outputs field")
	}

	// Reasoning runs last so anything it strips (temperature) has already been
	// written. Gated on the operator's declared `reasoning.mode`, never on the
	// model id; resolveReasoning — which the caller has already run — is the
	// shared, surface-agnostic lookup that merges the serving role's block over
	// the plugin-level one and folds the role's `effort:` into the depth.
	applyResponsesReasoning(body, reasoning, p.logger)

	return body
}

// buildResponsesInput converts the conversation into the Responses `input`
// array.
//
// Where Chat Completions carries a turn's tool calls as a field on the assistant
// message and each result as a `role: tool` message, Responses flattens all
// three into sibling Items in one list, and the pairing is by `call_id` alone:
//
//	{"role":"assistant","content":"I'll look that up"}
//	{"type":"function_call","call_id":"call_1","name":"search","arguments":"{}"}
//	{"type":"function_call_output","call_id":"call_1","output":"..."}
//
// So an assistant message with both text and tool calls becomes several Items,
// and one with tool calls and no text becomes only the `function_call` ones —
// an empty message Item is not a thing the API accepts.
//
// Content is a plain string on every Item that carries no events.MessagePart,
// and only becomes a typed content-parts array when parts are present. That is
// deliberate rather than an unfinished half: the API accepts both, the string
// is the shape the overwhelming majority of turns have, and restructuring a
// text-only conversation into one-element arrays would make every captured
// request body harder to read for no behavioural gain. The array shapes live in
// responses_multimodal.go.
//
// A part that cannot be serialized degrades to the string Content with a
// warning rather than failing the turn — the same contract convertMessage makes
// on the chat path, for the same reason: the messages an agent loop assembles
// are not the operator's to fix mid-turn.
func (p *Plugin) buildResponsesInput(msgs []events.Message) []map[string]any {
	var items []map[string]any
	for _, msg := range msgs {
		switch msg.Role {
		case "tool":
			// A tool result is `output`, which is a string in the ordinary
			// case and a content-parts array when the tool returned images or
			// files — the Responses counterpart of the chat path's
			// multimodal `role: tool` message.
			item := map[string]any{
				"type":    "function_call_output",
				"call_id": msg.ToolCallID,
				"output":  msg.Content,
			}
			if parts := p.responsesParts(msg); parts != nil {
				item["output"] = parts
			}
			items = append(items, item)

		case "assistant":
			if parts := p.responsesParts(msg); parts != nil {
				items = append(items, map[string]any{
					"role":    "assistant",
					"content": parts,
				})
			} else if msg.Content != "" {
				items = append(items, map[string]any{
					"role":    "assistant",
					"content": msg.Content,
				})
			}
			for _, tc := range msg.ToolCalls {
				// Arguments is a JSON string on the wire. An empty one is not
				// valid JSON, and a replayed call that lost its arguments would
				// fail the whole request rather than the one call, so it is
				// normalised to the empty object.
				args := tc.Arguments
				if args == "" {
					args = "{}"
				}
				items = append(items, map[string]any{
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      tc.Name,
					"arguments": args,
				})
			}

		case "system":
			// Same prompt-registry pass the chat path makes. System stays an
			// ordinary Item in the list rather than being hoisted to the
			// top-level `instructions` field: hoisting would reorder it
			// relative to the rest of the conversation, and Nexus composes
			// system content from several plugins that expect their position.
			decorated := msg
			if p.prompts != nil {
				decorated.Content = p.prompts.Apply(msg.Content)
			}
			item := map[string]any{
				"role":    "system",
				"content": decorated.Content,
			}
			if parts := p.responsesParts(decorated); parts != nil {
				item["content"] = parts
			}
			items = append(items, item)

		default:
			item := map[string]any{
				"role":    msg.Role,
				"content": msg.Content,
			}
			if parts := p.responsesParts(msg); parts != nil {
				item["content"] = parts
			}
			items = append(items, item)
		}
	}
	return items
}

// responsesParts serializes a message's multimodal parts, or returns nil when
// there is nothing to serialize — no parts, every part suppressed by
// `multimodal.vision: false`, or a part this surface cannot carry. The last
// case is logged and degraded rather than propagated: a malformed image must
// not cost the turn.
func (p *Plugin) responsesParts(msg events.Message) []map[string]any {
	parts, err := buildResponsesContentParts(msg, p.multimodal)
	if err != nil {
		if p.logger != nil {
			p.logger.Warn("openai: failed to build Responses content parts; falling back to text",
				"role", msg.Role, "error", err)
		}
		return nil
	}
	return parts
}

// responsesTools converts tool definitions into the Responses API's flattened
// function shape.
//
// Chat Completions nests the definition under a `function` object; Responses
// puts `name`, `description` and `parameters` directly on the tool.
//
// `strict` is the trap, and it is answered deliberately rather than inherited.
// On Chat Completions an absent `strict` means off. On Responses an absent
// `strict` *attempts* strict mode, which imposes the Structured Outputs subset
// on the schema: every property listed in `required`, `additionalProperties:
// false` at every object level, and a restricted keyword set. Nexus tool
// schemas come from everywhere — shipped plugins, MCP servers, skills,
// operator-authored catalogs — and plenty of them are valid JSON Schema that
// strict mode rejects. Letting the default apply would turn a working tool
// catalog into an HTTP 400 for no reason the operator asked for, purely from
// changing `api:`. So every function tool carries an explicit `"strict": false`
// and the two surfaces behave identically.
//
// Structured *output* is the separate case, and the one place a caller did ask:
// events.ResponseFormat.Strict is forwarded verbatim by responsesText.
func responsesTools(tools []events.ToolDef) []map[string]any {
	var out []map[string]any
	for _, t := range tools {
		out = append(out, map[string]any{
			"type":        "function",
			"name":        t.Name,
			"description": t.Description,
			"parameters":  t.Parameters,
			"strict":      false,
		})
	}
	return out
}

// responsesText maps events.ResponseFormat onto the Responses `text.format`
// object, which is where structured output lives on this surface.
//
// The shape is flattened as well as moved: Chat Completions wraps the schema in
// a `json_schema` sub-object, Responses puts `name`, `schema` and `strict`
// directly on the format. `strict` is forwarded exactly as the caller set it —
// here it is a deliberate statement (the `nexus.gate.json_schema` gate and the
// structured-output path both set it on purpose), unlike the tool-definition
// case above.
//
// nil means send no `text` at all, which is the API's own default of free text.
func responsesText(rf *events.ResponseFormat) map[string]any {
	if rf == nil {
		return nil
	}
	switch rf.Type {
	case "json_object":
		return map[string]any{
			"format": map[string]any{"type": "json_object"},
		}
	case "json_schema":
		return map[string]any{
			"format": map[string]any{
				"type":   "json_schema",
				"name":   rf.Name,
				"schema": rf.Schema,
				"strict": rf.Strict,
			},
		}
	}
	// "text" is the API default — no field needed.
	return nil
}

// applyResponsesReasoning mutates a Responses body for reasoning-model calls.
//
// Same gate as the chat path — the operator's declared `reasoning.mode`, never
// the model id — and the same sampling-parameter strip, which is shared rather
// than restated. What differs is the wire shape and the reach: Chat Completions
// has a single top-level `reasoning_effort` scalar and no summary field at all,
// while Responses takes a `reasoning` object that carries both.
//
//	"reasoning": {"effort": "high", "summary": "auto"}
//
// So `reasoning.summary` — parsed and validated since E3-S1, and warned about at
// boot because it could never reach the wire — reaches it here. The boot warning
// is now conditional on the effective surface; see warnSummaryOnChatCompletions.
//
// A mode of effort with neither an effort nor a summary resolved writes no
// `reasoning` key: an empty object says nothing the model's own defaults do not
// already say.
func applyResponsesReasoning(body map[string]any, cfg reasoningConfig, logger *slog.Logger) {
	switch cfg.Mode {
	case "", reasoningModeOff:
		return
	}
	stripReasoningRejectedFields(body, cfg.Mode, logger)

	if cfg.Mode != reasoningModeEffort {
		return
	}
	reasoning := map[string]any{}
	if cfg.Effort != "" {
		reasoning["effort"] = cfg.Effort
	}
	if cfg.Summary != "" {
		reasoning["summary"] = cfg.Summary
	}
	if len(reasoning) > 0 {
		body["reasoning"] = reasoning
	}
}
