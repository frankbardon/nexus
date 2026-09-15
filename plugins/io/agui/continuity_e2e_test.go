package agui

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// The whole point of carrying continuity metadata is that it reaches a BROWSER
// and comes back, so the assertion is on the SSE bytes a client would actually
// receive rather than on the translator that produced them. A token that is
// correct in the run's own bookkeeping and absent from the stream buys nothing:
// the client cannot replay what it was never sent, and the next request 400s.
func TestASignatureReachesTheStreamOnTheCallItWasIssuedFor(t *testing.T) {
	_, bus, url := newTestPlugin(t)

	inputSeen := make(chan events.UserInput, 1)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		if in, ok := e.Payload.(events.UserInput); ok {
			select {
			case inputSeen <- in:
			default:
			}
		}
	})

	go func() {
		<-inputSeen
		turn := events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1"}
		_ = bus.Emit("agent.turn.start", turn)
		// Gemini signs the FIRST functionCall of a parallel batch and no other,
		// so the response carries a signature for call_0_a alone.
		_ = bus.Emit("llm.response", events.LLMResponse{
			ToolCalls: []events.ToolCallRequest{{ID: "call_0_a", Name: "a"}, {ID: "call_1_b", Name: "b"}},
			Metadata: map[string]any{
				"gemini_thought_signatures": map[string]string{"call_0_a": "opaque-token"},
				// An engine-internal routing hint rides the same map and must not
				// leave the process.
				"_source": "nexus.agent.react",
			},
		})
		_ = bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion,
			ID: "call_0_a", Name: "a", TurnID: "turn-1"})
		_ = bus.Emit("tool.invoke", events.ToolCall{SchemaVersion: events.ToolCallVersion,
			ID: "call_1_b", Name: "b", TurnID: "turn-1"})
		_ = bus.Emit("agent.turn.end", turn)
	}()

	body := `{"threadId":"thread-1","runId":"run-1","messages":[{"id":"m1","role":"user","content":"go"}]}`
	resp := post(t, url, "", "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	evs, err := agui.NewSSEReader(resp.Body).ReadAll()
	if err != nil {
		t.Fatalf("read sse: %v", err)
	}

	starts := map[string]*agui.ToolCallStartEvent{}
	for _, e := range evs {
		if s, ok := e.(*agui.ToolCallStartEvent); ok {
			starts[s.ToolCallID] = s
		}
	}
	signed, ok := starts["call_0_a"]
	if !ok {
		t.Fatal("no ToolCallStart for the signed call")
	}
	sigs, ok := signed.Metadata["gemini_thought_signatures"].(map[string]any)
	if !ok {
		t.Fatalf("signed call metadata = %#v, want a signature map", signed.Metadata)
	}
	if sigs["call_0_a"] != "opaque-token" {
		t.Errorf("signature = %v, want the issued token", sigs["call_0_a"])
	}
	if _, leaked := signed.Metadata["_source"]; leaked {
		t.Error("an engine-internal routing hint reached the client")
	}

	unsigned, ok := starts["call_1_b"]
	if !ok {
		t.Fatal("no ToolCallStart for the unsigned call")
	}
	if len(unsigned.Metadata) != 0 {
		t.Errorf("unsigned call carried %v, want nothing", unsigned.Metadata)
	}
}

// The client replays what it was sent. This closes the loop the previous test
// opens: the bytes that left become the RunAgentInput of the next run, and the
// reassembled message is the one the provider replays the call from.
func TestTheStreamedSignatureReplaysBackOntoTheMessage(t *testing.T) {
	// Exactly the shape the patched client sends back: the token on the call it
	// was issued against, inside the assistant message that made the call.
	raw := `{
	  "threadId": "thread-1",
	  "runId": "run-2",
	  "messages": [
	    {"id": "m1", "role": "user", "content": "go"},
	    {"id": "m2", "role": "assistant", "toolCalls": [
	      {"id": "call_0_a", "type": "function", "function": {"name": "a", "arguments": "{}"},
	       "metadata": {"gemini_thought_signatures": {"call_0_a": "opaque-token"}}},
	      {"id": "call_1_b", "type": "function", "function": {"name": "b", "arguments": "{}"}}
	    ]},
	    {"id": "m3", "role": "tool", "toolCallId": "call_0_a", "content": "ok"},
	    {"id": "m4", "role": "user", "content": "and the quarter before"}
	  ]
	}`
	var in struct {
		ThreadID string         `json:"threadId"`
		Messages []agui.Message `json:"messages"`
	}
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("decode: %v", err)
	}

	p := &Plugin{}
	ui := p.buildUserInput(runInput{threadID: in.ThreadID, messages: in.Messages})

	var assistant *events.Message
	for i := range ui.PreloadMessages {
		if len(ui.PreloadMessages[i].ToolCalls) > 0 {
			assistant = &ui.PreloadMessages[i]
		}
	}
	if assistant == nil {
		t.Fatal("the replayed assistant turn did not survive preload")
	}
	sigs, ok := assistant.Metadata["gemini_thought_signatures"].(map[string]any)
	if !ok {
		t.Fatalf("replayed metadata = %#v, want the signature map", assistant.Metadata)
	}
	if sigs["call_0_a"] != "opaque-token" {
		t.Errorf("replayed signature = %v, want the token the stream carried", sigs["call_0_a"])
	}
	if len(assistant.ToolCalls) != 2 {
		t.Errorf("replayed %d tool calls, want both", len(assistant.ToolCalls))
	}
}
