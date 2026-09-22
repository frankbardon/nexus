package openai

import (
	"reflect"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file covers the Responses SSE reader. Everything here starts from a
// stream of typed events and ends at what reached the bus.

// sse renders a list of `data:` payloads as the wire sends them. The Responses
// stream sends an `event:` line too, and the reader tolerates its absence
// because every payload names itself in `type`; the one test that needs the
// header spells it out itself.
func sse(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: ")
		b.WriteString(f)
		b.WriteString("\n\n")
	}
	return b.String()
}

// gateSegments subscribes a before:llm.stream.chunk handler, unwrapping the
// VetoablePayload the bus delivers.
func gateSegments(bus engine.EventBus, fn func(seg *events.StreamSegment, vp *engine.VetoablePayload)) {
	bus.Subscribe("before:llm.stream.chunk", func(e engine.Event[any]) {
		vp, ok := e.Payload.(*engine.VetoablePayload)
		if !ok {
			return
		}
		seg, ok := vp.Original.(*events.StreamSegment)
		if !ok {
			return
		}
		fn(seg, vp)
	})
}

// streamRecorder captures everything one reader run put on the bus.
type streamRecorder struct {
	bus       engine.EventBus
	chunks    []events.StreamChunk
	segments  []string
	ends      []events.StreamEnd
	responses []events.LLMResponse
	errors    []events.ErrorInfo
	holds     []events.StreamHold
	retracts  int
	types     map[string]bool
}

func newStreamRecorder() *streamRecorder {
	r := &streamRecorder{bus: engine.NewEventBus(), types: map[string]bool{}}
	for _, t := range []string{
		"llm.stream.chunk", "before:llm.stream.chunk", "llm.stream.hold",
		"llm.stream.retract", "llm.stream.end", "llm.response",
		"before:llm.response", "core.error", "before:core.error",
	} {
		t := t
		r.bus.Subscribe(t, func(e engine.Event[any]) {
			r.types[t] = true
			payload := e.Payload
			if vp, ok := payload.(*engine.VetoablePayload); ok {
				payload = vp.Original
			}
			switch p := payload.(type) {
			case events.StreamChunk:
				if t == "llm.stream.chunk" {
					r.chunks = append(r.chunks, p)
				}
			case *events.StreamSegment:
				r.segments = append(r.segments, p.Content)
			case events.StreamHold:
				r.holds = append(r.holds, p)
			case events.StreamEnd:
				r.ends = append(r.ends, p)
			case events.LLMResponse:
				if t == "llm.response" {
					r.responses = append(r.responses, p)
				}
			case *events.ErrorInfo:
				// before:core.error carries a pointer.
			case events.ErrorInfo:
				if t == "core.error" {
					r.errors = append(r.errors, p)
				}
			}
			if t == "llm.stream.retract" {
				r.retracts++
			}
		})
	}
	return r
}

// text returns the text of every llm.stream.chunk, concatenated — what a UI
// would actually have shown.
func (r *streamRecorder) text() string {
	var b strings.Builder
	for _, c := range r.chunks {
		b.WriteString(c.Content)
	}
	return b.String()
}

func (r *streamRecorder) run(t *testing.T, body string) {
	t.Helper()
	p := quietPlugin()
	p.bus = r.bus
	p.handleResponsesStreamResponse(strings.NewReader(body), "req-1", nil, nil)
}

func (r *streamRecorder) onlyResponse(t *testing.T) events.LLMResponse {
	t.Helper()
	if len(r.responses) != 1 {
		t.Fatalf("published %d llm.response events, want 1", len(r.responses))
	}
	return r.responses[0]
}

// A representative completed turn: created, some text, a completed snapshot.
const streamCompleted = `{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello, world."}]}],"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"output_tokens_details":{"reasoning_tokens":3}}}}`

// --- text ------------------------------------------------------------------

// Text deltas reach the bus as llm.stream.chunk, and they get there through
// engine.StreamPublisher — which is what before:llm.stream.chunk firing proves.
func TestResponsesStream_TextGoesThroughTheGate(t *testing.T) {
	r := newStreamRecorder()
	r.run(t, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1","status":"in_progress"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"Hello, "}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"world."}`,
		`{"type":"response.output_text.done","item_id":"msg_1","output_index":0,"text":"Hello, world."}`,
		streamCompleted,
	))

	if got := r.text(); got != "Hello, world." {
		t.Errorf("streamed text = %q, want %q", got, "Hello, world.")
	}
	if len(r.segments) == 0 {
		t.Error("before:llm.stream.chunk never fired — text bypassed StreamPublisher")
	}
	if len(r.chunks) != 2 {
		t.Errorf("emitted %d chunks, want 2 (output_text.done must not be re-emitted)", len(r.chunks))
	}
	for _, c := range r.chunks {
		if c.TurnID != "resp_1" {
			t.Errorf("chunk TurnID = %q, want resp_1", c.TurnID)
		}
	}

	resp := r.onlyResponse(t)
	if resp.Content != "Hello, world." {
		t.Errorf("Content = %q", resp.Content)
	}
	if resp.Model != "gpt-5.1" {
		t.Errorf("Model = %q, want gpt-5.1", resp.Model)
	}
	if resp.RequestID != "req-1" {
		t.Errorf("RequestID = %q, want req-1", resp.RequestID)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", resp.FinishReason)
	}
}

// The `event:` header line is tolerated as well as ignored: the payload's own
// `type` is what the reader dispatches on, and a payload missing one falls back
// to the header.
func TestResponsesStream_EventHeaderLines(t *testing.T) {
	r := newStreamRecorder()
	r.run(t, "event: response.created\n"+
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`+"\n\n"+
		"event: response.output_text.delta\n"+
		`data: {"item_id":"msg_1","output_index":0,"delta":"typeless"}`+"\n\n"+
		"event: response.completed\n"+
		"data: "+streamCompleted+"\n\n")

	if len(r.chunks) != 1 || r.chunks[0].Content != "typeless" {
		t.Errorf("chunks = %+v, want one carrying %q", r.chunks, "typeless")
	}
}

// --- gating parity ---------------------------------------------------------

// A gate holding a trailing fragment behaves exactly as it does on the chat
// path: the held bytes are withheld, re-offered with the next delta, and
// released at Close.
func TestResponsesStream_HoldBehavesAsOnTheChatPath(t *testing.T) {
	r := newStreamRecorder()
	// Hold the last 3 bytes of every segment until the final flush.
	gateSegments(r.bus, func(seg *events.StreamSegment, vp *engine.VetoablePayload) {
		if len(seg.Content) >= 3 {
			seg.Hold = 3
		}
	})

	r.run(t, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`,
		`{"type":"response.output_text.delta","delta":"abcdef"}`,
		`{"type":"response.output_text.delta","delta":"ghijkl"}`,
		streamCompleted,
	))

	// Nothing is lost: every byte the model produced is released by the end.
	if got := r.text(); got != "abcdefghijkl" {
		t.Errorf("released text = %q, want abcdefghijkl", got)
	}
	// The first chunk stopped short of the delta it was offered.
	if len(r.chunks) == 0 || r.chunks[0].Content != "abc" {
		t.Errorf("first chunk = %+v, want content %q", r.chunks, "abc")
	}
	// A partial hold releases something on every segment, so it is ordinary
	// operation rather than a suspension and announces nothing — same as on the
	// chat path. The sustained case is covered below.
	if len(r.holds) != 0 {
		t.Errorf("a releasing hold announced %d suspensions, want 0", len(r.holds))
	}
}

// A gate blocking a segment stops the stream dead: nothing more is released,
// tool-call chunks are suppressed too, and the released prefix is disowned.
func TestResponsesStream_BlockBehavesAsOnTheChatPath(t *testing.T) {
	r := newStreamRecorder()
	gateSegments(r.bus, func(seg *events.StreamSegment, vp *engine.VetoablePayload) {
		if strings.Contains(seg.Full(), "secret") {
			vp.Veto = engine.VetoResult{Vetoed: true, Reason: "contains a secret"}
		}
	})

	r.run(t, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`,
		`{"type":"response.output_text.delta","delta":"safe "}`,
		`{"type":"response.output_text.delta","delta":"secret"}`,
		`{"type":"response.output_text.delta","delta":" more"}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{}"}`,
		`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.1","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}"}]}}`,
	))

	if got := r.text(); got != "safe " {
		t.Errorf("released text = %q, want only the pre-block prefix %q", got, "safe ")
	}
	for _, c := range r.chunks {
		if c.ToolCall != nil {
			t.Error("a blocked stream still published a tool-call chunk")
		}
	}
	if r.retracts == 0 {
		t.Error("llm.stream.retract never disowned the released prefix")
	}
	// The response still carries what the model said — the response-level hook
	// is what substitutes it, exactly as on the chat path.
	resp := r.onlyResponse(t)
	if len(resp.ToolCalls) != 1 {
		t.Errorf("ToolCalls = %+v, want the turn's one call to still reach before:llm.response", resp.ToolCalls)
	}
}

// --- function calls --------------------------------------------------------

// Arguments accumulate across deltas, and the tool's name and call_id come from
// the output_item.added event the deltas do not repeat.
func TestResponsesStream_ToolArgumentsAccumulate(t *testing.T) {
	r := newStreamRecorder()
	r.run(t, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_a","name":"search","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"q\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"\"go\"}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"arguments":"{\"q\":\"go\"}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_a","name":"search","arguments":"{\"q\":\"go\"}"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.1","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_a","name":"search","arguments":"{\"q\":\"go\"}"}]}}`,
	))

	resp := r.onlyResponse(t)
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want 1", resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_a" || tc.Name != "search" || tc.Arguments != `{"q":"go"}` {
		t.Errorf("tool call = %+v", tc)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}

	// A tool-call chunk is published so a transport can show the call.
	var toolChunks int
	for _, c := range r.chunks {
		if c.ToolCall != nil {
			toolChunks++
		}
	}
	if toolChunks != 1 {
		t.Errorf("published %d tool-call chunks, want 1", toolChunks)
	}
}

// Two calls in one turn interleave their argument deltas; output_index is what
// keeps them apart, and the order is the output order rather than the arrival
// order.
func TestResponsesStream_InterleavedMultiToolTurn(t *testing.T) {
	r := newStreamRecorder()
	r.run(t, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_a","name":"alpha"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_b","name":"beta"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_2","output_index":1,"delta":"{\"b\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"a\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_2","output_index":1,"delta":"2}"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"1}"}`,
		// No terminal snapshot: this is the accumulation path on its own.
	))

	resp := r.onlyResponse(t)
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %+v, want 2", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Name != "alpha" || resp.ToolCalls[0].Arguments != `{"a":1}` {
		t.Errorf("first call = %+v", resp.ToolCalls[0])
	}
	if resp.ToolCalls[1].Name != "beta" || resp.ToolCalls[1].Arguments != `{"b":2}` {
		t.Errorf("second call = %+v", resp.ToolCalls[1])
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
}

// A call whose arguments never arrive is normalised to the empty object rather
// than to "", which is not valid JSON and would fail the whole next request.
func TestResponsesStream_EmptyArgumentsBecomeAnObject(t *testing.T) {
	r := newStreamRecorder()
	r.run(t, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_a","name":"ping"}}`,
	))

	resp := r.onlyResponse(t)
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Arguments != "{}" {
		t.Errorf("ToolCalls = %+v, want one call with arguments {}", resp.ToolCalls)
	}
}

// --- reasoning -------------------------------------------------------------

// The replayable reasoning Items come from the response.completed snapshot,
// with encrypted_content intact. This is the specific thing several other SDKs
// lose in their stream converters: the delta stream never carries it.
func TestResponsesStream_ReasoningItemsSurviveFromTheSnapshot(t *testing.T) {
	r := newStreamRecorder()
	r.run(t, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`,
		`{"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"Thinking "}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"about it."}`,
		`{"type":"response.output_text.delta","delta":"42"}`,
		`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.1","status":"completed","output":[{"type":"reasoning","id":"rs_1","encrypted_content":"gAAAAA-opaque","summary":[{"type":"summary_text","text":"Thinking about it."}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"42"}]}]}}`,
	))

	resp := r.onlyResponse(t)
	raw, ok := resp.Metadata[reasoningItemsMetaKey].([]map[string]any)
	if !ok {
		t.Fatalf("%s = %T, want []map[string]any", reasoningItemsMetaKey, resp.Metadata[reasoningItemsMetaKey])
	}
	if len(raw) != 1 {
		t.Fatalf("captured %d reasoning items, want 1", len(raw))
	}
	if raw[0]["encrypted_content"] != "gAAAAA-opaque" {
		t.Errorf("encrypted_content lost in the stream converter: %v", raw[0])
	}
	if raw[0]["id"] != "rs_1" {
		t.Errorf("reasoning item id = %v, want rs_1", raw[0]["id"])
	}

	// The summary text is accumulated separately, for E5-S1 to turn into
	// thinking.step events. It is never released as output text.
	summaries, ok := resp.Metadata[reasoningSummaryMetaKey].([]string)
	if !ok || len(summaries) != 1 || summaries[0] != "Thinking about it." {
		t.Errorf("%s = %v, want one part reading %q", reasoningSummaryMetaKey, resp.Metadata[reasoningSummaryMetaKey], "Thinking about it.")
	}
	if got := r.text(); got != "42" {
		t.Errorf("released text = %q — reasoning summary must not be streamed as output", got)
	}
}

// Some models stream only response.reasoning_text.delta, with no summary events
// at all. Both spellings accumulate into the same place.
func TestResponsesStream_ReasoningTextSpellingIsAccepted(t *testing.T) {
	r := newStreamRecorder()
	r.run(t, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`,
		`{"type":"response.reasoning_text.delta","output_index":0,"summary_index":1,"delta":"second"}`,
		`{"type":"response.reasoning_text.delta","output_index":0,"summary_index":0,"delta":"first"}`,
		streamCompleted,
	))

	resp := r.onlyResponse(t)
	got, _ := resp.Metadata[reasoningSummaryMetaKey].([]string)
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("%s = %v, want [first second] in summary_index order", reasoningSummaryMetaKey, got)
	}
}

// --- lifecycle and failure -------------------------------------------------

// response.completed carries the usage this surface never puts on a chunk, and
// it drives both llm.stream.end and the published response.
func TestResponsesStream_CompletedDrivesUsageAndEnd(t *testing.T) {
	r := newStreamRecorder()
	r.run(t, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`,
		`{"type":"response.output_text.delta","delta":"Hello, world."}`,
		streamCompleted,
	))

	if len(r.ends) != 1 {
		t.Fatalf("emitted %d llm.stream.end events, want 1", len(r.ends))
	}
	end := r.ends[0]
	if end.TurnID != "resp_1" || end.FinishReason != "stop" {
		t.Errorf("stream end = %+v", end)
	}
	if end.Usage.PromptTokens != 10 || end.Usage.CompletionTokens != 4 || end.Usage.ReasoningTokens != 3 {
		t.Errorf("stream end usage = %+v", end.Usage)
	}
	resp := r.onlyResponse(t)
	if !reflect.DeepEqual(resp.Usage, end.Usage) {
		t.Errorf("response usage %+v != stream end usage %+v", resp.Usage, end.Usage)
	}
}

// A run that stopped on the token cap reports the chat path's vocabulary.
func TestResponsesStream_IncompleteMapsToLength(t *testing.T) {
	r := newStreamRecorder()
	r.run(t, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`,
		`{"type":"response.output_text.delta","delta":"truncat"}`,
		`{"type":"response.incomplete","response":{"id":"resp_1","model":"gpt-5.1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"truncat"}]}]}}`,
	))

	if got := r.onlyResponse(t).FinishReason; got != "length" {
		t.Errorf("FinishReason = %q, want length", got)
	}
}

// A run that failed mid-stream becomes core.error and no llm.response — the
// fallback coordinator watches core.error and would never see a failure
// disguised as an empty turn.
func TestResponsesStream_FailedRunBecomesAnError(t *testing.T) {
	r := newStreamRecorder()
	r.run(t, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`,
		`{"type":"response.output_text.delta","delta":"partial"}`,
		`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_error","message":"boom"}}}`,
		`{"type":"response.output_text.delta","delta":"never"}`,
	))

	if len(r.responses) != 0 {
		t.Errorf("published %d llm.response events for a failed run, want 0", len(r.responses))
	}
	if len(r.errors) != 1 {
		t.Fatalf("emitted %d core.error events, want 1", len(r.errors))
	}
	if !strings.Contains(r.errors[0].Err.Error(), "boom") {
		t.Errorf("error = %v", r.errors[0].Err)
	}
	// The transport still gets a terminator, and nothing after the failure is
	// released.
	if len(r.ends) != 1 {
		t.Errorf("emitted %d llm.stream.end events, want 1", len(r.ends))
	}
	if got := r.text(); got != "partial" {
		t.Errorf("released text = %q, want only what preceded the failure", got)
	}
}

// The flat `error` event is the transport-level counterpart and settles the
// same way.
func TestResponsesStream_ErrorEventBecomesAnError(t *testing.T) {
	r := newStreamRecorder()
	r.run(t, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`,
		`{"type":"error","code":"rate_limit_exceeded","message":"slow down"}`,
	))

	if len(r.responses) != 0 || len(r.errors) != 1 {
		t.Fatalf("responses = %d, errors = %d; want 0 and 1", len(r.responses), len(r.errors))
	}
	if !strings.Contains(r.errors[0].Err.Error(), "rate_limit_exceeded") {
		t.Errorf("error = %v", r.errors[0].Err)
	}
}

// A stream that ends without a terminal event still publishes what it got.
// Usage rides response.completed alone on this surface, so a truncated turn
// reports none — but the text and the tool calls are not lost.
func TestResponsesStream_TruncatedStreamFallsBackToAccumulation(t *testing.T) {
	r := newStreamRecorder()
	r.run(t, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`,
		`{"type":"response.output_text.delta","delta":"half a th"}`,
	))

	resp := r.onlyResponse(t)
	if resp.Content != "half a th" {
		t.Errorf("Content = %q", resp.Content)
	}
	if resp.Model != "gpt-5.1" {
		t.Errorf("Model = %q, want gpt-5.1 from response.created", resp.Model)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", resp.FinishReason)
	}
	if resp.Usage.TotalTokens != 0 {
		t.Errorf("Usage = %+v, want zero for a turn that never completed", resp.Usage)
	}
}

// An undecodable frame is stepped over rather than failing the turn, and an
// event type the reader has never heard of is ignored the same way.
func TestResponsesStream_UnknownAndUndecodableFramesAreTolerated(t *testing.T) {
	r := newStreamRecorder()
	r.run(t, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`,
		`{not json`,
		`{"type":"response.web_search_call.in_progress","output_index":0}`,
		`{"type":"response.output_text.delta","delta":"fine"}`,
		streamCompleted,
	))

	if len(r.errors) != 0 {
		t.Errorf("emitted %d core.error events, want 0", len(r.errors))
	}
	if got := r.text(); got != "fine" {
		t.Errorf("released text = %q", got)
	}
}

// --- passthrough and declared emissions ------------------------------------

// Request metadata and tags ride the published response, and request metadata
// wins the merge — the same contract the two non-streaming paths hold.
func TestResponsesStream_MetadataAndTagsPassThrough(t *testing.T) {
	bus := engine.NewEventBus()
	p := quietPlugin()
	p.bus = bus

	var got []events.LLMResponse
	bus.Subscribe("llm.response", func(e engine.Event[any]) {
		if resp, ok := e.Payload.(events.LLMResponse); ok {
			got = append(got, resp)
		}
	})

	p.handleResponsesStreamResponse(strings.NewReader(sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`,
		`{"type":"response.output_text.delta","delta":"ok"}`,
		streamCompleted,
	)), "req-9", map[string]any{"_structured_output": true}, map[string]string{"team": "core"})

	if len(got) != 1 {
		t.Fatalf("published %d responses, want 1", len(got))
	}
	if got[0].Metadata["_structured_output"] != true {
		t.Errorf("request metadata lost: %v", got[0].Metadata)
	}
	if got[0].Tags["team"] != "core" {
		t.Errorf("Tags = %v", got[0].Tags)
	}
}

// Everything the reader actually puts on the bus is declared in Emissions().
func TestResponsesStream_EmissionsAreDeclared(t *testing.T) {
	r := newStreamRecorder()
	// A sustained hold and then a block between them exercise every
	// stream-shaped event: chunk, hold (both edges) and retract.
	gateSegments(r.bus, func(seg *events.StreamSegment, vp *engine.VetoablePayload) {
		switch {
		case strings.Contains(seg.Full(), "stop"):
			vp.Veto = engine.VetoResult{Vetoed: true, Reason: "nope"}
		case strings.HasSuffix(seg.Content, "?"):
			// Deliberating: withhold the whole candidate.
			seg.HoldFrom(0)
		}
	})
	r.run(t, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`,
		`{"type":"response.output_text.delta","delta":"aaa?"}`,
		`{"type":"response.output_text.delta","delta":"bbb?"}`,
		`{"type":"response.output_text.delta","delta":"ccc"}`,
		`{"type":"response.output_text.delta","delta":" stop"}`,
		streamCompleted,
	))
	// And a second run that fails, for the error events.
	r.run(t, sse(`{"type":"error","code":"x","message":"y"}`))

	declared := map[string]bool{}
	for _, e := range New().Emissions() {
		declared[e] = true
	}
	for typ := range r.types {
		if !declared[typ] {
			t.Errorf("emitted %q at runtime but Emissions() does not declare it", typ)
		}
	}
	for _, want := range []string{
		"llm.stream.chunk", "llm.stream.hold", "llm.stream.retract",
		"llm.stream.end", "llm.response", "core.error",
	} {
		if !r.types[want] {
			t.Errorf("the reader never emitted %q — the test no longer covers it", want)
		}
	}
}

// A streamed multi-tool turn closes the loop: the calls the reader published
// go back out as function_call Items paired with their results by call_id,
// which is what an agent loop does between one turn and the next.
//
// This covers the two halves that meet: what the stream produced, and what the
// serializer makes of it. (The turn now has a URL to be posted to — see
// TestResponsesPath_ReachableEndToEnd — but the replay of `reasoning` Items
// between rounds is still E5-S2's.)
func TestResponsesStream_ToolCallsRoundTripBackIntoTheNextRequest(t *testing.T) {
	r := newStreamRecorder()
	r.run(t, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`,
		`{"type":"response.output_text.delta","delta":"Looking both up."}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_a","name":"alpha"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\"a\":1}"}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_2","call_id":"call_b","name":"beta"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_2","output_index":2,"delta":"{\"b\":2}"}`,
		`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Looking both up."}]},{"type":"function_call","id":"fc_1","call_id":"call_a","name":"alpha","arguments":"{\"a\":1}"},{"type":"function_call","id":"fc_2","call_id":"call_b","name":"beta","arguments":"{\"b\":2}"}]}}`,
	))

	resp := r.onlyResponse(t)
	if resp.FinishReason != "tool_calls" || len(resp.ToolCalls) != 2 {
		t.Fatalf("turn = %q with %d calls, want tool_calls with 2", resp.FinishReason, len(resp.ToolCalls))
	}

	// The agent loop's next request: the assistant turn, then a result per call.
	msgs := []events.Message{
		{Role: "user", Content: "look things up"},
		{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls},
		{Role: "tool", ToolCallID: "call_a", Content: "alpha result"},
		{Role: "tool", ToolCallID: "call_b", Content: "beta result"},
	}
	got := quietPlugin().buildResponsesInput(msgs)

	want := []map[string]any{
		{"role": "user", "content": "look things up"},
		{"role": "assistant", "content": "Looking both up."},
		{"type": "function_call", "call_id": "call_a", "name": "alpha", "arguments": `{"a":1}`},
		{"type": "function_call", "call_id": "call_b", "name": "beta", "arguments": `{"b":2}`},
		{"type": "function_call_output", "call_id": "call_a", "output": "alpha result"},
		{"type": "function_call_output", "call_id": "call_b", "output": "beta result"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("next request input =\n%v\nwant\n%v", got, want)
	}
}
