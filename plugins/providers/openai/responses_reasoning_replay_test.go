package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/roundtrip"
)

// This file covers the second half of encrypted-reasoning continuity on the
// Responses surface: the Items a turn returned going back out, verbatim and in
// order, on the request that follows.
//
// The capture half is responses_reply_test.go's and responses_stream_test.go's.
// What is tested here is everything between: the pkg/roundtrip allowlist that
// carries the Items onto a stored assistant Message, the serializer that
// splices them back into `input`, and — because this is the half other SDKs
// have shipped broken — the whole loop driven through the live handler on the
// streaming path.

// reasoningItem is one captured Item, with both the blob and the summary that
// rides beside it. The combination is deliberate: several SDKs drop
// encrypted_content specifically when a summary is present, so every fixture
// here carries both.
func reasoningItem(id, blob, summary string) map[string]any {
	return map[string]any{
		"type":              "reasoning",
		"id":                id,
		"encrypted_content": blob,
		"summary": []any{
			map[string]any{"type": "summary_text", "text": summary},
		},
	}
}

// --- the serializer ---------------------------------------------------------

// The head of the assistant turn, ahead of its text and its function calls,
// with each Item exactly as it arrived.
func TestResponsesInput_ReplaysReasoningItemsAtTheHeadOfTheTurn(t *testing.T) {
	stored := []map[string]any{
		reasoningItem("rs_1", "blob-1", "Checking the units first."),
		reasoningItem("rs_2", "blob-2", "Then the conversion."),
	}

	got := quietPlugin().buildResponsesInput([]events.Message{
		{Role: "user", Content: "convert it"},
		{
			Role:      "assistant",
			Content:   "I'll look that up",
			ToolCalls: []events.ToolCallRequest{{ID: "call_1", Name: "convert", Arguments: `{"v":1}`}},
			Metadata:  map[string]any{reasoningItemsMetaKey: stored},
		},
		{Role: "tool", ToolCallID: "call_1", Content: "42"},
	})

	want := []map[string]any{
		{"role": "user", "content": "convert it"},
		stored[0],
		stored[1],
		{"role": "assistant", "content": "I'll look that up"},
		{"type": "function_call", "call_id": "call_1", "name": "convert", "arguments": `{"v":1}`},
		{"type": "function_call_output", "call_id": "call_1", "output": "42"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("input =\n%v\nwant\n%v", got, want)
	}
}

// Verbatim means whole. Nothing is narrowed to the fields this package happens
// to name, because the blob is verified server-side and the dropped field is
// always the one nobody thought of.
func TestResponsesInput_ReasoningItemsAreNotRebuiltFromNamedFields(t *testing.T) {
	item := map[string]any{
		"type":                       "reasoning",
		"id":                         "rs_1",
		"encrypted_content":          "blob",
		"status":                     "completed",
		"summary":                    []any{map[string]any{"type": "summary_text", "text": "Thinking."}},
		"a_field_added_next_quarter": map[string]any{"nested": true},
	}

	got := quietPlugin().buildResponsesInput([]events.Message{{
		Role:     "assistant",
		Content:  "hi",
		Metadata: map[string]any{reasoningItemsMetaKey: []map[string]any{item}},
	}})

	if len(got) != 2 {
		t.Fatalf("got %d Items, want the reasoning Item then the message", len(got))
	}
	if !reflect.DeepEqual(got[0], item) {
		t.Errorf("replayed Item =\n%v\nwant it byte-for-byte:\n%v", got[0], item)
	}
}

// A message that has been through persisted history carries []any of
// map[string]any, not []map[string]any. A single-shape assertion would work
// live and silently replay nothing on exactly the long session that needs it.
func TestResponsesInput_ReplaysReasoningItemsAfterAJSONRoundTrip(t *testing.T) {
	live := map[string]any{reasoningItemsMetaKey: []map[string]any{
		reasoningItem("rs_1", "blob-1", "First."),
		reasoningItem("rs_2", "blob-2", "Second."),
	}}

	raw, err := json.Marshal(live)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, stillLive := persisted[reasoningItemsMetaKey].([]map[string]any); stillLive {
		t.Fatal("fixture is not exercising the post-JSON shape")
	}

	got := quietPlugin().buildResponsesInput([]events.Message{{
		Role:     "assistant",
		Content:  "hi",
		Metadata: persisted,
	}})

	if len(got) != 3 {
		t.Fatalf("got %d Items, want both reasoning Items then the message", len(got))
	}
	for i, wantBlob := range []string{"blob-1", "blob-2"} {
		if got[i]["encrypted_content"] != wantBlob {
			t.Errorf("Item %d encrypted_content = %v, want %q", i, got[i]["encrypted_content"], wantBlob)
		}
		if got[i]["summary"] == nil {
			t.Errorf("Item %d lost its summary across the round trip", i)
		}
	}
}

// A turn with no reasoning to replay serialises exactly as it did before, and
// a metadata value of a shape this does not recognise is stepped over rather
// than sent as garbage.
func TestResponsesInput_NothingToReplayAddsNothing(t *testing.T) {
	for name, meta := range map[string]map[string]any{
		"no metadata":    nil,
		"other keys":     {reasoningSummaryMetaKey: []string{"prose"}},
		"wrong shape":    {reasoningItemsMetaKey: "not a list"},
		"empty any list": {reasoningItemsMetaKey: []any{"not an object"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := quietPlugin().buildResponsesInput([]events.Message{{
				Role: "assistant", Content: "hi", Metadata: meta,
			}})
			want := []map[string]any{{"role": "assistant", "content": "hi"}}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("input = %v, want just the message", got)
			}
		})
	}
}

// The stored slice belongs to a Message that may be replayed again, so the
// caller must not be able to reach it through what it appends to.
func TestReplayReasoningItems_DoesNotAliasTheStoredSlice(t *testing.T) {
	stored := []map[string]any{reasoningItem("rs_1", "blob", "x")}
	out := replayReasoningItems(map[string]any{reasoningItemsMetaKey: stored})
	out = append(out, map[string]any{"type": "reasoning", "id": "injected"})
	if len(stored) != 1 {
		t.Errorf("the stored slice grew to %d — the caller is aliasing it", len(stored))
	}
	_ = out
}

// --- the allowlist ----------------------------------------------------------

// The provider's own spelling and pkg/roundtrip's string literal are coupled by
// convention only, so the coupling is pinned here: whatever the parser stashes
// is what every history builder forwards.
func TestReasoningItemsKeyIsOnTheRoundtripAllowlist(t *testing.T) {
	items := []map[string]any{reasoningItem("rs_1", "blob", "Thinking.")}
	resp := events.LLMResponse{Metadata: map[string]any{
		reasoningItemsMetaKey:   items,
		reasoningSummaryMetaKey: []string{"Thinking."},
		"_source":               "delegate.abc",
	}}

	fwd := roundtrip.ForwardMessageMetadata(resp.Metadata)
	if _, ok := fwd[reasoningItemsMetaKey]; !ok {
		t.Fatalf("%q is not forwarded — pkg/roundtrip and this package disagree on the spelling", reasoningItemsMetaKey)
	}

	// And what is forwarded is what replays.
	got := quietPlugin().buildResponsesInput([]events.Message{{
		Role: "assistant", Content: "hi", Metadata: fwd,
	}})
	if len(got) != 2 || got[0]["encrypted_content"] != "blob" {
		t.Errorf("input = %v, want the forwarded Item replayed", got)
	}
}

// --- the whole loop, through the handler ------------------------------------

// reasoningLoopServer is a two-turn Responses endpoint: the first turn streams
// a reasoning Item and a tool call, the second answers in one JSON body. It
// records every request body so the test can read what the second turn sent.
type reasoningLoopServer struct {
	*httptest.Server
	bodies []string
}

func newReasoningLoopServer(t *testing.T, turns ...func(w http.ResponseWriter)) *reasoningLoopServer {
	t.Helper()
	s := &reasoningLoopServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.bodies = append(s.bodies, string(raw))
		n := len(s.bodies) - 1
		if n >= len(turns) {
			t.Errorf("server got %d requests, scripted %d", n+1, len(turns))
			return
		}
		turns[n](w)
	}))
	t.Cleanup(s.Close)
	return s
}

// streamedReasoningTurn writes a streamed turn whose response.completed
// snapshot carries a reasoning Item — blob and summary together — alongside a
// function call. The Item exists nowhere in the deltas, which is the whole
// reason the reader takes it from the snapshot.
func streamedReasoningTurn(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1"}}`,
		`{"type":"response.output_text.delta","delta":"Looking that up."}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"Checking the units first."}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_a","name":"convert"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":2,"delta":"{\"v\":1}"}`,
		`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.1","status":"completed","output":[`+
			`{"type":"reasoning","id":"rs_1","encrypted_content":"ENCRYPTED-BLOB-1","summary":[{"type":"summary_text","text":"Checking the units first."}]},`+
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Looking that up."}]},`+
			`{"type":"function_call","id":"fc_1","call_id":"call_a","name":"convert","arguments":"{\"v\":1}"}`+
			`],"usage":{"input_tokens":10,"output_tokens":4}}}`,
	))
}

// finalTurn answers the tool result and carries a second reasoning Item of its
// own, so the test can also see a turn that both replays and captures.
func finalTurn(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{
	  "id": "resp_2",
	  "model": "gpt-5.1",
	  "status": "completed",
	  "output": [
	    {"type":"reasoning","id":"rs_2","encrypted_content":"ENCRYPTED-BLOB-2","summary":[{"type":"summary_text","text":"Now the answer."}]},
	    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"42 metres."}]}
	  ],
	  "usage": {"input_tokens": 20, "output_tokens": 3}
	}`)
}

// A genuine two-round tool loop on the Responses surface, driven through the
// live handler on the STREAMING path, with history assembled the way an agent
// loop assembles it — roundtrip.ForwardMessageMetadata onto the assistant
// Message, nothing provider-specific.
//
// The streaming half is the point. `encrypted_content` appears only on the
// terminal snapshot's `output` array and never in the deltas, and dropping it
// in the stream converter is the bug several other SDKs shipped. Asserting that
// the snapshot parser works would not catch it; asserting what the second HTTP
// request carried does.
func TestResponsesLoop_StreamedReasoningSurvivesIntoTheNextRequest(t *testing.T) {
	srv := newReasoningLoopServer(t, streamedReasoningTurn, finalTurn)

	bus := engine.NewEventBus()
	p := &Plugin{}
	if err := p.Init(engine.PluginContext{
		Config: map[string]any{
			"api_key":   "sk-not-used",
			"api":       "responses",
			"base_url":  srv.URL + "/v1/chat/completions",
			"reasoning": map[string]any{"mode": "effort", "effort": "high", "summary": "auto"},
		},
		Bus:    bus,
		Logger: silentLogger(),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	var responses []events.LLMResponse
	unsub := bus.Subscribe("llm.response", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.LLMResponse); ok {
			responses = append(responses, r)
		}
	})
	defer unsub()

	// --- round one: streamed, returns a tool call -----------------------
	history := []events.Message{{Role: "user", Content: "convert it"}}
	if err := bus.Emit("llm.request", events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Model:         "gpt-5.1",
		Stream:        true,
		Messages:      history,
		Tools:         []events.ToolDef{{Name: "convert", Description: "convert units"}},
	}); err != nil {
		t.Fatalf("emit round one: %v", err)
	}
	if len(responses) != 1 {
		t.Fatalf("round one published %d responses, want 1", len(responses))
	}
	first := responses[0]
	if len(first.ToolCalls) != 1 {
		t.Fatalf("round one returned %d tool calls, want 1", len(first.ToolCalls))
	}

	// The streamed turn kept the blob. If this fails the converter dropped it
	// and nothing downstream could have replayed it.
	items, ok := first.Metadata[reasoningItemsMetaKey].([]map[string]any)
	if !ok || len(items) != 1 || items[0]["encrypted_content"] != "ENCRYPTED-BLOB-1" {
		t.Fatalf("streamed turn's reasoning Items = %v, want the blob intact", first.Metadata[reasoningItemsMetaKey])
	}

	// --- what an agent loop does between the rounds ---------------------
	history = append(history,
		events.Message{
			Role:      "assistant",
			Content:   first.Content,
			ToolCalls: first.ToolCalls,
			Metadata:  roundtrip.ForwardMessageMetadata(first.Metadata),
		},
		events.Message{Role: "tool", ToolCallID: first.ToolCalls[0].ID, Content: "42"},
	)

	// --- round two ------------------------------------------------------
	if err := bus.Emit("llm.request", events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Model:         "gpt-5.1",
		Messages:      history,
		Tools:         []events.ToolDef{{Name: "convert", Description: "convert units"}},
	}); err != nil {
		t.Fatalf("emit round two: %v", err)
	}
	if len(responses) != 2 {
		t.Fatalf("round two published %d responses in total, want 2", len(responses))
	}
	if responses[1].Content != "42 metres." {
		t.Errorf("round two Content = %q, want the final answer", responses[1].Content)
	}

	// --- the assertion the whole story is about -------------------------
	if len(srv.bodies) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(srv.bodies))
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(srv.bodies[1]), &second); err != nil {
		t.Fatalf("decode round two body: %v", err)
	}
	input, ok := second["input"].([]any)
	if !ok {
		t.Fatalf("round two input is %T, want a list of Items", second["input"])
	}

	var replayed []map[string]any
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		if item["type"] == "reasoning" {
			replayed = append(replayed, item)
		}
	}
	if len(replayed) != 1 {
		t.Fatalf("round two replayed %d reasoning Items, want 1 — the loop lost its reasoning", len(replayed))
	}
	if replayed[0]["encrypted_content"] != "ENCRYPTED-BLOB-1" {
		t.Errorf("replayed encrypted_content = %v, want the blob round one returned", replayed[0]["encrypted_content"])
	}
	if replayed[0]["id"] != "rs_1" {
		t.Errorf("replayed Item id = %v, want rs_1", replayed[0]["id"])
	}
	// The blob-plus-summary combination other SDKs drop.
	if replayed[0]["summary"] == nil {
		t.Error("the summary beside the blob was dropped on replay")
	}

	// Position: the reasoning Item leads the assistant turn it belongs to, and
	// so precedes that turn's function_call.
	reasoningAt, callAt := -1, -1
	for i, raw := range input {
		item, _ := raw.(map[string]any)
		switch item["type"] {
		case "reasoning":
			reasoningAt = i
		case "function_call":
			callAt = i
		}
	}
	if reasoningAt == -1 || callAt == -1 || reasoningAt > callAt {
		t.Errorf("reasoning at %d, function_call at %d — the Item must precede the call it reasoned about", reasoningAt, callAt)
	}

	// `store: false` is what makes any of this necessary, so it is pinned here
	// too: the blobs exist because Nexus asked for no server-side state.
	if store, ok := second["store"].(bool); !ok || store {
		t.Errorf("store = %v on the replaying request, want false", second["store"])
	}

	// And round two's own Items were captured in turn, so a third round would
	// have something to replay.
	next, ok := responses[1].Metadata[reasoningItemsMetaKey].([]map[string]any)
	if !ok || len(next) != 1 || next[0]["encrypted_content"] != "ENCRYPTED-BLOB-2" {
		t.Errorf("round two captured %v, want its own blob for the round after", responses[1].Metadata[reasoningItemsMetaKey])
	}
}

// The non-streaming path of the same loop, so that a turn's reasoning survives
// whichever way the turn was read. Round one here is a plain JSON body.
func TestResponsesLoop_NonStreamedReasoningSurvivesIntoTheNextRequest(t *testing.T) {
	syncToolTurn := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
		  "id": "resp_1",
		  "model": "gpt-5.1",
		  "status": "completed",
		  "output": [
		    {"type":"reasoning","id":"rs_1","encrypted_content":"ENCRYPTED-BLOB-1","summary":[{"type":"summary_text","text":"Checking."}]},
		    {"type":"function_call","id":"fc_1","call_id":"call_a","name":"convert","arguments":"{\"v\":1}"}
		  ],
		  "usage": {"input_tokens": 10, "output_tokens": 4}
		}`)
	}
	srv := newReasoningLoopServer(t, syncToolTurn, finalTurn)

	bus := engine.NewEventBus()
	p := &Plugin{}
	if err := p.Init(engine.PluginContext{
		Config: map[string]any{
			"api_key":  "sk-not-used",
			"api":      "responses",
			"base_url": srv.URL + "/v1/chat/completions",
		},
		Bus:    bus,
		Logger: silentLogger(),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	var responses []events.LLMResponse
	unsub := bus.Subscribe("llm.response", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.LLMResponse); ok {
			responses = append(responses, r)
		}
	})
	defer unsub()

	history := []events.Message{{Role: "user", Content: "convert it"}}
	if err := bus.Emit("llm.request", events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Model:         "gpt-5.1",
		Messages:      history,
	}); err != nil {
		t.Fatalf("emit round one: %v", err)
	}
	if len(responses) != 1 || len(responses[0].ToolCalls) != 1 {
		t.Fatalf("round one did not return a tool call: %+v", responses)
	}
	first := responses[0]

	history = append(history,
		events.Message{
			Role:      "assistant",
			ToolCalls: first.ToolCalls,
			Metadata:  roundtrip.ForwardMessageMetadata(first.Metadata),
		},
		events.Message{Role: "tool", ToolCallID: first.ToolCalls[0].ID, Content: "42"},
	)
	if err := bus.Emit("llm.request", events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Model:         "gpt-5.1",
		Messages:      history,
	}); err != nil {
		t.Fatalf("emit round two: %v", err)
	}

	if len(srv.bodies) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(srv.bodies))
	}
	if !strings.Contains(srv.bodies[1], "ENCRYPTED-BLOB-1") {
		t.Errorf("round two body did not replay the blob:\n%s", srv.bodies[1])
	}
	// An assistant turn that was only a tool call has no message Item at all,
	// so the reasoning Item must not have been attached to one.
	if strings.Contains(srv.bodies[1], `"role":"assistant","content":""`) {
		t.Error("an empty assistant message Item was emitted alongside the replay")
	}
}
