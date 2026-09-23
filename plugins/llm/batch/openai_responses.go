package batch

import (
	"encoding/json"
	"fmt"

	"github.com/frankbardon/nexus/pkg/events"
)

// The Responses half of the OpenAI batch adapter: one body builder and one
// reply decoder, siblings of buildOpenAIChatBody and decodeOpenAIChatBody in
// openai.go.
//
// They are separate functions rather than branches because the Responses
// request and reply are not renamed Chat Completions ones:
//
//	Chat Completions                      Responses
//	------------------------------------  ------------------------------------
//	messages: [...]                       input: [Item, ...]
//	assistant .tool_calls                 separate `function_call` Items
//	role: tool message                    `function_call_output` Item, by call_id
//	{"type":"function","function":{..}}   {"type":"function","name":...} — flat
//	strict defaults off                   omitting strict ATTEMPTS strict mode
//	response_format                       text.format
//	max_tokens                            max_output_tokens
//	choices[0].message.content            `message` Items of `output_text` parts
//	choices[0].message.tool_calls         `function_call` Items
//	choices[0].finish_reason              status + incomplete_details.reason
//	usage.prompt_tokens/completion_tokens usage.input_tokens/output_tokens
//
// Both mirror nexus.llm.openai's responses.go and responses_reply.go, which
// remain the authority on every shape here; see openai_surface.go for why the
// code is mirrored rather than shared. Scope is the coordinator's existing
// text-only one — what is deliberately NOT carried is listed on
// buildOpenAIResponsesBody.

// buildOpenAIResponsesBody is the LLMRequest -> Responses adapter for one batch
// line. It is buildOpenAIChatBody's sibling and keeps the same scope: text
// messages, function tools, structured output, plus — new here, and the reason
// this surface exists — the resolved reasoning configuration.
//
// Two Responses-specific choices, both taken to match nexus.llm.openai rather
// than inherited by accident:
//
//   - `store: false`. Nexus keeps its own history, so server-side state would
//     be a second and divergent source of truth for the same turn.
//   - every function tool carries an explicit `"strict": false`. On this
//     surface an ABSENT `strict` attempts strict mode, the reverse of Chat
//     Completions, and Nexus tool schemas come from everywhere — a working tool
//     catalog must not become an HTTP 400 purely from changing `api:`.
//
// `stream` is never written: a batch line is not a stream.
//
// NOT carried, matching the coordinator's documented v1 scope and the chat
// builder beside it: multimodal content parts, prompt-registry decoration,
// predicted outputs (no counterpart on this surface anyway), tool_choice, and
// the replay of a previous turn's encrypted `reasoning` Items. The last one is
// the notable omission — it is what keeps reasoning alive ACROSS tool rounds on
// the synchronous path — and it is out of scope because a batch line is a
// one-shot request, not a loop: there is no next round inside a batch for the
// Items to reach. A multi-round conversation belongs on the synchronous
// llm.request path.
func buildOpenAIResponsesBody(req events.LLMRequest, defaultMaxTokens int, reasoning openaiReasoning) (map[string]any, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("model is required for openai batch requests")
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	body := map[string]any{
		"model": req.Model,
		"store": false,
	}
	if maxTokens > 0 {
		body["max_output_tokens"] = maxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}

	body["input"] = buildOpenAIResponsesInput(req.Messages)

	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type":        "function",
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
				"strict":      false,
			})
		}
		body["tools"] = tools
	}

	if rf := req.ResponseFormat; rf != nil {
		switch rf.Type {
		case "json_object":
			body["text"] = map[string]any{"format": map[string]any{"type": "json_object"}}
		case "json_schema":
			body["text"] = map[string]any{
				"format": map[string]any{
					"type":   "json_schema",
					"name":   rf.Name,
					"schema": rf.Schema,
					"strict": rf.Strict,
				},
			}
		}
	}

	// Last, so anything it strips (temperature) has already been written.
	applyOpenAIResponsesReasoning(body, reasoning)

	return body, nil
}

// buildOpenAIResponsesInput flattens the conversation into the `input` array.
//
// Where Chat Completions carries a turn's tool calls as a field on the
// assistant message and each result as a `role: tool` message, Responses makes
// all three sibling Items and pairs them by `call_id` alone. An assistant turn
// with both text and tool calls therefore becomes several Items, and one with
// calls and no text becomes only the `function_call` ones — an empty message
// Item is not something the API accepts.
func buildOpenAIResponsesInput(msgs []events.Message) []map[string]any {
	items := make([]map[string]any, 0, len(msgs))
	for _, msg := range msgs {
		switch msg.Role {
		case "tool":
			items = append(items, map[string]any{
				"type":    "function_call_output",
				"call_id": msg.ToolCallID,
				"output":  msg.Content,
			})

		case "assistant":
			if msg.Content != "" {
				items = append(items, map[string]any{
					"role":    "assistant",
					"content": msg.Content,
				})
			}
			for _, tc := range msg.ToolCalls {
				// Arguments is a JSON string on the wire and an empty one is
				// not valid JSON, so a replayed call that lost its arguments
				// is normalised rather than failing the whole line.
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

		default:
			// system, user and anything else stay ordinary role/content Items.
			items = append(items, map[string]any{
				"role":    msg.Role,
				"content": msg.Content,
			})
		}
	}
	return items
}

// openaiResultBodyIsResponses reports whether one result line's body is a
// Responses reply rather than a Chat Completions one.
//
// The shape is sniffed rather than remembered. The alternative — persisting the
// surface on batchState — would decode correctly only for batches submitted
// after this change, and the coordinator's whole restart story is that it
// resumes state files written by an earlier process. Sniffing needs no
// migration and cannot disagree with the bytes in front of it.
//
// `object` is the reliable discriminator (`response` vs `chat.completion`);
// the `choices`/`output` fallbacks cover a proxy that omits it.
func openaiResultBodyIsResponses(raw json.RawMessage) bool {
	var probe struct {
		Object  string          `json:"object"`
		Output  json.RawMessage `json:"output"`
		Choices json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	switch probe.Object {
	case "response":
		return true
	case "chat.completion":
		return false
	}
	if len(probe.Choices) > 0 {
		return false
	}
	return len(probe.Output) > 0
}

// decodeOpenAIResponsesBody maps a Responses reply onto our LLMResponse.
// Mirrors nexus.llm.openai's convertResponsesReply.
//
// The output array is walked in order because order is meaningful on this
// surface: one turn can interleave reasoning, text and several function calls.
// `reasoning` Items are counted for their tokens and otherwise stepped over —
// see buildOpenAIResponsesBody on why a batch line has no next round to replay
// them into.
func decodeOpenAIResponsesBody(raw json.RawMessage) (*events.LLMResponse, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	var reply struct {
		ID                string `json:"id"`
		Model             string `json:"model"`
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			} `json:"content"`
			ID        string `json:"id"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			TotalTokens        int `json:"total_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, err
	}

	// A Responses run can fail inside an HTTP 200 line: the request was
	// accepted, the run was not completed. Reporting it as an error is what
	// keeps a failed line from arriving as a successful empty one.
	if reply.Error != nil && (reply.Error.Code != "" || reply.Error.Message != "") {
		return nil, fmt.Errorf("responses run failed (%s): %s", reply.Error.Code, reply.Error.Message)
	}

	out := &events.LLMResponse{
		SchemaVersion: events.LLMResponseVersion,
		Model:         reply.Model,
		Usage: events.Usage{
			PromptTokens:     reply.Usage.InputTokens,
			CompletionTokens: reply.Usage.OutputTokens,
			TotalTokens:      reply.Usage.TotalTokens,
			CachedTokens:     reply.Usage.InputTokensDetails.CachedTokens,
			ReasoningTokens:  reply.Usage.OutputTokensDetails.ReasoningTokens,
		},
	}

	var content []byte
	for _, item := range reply.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					content = append(content, part.Text...)
				}
			}
		case "function_call":
			// call_id is the pairing handle; the Item id is a fallback only
			// because a call with no id at all cannot be answered.
			id := item.CallID
			if id == "" {
				id = item.ID
			}
			args := item.Arguments
			if args == "" {
				args = "{}"
			}
			out.ToolCalls = append(out.ToolCalls, events.ToolCallRequest{
				ID:        id,
				Name:      item.Name,
				Arguments: args,
			})
		}
	}
	out.Content = string(content)

	incomplete := ""
	if reply.IncompleteDetails != nil {
		incomplete = reply.IncompleteDetails.Reason
	}
	out.FinishReason = openaiResponsesFinishReason(reply.Status, incomplete, len(out.ToolCalls) > 0)
	return out, nil
}

// openaiResponsesFinishReason translates the Responses lifecycle into the
// finish-reason vocabulary the chat path already publishes, so a role flipping
// `api:` does not change what llm.batch.results says. Mirrors
// nexus.llm.openai's responsesFinishReason. A status this does not know is
// passed through, which is more useful than flattening it to "stop".
func openaiResponsesFinishReason(status, incompleteReason string, hasToolCalls bool) string {
	switch status {
	case "completed", "":
		if hasToolCalls {
			return "tool_calls"
		}
		return "stop"
	case "incomplete":
		switch incompleteReason {
		case "max_output_tokens":
			return "length"
		case "":
			return "incomplete"
		default:
			return incompleteReason
		}
	default:
		return status
	}
}
