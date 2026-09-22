package openai

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file is the SSE reader for OpenAI's Responses API. It is a third
// implementation alongside the request serializer (responses.go) and the
// non-streaming reply parser (responses_reply.go), for the same reason both of
// those are separate from their chat counterparts: the wire is not a renamed
// version of the chat one.
//
//	Chat Completions                      Responses
//	------------------------------------  ------------------------------------
//	one `chat.completion.chunk` shape,     ~40 typed events, each naming itself
//	  discriminated by which delta field     in a `type` field
//	  happens to be populated
//	choices[0].delta.content               response.output_text.delta
//	choices[0].delta.tool_calls[].         response.function_call_arguments
//	  function.arguments                     .delta / .done, with the tool's
//	                                         name and call_id arriving
//	                                         separately on
//	                                         response.output_item.added
//	choices[0].finish_reason               the terminal lifecycle event's
//	                                         `response.status`
//	usage on the final chunk, and only     usage on response.completed, always
//	  when stream_options asks for it
//	data: [DONE]                           response.completed (no sentinel)
//
// What does NOT differ is the publication contract: every text delta goes
// through engine.StreamPublisher and the terminal response through
// engine.PublishLLMResponse, so a gate that holds, redacts or blocks a segment
// behaves identically whichever surface produced it.

// reasoningSummaryMetaKey is where a streamed turn's reasoning summary text is
// stashed on events.LLMResponse.Metadata, as an ordered []string — one entry
// per summary part, in the order the API opened them.
//
// It is display material, not continuity state, and the distinction is the
// whole reason this key is separate from reasoningItemsMetaKey. The summary is
// prose about the model's reasoning, safe to show a user and useless to replay;
// the reasoning *Items* are opaque `encrypted_content` blobs that must be
// replayed verbatim and would mean nothing to a reader. E5-S1 turns this key
// into thinking.step events; E5-S2 replays the other one.
//
// Two spellings are accumulated into it — response.reasoning_summary_text.delta
// and response.reasoning_text.delta — because at least one model streams only
// the latter, with no summary events at all.
const reasoningSummaryMetaKey = "openai_reasoning_summary"

// responsesStreamEvent is the union of the Responses streaming events this
// reader acts on. Every event names itself in `type`, so one struct with the
// superset of fields is cheaper and more tolerant than a decode-twice
// dispatch: an event carrying fields this reader ignores costs nothing, and an
// event type it has never heard of still decodes.
type responsesStreamEvent struct {
	Type string `json:"type"`

	// Lifecycle events (response.created / .in_progress / .completed /
	// .incomplete / .failed) carry the whole run object, which on the terminal
	// ones is byte-identical to a non-streaming reply — including the
	// `output` array this reader takes its reasoning Items from.
	Response *responsesReply `json:"response"`

	// Item-scoped addressing, on every delta event. output_index is what this
	// reader keys on: it is the position in the run's `output` array, so it
	// both identifies the Item and orders it against its siblings, which
	// item_id alone does not. item_id is decoded for completeness and for
	// anything that wants to correlate against the terminal snapshot.
	ItemID      string `json:"item_id"`
	OutputIndex int    `json:"output_index"`

	// response.output_item.added / .done carry the Item itself. On a
	// function_call that is where `name` and `call_id` arrive — the argument
	// deltas that follow carry neither.
	Item *responsesOutputItem `json:"item"`

	// Delta text: output text, function-call arguments and reasoning summary
	// all use the same field name.
	Delta string `json:"delta"`

	// response.function_call_arguments.done carries the complete arguments
	// string, which supersedes whatever the deltas accumulated.
	Arguments string `json:"arguments"`

	// summary_index orders reasoning summary parts within one reasoning Item.
	SummaryIndex int `json:"summary_index"`

	// The `error` event is flat rather than nested under `error`, unlike the
	// non-streaming reply's failure object.
	Code    string `json:"code"`
	Message string `json:"message"`
}

// responsesStreamTool accumulates one function call across the events that
// build it.
type responsesStreamTool struct {
	callID string
	name   string
	// args is what the deltas have accumulated so far. It is the fallback:
	// response.function_call_arguments.done and response.output_item.done both
	// carry the complete string, and either one supersedes it.
	args  strings.Builder
	final string
}

// arguments returns the tool call's arguments, preferring a completed string
// over the accumulated deltas. An empty result becomes the empty object: a
// tool call whose arguments are not valid JSON fails the whole next request
// rather than the one call.
func (t *responsesStreamTool) arguments() string {
	if t.final != "" {
		return t.final
	}
	if s := t.args.String(); s != "" {
		return s
	}
	return "{}"
}

// summaryKey addresses one reasoning summary part. Parts are ordered by the
// Item they belong to and then by their index within it, which is the order a
// reader should see them in.
type summaryKey struct {
	outputIndex  int
	summaryIndex int
}

// responsesStreamState is everything one turn's SSE loop accumulates.
type responsesStreamState struct {
	turnID    string
	requestID string
	model     string

	// text is the model's output text verbatim. It diverges from what the
	// publisher released the moment a gate redacts, which is deliberate: the
	// response-level hook still sees what the model actually said.
	text strings.Builder

	// Function calls by output_index. Ordering is by index rather than by
	// arrival because the API interleaves several calls' argument deltas.
	toolIdx []int
	tools   map[int]*responsesStreamTool

	summaryIdx []summaryKey
	summaries  map[summaryKey]*strings.Builder

	// snapshot is the run object from the terminal lifecycle event. It is the
	// authority on usage, status and — crucially — the reasoning Items, which
	// carry encrypted_content and exist nowhere in the delta stream. Losing
	// them in the stream converter is the documented failure mode of several
	// other SDKs, so they are taken from here rather than reconstructed.
	snapshot *responsesReply

	// fatal latches when the run failed. The turn then publishes core.error
	// and no llm.response, exactly as the non-streaming path does, because the
	// fallback coordinator watches core.error and would never see a failure
	// dressed up as an empty response.
	fatal bool

	pub *engine.StreamPublisher
}

// ensurePub creates the turn's publisher on first use.
//
// It is lazy for the same reason the chat reader's is: a stream that yields no
// parsable event should publish nothing at all, and the turn ID only exists
// once the first lifecycle event has arrived.
func (st *responsesStreamState) ensurePub(bus engine.EventBus) *engine.StreamPublisher {
	if st.pub == nil {
		st.pub = engine.NewStreamPublisher(bus, st.turnID, st.requestID)
	}
	return st.pub
}

// tool returns the accumulator for one output index, allocating on first
// sight. Allocation here rather than only on response.output_item.added means
// a stream whose `added` event was missed still collects its arguments; the
// name and call_id then come from response.output_item.done or from the
// terminal snapshot.
func (st *responsesStreamState) tool(outputIndex int) *responsesStreamTool {
	if st.tools == nil {
		st.tools = make(map[int]*responsesStreamTool)
	}
	t, ok := st.tools[outputIndex]
	if !ok {
		t = &responsesStreamTool{}
		st.tools[outputIndex] = t
		st.toolIdx = append(st.toolIdx, outputIndex)
	}
	return t
}

// toolCalls renders the accumulated function calls in output order.
func (st *responsesStreamState) toolCalls() []events.ToolCallRequest {
	if len(st.toolIdx) == 0 {
		return nil
	}
	idx := append([]int(nil), st.toolIdx...)
	sort.Ints(idx)
	out := make([]events.ToolCallRequest, 0, len(idx))
	for _, i := range idx {
		t := st.tools[i]
		id := t.callID
		if id == "" {
			// callID has already fallen back to the Item's own id. Reaching
			// here means the stream never carried either, and a call with no
			// handle at all cannot be paired with its function_call_output —
			// so the output index, which is unique within the turn, stands in.
			id = fmt.Sprintf("call_%d", i)
		}
		out = append(out, events.ToolCallRequest{ID: id, Name: t.name, Arguments: t.arguments()})
	}
	return out
}

// addSummary accumulates one reasoning summary delta under its part.
func (st *responsesStreamState) addSummary(k summaryKey, delta string) {
	if delta == "" {
		return
	}
	if st.summaries == nil {
		st.summaries = make(map[summaryKey]*strings.Builder)
	}
	b, ok := st.summaries[k]
	if !ok {
		b = &strings.Builder{}
		st.summaries[k] = b
		st.summaryIdx = append(st.summaryIdx, k)
	}
	b.WriteString(delta)
}

// summaries renders the accumulated reasoning summary parts in Item order.
func (st *responsesStreamState) renderSummaries() []string {
	if len(st.summaryIdx) == 0 {
		return nil
	}
	keys := append([]summaryKey(nil), st.summaryIdx...)
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].outputIndex != keys[j].outputIndex {
			return keys[i].outputIndex < keys[j].outputIndex
		}
		return keys[i].summaryIndex < keys[j].summaryIndex
	})
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if s := st.summaries[k].String(); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// handleResponsesStreamResponse is handleStreamResponse's counterpart on the
// Responses surface. handleRequest picks between them on the surface it already
// resolved, exactly as it picks between the two body builders and the two
// non-streaming parsers.
func (p *Plugin) handleResponsesStreamResponse(body io.Reader, requestID string, requestMeta map[string]any, tags map[string]string) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	st := &responsesStreamState{requestID: requestID}
	// Error-path backstop: a stream abandoned before the explicit Close below
	// would otherwise leave a gate's hold outstanding and every UI showing a
	// review indicator nothing ever clears. Close is idempotent.
	defer func() { st.pub.Close() }()

	// eventName is the SSE `event:` line, kept only as a fallback for a data
	// payload that somehow carries no `type` of its own.
	eventName := ""

	for scanner.Scan() {
		line := scanner.Text()

		if name, ok := strings.CutPrefix(line, "event: "); ok {
			eventName = name
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		name := eventName
		eventName = ""

		// The Responses stream terminates on response.completed rather than a
		// sentinel, but a proxy in the middle may still send one.
		if data == "[DONE]" {
			break
		}

		var ev responsesStreamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			p.logWarn("openai: failed to parse responses stream event", "error", err)
			continue
		}
		if ev.Type == "" {
			ev.Type = name
		}

		p.applyResponsesStreamEvent(&ev, st)
		if st.fatal {
			// The run failed. Nothing further on this stream can be published,
			// and the error has already gone out.
			return
		}
	}

	if err := scanner.Err(); err != nil {
		p.emitError(fmt.Errorf("openai: responses stream read error: %w", err))
	}

	p.publishResponsesStream(st, requestID, requestMeta, tags)
}

// applyResponsesStreamEvent folds one SSE event into the turn's state.
func (p *Plugin) applyResponsesStreamEvent(ev *responsesStreamEvent, st *responsesStreamState) {
	switch ev.Type {

	// --- lifecycle ---------------------------------------------------------

	case "response.created", "response.in_progress", "response.queued":
		if ev.Response != nil {
			if st.turnID == "" {
				st.turnID = ev.Response.ID
			}
			if ev.Response.Model != "" {
				st.model = ev.Response.Model
			}
		}
		// Created is where the turn acquires an identity, so the publisher is
		// built here and every later delta finds it ready.
		st.ensurePub(p.bus)

	case "response.completed", "response.incomplete":
		st.snapshot = ev.Response
		if ev.Response != nil && ev.Response.Model != "" {
			st.model = ev.Response.Model
		}

	case "response.failed":
		st.snapshot = ev.Response
		code, msg := "failed", "the responses run failed"
		if ev.Response != nil && ev.Response.Error != nil {
			code, msg = ev.Response.Error.Code, ev.Response.Error.Message
		}
		p.failResponsesStream(st, code, msg)

	case "error":
		// A transport-level error event, flat rather than nested.
		p.failResponsesStream(st, ev.Code, ev.Message)

	// --- output text -------------------------------------------------------

	case "response.output_text.delta":
		if ev.Delta == "" {
			return
		}
		st.text.WriteString(ev.Delta)
		st.ensurePub(p.bus).Text(ev.Delta)

	case "response.output_text.done":
		// Carries the whole text again; accumulating it would double every
		// turn. The deltas are the record.

	// --- function calls ----------------------------------------------------

	case "response.output_item.added":
		if ev.Item == nil || ev.Item.Type != "function_call" {
			return
		}
		t := st.tool(ev.OutputIndex)
		t.callID = firstNonEmpty(ev.Item.CallID, ev.Item.ID, t.callID)
		if ev.Item.Name != "" {
			t.name = ev.Item.Name
		}
		// An `added` Item may already carry a partial arguments string.
		t.args.WriteString(ev.Item.Arguments)

	case "response.function_call_arguments.delta":
		st.tool(ev.OutputIndex).args.WriteString(ev.Delta)

	case "response.function_call_arguments.done":
		if ev.Arguments != "" {
			st.tool(ev.OutputIndex).final = ev.Arguments
		}

	case "response.output_item.done":
		if ev.Item == nil || ev.Item.Type != "function_call" {
			return
		}
		t := st.tool(ev.OutputIndex)
		t.callID = firstNonEmpty(ev.Item.CallID, ev.Item.ID, t.callID)
		if ev.Item.Name != "" {
			t.name = ev.Item.Name
		}
		if ev.Item.Arguments != "" {
			t.final = ev.Item.Arguments
		}

	// --- reasoning ---------------------------------------------------------

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		st.addSummary(summaryKey{outputIndex: ev.OutputIndex, summaryIndex: ev.SummaryIndex}, ev.Delta)

	default:
		// Part-boundary events, server-side tool events and anything the API
		// adds later. Stepped over rather than failing the turn — the same
		// tolerance the non-streaming parser shows unknown Item types.
		p.logDebug("openai: ignoring unhandled responses stream event", "type", ev.Type)
	}
}

// failResponsesStream settles a run that failed mid-stream.
//
// The turn publishes core.error and no llm.response, which is what the
// non-streaming path does with the same failure and what the fallback
// coordinator needs: it watches core.error and never llm.response, so a
// failure dressed up as an empty response would silently consume the turn.
// llm.stream.end still goes out, because a transport that opened on the first
// delta needs a terminator whatever the outcome.
func (p *Plugin) failResponsesStream(st *responsesStreamState, code, message string) {
	st.fatal = true
	st.pub.Close()
	_ = p.bus.Emit("llm.stream.end", events.StreamEnd{
		SchemaVersion: events.StreamEndVersion,
		TurnID:        st.turnID,
		FinishReason:  "error",
	})
	p.emitErrorInfo(events.ErrorInfo{
		SchemaVersion: events.ErrorInfoVersion,
		Err:           fmt.Errorf("openai: responses stream failed (%s): %s", code, message),
		Retryable:     false,
		RequestID:     st.requestID,
	})
}

// publishResponsesStream settles the turn: tool-call chunks, the gated stream,
// llm.stream.end, then the complete response.
//
// The terminal response is built from the response.completed snapshot whenever
// there is one, rather than from the accumulated deltas, so that the streaming
// and non-streaming paths produce the same LLMResponse for the same turn —
// same finish-reason mapping, same usage, and the same reasoning Items under
// reasoningItemsMetaKey, encrypted_content intact. The accumulated deltas fill
// in only what a truncated stream left missing.
func (p *Plugin) publishResponsesStream(st *responsesStreamState, requestID string, requestMeta map[string]any, tags map[string]string) {
	var resp events.LLMResponse
	if st.snapshot != nil {
		resp = p.convertResponsesReply(*st.snapshot)
	} else {
		// No terminal event — the connection ended early. Usage rides
		// response.completed alone on this surface (there is no
		// stream_options.include_usage to ask for it per chunk), so a
		// truncated turn reports none, and therefore no cost.
		resp = events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			FinishReason:  responsesFinishReason(responsesReply{}, len(st.toolIdx) > 0),
		}
	}
	if resp.Content == "" {
		resp.Content = st.text.String()
	}
	if len(resp.ToolCalls) == 0 {
		resp.ToolCalls = st.toolCalls()
	}
	if resp.Model == "" {
		resp.Model = st.model
	}
	resp.RequestID = requestID

	// Tool-call chunks go out before the stream is settled, matching the chat
	// reader's ordering. StreamPublisher suppresses them on a blocked stream.
	pub := st.pub
	for i := range resp.ToolCalls {
		pub.ToolCall(&resp.ToolCalls[i])
	}

	if summaries := st.renderSummaries(); len(summaries) > 0 {
		resp.Metadata = mergeMetadata(resp.Metadata, map[string]any{reasoningSummaryMetaKey: summaries})
	}
	// Request-passthrough metadata wins over anything the parser attached, the
	// same merge order both non-streaming paths use.
	resp.Metadata = mergeMetadata(resp.Metadata, requestMeta)
	resp.Tags = tags

	// Settle the gated stream before announcing the end of it: Close gives a
	// handler one last look at any tail it was holding, and releasing that
	// tail after llm.stream.end would arrive out of order. The deferred Close
	// in the reader is the error-path backstop; this one fixes the ordering.
	pub.Close()

	_ = p.bus.Emit("llm.stream.end", events.StreamEnd{
		SchemaVersion: events.StreamEndVersion,
		TurnID:        st.turnID,
		FinishReason:  resp.FinishReason,
		Usage:         resp.Usage,
	})

	engine.PublishLLMResponse(p.bus, resp)
}

// firstNonEmpty returns the first non-empty argument, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
