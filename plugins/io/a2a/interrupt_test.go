package a2a

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/storage"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// This file covers the two ways a task leaves the ordinary WORKING path — a
// human-in-the-loop interruption and a cancellation — plus the change that
// makes both possible: a run's lifetime is its TASK's, not its request's.

// ---- fixtures ----

// hitlAgent is a stand-in for the pair of plugins a real interruption involves:
// an agent that asks a question mid-turn, and nexus.control.hitl, which owns
// the pending-request registry and unblocks the asker when an answer arrives.
//
// The blocking is the point. The real ask_user tool blocks the turn's goroutine
// inside a tool.invoke dispatch until hitl.responded fires, so a turn parked on
// a question really does hold the process's one agent loop. Anything that only
// simulated the STATE would not exercise what the deadline and CancelTask exist
// to protect against.
type hitlAgent struct {
	requestID string
	question  string
	choices   []events.HITLChoice
	// answered receives the response that unblocked the turn.
	answered chan events.HITLResponse
	// asked closes once the question has been put on the bus.
	asked chan struct{}

	mu       sync.Mutex
	cancels  []events.HITLCancel
	requests []events.CancelRequest
	turns    int
}

// newHITLAgent wires the fake onto the bus and returns it. reply is the text the
// turn publishes once its question is answered.
func newHITLAgent(t *testing.T, bus engine.EventBus, reply string, choices ...events.HITLChoice) *hitlAgent {
	t.Helper()
	h := &hitlAgent{
		requestID: "hitl-1",
		question:  "which environment should I deploy to?",
		choices:   choices,
		answered:  make(chan events.HITLResponse, 1),
		asked:     make(chan struct{}),
	}
	// A turn parked for ever would leak the goroutine holding it once the test
	// ends, so an unanswered question is released on cleanup exactly as the real
	// hitl plugin releases one: a cancelled response.
	t.Cleanup(func() {
		select {
		case h.answered <- events.HITLResponse{
			SchemaVersion: events.HITLResponseVersion, RequestID: h.requestID, Cancelled: true,
		}:
		default:
		}
	})

	// The control/hitl half: route an answer to the waiting turn, and record the
	// retractions this transport emits.
	bus.Subscribe("hitl.responded", func(e engine.Event[any]) {
		resp, ok := e.Payload.(events.HITLResponse)
		if !ok {
			return
		}
		select {
		case h.answered <- resp:
		default:
		}
	}, engine.WithSource("test.hitl"))
	bus.Subscribe("hitl.cancel", func(e engine.Event[any]) {
		c, ok := e.Payload.(events.HITLCancel)
		if !ok {
			return
		}
		h.mu.Lock()
		h.cancels = append(h.cancels, c)
		h.mu.Unlock()
		// Exactly what the real plugin does: a retraction becomes a synthetic
		// cancelled response so the waiter unblocks.
		_ = bus.Emit("hitl.responded", events.HITLResponse{
			SchemaVersion: events.HITLResponseVersion,
			RequestID:     c.RequestID, Cancelled: true, CancelReason: c.Reason,
		})
	}, engine.WithSource("test.hitl"))
	bus.Subscribe("cancel.request", func(e engine.Event[any]) {
		req, ok := e.Payload.(events.CancelRequest)
		if !ok {
			return
		}
		h.mu.Lock()
		h.requests = append(h.requests, req)
		h.mu.Unlock()
	}, engine.WithSource("test.cancel"))

	playAgent(t, bus, func(b engine.EventBus, in events.UserInput) {
		h.mu.Lock()
		h.turns++
		h.mu.Unlock()

		_ = b.Emit("agent.turn.start", events.TurnInfo{
			SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1", SessionID: in.SessionID,
		})
		_ = b.Emit("hitl.requested", events.HITLRequest{
			SchemaVersion:   events.HITLRequestVersion,
			ID:              h.requestID,
			TurnID:          "turn-1",
			RequesterPlugin: "nexus.control.hitl",
			Prompt:          h.question,
			Choices:         h.choices,
		})
		close(h.asked)

		// Blocked exactly as ask_user blocks, and for as long.
		resp := <-h.answered
		if resp.Cancelled {
			return
		}
		answer := resp.FreeText
		if resp.ChoiceID != "" {
			answer = resp.ChoiceID
		}
		_ = b.Emit("io.output", events.AgentOutput{
			SchemaVersion: events.AgentOutputVersion,
			Content:       reply + answer, Role: "assistant", TurnID: "turn-1",
		})
		_ = b.Emit("agent.turn.end", events.TurnInfo{
			SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1",
		})
	})
	return h
}

func (h *hitlAgent) canceled() []events.HITLCancel {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]events.HITLCancel(nil), h.cancels...)
}

func (h *hitlAgent) cancelRequests() []events.CancelRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]events.CancelRequest(nil), h.requests...)
}

func (h *hitlAgent) turnCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.turns
}

// resumeParams builds a SendMessage that continues an interrupted task: the
// same taskId and contextId, which is A2A's own resume mechanism (§3.4).
func resumeParams(text, taskID, contextID string) map[string]any {
	params := sendMessageParams(text, contextID)
	msg := params["message"].(map[string]any)
	msg["taskId"] = taskID
	return params
}

// awaitTaskState polls GetTask over a real listener until the task reports the
// wanted state. It reads through the same HTTP surface a client would, so it
// proves the state is visible to a client rather than merely present in memory.
func awaitTaskState(ctx context.Context, t *testing.T, base, taskID string, want a2a.TaskState) a2a.Task {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last a2a.Task
	for time.Now().Before(deadline) {
		body, err := get(ctx, base+"/a2a/v1/tasks/"+taskID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if err := json.Unmarshal(body, &last); err != nil {
			t.Fatalf("decode task: %v (%s)", err, body)
		}
		if last.Status.State == want {
			return last
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("task %s never reached %s (last %s)", taskID, want, last.Status.State)
	return last
}

// ---- INPUT_REQUIRED ----

// TestHITLRequestParksTheTask is the first acceptance criterion: hitl.requested
// moves the task to INPUT_REQUIRED with the question on the status message, and
// the blocking call returns rather than waiting for a human through an HTTP
// request.
func TestHITLRequestParksTheTask(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	agent := newHITLAgent(t, bus, "deploying to ")

	rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
		jsonrpcBody(t, a2a.MethodSendMessage, sendMessageParams("deploy the service", "ctx-hitl")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	resp := rpcResponse(t, rec.Body.Bytes())
	if resp.Error != nil {
		t.Fatalf("error response: %+v", resp.Error)
	}
	var result a2a.SendMessageResponse
	if err := resp.DecodeResult(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Task == nil {
		t.Fatal("no task returned")
	}
	if result.Task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("state = %s, want %s", result.Task.Status.State, a2a.TaskStateInputRequired)
	}
	if result.Task.Status.Message == nil {
		t.Fatal("the INPUT_REQUIRED status carries no question")
	}
	text, _ := result.Task.Status.Message.Parts[0].TextValue()
	if !strings.Contains(text, agent.question) {
		t.Errorf("status message = %q, want the agent's question", text)
	}
	if result.Task.Status.Message.TaskID != result.Task.ID {
		t.Error("the question is not stamped with the task it belongs to")
	}
	if id, _ := result.Task.Status.Message.Metadata[metadataHITLRequestID].(string); id != agent.requestID {
		t.Errorf("question metadata %v, want the originating hitl request id", result.Task.Status.Message.Metadata)
	}

	// The task is still LIVE: a parked task is interrupted, not finished.
	if result.Task.Status.State.IsTerminal() {
		t.Error("an interrupted task must not be terminal")
	}
	// And durable: the transition went through the write-through path.
	stored := do(t, p.server, http.MethodGet, "/a2a/v1/tasks/"+result.Task.ID, withVersion("1.0"))
	var fromStore a2a.Task
	if err := json.Unmarshal(stored.Body.Bytes(), &fromStore); err != nil {
		t.Fatalf("decode stored task: %v", err)
	}
	if fromStore.Status.State != a2a.TaskStateInputRequired {
		t.Errorf("stored state = %s, want the parked state to be durable", fromStore.Status.State)
	}
}

// TestStreamStaysOpenWhileParked pins pkg/a2a's parking contract at the serving
// layer: an INPUT_REQUIRED frame is written and the stream does NOT close,
// because closing on a non-terminal state is indistinguishable client-side from
// a dropped connection.
func TestStreamStaysOpenWhileParked(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	newHITLAgent(t, bus, "answer: ")

	srv := httptest.NewServer(p.server.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	body, err := postSSE(ctx, srv.URL+"/a2a", a2a.ContentTypeJSON,
		jsonrpcPayload(t, a2a.MethodSendStreamingMessage, sendMessageParams("deploy", "ctx-park")))
	if err != nil {
		t.Fatalf("SendStreamingMessage: %v", err)
	}
	defer body.Close()

	reader := a2a.NewSSEReader(body)
	opening, err := reader.Next()
	if err != nil {
		t.Fatalf("opening frame: %v", err)
	}
	taskID := opening.Task.ID

	// SUBMITTED, WORKING, then the question — and the reader is still able to
	// wait for more, which is what "stayed open" means.
	var seen []a2a.TaskState
	for range 2 {
		frame, err := reader.Next()
		if err != nil {
			t.Fatalf("reading the parked stream: %v", err)
		}
		seen = append(seen, frame.StatusUpdate.Status.State)
	}
	if len(seen) != 2 || seen[0] != a2a.TaskStateWorking || seen[1] != a2a.TaskStateInputRequired {
		t.Fatalf("states = %v, want WORKING then INPUT_REQUIRED", seen)
	}

	// Answer on a second call and watch the SAME stream carry the rest.
	answer := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
		jsonrpcBody(t, a2a.MethodSendMessage, resumeParams("staging", taskID, "ctx-park")))
	if resp := rpcResponse(t, answer.Body.Bytes()); resp.Error != nil {
		t.Fatalf("resume refused: %+v", resp.Error)
	}

	rest := summarize(drain(reader))
	want := []string{
		"status:" + string(a2a.TaskStateWorking),
		"artifact:" + taskID + artifactSuffix,
		"status:" + string(a2a.TaskStateCompleted),
	}
	if strings.Join(rest, "|") != strings.Join(want, "|") {
		t.Errorf("the parked stream carried %v after the answer, want %v", rest, want)
	}
}

// TestAskDisconnectReattachAnswerComplete is the story's headline test, taken
// literally: the agent asks, the client's connection goes away entirely, a new
// connection reattaches with SubscribeToTask, the answer arrives on a third
// call, and the task completes — all inside ONE turn.
//
// It runs against a real listener because "the client disconnected" only means
// anything over a real socket: the test closes the response body, which is what
// cancels the server-side request context.
func TestAskDisconnectReattachAnswerComplete(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	agent := newHITLAgent(t, bus, "deployed to ")

	srv := httptest.NewServer(p.server.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// 1. Ask. The originating stream sees the question.
	body, err := postSSE(ctx, srv.URL+"/a2a", a2a.ContentTypeJSON,
		jsonrpcPayload(t, a2a.MethodSendStreamingMessage, sendMessageParams("deploy the service", "ctx-1")))
	if err != nil {
		t.Fatalf("SendStreamingMessage: %v", err)
	}
	reader := a2a.NewSSEReader(body)
	opening, err := reader.Next()
	if err != nil {
		t.Fatalf("opening frame: %v", err)
	}
	taskID := opening.Task.ID
	<-agent.asked

	// 2. Disconnect, hard. The task must survive it: this is the case that used
	//    to fail its own task the moment the client vanished.
	_ = body.Close()

	parked := awaitTaskState(ctx, t, srv.URL, taskID, a2a.TaskStateInputRequired)
	if parked.Status.Message == nil {
		t.Fatal("the reconnecting client cannot see what it was asked")
	}
	if text, _ := parked.Status.Message.Parts[0].TextValue(); !strings.Contains(text, agent.question) {
		t.Errorf("stored question = %q, want the agent's question", text)
	}

	// 3. Reattach. The new stream opens on the state the task is actually in.
	reattached, err := postSSE(ctx, srv.URL+"/a2a/v1/tasks/"+taskID+":subscribe", a2a.ContentTypeJSON, nil)
	if err != nil {
		t.Fatalf("SubscribeToTask: %v", err)
	}
	defer reattached.Close()
	follow := a2a.NewSSEReader(reattached)
	snapshot, err := follow.Next()
	if err != nil {
		t.Fatalf("reattached opening frame: %v", err)
	}
	if snapshot.Task == nil || snapshot.Task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("reattached snapshot = %+v, want the parked task", snapshot.Task)
	}

	// 4. Answer, naming the same taskId and contextId.
	answered := make(chan a2a.Task, 1)
	go func() {
		reply, err := postJSON(ctx, srv.URL+"/a2a",
			jsonrpcPayload(t, a2a.MethodSendMessage, resumeParams("staging", taskID, "ctx-1")))
		if err != nil {
			answered <- a2a.Task{}
			return
		}
		var resp a2a.Response
		if err := json.Unmarshal(reply, &resp); err != nil || resp.Error != nil {
			answered <- a2a.Task{}
			return
		}
		var result a2a.SendMessageResponse
		if err := resp.DecodeResult(&result); err != nil || result.Task == nil {
			answered <- a2a.Task{}
			return
		}
		answered <- *result.Task
	}()

	// 5. The reattached stream carries the rest of the SAME turn.
	rest := summarize(drain(follow))
	want := []string{
		"status:" + string(a2a.TaskStateWorking),
		"artifact:" + taskID + artifactSuffix,
		"status:" + string(a2a.TaskStateCompleted),
	}
	if strings.Join(rest, "|") != strings.Join(want, "|") {
		t.Fatalf("the reattached stream saw %v, want %v", rest, want)
	}

	select {
	case task := <-answered:
		if task.Status.State != a2a.TaskStateCompleted {
			t.Errorf("the resuming call returned %s, want COMPLETED", task.Status.State)
		}
		if len(task.Artifacts) != 1 {
			t.Fatalf("artifacts = %+v, want the turn's answer", task.Artifacts)
		}
		if text, _ := task.Artifacts[0].Parts[0].TextValue(); text != "deployed to staging" {
			t.Errorf("artifact = %q, want the answer folded into the turn's reply", text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the resuming call never returned")
	}

	// ONE turn, not two: a resumed task continues the turn that asked.
	if got := agent.turnCount(); got != 1 {
		t.Errorf("the agent ran %d turns, want exactly 1 — resuming must not start a new one", got)
	}
	// And one task: no second row appeared for the answer.
	list, err := get(ctx, srv.URL+"/a2a/v1/tasks")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	var page a2a.ListTasksResponse
	if err := json.Unmarshal(list, &page); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(page.Tasks) != 1 || page.Tasks[0].ID != taskID {
		t.Errorf("tasks = %v, want only the one that was resumed", listedIDs(page.Tasks))
	}
	// The history holds both halves of the exchange the client took part in.
	if len(page.Tasks[0].History) < 3 {
		t.Errorf("history = %d messages, want the prompt, the question and the answer",
			len(page.Tasks[0].History))
	}
}

// TestResumeMatchesAChoiceID pins the multiple-choice half: an A2A client can
// only send text, so the text is matched against the question's option ids and
// the requesting plugin sees the id it published.
func TestResumeMatchesAChoiceID(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	newHITLAgent(t, bus, "chose ",
		events.HITLChoice{ID: "staging", Label: "Staging"},
		events.HITLChoice{ID: "production", Label: "Production"})

	first := jsonrpcSend(t, p, a2a.MethodSendMessage, sendMessageParams("deploy", "ctx-choice"))
	var result a2a.SendMessageResponse
	if err := first.DecodeResult(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	question, _ := result.Task.Status.Message.Parts[0].TextValue()
	for _, want := range []string{"staging", "production", "Staging"} {
		if !strings.Contains(question, want) {
			t.Errorf("question %q does not offer %q; a text-only client cannot answer it", question, want)
		}
	}

	second := jsonrpcSend(t, p, a2a.MethodSendMessage,
		resumeParams("  PRODUCTION ", result.Task.ID, "ctx-choice"))
	if second.Error != nil {
		t.Fatalf("resume refused: %+v", second.Error)
	}
	var done a2a.SendMessageResponse
	if err := second.DecodeResult(&done); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if text, _ := done.Task.Artifacts[0].Parts[0].TextValue(); text != "chose production" {
		t.Errorf("answer = %q, want the canonical choice id, matched case-insensitively", text)
	}
}

// TestResumeRefusals pins every way continuing a task is not allowed, each with
// the error the specification reserves for it. The unknown-task case is the
// security-relevant one: it is answered before any state is revealed.
func TestResumeRefusals(t *testing.T) {
	t.Run("a task nobody minted", func(t *testing.T) {
		p, bus := newTestPlugin(t, nil)
		playAgent(t, bus, scriptedTurn("never reached"))
		resp := jsonrpcSend(t, p, a2a.MethodSendMessage, resumeParams("hi", "task-ghost", "ctx-1"))
		if resp.Error == nil || resp.Error.Code != a2a.CodeTaskNotFound {
			t.Fatalf("error = %+v, want TaskNotFoundError", resp.Error)
		}
	})

	t.Run("a terminal task", func(t *testing.T) {
		p, created := completedTurn(t, "go", "done", "ctx-1")
		resp := jsonrpcSend(t, p, a2a.MethodSendMessage, resumeParams("more", created.ID, "ctx-1"))
		if resp.Error == nil || resp.Error.Code != a2a.CodeUnsupportedOperation {
			t.Fatalf("error = %+v, want UnsupportedOperationError", resp.Error)
		}
		if !strings.Contains(resp.Error.Message, "COMPLETED") {
			t.Errorf("message = %q, want it to name the terminal state", resp.Error.Message)
		}
	})

	t.Run("the wrong context", func(t *testing.T) {
		p, bus := newTestPlugin(t, nil)
		newHITLAgent(t, bus, "answer ")
		first := jsonrpcSend(t, p, a2a.MethodSendMessage, sendMessageParams("deploy", "ctx-right"))
		var parked a2a.SendMessageResponse
		if err := first.DecodeResult(&parked); err != nil {
			t.Fatalf("decode: %v", err)
		}
		resp := jsonrpcSend(t, p, a2a.MethodSendMessage,
			resumeParams("staging", parked.Task.ID, "ctx-wrong"))
		if resp.Error == nil {
			t.Fatal("a task was continued from a context it does not belong to")
		}
		if !strings.Contains(resp.Error.Message, "ctx-right") {
			t.Errorf("message = %q, want it to name the task's real context", resp.Error.Message)
		}
	})

	t.Run("a task that is not waiting for input", func(t *testing.T) {
		p, bus := newTestPlugin(t, nil)
		// An agent that starts and then stalls: WORKING, never parked.
		playAgent(t, bus, func(b engine.EventBus, in events.UserInput) {
			_ = b.Emit("agent.turn.start", events.TurnInfo{
				SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1",
			})
		})
		srv := httptest.NewServer(p.server.Handler())
		defer srv.Close()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()

		body, err := postSSE(ctx, srv.URL+"/a2a", a2a.ContentTypeJSON,
			jsonrpcPayload(t, a2a.MethodSendStreamingMessage, sendMessageParams("go", "ctx-working")))
		if err != nil {
			t.Fatalf("SendStreamingMessage: %v", err)
		}
		defer body.Close()
		reader := a2a.NewSSEReader(body)
		opening, err := reader.Next()
		if err != nil {
			t.Fatalf("opening frame: %v", err)
		}
		taskID := opening.Task.ID
		awaitTaskState(ctx, t, srv.URL, taskID, a2a.TaskStateWorking)

		reply, err := postJSON(ctx, srv.URL+"/a2a",
			jsonrpcPayload(t, a2a.MethodSendMessage, resumeParams("unasked for", taskID, "ctx-working")))
		if err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
		var resp a2a.Response
		if err := json.Unmarshal(reply, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Error == nil || resp.Error.Code != a2a.CodeUnsupportedOperation {
			t.Fatalf("error = %+v, want UnsupportedOperationError", resp.Error)
		}
	})
}

// TestForeignTaskCannotBeResumedOrCanceled extends E2-S1's indistinguishability
// rule to the two operations this story adds. A task belonging to another
// principal must answer exactly as an unknown id does, byte for byte — a
// different refusal for "exists but is not yours" would be an existence oracle,
// and for CancelTask it would additionally leak whether the task was still live.
func TestForeignTaskCannotBeResumedOrCanceled(t *testing.T) {
	p, _ := newTestPlugin(t, twoPrincipalConfig(t))
	seedTask(t, p, "partner-b", "task-owned-by-b", "ctx-b", a2a.TaskStateSubmitted)
	const unknownID = "task-nobody-ever-minted"

	t.Run("CancelTask", func(t *testing.T) {
		foreign := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"), withBearer("token-a"),
			jsonrpcBody(t, a2a.MethodCancelTask, map[string]any{"id": "task-owned-by-b"}))
		unknown := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"), withBearer("token-a"),
			jsonrpcBody(t, a2a.MethodCancelTask, map[string]any{"id": unknownID}))
		assertIndistinguishable(t, foreign, unknown, "task-owned-by-b", unknownID)

		var resp a2a.Response
		if err := json.Unmarshal(foreign.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Error == nil || resp.Error.Code != a2a.CodeTaskNotFound {
			t.Fatalf("error = %+v, want TaskNotFoundError", resp.Error)
		}
	})

	t.Run("resume", func(t *testing.T) {
		foreign := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"), withBearer("token-a"),
			jsonrpcBody(t, a2a.MethodSendMessage, resumeParams("carry on", "task-owned-by-b", "ctx-b")))
		unknown := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"), withBearer("token-a"),
			jsonrpcBody(t, a2a.MethodSendMessage, resumeParams("carry on", unknownID, "ctx-b")))
		assertIndistinguishable(t, foreign, unknown, "task-owned-by-b", unknownID)
	})

	t.Run("its owner is refused for the honest reason", func(t *testing.T) {
		rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"), withBearer("token-b"),
			jsonrpcBody(t, a2a.MethodCancelTask, map[string]any{"id": "task-owned-by-b"}))
		resp := rpcResponse(t, rec.Body.Bytes())
		if resp.Error != nil {
			t.Fatalf("partner-b could not cancel its own task: %+v", resp.Error)
		}
		var task a2a.Task
		if err := resp.DecodeResult(&task); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if task.Status.State != a2a.TaskStateCanceled {
			t.Errorf("state = %s, want CANCELED", task.Status.State)
		}
	})
}

// ---- the input deadline ----

// TestInputDeadlineFailsTheTask pins the deadline policy: an unanswered question
// must not pin the session for ever. On expiry the task is driven to a REAL
// terminal state — which closes every attached stream by the ordinary rule —
// and hitl.cancel retracts the question so the blocked agent loop unblocks.
func TestInputDeadlineFailsTheTask(t *testing.T) {
	p, bus := newTestPlugin(t, map[string]any{
		"tasks": map[string]any{"input_timeout": "50ms"},
	})
	agent := newHITLAgent(t, bus, "unreachable ")

	rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
		jsonrpcBody(t, a2a.MethodSendStreamingMessage, sendMessageParams("deploy", "ctx-deadline")))
	fs := frames(t, rec.Body.Bytes())
	want := []a2a.TaskState{
		a2a.TaskStateSubmitted, a2a.TaskStateWorking, a2a.TaskStateInputRequired, a2a.TaskStateFailed,
	}
	if got := states(fs); !equalStates(got, want) {
		t.Fatalf("task states = %v, want %v", got, want)
	}
	last := fs[len(fs)-1].StatusUpdate.Status
	if last.Message == nil {
		t.Fatal("the timed-out task carries no explanation")
	}
	if text, _ := last.Message.Parts[0].TextValue(); !strings.Contains(text, "no answer arrived") {
		t.Errorf("failure message = %q, want it to say the input never came", text)
	}

	// The question was retracted on the bus, which is what frees the agent.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(agent.canceled()) > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	got := agent.canceled()
	if len(got) != 1 || got[0].RequestID != agent.requestID {
		t.Fatalf("hitl.cancel = %+v, want one retraction naming the parked question", got)
	}

	// The slot is free again: the next caller is not refused.
	p2 := jsonrpcSend(t, p, a2a.MethodGetTask, map[string]any{"id": fs[0].Task.ID})
	var task a2a.Task
	if err := p2.DecodeResult(&task); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if task.Status.State != a2a.TaskStateFailed {
		t.Errorf("stored state = %s, want the timeout to be durable", task.Status.State)
	}
}

// TestInputDeadlineIsConfigurable pins both ends of the knob, including the
// documented default. The default is the one number an operator inherits
// without asking for it, so it is asserted rather than assumed.
func TestInputDeadlineIsConfigurable(t *testing.T) {
	base, err := parseConfig(testConfig(t, nil))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if base.inputTimeout != 15*time.Minute {
		t.Errorf("default input_timeout = %s, want 15m", base.inputTimeout)
	}

	set, err := parseConfig(testConfig(t, map[string]any{
		"tasks": map[string]any{"input_timeout": "90s"},
	}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if set.inputTimeout != 90*time.Second {
		t.Errorf("input_timeout = %s, want 90s", set.inputTimeout)
	}

	off, err := parseConfig(testConfig(t, map[string]any{
		"tasks": map[string]any{"input_timeout": "0s"},
	}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if off.inputTimeout != 0 {
		t.Errorf("input_timeout = %s, want the deadline disabled", off.inputTimeout)
	}

	for _, bad := range []any{600, "-1m", "not a duration"} {
		if _, err := parseConfig(testConfig(t, map[string]any{
			"tasks": map[string]any{"input_timeout": bad},
		})); err == nil {
			t.Errorf("input_timeout %v was accepted", bad)
		}
	}
}

// ---- CancelTask ----

// TestCancelTaskSettlesAtCanceled is the cancellation criterion: the operation
// invokes the control.cancel capability through the bus and settles the task at
// CANCELED, and the open stream closes on the terminal transition.
func TestCancelTaskSettlesAtCanceled(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	agent := newHITLAgent(t, bus, "never ")

	srv := httptest.NewServer(p.server.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	body, err := postSSE(ctx, srv.URL+"/a2a", a2a.ContentTypeJSON,
		jsonrpcPayload(t, a2a.MethodSendStreamingMessage, sendMessageParams("deploy", "ctx-cancel")))
	if err != nil {
		t.Fatalf("SendStreamingMessage: %v", err)
	}
	defer body.Close()
	reader := a2a.NewSSEReader(body)
	opening, err := reader.Next()
	if err != nil {
		t.Fatalf("opening frame: %v", err)
	}
	taskID := opening.Task.ID
	<-agent.asked
	awaitTaskState(ctx, t, srv.URL, taskID, a2a.TaskStateInputRequired)

	reply, err := postJSON(ctx, srv.URL+"/a2a",
		jsonrpcPayload(t, a2a.MethodCancelTask, map[string]any{"id": taskID}))
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	var resp a2a.Response
	if err := json.Unmarshal(reply, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("CancelTask refused: %+v", resp.Error)
	}
	var task a2a.Task
	if err := resp.DecodeResult(&task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	if task.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("state = %s, want CANCELED", task.Status.State)
	}
	if task.ID != taskID {
		t.Errorf("CancelTask answered about task %q, want %q", task.ID, taskID)
	}

	// The open stream ends at the terminal transition, on its own.
	rest := summarize(drain(reader))
	if len(rest) == 0 || rest[len(rest)-1] != "status:"+string(a2a.TaskStateCanceled) {
		t.Errorf("the open stream ended with %v, want a CANCELED terminal frame", rest)
	}

	// The bus was told, in both the ways a parked task needs.
	if got := agent.canceled(); len(got) != 1 || got[0].RequestID != agent.requestID {
		t.Errorf("hitl.cancel = %+v, want the parked question retracted", got)
	}
	if got := agent.cancelRequests(); len(got) != 1 || got[0].Source != cancelSource {
		t.Errorf("cancel.request = %+v, want one naming this transport", got)
	}
}

// TestCancelTerminalTaskIsAWellDefinedError pins the refusal: a terminal state
// is final, so cancelling one is a client mistake with a named error rather
// than a silent success that would tell the client its cancel took effect on a
// task that had already completed.
func TestCancelTerminalTaskIsAWellDefinedError(t *testing.T) {
	p, created := completedTurn(t, "go", "done", "ctx-1")

	rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
		jsonrpcBody(t, a2a.MethodCancelTask, map[string]any{"id": created.ID}))
	resp := rpcResponse(t, rec.Body.Bytes())
	if resp.Error == nil || resp.Error.Code != a2a.CodeTaskNotCancelable {
		t.Fatalf("error = %+v, want TaskNotCancelableError", resp.Error)
	}

	// And nothing was written: the completed task still reads as completed,
	// with its artifact intact.
	after := do(t, p.server, http.MethodGet, "/a2a/v1/tasks/"+created.ID, withVersion("1.0"))
	var task a2a.Task
	if err := json.Unmarshal(after.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if task.Status.State != a2a.TaskStateCompleted {
		t.Errorf("state = %s after a refused cancel, want it untouched", task.Status.State)
	}
	if len(task.Artifacts) != 1 {
		t.Errorf("artifacts = %+v after a refused cancel, want them untouched", task.Artifacts)
	}

	t.Run("rest binding", func(t *testing.T) {
		rec := do(t, p.server, http.MethodPost, "/a2a/v1/tasks/"+created.ID+":cancel", withVersion("1.0"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (FAILED_PRECONDITION): %s", rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "FAILED_PRECONDITION") {
			t.Errorf("body = %s, want the google.rpc.Status shape", rec.Body)
		}
	})
}

// TestCancelRecoversTheSlot is the reason CancelTask had to land alongside the
// detached-lifetime change: a turn nobody is watching would otherwise hold this
// listener's single active-task slot for ever.
func TestCancelRecoversTheSlot(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	newHITLAgent(t, bus, "never ")

	first := jsonrpcSend(t, p, a2a.MethodSendMessage, sendMessageParams("deploy", "ctx-slot"))
	var parked a2a.SendMessageResponse
	if err := first.DecodeResult(&parked); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The slot is held while the task is parked.
	busy := jsonrpcSend(t, p, a2a.MethodSendMessage, sendMessageParams("something else", "ctx-slot"))
	if busy.Error == nil || busy.Error.Code != a2a.CodeUnsupportedOperation {
		t.Fatalf("a second task was accepted while one was parked: %+v", busy.Error)
	}

	canceled := jsonrpcSend(t, p, a2a.MethodCancelTask, map[string]any{"id": parked.Task.ID})
	if canceled.Error != nil {
		t.Fatalf("CancelTask refused: %+v", canceled.Error)
	}
	if p.currentRun() != nil {
		t.Fatal("the active-task slot was not released by the cancellation")
	}

	// A fresh turn is accepted again.
	next := jsonrpcSend(t, p, a2a.MethodSendMessage, sendMessageParams("try again", "ctx-slot"))
	if next.Error != nil {
		t.Fatalf("the next turn was refused after a cancellation: %+v", next.Error)
	}
}

// ---- detached lifetime ----

// TestClientDisconnectDoesNotFailTheTask is the root fix stated on its own: the
// task belongs to the agent, not to the socket that asked for it.
func TestClientDisconnectDoesNotFailTheTask(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	release := make(chan struct{})
	playAgent(t, bus, func(b engine.EventBus, in events.UserInput) {
		_ = b.Emit("agent.turn.start", events.TurnInfo{
			SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1",
		})
		<-release
		_ = b.Emit("io.output", events.AgentOutput{
			SchemaVersion: events.AgentOutputVersion,
			Content:       "answered anyway", Role: "assistant", TurnID: "turn-1",
		})
		_ = b.Emit("agent.turn.end", events.TurnInfo{
			SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1",
		})
	})

	srv := httptest.NewServer(p.server.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	body, err := postSSE(ctx, srv.URL+"/a2a", a2a.ContentTypeJSON,
		jsonrpcPayload(t, a2a.MethodSendStreamingMessage, sendMessageParams("go", "ctx-drop")))
	if err != nil {
		t.Fatalf("SendStreamingMessage: %v", err)
	}
	reader := a2a.NewSSEReader(body)
	opening, err := reader.Next()
	if err != nil {
		t.Fatalf("opening frame: %v", err)
	}
	taskID := opening.Task.ID
	awaitTaskState(ctx, t, srv.URL, taskID, a2a.TaskStateWorking)

	// Gone.
	_ = body.Close()

	close(release)
	done := awaitTaskState(ctx, t, srv.URL, taskID, a2a.TaskStateCompleted)
	if len(done.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v, want the answer the agent produced after the client left", done.Artifacts)
	}
	if text, _ := done.Artifacts[0].Parts[0].TextValue(); text != "answered anyway" {
		t.Errorf("artifact = %q", text)
	}
}

// TestReturnImmediatelyAnswersWithTheTask pins the configuration option that
// was refused for exactly as long as a run's lifetime was its request's.
func TestReturnImmediatelyAnswersWithTheTask(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	playAgent(t, bus, func(b engine.EventBus, in events.UserInput) {
		_ = b.Emit("agent.turn.start", events.TurnInfo{
			SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1",
		})
		<-release
		_ = b.Emit("io.output", events.AgentOutput{
			SchemaVersion: events.AgentOutputVersion,
			Content:       "eventually", Role: "assistant", TurnID: "turn-1",
		})
		_ = b.Emit("agent.turn.end", events.TurnInfo{
			SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1",
		})
	})

	params := sendMessageParams("go", "ctx-async")
	params["configuration"] = map[string]any{"returnImmediately": true}

	answered := make(chan a2a.Response, 1)
	go func() { answered <- jsonrpcSend(t, p, a2a.MethodSendMessage, params) }()

	select {
	case resp := <-answered:
		if resp.Error != nil {
			t.Fatalf("returnImmediately refused: %+v", resp.Error)
		}
		var result a2a.SendMessageResponse
		if err := resp.DecodeResult(&result); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if result.Task == nil || result.Task.ID == "" {
			t.Fatal("returnImmediately answered without a task id to poll")
		}
		if result.Task.Status.State.IsTerminal() {
			t.Errorf("state = %s, want a live task the client can follow", result.Task.Status.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("returnImmediately blocked; it is supposed to return before the turn ends")
	}

	// The turn carries on and the task still completes, which is the half that
	// used to be impossible.
	close(release)
	srv := httptest.NewServer(p.server.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	list, err := get(ctx, srv.URL+"/a2a/v1/tasks?pageSize=1")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	var page a2a.ListTasksResponse
	if err := json.Unmarshal(list, &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Tasks) != 1 {
		t.Fatalf("tasks = %d, want the one that was started", len(page.Tasks))
	}
	awaitTaskState(ctx, t, srv.URL, page.Tasks[0].ID, a2a.TaskStateCompleted)
}

// ---- zombies ----

// TestTasksLeftInFlightAreFailedOnOpen pins the store's open-time repair. A task
// the process was serving when it stopped can never move again: no run drives
// it, no bus event will name it, and retention cannot evict it because only
// terminal tasks are evictable. Left alone it is an immortal row that reads as
// WORKING for ever.
func TestTasksLeftInFlightAreFailedOnOpen(t *testing.T) {
	dir := t.TempDir()
	owner := nexusauth.Principal{ID: "partner-a"}

	opener, closeFirst := storageAt(t, dir)
	st, err := opener(storage.ScopeSession)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	store, err := openTaskStore(st, retention{}, discardLogger())
	if err != nil {
		t.Fatalf("openTaskStore: %v", err)
	}
	view := store.For(owner)
	for _, c := range []struct {
		id    string
		state a2a.TaskState
	}{
		{"task-working", a2a.TaskStateWorking},
		{"task-parked", a2a.TaskStateInputRequired},
		{"task-done", a2a.TaskStateCompleted},
	} {
		if err := view.Create(c.id, "ctx-1", a2a.NewTaskStatus(a2a.TaskStateSubmitted), messageRef{
			MessageID: "m-" + c.id, Role: a2a.RoleUser, Text: "ask",
		}); err != nil {
			t.Fatalf("create %s: %v", c.id, err)
		}
		if err := view.RecordStatus(c.id, a2a.NewTaskStatus(c.state)); err != nil {
			t.Fatalf("status %s: %v", c.id, err)
		}
	}
	// The process goes away mid-turn.
	closeFirst()

	reopened, _ := storageAt(t, dir)
	st2, err := reopened(storage.ScopeSession)
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	store2, err := openTaskStore(st2, retention{}, discardLogger())
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	view2 := store2.For(owner)

	for id, want := range map[string]a2a.TaskState{
		"task-working": a2a.TaskStateFailed,
		"task-parked":  a2a.TaskStateFailed,
		"task-done":    a2a.TaskStateCompleted,
	} {
		rec, found, err := view2.Get(id)
		if err != nil || !found {
			t.Fatalf("read %s: found=%v err=%v", id, found, err)
		}
		if rec.Status.State != want {
			t.Errorf("%s = %s after a restart, want %s", id, rec.Status.State, want)
		}
		if want == a2a.TaskStateFailed {
			if rec.Status.Message == nil {
				t.Fatalf("%s was failed with no explanation", id)
			}
			if text, _ := rec.Status.Message.Parts[0].TextValue(); !strings.Contains(text, "agent stopped") {
				t.Errorf("%s explanation = %q, want it to say the agent went away", id, text)
			}
			// The trail records it as a transition, not a silent rewrite.
			if n := len(rec.StatusHistory); n < 3 {
				t.Errorf("%s history = %d entries, want the repair appended to it", id, n)
			}
		}
	}
}

// ---- concurrency ----

// TestInterruptAndCancelUnderLoad drives the new lifetime shape hard: many
// clients attach to one parked task while the bus is busy, one of them cancels
// it, and the rest must still end cleanly.
//
// It exists to be run under -race. Detaching a run's lifetime from its request
// means the run is now reachable from three directions at once — bus handlers,
// the timer goroutine and several HTTP goroutines — and this is what catches a
// frame reaching a writer from any of them.
func TestInterruptAndCancelUnderLoad(t *testing.T) {
	const subscribers = 8

	p, bus := newTestPlugin(t, nil)
	agent := newHITLAgent(t, bus, "answer ")

	srv := httptest.NewServer(p.server.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	primary := make(chan []string, 1)
	go func() {
		body, err := postSSE(ctx, srv.URL+"/a2a", a2a.ContentTypeJSON,
			jsonrpcPayload(t, a2a.MethodSendStreamingMessage, sendMessageParams("deploy", "ctx-load")))
		if err != nil {
			primary <- []string{"error: " + err.Error()}
			return
		}
		primary <- summarize(readFramesFrom(body))
	}()

	<-agent.asked
	taskID := awaitFirstTaskState(ctx, t, srv.URL, a2a.TaskStateInputRequired)

	var (
		wg      sync.WaitGroup
		attach  sync.WaitGroup
		mu      sync.Mutex
		endings []a2a.TaskState
	)
	attach.Add(subscribers)
	for i := range subscribers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, err := postSSE(ctx, srv.URL+"/a2a/v1/tasks/"+taskID+":subscribe", a2a.ContentTypeJSON, nil)
			if err != nil {
				attach.Done()
				return
			}
			defer body.Close()
			reader := a2a.NewSSEReader(body)
			if _, err := reader.Next(); err != nil {
				attach.Done()
				return
			}
			attach.Done()
			seen := drain(reader)
			mu.Lock()
			if n := len(seen); n > 0 && seen[n-1].Kind() == a2a.StreamPayloadStatusUpdate {
				endings = append(endings, seen[n-1].StatusUpdate.Status.State)
			}
			mu.Unlock()
		}(i)
	}
	attach.Wait()

	// Chatter on the bus from other goroutines while the cancel lands.
	var noise sync.WaitGroup
	for range 4 {
		noise.Add(1)
		go func() {
			defer noise.Done()
			for range 25 {
				_ = bus.Emit("agent.turn.start", events.TurnInfo{
					SchemaVersion: events.TurnInfoVersion, TurnID: "other",
				})
				_ = bus.Emit("llm.response", events.LLMResponse{
					SchemaVersion: events.LLMResponseVersion, Content: "partial",
				})
			}
		}()
	}

	if _, err := postJSON(ctx, srv.URL+"/a2a",
		jsonrpcPayload(t, a2a.MethodCancelTask, map[string]any{"id": taskID})); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	noise.Wait()
	wg.Wait()

	if len(endings) != subscribers {
		t.Fatalf("%d of %d subscribers ended on a status frame", len(endings), subscribers)
	}
	for i, state := range endings {
		if state != a2a.TaskStateCanceled {
			t.Errorf("subscriber %d ended at %s, want CANCELED", i, state)
		}
	}
	select {
	case got := <-primary:
		if n := len(got); n == 0 || got[n-1] != "status:"+string(a2a.TaskStateCanceled) {
			t.Errorf("the originating stream ended with %v, want a CANCELED frame", got)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the originating stream never ended")
	}
	if p.currentRun() != nil {
		t.Error("the active-task slot survived the cancellation")
	}
}

// postJSON issues a POST and reads the whole body, for the operations that do
// not stream.
func postJSON(ctx context.Context, url string, payload []byte) ([]byte, error) {
	body, err := postSSE(ctx, url, a2a.ContentTypeJSON, payload)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(body)
}

// awaitFirstTaskState polls ListTasks until its single task reports want, and
// returns that task's id.
func awaitFirstTaskState(ctx context.Context, t *testing.T, base string, want a2a.TaskState) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		body, err := get(ctx, base+"/a2a/v1/tasks?pageSize=1")
		if err != nil {
			t.Fatalf("ListTasks: %v", err)
		}
		var page a2a.ListTasksResponse
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatalf("decode list: %v (%s)", err, body)
		}
		if len(page.Tasks) == 1 {
			task := awaitTaskState(ctx, t, base, page.Tasks[0].ID, want)
			return task.ID
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no task reached %s", want)
	return ""
}

// TestStaleTurnEndCannotCompleteTheNextTask pins the window that opened when
// task lifetime detached from the request: a cancellation releases the slot and
// THEN asks the agent loop to stop, so the agent.turn.end that follows arrives
// after the next task may already have started. It must not complete it.
func TestStaleTurnEndCannotCompleteTheNextTask(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	newHITLAgent(t, bus, "never ")

	first := jsonrpcSend(t, p, a2a.MethodSendMessage, sendMessageParams("deploy", "ctx-stale"))
	var parked a2a.SendMessageResponse
	if err := first.DecodeResult(&parked); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if canceled := jsonrpcSend(t, p, a2a.MethodCancelTask,
		map[string]any{"id": parked.Task.ID}); canceled.Error != nil {
		t.Fatalf("CancelTask refused: %+v", canceled.Error)
	}

	// A new task, started before the cancelled turn's teardown is heard from.
	next := p.startTurnForTest(t, "ctx-stale")

	// The dead turn finally ends.
	_ = bus.Emit("agent.turn.end", events.TurnInfo{
		SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1",
	})

	if next.terminated() {
		t.Fatal("a turn end from the cancelled turn completed the task that followed it")
	}
	next.fail("test teardown")
}

// startTurnForTest registers a run the way an inbound message would, without an
// agent behind it, so a test can observe what happens to a task nothing is
// driving.
func (p *Plugin) startTurnForTest(t *testing.T, contextID string) *run {
	t.Helper()
	r, sub, _, protoErr := p.startTurn(turnInput{
		contextID: contextID, text: "another task", messageID: "m-next",
	}, nexusauth.Principal{}, streamOptions{})
	if protoErr != nil {
		t.Fatalf("startTurn: %+v", protoErr)
	}
	r.detach(sub)
	return r
}

// TestStreamingResumeOpensOnTheParkedState pins the opening frame of a resuming
// stream. It must be the state the task was actually in when the stream
// attached — INPUT_REQUIRED — so the WORKING transition the answer causes
// arrives as an update the client can follow, rather than being folded into the
// snapshot and losing the fact that the answer is what moved it.
func TestStreamingResumeOpensOnTheParkedState(t *testing.T) {
	p, bus := newTestPlugin(t, nil)
	newHITLAgent(t, bus, "answer ")

	first := jsonrpcSend(t, p, a2a.MethodSendMessage, sendMessageParams("deploy", "ctx-resume-stream"))
	var parked a2a.SendMessageResponse
	if err := first.DecodeResult(&parked); err != nil {
		t.Fatalf("decode: %v", err)
	}

	rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
		jsonrpcBody(t, a2a.MethodSendStreamingMessage,
			resumeParams("staging", parked.Task.ID, "ctx-resume-stream")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	fs := frames(t, rec.Body.Bytes())
	want := []a2a.TaskState{
		a2a.TaskStateInputRequired, a2a.TaskStateWorking, a2a.TaskStateCompleted,
	}
	if got := states(fs); !equalStates(got, want) {
		t.Fatalf("resumed stream states = %v, want %v", got, want)
	}
	if fs[0].Kind() != a2a.StreamPayloadTask || fs[0].Task.ID != parked.Task.ID {
		t.Errorf("opening frame = %+v, want a snapshot of the resumed task", fs[0])
	}
}
