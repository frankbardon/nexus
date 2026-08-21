package a2aclient_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/a2a/a2aclient"
)

func TestResumeRequestCarriesIdentity(t *testing.T) {
	req := a2aclient.ResumeText("task-2", "ctx-2", "m2", "production")
	if req.Message.TaskID != "task-2" || req.Message.ContextID != "ctx-2" {
		t.Fatalf("identity = %s/%s", req.Message.TaskID, req.Message.ContextID)
	}
	if req.Message.Role != a2a.RoleUser {
		t.Fatalf("role = %s, want ROLE_USER", req.Message.Role)
	}
	text, _ := req.Message.Parts[0].TextValue()
	if text != "production" {
		t.Fatalf("text = %q", text)
	}
	if err := a2a.ValidateSendMessageRequest(&req); err != nil {
		t.Fatalf("resume request is not valid: %v", err)
	}
}

func TestStreamResultResumeRequest(t *testing.T) {
	interrupted := a2aclient.StreamResult{
		TaskID:    "task-2",
		ContextID: "ctx-2",
		State:     a2a.TaskStateInputRequired,
	}
	req, ok := interrupted.ResumeRequest("m2", a2a.TextPart("yes"))
	if !ok {
		t.Fatal("an interrupted result must be resumable")
	}
	if req.Message.TaskID != "task-2" || req.Message.ContextID != "ctx-2" {
		t.Fatalf("identity = %s/%s", req.Message.TaskID, req.Message.ContextID)
	}

	// A finished task is not resumable, and neither is a stream that never
	// reached a task.
	done := a2aclient.StreamResult{TaskID: "task-2", State: a2a.TaskStateCompleted}
	if _, ok := done.ResumeRequest("m2", a2a.TextPart("yes")); ok {
		t.Fatal("a completed result must not be resumable")
	}
	taskless := a2aclient.StreamResult{State: a2a.TaskStateInputRequired}
	if _, ok := taskless.ResumeRequest("m2", a2a.TextPart("yes")); ok {
		t.Fatal("a result with no task id must not be resumable")
	}
}

func TestResumeRequestForTask(t *testing.T) {
	task := a2a.NewTask("task-3", "ctx-3")
	task.Status = a2a.NewTaskStatus(a2a.TaskStateAuthRequired)
	req, ok := a2aclient.ResumeRequestForTask(&task, "m2", a2a.TextPart("here is my token"))
	if !ok {
		t.Fatal("an AUTH_REQUIRED task must be resumable")
	}
	if req.Message.TaskID != "task-3" || req.Message.ContextID != "ctx-3" {
		t.Fatalf("identity = %s/%s", req.Message.TaskID, req.Message.ContextID)
	}

	if _, ok := a2aclient.ResumeRequestForTask(nil, "m2"); ok {
		t.Fatal("a nil task must not be resumable")
	}
	working := a2a.NewTask("task-4", "ctx-4")
	working.Status = a2a.NewTaskStatus(a2a.TaskStateWorking)
	if _, ok := a2aclient.ResumeRequestForTask(&working, "m2"); ok {
		t.Fatal("a working task must not be resumable")
	}
}

// TestInterruptThenResumeRoundTrip is the whole interruption cycle end to end:
// stream until the remote parks on INPUT_REQUIRED, read the question, answer it
// with a follow-up message carrying the same taskId and contextId, and watch the
// same task complete.
func TestInterruptThenResumeRoundTrip(t *testing.T) {
	var mu sync.Mutex
	var resumeMessage *a2a.Message

	agent := newAgent(t, agentConfig{
		streamFrames: func(req *a2a.SendMessageRequest) []a2a.StreamResponse {
			mu.Lock()
			defer mu.Unlock()
			if req.Message.TaskID == "" {
				return interruptedRun("task-2", "ctx-2", "which environment?")
			}
			// A resumption: the remote picks the task back up.
			msg := req.Message
			resumeMessage = &msg
			task := a2a.NewTask(req.Message.TaskID, req.Message.ContextID)
			task.Status = a2a.NewTaskStatus(a2a.TaskStateInputRequired)
			return []a2a.StreamResponse{
				a2a.StreamTask(task),
				a2a.StreamStatusUpdate(a2a.NewStatusUpdate(task.ID, task.ContextID,
					a2a.NewTaskStatus(a2a.TaskStateWorking))),
				a2a.StreamArtifactUpdate(a2a.NewArtifactUpdate(task.ID, task.ContextID,
					a2a.NewTextArtifact("art-1", "answer", "deployed to production"))),
				a2a.StreamStatusUpdate(a2a.NewStatusUpdate(task.ID, task.ContextID,
					a2a.NewTaskStatus(a2a.TaskStateCompleted))),
			}
		},
	})
	client, err := a2aclient.New(agent.URL())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first, err := client.Run(context.Background(), a2a.SendMessageRequest{
		Message: a2a.NewUserMessage("m1", "deploy"),
	})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	question, interrupted := first.Interrupt()
	if !interrupted || question == nil {
		t.Fatalf("expected an interruption, got state %s", first.State)
	}

	resume, ok := first.ResumeRequest("m2", a2a.TextPart("production"))
	if !ok {
		t.Fatal("interrupted result is not resumable")
	}
	second, err := client.Run(context.Background(), resume)
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if !second.Succeeded() {
		t.Fatalf("resumed state = %s, want COMPLETED", second.State)
	}
	if second.TaskID != first.TaskID {
		t.Fatalf("resumed task = %s, want the same task %s", second.TaskID, first.TaskID)
	}
	if got := second.ArtifactText(); got != "deployed to production" {
		t.Fatalf("artifact text = %q", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if resumeMessage == nil {
		t.Fatal("the remote never saw a resumption message")
	}
	if resumeMessage.TaskID != "task-2" || resumeMessage.ContextID != "ctx-2" {
		t.Fatalf("resume identity on the wire = %s/%s", resumeMessage.TaskID, resumeMessage.ContextID)
	}
}

func TestResumeOverNonStreamingSend(t *testing.T) {
	agent := newAgent(t, agentConfig{
		sendMessage: func(req *a2a.SendMessageRequest) (a2a.SendMessageResponse, *a2a.Error) {
			if req.Message.TaskID == "" {
				task := a2a.NewTask("task-8", "ctx-8")
				task.Status = a2a.NewTaskStatus(a2a.TaskStateInputRequired).
					WithMessage(a2a.NewAgentMessage("m-ask", "confirm?"))
				return a2a.TaskResponse(task), nil
			}
			task := a2a.NewTask(req.Message.TaskID, req.Message.ContextID)
			task.Status = a2a.NewTaskStatus(a2a.TaskStateCompleted)
			return a2a.TaskResponse(task), nil
		},
	})
	client, err := a2aclient.New(agent.URL())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first, err := client.SendMessage(context.Background(), a2a.SendMessageRequest{
		Message: a2a.NewUserMessage("m1", "do it"),
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	resume, ok := a2aclient.ResumeRequestForTask(first.Task, "m2", a2a.TextPart("yes"))
	if !ok {
		t.Fatalf("task %+v is not resumable", first.Task)
	}

	second, err := client.SendMessage(context.Background(), resume)
	if err != nil {
		t.Fatalf("resumed SendMessage: %v", err)
	}
	if second.Task == nil || second.Task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("resumed task = %+v", second.Task)
	}
}

func TestResumeRequestSerializesTaskIdentity(t *testing.T) {
	req := a2aclient.ResumeText("task-2", "ctx-2", "m2", "yes")
	encoded, err := a2a.Encode(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	msg, _ := decoded["message"].(map[string]any)
	if msg["taskId"] != "task-2" || msg["contextId"] != "ctx-2" {
		t.Fatalf("wire message = %v, want camelCase taskId/contextId", msg)
	}
}
