package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// This file covers the three task-READ operations over the wire, on both
// bindings: that they answer from the durable record rather than from memory,
// that they are scoped to the caller, and that a task belonging to somebody else
// is byte-for-byte the same refusal as one that does not exist.

// ---- fixtures ----

// newA2AReadServer wires an A2A ingress with a durable store under dir and a
// scripted instance, mounted through the broker's real auth guard alongside
// every pre-existing route.
func newA2AReadServer(t *testing.T, cfg Config, dir string) (*httptest.Server, *A2AServer, *conformInstance) {
	t.Helper()
	cfg.StateDir = dir

	server, err := NewA2AServer(testLogger(), cfg)
	if err != nil {
		t.Fatalf("NewA2AServer: %v", err)
	}
	if dir != "" {
		server.useTaskStore(openTestTaskStore(t, dir, cfg.A2ATaskRetention))
	}
	instance := &conformInstance{}
	server.useLeaseProvider(&conformLeaseProvider{instance: instance})

	ts, _ := newBrokerTestServer(t, cfg, server.Register)
	return ts, server, instance
}

// answerEveryTurn makes the instance run a complete turn each time it is handed
// an input, from a goroutine — which is what an instance does: it answers on its
// own schedule while the HTTP goroutine is already streaming.
func answerEveryTurn(instance *conformInstance, answer string) {
	var turn int
	instance.onInput = func() {
		turn++
		id := fmt.Sprintf("t%d", turn)
		go func() {
			instance.deliver(brokerIOMessage{Type: ioTypeOutput, Content: answer, TurnID: id})
			instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: ioStateIdle})
		}()
	}
}

// sendMessageAs runs one blocking SendMessage and returns the task it produced.
func sendMessageAs(t *testing.T, ts *httptest.Server, token, contextID, text string) a2a.Task {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":`+
		`{"messageId":"m-%s","role":"ROLE_USER","contextId":%q,"parts":[{"text":%q}]}}}`,
		text, contextID, text)
	resp := doAuthed(t, http.MethodPost, ts.URL+agentJSONRPCPath("support"), token, body)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)

	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("SendMessage response is not a JSON-RPC envelope: %v (%s)", err, raw)
	}
	if envelope.Error != nil {
		t.Fatalf("SendMessage was refused: %s", envelope.Error)
	}
	var result a2a.SendMessageResponse
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatalf("result is not a SendMessageResponse: %v (%s)", err, envelope.Result)
	}
	if result.Task == nil {
		t.Fatalf("SendMessage answered with no task: %s", envelope.Result)
	}
	return *result.Task
}

// rpcResult runs one JSON-RPC call and returns its raw result, failing on an
// error envelope.
func rpcResult(t *testing.T, ts *httptest.Server, token, method string, params any) json.RawMessage {
	t.Helper()
	return rpcResultOn(t, ts, "support", token, method, params)
}

// readRESTSSEStates reads an SSE stream on the REST binding and renders each
// record as the state (or "artifact") it reports.
//
// It is separate from readSSEStates because the two bindings frame a stream
// differently: the JSON-RPC binding wraps every record in a response envelope,
// while the REST binding writes the StreamResponse itself. Parsing them with one
// reader would hide exactly that difference, which is the thing a binding test
// is for.
func readRESTSSEStates(t *testing.T, body io.Reader) []string {
	t.Helper()
	var states []string
	scanner := bufio.NewScanner(body)
	deadline := time.Now().Add(5 * time.Second)
	for scanner.Scan() {
		if time.Now().After(deadline) {
			t.Fatal("the SSE stream did not end within 5s")
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var record struct {
			Task *struct {
				Status struct {
					State string `json:"state"`
				} `json:"status"`
			} `json:"task"`
			StatusUpdate *struct {
				Status struct {
					State string `json:"state"`
				} `json:"status"`
			} `json:"statusUpdate"`
			ArtifactUpdate json.RawMessage `json:"artifactUpdate"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &record); err != nil {
			t.Fatalf("SSE record is not JSON: %v (%s)", err, line)
		}
		switch {
		case record.Task != nil:
			states = append(states, record.Task.Status.State)
		case record.StatusUpdate != nil:
			states = append(states, record.StatusUpdate.Status.State)
		case len(record.ArtifactUpdate) > 0:
			states = append(states, "artifact")
		}
	}
	return states
}

// rpcResultOn is rpcResult against a named profile's endpoint.
func rpcResultOn(t *testing.T, ts *httptest.Server, profile, token, method string, params any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("encode params: %v", err)
	}
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":%q,"params":%s}`, method, encoded)
	resp := doAuthed(t, http.MethodPost, ts.URL+agentJSONRPCPath(profile), token, body)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)

	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("%s response is not a JSON-RPC envelope: %v (%s)", method, err, raw)
	}
	if envelope.Error != nil {
		t.Fatalf("%s was refused: %s", method, envelope.Error)
	}
	return envelope.Result
}

// readBody reads and closes a response, returning its status and body.
func readBody(t *testing.T, resp *http.Response) (int, string) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(raw)
}

// ---- GetTask ----

// TestGetTaskAnswersAfterTheBrokerRestarts is the acceptance criterion at its
// sharpest. A turn runs, the store is closed, and a COMPLETELY NEW ingress is
// built over the same state_dir — the shape of a broker restart, with no live
// task, no lease and no instance anywhere. GetTask must still answer, with the
// turn's artifact.
func TestGetTaskAnswersAfterTheBrokerRestarts(t *testing.T) {
	dir := t.TempDir()
	cfg := a2aTestConfig(t, "")

	ts, server, instance := newA2AReadServer(t, cfg, dir)
	answerEveryTurn(instance, "the answer is 42")
	task := sendMessageAs(t, ts, "", "ctx-1", "what is the answer?")
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("the turn ended at %s, want COMPLETED", task.Status.State)
	}
	if err := server.store.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}

	// A different process, as far as anything here can tell.
	restarted, _, _ := newA2AReadServer(t, cfg, dir)
	result := rpcResult(t, restarted, "", a2a.MethodGetTask, map[string]any{"id": task.ID})

	var got a2a.Task
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("GetTask result is not a Task: %v (%s)", err, result)
	}
	if got.ID != task.ID {
		t.Errorf("task id = %q, want %q", got.ID, task.ID)
	}
	if got.Status.State != a2a.TaskStateCompleted {
		t.Errorf("state = %s, want COMPLETED", got.Status.State)
	}
	if len(got.Artifacts) != 1 {
		t.Fatalf("artifacts = %d, want the turn's answer", len(got.Artifacts))
	}
	if text, _ := got.Artifacts[0].Parts[0].TextValue(); text != "the answer is 42" {
		t.Errorf("artifact text = %q, want the answer the turn produced", text)
	}
	// And the message that started it, which is what makes a task read back
	// self-explanatory rather than an answer with no question.
	if len(got.History) != 1 {
		t.Fatalf("history = %d messages, want the inbound message", len(got.History))
	}
	if text, _ := got.History[0].Parts[0].TextValue(); text != "what is the answer?" {
		t.Errorf("history text = %q, want the inbound message", text)
	}
}

// TestGetTaskOnBothBindings: the REST binding carries its parameters in the path
// and query string, and must decode into the same request object the JSON-RPC
// body does.
func TestGetTaskOnBothBindings(t *testing.T) {
	ts, _, instance := newA2AReadServer(t, a2aTestConfig(t, ""), t.TempDir())
	answerEveryTurn(instance, "hello back")
	task := sendMessageAs(t, ts, "", "ctx-1", "hello")

	status, body := readBody(t, doAuthed(t, http.MethodGet,
		ts.URL+agentRESTPrefix("support")+"/tasks/"+task.ID, "", ""))
	if status != http.StatusOK {
		t.Fatalf("REST GetTask = %d, want 200: %s", status, body)
	}
	var got a2a.Task
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("REST GetTask body is not a Task: %v (%s)", err, body)
	}
	if got.ID != task.ID || got.Status.State != a2a.TaskStateCompleted {
		t.Errorf("REST GetTask returned %s at %s, want %s COMPLETED", got.ID, got.Status.State, task.ID)
	}

	// historyLength rides the query string on this binding.
	_, capped := readBody(t, doAuthed(t, http.MethodGet,
		ts.URL+agentRESTPrefix("support")+"/tasks/"+task.ID+"?historyLength=0", "", ""))
	var trimmed a2a.Task
	if err := json.Unmarshal([]byte(capped), &trimmed); err != nil {
		t.Fatalf("REST GetTask body is not a Task: %v (%s)", err, capped)
	}
	if len(trimmed.History) != 0 {
		t.Errorf("historyLength=0 returned %d messages, want none", len(trimmed.History))
	}
}

// ---- the security criterion ----

// TestBrokerForeignTaskIsIndistinguishableFromUnknown is the security criterion,
// asserted the only way that means anything: the two responses are compared BYTE
// FOR BYTE after substituting the task id, so a difference in error type,
// message, detail or status could not slip through.
//
// It matches nexus.io.a2a's test of the same name deliberately. Two Nexus
// deployments must not leak different amounts about a task id a caller was never
// told.
func TestBrokerForeignTaskIsIndistinguishableFromUnknown(t *testing.T) {
	cfg := a2aTestConfig(t, twoPrincipalAuthYAML)
	ts, _, instance := newA2AReadServer(t, cfg, t.TempDir())
	answerEveryTurn(instance, "an answer")

	// A task genuinely owned by the OTHER principal.
	foreignTask := sendMessageAs(t, ts, otherToken, "ctx-other", "other's question")
	const unknownID = "task-nobody-ever-minted"

	assertSameRefusal := func(t *testing.T, foreign, unknown *http.Response) {
		t.Helper()
		fStatus, fBody := readBody(t, foreign)
		uStatus, uBody := readBody(t, unknown)
		if fStatus != uStatus {
			t.Fatalf("status differs: foreign %d, unknown %d", fStatus, uStatus)
		}
		normalized := strings.ReplaceAll(fBody, foreignTask.ID, unknownID)
		if normalized != uBody {
			t.Fatalf("the two refusals differ:\n foreign: %s\n unknown: %s", normalized, uBody)
		}
	}

	getRPC := func(id string) *http.Response {
		return doAuthed(t, http.MethodPost, ts.URL+agentJSONRPCPath("support"), ownerToken,
			fmt.Sprintf(`{"jsonrpc":"2.0","id":5,"method":"GetTask","params":{"id":%q}}`, id))
	}
	subscribeRPC := func(id string) *http.Response {
		return doAuthed(t, http.MethodPost, ts.URL+agentJSONRPCPath("support"), ownerToken,
			fmt.Sprintf(`{"jsonrpc":"2.0","id":5,"method":"SubscribeToTask","params":{"id":%q}}`, id))
	}

	t.Run("jsonrpc GetTask", func(t *testing.T) {
		foreign := getRPC(foreignTask.ID)
		if ct := foreign.Header.Get("Content-Type"); strings.Contains(ct, a2a.ContentTypeSSE) {
			t.Errorf("the refusal opened a stream (%s)", ct)
		}
		assertSameRefusal(t, foreign, getRPC(unknownID))
	})

	t.Run("rest GetTask", func(t *testing.T) {
		foreign := doAuthed(t, http.MethodGet, ts.URL+agentRESTPrefix("support")+"/tasks/"+foreignTask.ID, ownerToken, "")
		unknown := doAuthed(t, http.MethodGet, ts.URL+agentRESTPrefix("support")+"/tasks/"+unknownID, ownerToken, "")
		if foreign.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", foreign.StatusCode)
		}
		assertSameRefusal(t, foreign, unknown)
	})

	t.Run("jsonrpc SubscribeToTask", func(t *testing.T) {
		foreign := subscribeRPC(foreignTask.ID)
		if ct := foreign.Header.Get("Content-Type"); strings.Contains(ct, a2a.ContentTypeSSE) {
			t.Errorf("the refusal opened a stream (%s); it must be answered before one", ct)
		}
		assertSameRefusal(t, foreign, subscribeRPC(unknownID))
	})

	t.Run("rest SubscribeToTask", func(t *testing.T) {
		foreign := doAuthed(t, http.MethodPost,
			ts.URL+agentRESTPrefix("support")+"/tasks/"+foreignTask.ID+":subscribe", ownerToken, "")
		unknown := doAuthed(t, http.MethodPost,
			ts.URL+agentRESTPrefix("support")+"/tasks/"+unknownID+":subscribe", ownerToken, "")
		assertSameRefusal(t, foreign, unknown)
	})

	t.Run("its owner still sees it", func(t *testing.T) {
		status, body := readBody(t, doAuthed(t, http.MethodGet,
			ts.URL+agentRESTPrefix("support")+"/tasks/"+foreignTask.ID, otherToken, ""))
		if status != http.StatusOK {
			t.Fatalf("the owner cannot read its own task: %d %s", status, body)
		}
	})

	t.Run("CancelTask is refused the same way", func(t *testing.T) {
		// The refusal for a foreign task must be TaskNotFound, not
		// TaskNotCancelable: the second would confirm the task exists and reveal
		// the state it is in.
		result := doAuthed(t, http.MethodPost, ts.URL+agentJSONRPCPath("support"), ownerToken,
			fmt.Sprintf(`{"jsonrpc":"2.0","id":6,"method":"CancelTask","params":{"id":%q}}`, foreignTask.ID))
		_, body := readBody(t, result)
		var resp a2a.Response
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("not a JSON-RPC envelope: %v (%s)", err, body)
		}
		if resp.Error == nil || resp.Error.Code != a2a.CodeTaskNotFound {
			t.Fatalf("error = %+v, want TaskNotFoundError", resp.Error)
		}
	})
}

// TestATaskIsScopedToItsProfile: a profile is a distinct public agent with its
// own card, its own config and its own conversations, so a task addressed to one
// is absent from another — reported exactly as an unknown id is, since a distinct
// answer would tell a caller that a task it may not read here exists somewhere.
//
// It is the same key a2aContextRecord uses for conversations, applied to the
// tasks those conversations produce.
func TestATaskIsScopedToItsProfile(t *testing.T) {
	ts, _, instance := newA2AReadServer(t, a2aTestConfig(t, ""), t.TempDir())
	answerEveryTurn(instance, "an answer")
	task := sendMessageAs(t, ts, "", "ctx-1", "a support question")

	// The same caller, the same task id, the OTHER profile's route.
	status, body := readBody(t, doAuthed(t, http.MethodGet,
		ts.URL+agentRESTPrefix("research")+"/tasks/"+task.ID, "", ""))
	if status != http.StatusNotFound {
		t.Fatalf("reading a support task through the research route = %d, want 404: %s", status, body)
	}
	unknownStatus, unknownBody := readBody(t, doAuthed(t, http.MethodGet,
		ts.URL+agentRESTPrefix("research")+"/tasks/task-nobody-ever-minted", "", ""))
	if unknownStatus != status {
		t.Fatalf("status differs: cross-profile %d, unknown %d", status, unknownStatus)
	}
	if strings.ReplaceAll(body, task.ID, "task-nobody-ever-minted") != unknownBody {
		t.Fatalf("the two refusals differ:\n cross-profile: %s\n unknown: %s", body, unknownBody)
	}

	// And the profile it belongs to still answers.
	ownStatus, _ := readBody(t, doAuthed(t, http.MethodGet,
		ts.URL+agentRESTPrefix("support")+"/tasks/"+task.ID, "", ""))
	if ownStatus != http.StatusOK {
		t.Errorf("the owning profile answered %d, want 200", ownStatus)
	}

	// A listing on the other profile is empty rather than showing the task.
	result := rpcResultOn(t, ts, "research", "", a2a.MethodListTasks, map[string]any{})
	var resp a2a.ListTasksResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("ListTasks result is not a ListTasksResponse: %v (%s)", err, result)
	}
	if len(resp.Tasks) != 0 || resp.TotalSize != 0 {
		t.Errorf("the research agent listed %d task(s) (total %d) belonging to the support agent",
			len(resp.Tasks), resp.TotalSize)
	}
}

// ---- ListTasks ----

// TestListTasksOverTheWire covers the operation's whole contract in one place:
// scoping, filtering, pagination, and the artifacts-off default.
func TestListTasksOverTheWire(t *testing.T) {
	cfg := a2aTestConfig(t, twoPrincipalAuthYAML)
	ts, _, instance := newA2AReadServer(t, cfg, t.TempDir())
	answerEveryTurn(instance, "an answer")

	// Two conversations for the owner, one for somebody else. They are run
	// sequentially, so each is a completed task by the time the next starts.
	first := sendMessageAs(t, ts, ownerToken, "ctx-1", "one")
	second := sendMessageAs(t, ts, ownerToken, "ctx-1", "two")
	third := sendMessageAs(t, ts, ownerToken, "ctx-2", "three")
	foreign := sendMessageAs(t, ts, otherToken, "ctx-3", "not yours")

	list := func(t *testing.T, token string, params map[string]any) a2a.ListTasksResponse {
		t.Helper()
		result := rpcResult(t, ts, token, a2a.MethodListTasks, params)
		var resp a2a.ListTasksResponse
		if err := json.Unmarshal(result, &resp); err != nil {
			t.Fatalf("ListTasks result is not a ListTasksResponse: %v (%s)", err, result)
		}
		return resp
	}
	ids := func(tasks []a2a.Task) []string {
		out := make([]string, 0, len(tasks))
		for _, task := range tasks {
			out = append(out, task.ID)
		}
		return out
	}

	t.Run("scoped to the caller", func(t *testing.T) {
		resp := list(t, ownerToken, map[string]any{})
		got := strings.Join(ids(resp.Tasks), ",")
		want := strings.Join([]string{third.ID, second.ID, first.ID}, ",")
		if got != want {
			t.Errorf("listed %s, want %s (newest first, and only this caller's)", got, want)
		}
		if resp.TotalSize != 3 {
			t.Errorf("totalSize = %d, want 3: the count must be scoped too", resp.TotalSize)
		}
		for _, task := range resp.Tasks {
			if task.ID == foreign.ID {
				t.Error("another principal's task appeared in the listing")
			}
		}
	})

	t.Run("artifacts are off by default", func(t *testing.T) {
		resp := list(t, ownerToken, map[string]any{})
		for _, task := range resp.Tasks {
			if len(task.Artifacts) != 0 {
				t.Errorf("task %s carried artifacts without includeArtifacts", task.ID)
			}
		}
		with := list(t, ownerToken, map[string]any{"includeArtifacts": true})
		if len(with.Tasks[0].Artifacts) != 1 {
			t.Errorf("includeArtifacts=true returned %d artifacts, want 1", len(with.Tasks[0].Artifacts))
		}
	})

	t.Run("filtered by context", func(t *testing.T) {
		resp := list(t, ownerToken, map[string]any{"contextId": "ctx-2"})
		if got := ids(resp.Tasks); len(got) != 1 || got[0] != third.ID {
			t.Errorf("listed %v, want only the ctx-2 task", got)
		}
	})

	t.Run("filtered by state", func(t *testing.T) {
		resp := list(t, ownerToken, map[string]any{"status": string(a2a.TaskStateCanceled)})
		if len(resp.Tasks) != 0 {
			t.Errorf("listed %d tasks in CANCELED, want none", len(resp.Tasks))
		}
	})

	t.Run("paginates", func(t *testing.T) {
		var (
			seen  []string
			token string
			pages int
		)
		for {
			pages++
			if pages > 10 {
				t.Fatal("pagination did not terminate")
			}
			params := map[string]any{"pageSize": 2}
			if token != "" {
				params["pageToken"] = token
			}
			resp := list(t, ownerToken, params)
			seen = append(seen, ids(resp.Tasks)...)
			if resp.NextPageToken == "" {
				break
			}
			token = resp.NextPageToken
		}
		want := strings.Join([]string{third.ID, second.ID, first.ID}, ",")
		if strings.Join(seen, ",") != want {
			t.Errorf("the walk produced %v, want %s", seen, want)
		}
	})

	t.Run("a token this broker did not mint is refused", func(t *testing.T) {
		resp := doAuthed(t, http.MethodPost, ts.URL+agentJSONRPCPath("support"), ownerToken,
			`{"jsonrpc":"2.0","id":9,"method":"ListTasks","params":{"pageToken":"not-a-token"}}`)
		_, body := readBody(t, resp)
		var envelope a2a.Response
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			t.Fatalf("not a JSON-RPC envelope: %v (%s)", err, body)
		}
		if envelope.Error == nil {
			t.Fatal("a fabricated page token was accepted; a client's pagination loop would repeat for ever")
		}
	})
}

// ---- SubscribeToTask ----

// TestSubscribeToATerminalTaskReturnsItsStateAndCloses: a stream must always
// open with a Task frame (section 11.7), and a terminal state closes it by the
// same rule that closes a completed SendStreamingMessage. The client gets the
// outcome and an EOF rather than an open socket nothing will ever write to.
func TestSubscribeToATerminalTaskReturnsItsStateAndCloses(t *testing.T) {
	ts, _, instance := newA2AReadServer(t, a2aTestConfig(t, ""), t.TempDir())
	answerEveryTurn(instance, "the answer")
	task := sendMessageAs(t, ts, "", "ctx-1", "a question")

	resp := doAuthed(t, http.MethodPost,
		ts.URL+agentRESTPrefix("support")+"/tasks/"+task.ID+":subscribe", "", "")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("SubscribeToTask = %d, want 200: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, a2a.ContentTypeSSE) {
		t.Fatalf("Content-Type = %q, want an SSE stream", ct)
	}

	states := readRESTSSEStates(t, resp.Body)
	if strings.Join(states, ",") != "TASK_STATE_COMPLETED" {
		t.Errorf("the stream carried %v, want a single terminal snapshot", states)
	}
}

// TestSubscribeReattachesToALiveTask is the reattach half: a subscriber that
// joins mid-turn opens on the state it missed and then receives exactly the
// frames every other attached stream receives.
func TestSubscribeReattachesToALiveTask(t *testing.T) {
	cfg := a2aTestConfig(t, "")
	ts, server, instance := newA2AReadServer(t, cfg, t.TempDir())

	// Started directly rather than over HTTP so the turn stays live while the
	// subscriber attaches. With no auth block both callers are the anonymous
	// principal, which is what makes the read scoped to the same partition.
	card := server.card("support")
	task, sub, _, protoErr := server.startTask(context.Background(), card,
		a2aTurnInput{contextID: "ctx-live", text: "a long question", messageID: "m-1"},
		nexusauth.Principal{})
	if protoErr != nil {
		t.Fatalf("startTask: %s", protoErr.Message)
	}
	defer task.detach(sub)

	instance.deliver(brokerIOMessage{Type: ioTypeStreamDelta, Content: "still working", TurnID: "t1"})
	waitForTaskState(t, task, a2a.TaskStateWorking)

	resp := doAuthed(t, http.MethodPost,
		ts.URL+agentRESTPrefix("support")+"/tasks/"+task.taskID+":subscribe", "", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("SubscribeToTask = %d, want 200: %s", resp.StatusCode, raw)
	}

	// End the turn from the instance side; the reattached stream must carry the
	// artifact and the terminal status and then close.
	go func() {
		time.Sleep(20 * time.Millisecond)
		instance.deliver(brokerIOMessage{Type: ioTypeOutput, Content: "the late answer", TurnID: "t1"})
		instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: ioStateIdle})
	}()

	states := readRESTSSEStates(t, resp.Body)
	want := "TASK_STATE_WORKING,artifact,TASK_STATE_COMPLETED"
	if strings.Join(states, ",") != want {
		t.Errorf("the reattached stream carried %v, want %s (the state it joined at, then the ending)",
			states, want)
	}
}

// TestSubscribeToAQueuedTaskWaitsRatherThanClosing: a task queued behind a live
// turn is SUBMITTED and not terminal, so its stream must stay open — closing on a
// non-terminal state is indistinguishable client-side from a dropped connection.
func TestSubscribeToAQueuedTaskWaitsRatherThanClosing(t *testing.T) {
	cfg := a2aTestConfig(t, "")
	ts, server, instance := newA2AReadServer(t, cfg, t.TempDir())
	card := server.card("support")

	first, firstSub, _, protoErr := server.startTask(context.Background(), card,
		a2aTurnInput{contextID: "ctx-q", text: "first", messageID: "m-1"}, nexusauth.Principal{})
	if protoErr != nil {
		t.Fatalf("startTask: %s", protoErr.Message)
	}
	defer first.detach(firstSub)
	instance.deliver(brokerIOMessage{Type: ioTypeStreamDelta, Content: "working", TurnID: "t1"})
	waitForTaskState(t, first, a2a.TaskStateWorking)

	second, secondSub, _, protoErr := server.startTask(context.Background(), card,
		a2aTurnInput{contextID: "ctx-q", text: "second", messageID: "m-2"}, nexusauth.Principal{})
	if protoErr != nil {
		t.Fatalf("startTask: %s", protoErr.Message)
	}
	defer second.detach(secondSub)

	resp := doAuthed(t, http.MethodPost,
		ts.URL+agentRESTPrefix("support")+"/tasks/"+second.taskID+":subscribe", "", "")
	defer func() { _ = resp.Body.Close() }()

	// Free the conversation so the queued task runs and its stream can end.
	go func() {
		time.Sleep(20 * time.Millisecond)
		instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: ioStateIdle})
		time.Sleep(20 * time.Millisecond)
		instance.deliver(brokerIOMessage{Type: ioTypeOutput, Content: "the second answer", TurnID: "t2"})
		instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: ioStateIdle})
	}()

	states := readRESTSSEStates(t, resp.Body)
	if len(states) == 0 || states[0] != "TASK_STATE_SUBMITTED" {
		t.Fatalf("the stream opened with %v, want the queued SUBMITTED snapshot first", states)
	}
	if states[len(states)-1] != "TASK_STATE_COMPLETED" {
		t.Errorf("the stream carried %v, want it to stay open until the queued turn completed", states)
	}
}
