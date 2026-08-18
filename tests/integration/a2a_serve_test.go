//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/events"
)

// a2aBindAddr is the loopback address configured in configs/test-a2a-serve.yaml.
// Integration tests run sequentially, so a fixed high port is safe.
const a2aBindAddr = "127.0.0.1:18191"

// a2aToken is the shared bearer token that config guards the operations with.
const a2aToken = "test-a2a-token"

// a2aURL builds an absolute URL against the test listener.
func a2aURL(path string) string { return "http://" + a2aBindAddr + path }

// a2aPost issues an authenticated A2A request and returns the response. The
// caller owns the body.
func a2aPost(t *testing.T, path string, payload any) *http.Response {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, a2aURL(path), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+a2aToken)
	req.Header.Set("Content-Type", a2a.ContentTypeJSON)
	req.Header.Set(a2a.HeaderVersion, a2a.ProtocolVersion)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// a2aSendMessage builds the parameter object for a SendMessage-family call.
func a2aSendMessage(messageID, text, contextID string) map[string]any {
	msg := map[string]any{
		"messageId": messageID,
		"role":      string(a2a.RoleUser),
		"parts":     []any{map[string]any{"text": text}},
	}
	if contextID != "" {
		msg["contextId"] = contextID
	}
	return map[string]any{"message": msg}
}

// a2aRPC wraps params in a JSON-RPC envelope.
func a2aRPC(method string, params any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}
}

// a2aStates renders the task states a frame sequence reports, in order.
func a2aStates(fs []a2a.StreamResponse) []a2a.TaskState {
	var out []a2a.TaskState
	for _, f := range fs {
		switch f.Kind() {
		case a2a.StreamPayloadTask:
			out = append(out, f.Task.Status.State)
		case a2a.StreamPayloadStatusUpdate:
			out = append(out, f.StatusUpdate.Status.State)
		}
	}
	return out
}

// TestA2AServe_MockStreamingTurn is the end-to-end payoff of the A2A serve leg,
// in mock mode so it needs no API key: an external A2A client POSTs a message,
// a real Nexus turn runs, and the client watches one Task stream SUBMITTED ->
// WORKING -> COMPLETED with the final assistant text as an artifact.
func TestA2AServe_MockStreamingTurn(t *testing.T) {
	bootEngine(t, "configs/test-a2a-serve.yaml")
	waitForListener(t, a2aBindAddr)

	resp := a2aPost(t, "/a2a", a2aRPC(a2a.MethodSendStreamingMessage,
		a2aSendMessage("m-1", "What is the mock answer?", "a2a-ctx-1")))
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != a2a.ContentTypeSSE {
		t.Fatalf("content-type = %q, want %q", ct, a2a.ContentTypeSSE)
	}
	if v := resp.Header.Get(a2a.HeaderVersion); v != a2a.ProtocolVersion {
		t.Errorf("%s = %q, want %q", a2a.HeaderVersion, v, a2a.ProtocolVersion)
	}

	// The reader applies the section 11.7 stream contract, so a malformed
	// sequence fails here rather than in an assertion below.
	frames, err := a2a.NewSSEReader(resp.Body).ReadAll()
	if err != nil {
		t.Fatalf("reading the a2a stream: %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("no stream frames decoded")
	}

	want := []a2a.TaskState{a2a.TaskStateSubmitted, a2a.TaskStateWorking, a2a.TaskStateCompleted}
	got := a2aStates(frames)
	if len(got) != len(want) {
		t.Fatalf("task states = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("task states = %v, want %v", got, want)
		}
	}

	// One task per turn, and its context is the one the client named.
	task := frames[0].Task
	if task.ID == "" {
		t.Fatal("the opening frame carries no task id")
	}
	if task.ContextID != "a2a-ctx-1" {
		t.Errorf("contextId = %q, want the requested context", task.ContextID)
	}

	// The mocked assistant text arrived as an artifact with a text part.
	var artifactText string
	for _, f := range frames {
		if f.Kind() != a2a.StreamPayloadArtifactUpdate {
			continue
		}
		if f.ArtifactUpdate.TaskID != task.ID {
			t.Errorf("artifact names task %q, want %q", f.ArtifactUpdate.TaskID, task.ID)
		}
		for _, part := range f.ArtifactUpdate.Artifact.Parts {
			if text, ok := part.TextValue(); ok {
				artifactText += text
			}
		}
	}
	if !strings.Contains(artifactText, "Hello from a mocked Nexus agent") {
		t.Fatalf("artifact text = %q, want the mocked assistant response", artifactText)
	}
}

// TestA2AServe_MockContextContinuesTheSession asserts the contextId mapping: a
// second call on a known context runs another turn in the same Nexus session
// with the conversation history intact, and a foreign context is refused rather
// than silently handed this conversation.
func TestA2AServe_MockContextContinuesTheSession(t *testing.T) {
	eng := bootEngine(t, "configs/test-a2a-serve.yaml")
	waitForListener(t, a2aBindAddr)

	first := a2aSendBlocking(t, "m-1", "first question", "a2a-ctx-2")
	if first.Task == nil {
		t.Fatalf("first call returned no task: %+v", first)
	}
	if first.Task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("first task state = %s, want COMPLETED", first.Task.Status.State)
	}

	second := a2aSendBlocking(t, "m-2", "second question", first.Task.ContextID)
	if second.Task == nil {
		t.Fatalf("second call returned no task: %+v", second)
	}
	if second.Task.ID == first.Task.ID {
		t.Error("both turns reported the same task id; one task is one turn")
	}
	if second.Task.ContextID != first.Task.ContextID {
		t.Errorf("context changed between turns: %q then %q", first.Task.ContextID, second.Task.ContextID)
	}

	// History intact: the session's conversation buffer holds both prompts and
	// both answers, which is what "continues the session" has to mean.
	hq := &events.HistoryQuery{SchemaVersion: events.HistoryQueryVersion}
	if err := eng.Bus.Emit("memory.history.query", hq); err != nil {
		t.Fatalf("memory.history.query: %v", err)
	}
	var joined string
	for _, m := range hq.Messages {
		joined += m.Role + ":" + m.Content + "\n"
	}
	for _, want := range []string{"first question", "second question", "Hello from a mocked Nexus agent"} {
		if !strings.Contains(joined, want) {
			t.Errorf("conversation history is missing %q; history:\n%s", want, joined)
		}
	}

	// A different context is refused, naming the bound one.
	resp := a2aPost(t, "/a2a", a2aRPC(a2a.MethodSendMessage,
		a2aSendMessage("m-3", "third question", "some-other-context")))
	defer func() { _ = resp.Body.Close() }()
	var envelope a2a.Response
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Error == nil {
		t.Fatal("a foreign context was accepted; this listener serves one Nexus session")
	}
	if envelope.Error.Code != a2a.CodeUnsupportedOperation {
		t.Errorf("error code = %d, want %d", envelope.Error.Code, a2a.CodeUnsupportedOperation)
	}
}

// TestA2AServe_MockRESTBinding asserts the HTTP+JSON binding drives the same
// turn in its own framing, and that the card now advertises streaming.
func TestA2AServe_MockRESTBinding(t *testing.T) {
	bootEngine(t, "configs/test-a2a-serve.yaml")
	waitForListener(t, a2aBindAddr)

	// The card is the client's entry point, and it must not understate what the
	// listener does.
	cardResp, err := http.Get(a2aURL(a2a.AgentCardPath))
	if err != nil {
		t.Fatalf("fetch agent card: %v", err)
	}
	defer func() { _ = cardResp.Body.Close() }()
	var card a2a.AgentCard
	if err := json.NewDecoder(cardResp.Body).Decode(&card); err != nil {
		t.Fatalf("decode agent card: %v", err)
	}
	if !card.Capabilities.Streaming {
		t.Error("capabilities.streaming = false while SendStreamingMessage drives real turns")
	}

	resp := a2aPost(t, "/a2a/v1/message:send", a2aSendMessage("m-rest", "rest question", "a2a-ctx-rest"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var result a2a.SendMessageResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Task == nil || result.Task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("task = %+v, want a COMPLETED task", result.Task)
	}
	if len(result.Task.Artifacts) == 0 {
		t.Fatal("the completed task carries no artifact")
	}
}

// TestA2AServe_MockUnauthenticated pins that the turn-driving operations stay
// behind the credential guard.
func TestA2AServe_MockUnauthenticated(t *testing.T) {
	bootEngine(t, "configs/test-a2a-serve.yaml")
	waitForListener(t, a2aBindAddr)

	body, err := json.Marshal(a2aRPC(a2a.MethodSendMessage, a2aSendMessage("m-x", "hello", "a2a-ctx-auth")))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, a2aURL("/a2a"), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", a2a.ContentTypeJSON)
	req.Header.Set(a2a.HeaderVersion, a2a.ProtocolVersion)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if ch := resp.Header.Get("WWW-Authenticate"); ch == "" {
		t.Error("no RFC 6750 challenge on the refusal")
	}
}

// a2aSendBlocking runs one blocking SendMessage and returns the decoded result.
func a2aSendBlocking(t *testing.T, messageID, text, contextID string) a2a.SendMessageResponse {
	t.Helper()
	resp := a2aPost(t, "/a2a", a2aRPC(a2a.MethodSendMessage, a2aSendMessage(messageID, text, contextID)))
	defer func() { _ = resp.Body.Close() }()

	var envelope a2a.Response
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Error != nil {
		t.Fatalf("SendMessage refused: %+v", envelope.Error)
	}
	var result a2a.SendMessageResponse
	if err := envelope.DecodeResult(&result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return result
}
