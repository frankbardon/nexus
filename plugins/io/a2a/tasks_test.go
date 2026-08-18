package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// ---- helpers ----

// twoPrincipalConfig guards the listener with two static tokens bound to two
// distinct principals, which is what makes a genuine cross-principal test
// possible at the HTTP layer rather than only at the store.
func twoPrincipalConfig(t *testing.T) map[string]any {
	t.Helper()
	return testConfig(t, map[string]any{
		"auth": map[string]any{
			"validators": []any{
				map[string]any{
					"type": "static",
					"tokens": []any{
						map[string]any{"token": "token-a", "principal": "partner-a"},
						map[string]any{"token": "token-b", "principal": "partner-b"},
					},
				},
			},
		},
	})
}

// seedTask writes a task straight into the store under a named principal,
// bypassing the turn machinery. It is how a test produces tasks belonging to a
// caller other than the one making the request.
func seedTask(t *testing.T, p *Plugin, owner, taskID, contextID string, state a2a.TaskState) {
	t.Helper()
	view := p.tasks.For(nexusauth.Principal{ID: owner})
	if err := view.Create(taskID, contextID, a2a.NewTaskStatus(a2a.TaskStateSubmitted), messageRef{
		MessageID: "m-" + taskID, Role: a2a.RoleUser, Text: "ask " + taskID,
	}); err != nil {
		t.Fatalf("seed %s: %v", taskID, err)
	}
	if state == a2a.TaskStateSubmitted {
		return
	}
	if err := view.RecordArtifact(taskID, a2a.NewTextArtifact(taskID+artifactSuffix, artifactName, "answer "+taskID)); err != nil {
		t.Fatalf("seed artifact %s: %v", taskID, err)
	}
	if err := view.RecordMessage(taskID, messageRef{
		MessageID: "r-" + taskID, Role: a2a.RoleAgent, Text: "answer " + taskID,
	}); err != nil {
		t.Fatalf("seed reply %s: %v", taskID, err)
	}
	if err := view.RecordStatus(taskID, a2a.NewTaskStatus(state)); err != nil {
		t.Fatalf("seed status %s: %v", taskID, err)
	}
}

// rpcTask decodes a Task from a JSON-RPC result.
func rpcTask(t *testing.T, body []byte) a2a.Task {
	t.Helper()
	resp := rpcResponse(t, body)
	if resp.Error != nil {
		t.Fatalf("error response: %+v", resp.Error)
	}
	var task a2a.Task
	if err := resp.DecodeResult(&task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	return task
}

// rpcList decodes a ListTasksResponse from a JSON-RPC result.
func rpcList(t *testing.T, body []byte) a2a.ListTasksResponse {
	t.Helper()
	resp := rpcResponse(t, body)
	if resp.Error != nil {
		t.Fatalf("error response: %+v", resp.Error)
	}
	var list a2a.ListTasksResponse
	if err := resp.DecodeResult(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	return list
}

// listedIDs renders a page as its task ids.
func listedIDs(tasks []a2a.Task) []string {
	out := make([]string, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, task.ID)
	}
	return out
}

// completedTurn boots a plugin, runs one turn through it and returns the task
// the turn produced. It is the fixture every read test that needs a REAL task
// (rather than a seeded one) starts from.
func completedTurn(t *testing.T, prompt, answer, contextID string) (*Plugin, a2a.Task) {
	t.Helper()
	p, bus := newTestPlugin(t, nil)
	playAgent(t, bus, scriptedTurn(answer))
	rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
		jsonrpcBody(t, a2a.MethodSendMessage, sendMessageParams(prompt, contextID)))
	if rec.Code != http.StatusOK {
		t.Fatalf("SendMessage status = %d: %s", rec.Code, rec.Body)
	}
	return p, taskFromRPC(t, rec.Body.Bytes())
}

// ---- GetTask ----

// TestGetTaskReturnsStatusArtifactsAndHistory is the acceptance criterion taken
// literally, on a task produced by a real turn rather than a hand-seeded row.
func TestGetTaskReturnsStatusArtifactsAndHistory(t *testing.T) {
	p, created := completedTurn(t, "what is the answer?", "the answer is 42", "ctx-1")

	t.Run("jsonrpc", func(t *testing.T) {
		rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
			jsonrpcBody(t, a2a.MethodGetTask, map[string]any{"id": created.ID}))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
		assertFullTask(t, rpcTask(t, rec.Body.Bytes()), created.ID)
	})

	t.Run("rest", func(t *testing.T) {
		rec := do(t, p.server, http.MethodGet, "/a2a/v1/tasks/"+created.ID, withVersion("1.0"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
		var task a2a.Task
		if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
			t.Fatalf("decode task: %v (%s)", err, rec.Body)
		}
		assertFullTask(t, task, created.ID)
	})
}

// assertFullTask checks the three things GetTask promises: state, artifacts and
// history.
func assertFullTask(t *testing.T, task a2a.Task, wantID string) {
	t.Helper()
	if task.ID != wantID {
		t.Errorf("id = %q, want %q", task.ID, wantID)
	}
	if task.ContextID != "ctx-1" {
		t.Errorf("contextId = %q, want ctx-1", task.ContextID)
	}
	if task.Status.State != a2a.TaskStateCompleted {
		t.Errorf("state = %q, want COMPLETED", task.Status.State)
	}
	if len(task.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v, want the turn's one artifact", task.Artifacts)
	}
	if text, ok := task.Artifacts[0].Parts[0].TextValue(); !ok || text != "the answer is 42" {
		t.Errorf("artifact text = %q", text)
	}
	if len(task.History) != 2 {
		t.Fatalf("history = %+v, want the inbound message and the reply", task.History)
	}
	if task.History[0].Role != a2a.RoleUser {
		t.Errorf("history[0].role = %q, want the user's message first", task.History[0].Role)
	}
	if text, _ := task.History[0].Parts[0].TextValue(); text != "what is the answer?" {
		t.Errorf("history[0] text = %q", text)
	}
	if task.History[1].Role != a2a.RoleAgent {
		t.Errorf("history[1].role = %q, want the agent's reply second", task.History[1].Role)
	}
	if task.History[1].TaskID != wantID || task.History[1].ContextID != "ctx-1" {
		t.Errorf("history messages are not stamped with their task and context: %+v", task.History[1])
	}
	// Every rendered message must be a legal A2A message, non-empty parts and
	// all — this is the invariant that made History empty before it was wired.
	for i := range task.History {
		if err := a2a.ValidateMessage(&task.History[i], "history"); err != nil {
			t.Errorf("history[%d] is not a valid message: %v", i, err)
		}
	}
}

// TestGetTaskHonoursHistoryLength pins the two ends of the presence
// distinction: zero omits history, and a positive cap keeps the MOST RECENT
// messages.
func TestGetTaskHonoursHistoryLength(t *testing.T) {
	p, created := completedTurn(t, "what is the answer?", "the answer is 42", "ctx-1")

	cases := map[string]struct {
		params    map[string]any
		query     string
		wantCount int
		wantRole  a2a.Role
	}{
		"unset keeps everything": {params: map[string]any{"id": created.ID}, query: "", wantCount: 2, wantRole: a2a.RoleUser},
		"zero omits history":     {params: map[string]any{"id": created.ID, "historyLength": 0}, query: "?historyLength=0", wantCount: 0},
		"one keeps the newest":   {params: map[string]any{"id": created.ID, "historyLength": 1}, query: "?historyLength=1", wantCount: 1, wantRole: a2a.RoleAgent},
	}
	for name, c := range cases {
		t.Run(name+" (jsonrpc)", func(t *testing.T) {
			rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
				jsonrpcBody(t, a2a.MethodGetTask, c.params))
			task := rpcTask(t, rec.Body.Bytes())
			if len(task.History) != c.wantCount {
				t.Fatalf("history = %d messages, want %d", len(task.History), c.wantCount)
			}
			if c.wantCount > 0 && task.History[0].Role != c.wantRole {
				t.Errorf("history[0].role = %q, want %q", task.History[0].Role, c.wantRole)
			}
		})
		t.Run(name+" (rest)", func(t *testing.T) {
			rec := do(t, p.server, http.MethodGet, "/a2a/v1/tasks/"+created.ID+c.query, withVersion("1.0"))
			var task a2a.Task
			if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
				t.Fatalf("decode: %v (%s)", err, rec.Body)
			}
			if len(task.History) != c.wantCount {
				t.Fatalf("history = %d messages, want %d", len(task.History), c.wantCount)
			}
		})
	}
}

// TestForeignTaskIsIndistinguishableFromUnknown is the security criterion, and
// it is asserted the only way that means anything: the two responses are
// compared BYTE FOR BYTE after substituting the task id, so a difference in
// error type, message, detail or status could not slip through.
func TestForeignTaskIsIndistinguishableFromUnknown(t *testing.T) {
	p, _ := newTestPlugin(t, twoPrincipalConfig(t))
	seedTask(t, p, "partner-b", "task-owned-by-b", "ctx-b", a2a.TaskStateCompleted)

	const unknownID = "task-nobody-ever-minted"

	t.Run("jsonrpc GetTask", func(t *testing.T) {
		foreign := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"), withBearer("token-a"),
			jsonrpcBody(t, a2a.MethodGetTask, map[string]any{"id": "task-owned-by-b"}))
		unknown := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"), withBearer("token-a"),
			jsonrpcBody(t, a2a.MethodGetTask, map[string]any{"id": unknownID}))
		assertIndistinguishable(t, foreign, unknown, "task-owned-by-b", unknownID)

		var resp a2a.Response
		if err := json.Unmarshal(foreign.Body.Bytes(), &resp); err != nil {
			t.Fatalf("not a JSON-RPC envelope: %v", err)
		}
		if resp.Error == nil || resp.Error.Code != a2a.CodeTaskNotFound {
			t.Fatalf("error = %+v, want TaskNotFoundError", resp.Error)
		}
	})

	t.Run("rest GetTask", func(t *testing.T) {
		foreign := do(t, p.server, http.MethodGet, "/a2a/v1/tasks/task-owned-by-b", withVersion("1.0"), withBearer("token-a"))
		unknown := do(t, p.server, http.MethodGet, "/a2a/v1/tasks/"+unknownID, withVersion("1.0"), withBearer("token-a"))
		assertIndistinguishable(t, foreign, unknown, "task-owned-by-b", unknownID)
		if foreign.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", foreign.Code)
		}
	})

	t.Run("jsonrpc SubscribeToTask", func(t *testing.T) {
		foreign := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"), withBearer("token-a"),
			jsonrpcBody(t, a2a.MethodSubscribeToTask, map[string]any{"id": "task-owned-by-b"}))
		unknown := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"), withBearer("token-a"),
			jsonrpcBody(t, a2a.MethodSubscribeToTask, map[string]any{"id": unknownID}))
		assertIndistinguishable(t, foreign, unknown, "task-owned-by-b", unknownID)
		if ct := foreign.Header().Get("Content-Type"); strings.Contains(ct, a2a.ContentTypeSSE) {
			t.Errorf("the refusal opened a stream (%s); it must be answered before one", ct)
		}
	})

	t.Run("rest SubscribeToTask", func(t *testing.T) {
		foreign := do(t, p.server, http.MethodPost, "/a2a/v1/tasks/task-owned-by-b:subscribe", withVersion("1.0"), withBearer("token-a"))
		unknown := do(t, p.server, http.MethodPost, "/a2a/v1/tasks/"+unknownID+":subscribe", withVersion("1.0"), withBearer("token-a"))
		assertIndistinguishable(t, foreign, unknown, "task-owned-by-b", unknownID)
	})

	t.Run("its owner still sees it", func(t *testing.T) {
		rec := do(t, p.server, http.MethodGet, "/a2a/v1/tasks/task-owned-by-b", withVersion("1.0"), withBearer("token-b"))
		if rec.Code != http.StatusOK {
			t.Fatalf("partner-b cannot read its own task: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("ListTasks never crosses the partition", func(t *testing.T) {
		seedTask(t, p, "partner-a", "task-owned-by-a", "ctx-a", a2a.TaskStateCompleted)
		rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"), withBearer("token-a"),
			jsonrpcBody(t, a2a.MethodListTasks, map[string]any{}))
		list := rpcList(t, rec.Body.Bytes())
		if got := listedIDs(list.Tasks); len(got) != 1 || got[0] != "task-owned-by-a" {
			t.Fatalf("partner-a listed %v, want only its own task", got)
		}
		if list.TotalSize != 1 {
			t.Errorf("totalSize = %d, want 1: the count must be scoped too", list.TotalSize)
		}
	})
}

// assertIndistinguishable compares two refusals for any difference other than
// the task id they name.
func assertIndistinguishable(t *testing.T, foreign, unknown *httptest.ResponseRecorder, foreignID, unknownID string) {
	t.Helper()
	if foreign.Code != unknown.Code {
		t.Fatalf("status differs: foreign %d, unknown %d", foreign.Code, unknown.Code)
	}
	normalized := strings.ReplaceAll(foreign.Body.String(), foreignID, unknownID)
	if normalized != unknown.Body.String() {
		t.Fatalf("the two refusals differ:\n foreign: %s\n unknown: %s", normalized, unknown.Body)
	}
}

// ---- ListTasks ----

// TestListTasksPaginates walks every page and checks the walk is complete,
// ordered and free of repeats — the three ways a cursor goes wrong.
func TestListTasksPaginates(t *testing.T) {
	p, _ := newTestPlugin(t, nil)
	var want []string
	for i := range 5 {
		id := fmt.Sprintf("task-%d", i)
		seedTask(t, p, anonymousPrincipal, id, "ctx-1", a2a.TaskStateCompleted)
		// Newest first, so the expectation is the reverse of creation order.
		want = append([]string{id}, want...)
	}

	var (
		got   []string
		token string
		pages int
	)
	for {
		params := map[string]any{"pageSize": 2}
		if token != "" {
			params["pageToken"] = token
		}
		rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
			jsonrpcBody(t, a2a.MethodListTasks, params))
		list := rpcList(t, rec.Body.Bytes())
		pages++
		if list.PageSize != 2 {
			t.Errorf("pageSize echoed as %d, want the applied 2", list.PageSize)
		}
		if list.TotalSize != 5 {
			t.Errorf("totalSize = %d, want 5 on every page", list.TotalSize)
		}
		if len(list.Tasks) > 2 {
			t.Fatalf("page carried %d tasks, over the requested size", len(list.Tasks))
		}
		got = append(got, listedIDs(list.Tasks)...)
		token = list.NextPageToken
		if token == "" {
			break
		}
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}

	if pages != 3 {
		t.Errorf("walked %d pages of 5 tasks at 2 per page, want 3", pages)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("paged through %v, want %v (newest first, each exactly once)", got, want)
	}
}

// TestListTasksFilters exercises each filtering criterion the specification
// defines for ListTasks.
func TestListTasksFilters(t *testing.T) {
	p, _ := newTestPlugin(t, nil)
	seedTask(t, p, anonymousPrincipal, "task-done", "ctx-1", a2a.TaskStateCompleted)
	seedTask(t, p, anonymousPrincipal, "task-failed", "ctx-1", a2a.TaskStateFailed)
	seedTask(t, p, anonymousPrincipal, "task-other-context", "ctx-2", a2a.TaskStateCompleted)
	seedTask(t, p, anonymousPrincipal, "task-live", "ctx-1", a2a.TaskStateSubmitted)

	list := func(params map[string]any) a2a.ListTasksResponse {
		t.Helper()
		rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
			jsonrpcBody(t, a2a.MethodListTasks, params))
		return rpcList(t, rec.Body.Bytes())
	}

	t.Run("by context", func(t *testing.T) {
		got := listedIDs(list(map[string]any{"contextId": "ctx-2"}).Tasks)
		if len(got) != 1 || got[0] != "task-other-context" {
			t.Errorf("contextId filter returned %v", got)
		}
	})

	t.Run("by status", func(t *testing.T) {
		got := listedIDs(list(map[string]any{"status": string(a2a.TaskStateFailed)}).Tasks)
		if len(got) != 1 || got[0] != "task-failed" {
			t.Errorf("status filter returned %v", got)
		}
	})

	t.Run("by status timestamp", func(t *testing.T) {
		all := list(map[string]any{})
		if len(all.Tasks) != 4 {
			t.Fatalf("expected 4 seeded tasks, got %d", len(all.Tasks))
		}
		// The newest task's own status timestamp: an inclusive lower bound at
		// that instant must return it and nothing older.
		cutoff := all.Tasks[0].Status.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z")
		got := listedIDs(list(map[string]any{"statusTimestampAfter": cutoff}).Tasks)
		if len(got) == 0 || got[0] != all.Tasks[0].ID {
			t.Fatalf("statusTimestampAfter returned %v, want the newest task", got)
		}
		if len(got) == 4 {
			t.Errorf("statusTimestampAfter returned every task; the bound was not applied")
		}
	})

	t.Run("artifacts are off by default and opt-in", func(t *testing.T) {
		off := list(map[string]any{"contextId": "ctx-2"})
		if len(off.Tasks[0].Artifacts) != 0 {
			t.Errorf("artifacts = %+v, want them omitted by default", off.Tasks[0].Artifacts)
		}
		on := list(map[string]any{"contextId": "ctx-2", "includeArtifacts": true})
		if len(on.Tasks[0].Artifacts) != 1 {
			t.Errorf("includeArtifacts=true returned %d artifacts, want 1", len(on.Tasks[0].Artifacts))
		}
	})

	t.Run("history length applies per task", func(t *testing.T) {
		capped := list(map[string]any{"contextId": "ctx-2", "historyLength": 0})
		if len(capped.Tasks[0].History) != 0 {
			t.Errorf("history = %+v, want it omitted", capped.Tasks[0].History)
		}
	})
}

// TestListTasksRESTBinding pins the query-parameter half of the same operation:
// section 11.5 maps the body fields onto camelCase query parameters, and both
// bindings must answer identically.
func TestListTasksRESTBinding(t *testing.T) {
	p, _ := newTestPlugin(t, nil)
	seedTask(t, p, anonymousPrincipal, "task-1", "ctx-1", a2a.TaskStateCompleted)
	seedTask(t, p, anonymousPrincipal, "task-2", "ctx-1", a2a.TaskStateCompleted)
	seedTask(t, p, anonymousPrincipal, "task-3", "ctx-9", a2a.TaskStateCompleted)

	rec := do(t, p.server, http.MethodGet, "/a2a/v1/tasks?contextId=ctx-1&pageSize=1", withVersion("1.0"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var page a2a.ListTasksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if len(page.Tasks) != 1 || page.Tasks[0].ID != "task-2" {
		t.Fatalf("first page = %v, want the newest ctx-1 task", listedIDs(page.Tasks))
	}
	if page.TotalSize != 2 || page.NextPageToken == "" {
		t.Fatalf("totalSize = %d, nextPageToken = %q", page.TotalSize, page.NextPageToken)
	}

	next := do(t, p.server, http.MethodGet,
		"/a2a/v1/tasks?contextId=ctx-1&pageSize=1&pageToken="+page.NextPageToken, withVersion("1.0"))
	var second a2a.ListTasksResponse
	if err := json.Unmarshal(next.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode: %v (%s)", err, next.Body)
	}
	if len(second.Tasks) != 1 || second.Tasks[0].ID != "task-1" {
		t.Fatalf("second page = %v, want the older ctx-1 task", listedIDs(second.Tasks))
	}
	if second.NextPageToken != "" {
		t.Errorf("nextPageToken = %q on the last page, want empty", second.NextPageToken)
	}
}

// TestListTasksRejectsAMalformedPageToken pins the refusal: silently restarting
// from the top would turn a client's pagination loop into an infinite one.
func TestListTasksRejectsAMalformedPageToken(t *testing.T) {
	p, _ := newTestPlugin(t, nil)
	rec := do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
		jsonrpcBody(t, a2a.MethodListTasks, map[string]any{"pageToken": "not-a-cursor!!"}))
	resp := rpcResponse(t, rec.Body.Bytes())
	if resp.Error == nil || resp.Error.Code != a2a.CodeInvalidParams {
		t.Fatalf("error = %+v, want InvalidParamsError", resp.Error)
	}
}

// TestPageTokenRoundTrips pins the encoding itself, including the empty token
// that spells "no further page".
func TestPageTokenRoundTrips(t *testing.T) {
	if got := encodePageToken(listCursor{}); got != "" {
		t.Errorf("an unset cursor encoded as %q, want the empty token", got)
	}
	want := listCursor{createdAt: 1723987654321, rowID: 42, set: true}
	got, protoErr := decodePageToken(encodePageToken(want))
	if protoErr != nil {
		t.Fatalf("decode: %v", protoErr)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

// ---- SubscribeToTask ----

// TestSubscribeToTerminalTaskClosesImmediately is the criterion stated as a
// deadline: the response must complete on its own, not because a test timeout
// tore the connection down.
func TestSubscribeToTerminalTaskClosesImmediately(t *testing.T) {
	p, _ := newTestPlugin(t, nil)
	seedTask(t, p, anonymousPrincipal, "task-done", "ctx-1", a2a.TaskStateCompleted)

	cases := map[string]func() *httptest.ResponseRecorder{
		"jsonrpc": func() *httptest.ResponseRecorder {
			return do(t, p.server, http.MethodPost, "/a2a", withVersion("1.0"),
				jsonrpcBody(t, a2a.MethodSubscribeToTask, map[string]any{"id": "task-done"}))
		},
		"rest": func() *httptest.ResponseRecorder {
			return do(t, p.server, http.MethodPost, "/a2a/v1/tasks/task-done:subscribe", withVersion("1.0"))
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- call() }()
			select {
			case rec := <-done:
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d: %s", rec.Code, rec.Body)
				}
				fs := frames(t, rec.Body.Bytes())
				if len(fs) != 1 {
					t.Fatalf("frames = %d, want exactly the terminal snapshot: %s", len(fs), rec.Body)
				}
				if fs[0].Kind() != a2a.StreamPayloadTask {
					t.Fatalf("opening frame is %s, want a task snapshot", fs[0].Kind())
				}
				if fs[0].Task.Status.State != a2a.TaskStateCompleted {
					t.Errorf("state = %q, want the stored terminal state", fs[0].Task.Status.State)
				}
				if len(fs[0].Task.Artifacts) != 1 {
					t.Errorf("the terminal snapshot carried no artifacts: %+v", fs[0].Task)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("SubscribeToTask hung on an already-terminal task")
			}
		})
	}
}

// TestConcurrentSubscribersReceiveIdenticalBroadcasts is the fan-out criterion,
// and the reason this story changed the run's concurrency shape.
//
// It runs against a REAL HTTP server so each subscriber is a separate connection
// read incrementally by its own goroutine, which is the only arrangement in
// which "identical broadcasts" means anything. The agent is held mid-turn until
// every subscriber has read its opening frame, so all of them attach at the same
// state and any difference in what follows is a genuine fan-out bug rather than
// a race in the fixture.
func TestConcurrentSubscribersReceiveIdenticalBroadcasts(t *testing.T) {
	const subscribers = 8

	p, bus := newTestPlugin(t, nil)
	release := make(chan struct{})
	playAgent(t, bus, func(b engine.EventBus, in events.UserInput) {
		_ = b.Emit("agent.turn.start", events.TurnInfo{
			SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1", SessionID: in.SessionID,
		})
		<-release
		_ = b.Emit("io.output", events.AgentOutput{
			SchemaVersion: events.AgentOutputVersion, Content: "the answer is 42",
			Role: "assistant", TurnID: "turn-1",
		})
		_ = b.Emit("agent.turn.end", events.TurnInfo{
			SchemaVersion: events.TurnInfoVersion, TurnID: "turn-1",
		})
	})

	srv := httptest.NewServer(p.server.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// The originating stream. It runs for the whole test; the turn is not
	// released until every subscriber has joined.
	primary := make(chan []string, 1)
	go func() {
		body, err := postSSE(ctx, srv.URL+"/a2a", a2a.ContentTypeJSON, jsonrpcPayload(t, a2a.MethodSendStreamingMessage,
			sendMessageParams("what is the answer?", "ctx-1")))
		if err != nil {
			primary <- []string{"error: " + err.Error()}
			return
		}
		primary <- summarize(readFramesFrom(body))
	}()

	// Wait until the task exists AND is WORKING, so every subscriber attaches at
	// the same state.
	taskID := awaitWorkingTask(ctx, t, srv.URL)

	var (
		wg      sync.WaitGroup
		attach  sync.WaitGroup
		mu      sync.Mutex
		streams [][]string
	)
	attach.Add(subscribers)
	for i := range subscribers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var (
				body io.ReadCloser
				err  error
			)
			// Both bindings, alternating, so the fan-out is exercised through
			// each framing rather than only the one that happens to be first.
			if i%2 == 0 {
				body, err = postSSE(ctx, srv.URL+"/a2a", a2a.ContentTypeJSON,
					jsonrpcPayload(t, a2a.MethodSubscribeToTask, map[string]any{"id": taskID}))
			} else {
				body, err = postSSE(ctx, srv.URL+"/a2a/v1/tasks/"+taskID+":subscribe", a2a.ContentTypeJSON, nil)
			}
			if err != nil {
				attach.Done()
				mu.Lock()
				streams = append(streams, []string{"error: " + err.Error()})
				mu.Unlock()
				return
			}
			defer body.Close()

			reader := a2a.NewSSEReader(body)
			opening, err := reader.Next()
			// The opening frame is proof this subscriber is attached.
			attach.Done()
			if err != nil {
				mu.Lock()
				streams = append(streams, []string{"open error: " + err.Error()})
				mu.Unlock()
				return
			}
			seen := summarize(append([]a2a.StreamResponse{*opening}, drain(reader)...))
			mu.Lock()
			streams = append(streams, seen)
			mu.Unlock()
		}(i)
	}

	attach.Wait()
	close(release)
	wg.Wait()

	if len(streams) != subscribers {
		t.Fatalf("collected %d streams, want %d", len(streams), subscribers)
	}
	want := []string{
		"task:" + string(a2a.TaskStateWorking),
		"artifact:" + taskID + artifactSuffix,
		"status:" + string(a2a.TaskStateCompleted),
	}
	for i, got := range streams {
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("subscriber %d saw %v, want %v", i, got, want)
		}
	}

	select {
	case got := <-primary:
		want := []string{
			"task:" + string(a2a.TaskStateSubmitted),
			"status:" + string(a2a.TaskStateWorking),
			"artifact:" + taskID + artifactSuffix,
			"status:" + string(a2a.TaskStateCompleted),
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("the originating stream saw %v, want %v", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the originating stream never completed")
	}
}

// awaitWorkingTask polls ListTasks for the in-flight task and then GetTask until
// it reports WORKING. It doubles as a read-path exercise against a LIVE task:
// both operations must answer while the turn is still running.
func awaitWorkingTask(ctx context.Context, t *testing.T, base string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		body, err := get(ctx, base+"/a2a/v1/tasks?pageSize=1")
		if err != nil {
			t.Fatalf("ListTasks: %v", err)
		}
		var list a2a.ListTasksResponse
		if err := json.Unmarshal(body, &list); err != nil {
			t.Fatalf("decode list: %v (%s)", err, body)
		}
		if len(list.Tasks) == 1 {
			taskBody, err := get(ctx, base+"/a2a/v1/tasks/"+list.Tasks[0].ID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			var task a2a.Task
			if err := json.Unmarshal(taskBody, &task); err != nil {
				t.Fatalf("decode task: %v (%s)", err, taskBody)
			}
			if task.Status.State == a2a.TaskStateWorking {
				return task.ID
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no task reached WORKING")
	return ""
}

// summarize renders a frame sequence as comparable strings.
func summarize(fs []a2a.StreamResponse) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		switch f.Kind() {
		case a2a.StreamPayloadTask:
			out = append(out, "task:"+string(f.Task.Status.State))
		case a2a.StreamPayloadStatusUpdate:
			out = append(out, "status:"+string(f.StatusUpdate.Status.State))
		case a2a.StreamPayloadArtifactUpdate:
			out = append(out, "artifact:"+f.ArtifactUpdate.Artifact.ArtifactID)
		}
	}
	return out
}

// drain reads the rest of a stream, stopping at EOF or the terminal frame.
func drain(reader *a2a.SSEReader) []a2a.StreamResponse {
	var out []a2a.StreamResponse
	for {
		frame, err := reader.Next()
		if err != nil {
			return out
		}
		out = append(out, *frame)
	}
}

// readFramesFrom drains a whole SSE body.
func readFramesFrom(body io.ReadCloser) []a2a.StreamResponse {
	defer body.Close()
	return drain(a2a.NewSSEReader(body))
}

// postSSE issues a POST and returns the still-open response body, so the caller
// reads records as they are flushed rather than after the response completes.
func postSSE(ctx context.Context, url, contentType string, payload []byte) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(a2a.HeaderVersion, a2a.ProtocolVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return resp.Body, nil
}

// get issues a GET and reads the whole body.
func get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(a2a.HeaderVersion, a2a.ProtocolVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return body, nil
}

// jsonrpcPayload encodes a JSON-RPC request body.
func jsonrpcPayload(t *testing.T, method string, params any) []byte {
	t.Helper()
	req, err := a2a.NewRequest(1, method, params)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	data, err := req.Encode()
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	return data
}
