package a2aclient

import (
	"github.com/frankbardon/nexus/pkg/a2a"

	"strings"
)

// Resuming an interrupted task.
//
// A2A has no "resume" operation. A task parked in INPUT_REQUIRED or
// AUTH_REQUIRED is resumed by sending an ORDINARY message that carries the same
// taskId and contextId (specification section 3.4) — the identity is what makes
// it a continuation rather than a new conversation. These helpers exist so that
// identity is carried by construction instead of by each caller remembering to
// stamp two fields.

// ResumeRequest builds a SendMessageRequest that continues an existing task.
//
// It is the same request shape as any other send; the taskId and contextId on
// the message are what make it a resumption. Use it with SendMessage for a
// blocking continuation or SendStreamingMessage for a streaming one — a resumed
// task streams on a fresh connection, since the stream the interruption arrived
// on belongs to the earlier call.
func ResumeRequest(taskID, contextID, messageID string, parts ...a2a.Part) a2a.SendMessageRequest {
	msg := a2a.NewMessage(messageID, a2a.RoleUser, parts...).
		ForTask(taskID).
		InContext(contextID)
	return a2a.SendMessageRequest{Message: msg}
}

// ResumeText builds a resume request whose payload is a single text part, which
// is what answering an INPUT_REQUIRED question usually amounts to.
func ResumeText(taskID, contextID, messageID, text string) a2a.SendMessageRequest {
	return ResumeRequest(taskID, contextID, messageID, a2a.TextPart(text))
}

// ResumeRequest builds the continuation for an interrupted stream result,
// carrying the task and context identity the stream established. The second
// return is false when the result is not resumable — it never reached a task,
// or it ended in a terminal state rather than an interruption — so a caller
// cannot accidentally address a continuation at a task that is finished.
func (r StreamResult) ResumeRequest(messageID string, parts ...a2a.Part) (a2a.SendMessageRequest, bool) {
	if strings.TrimSpace(r.TaskID) == "" || !r.State.IsInterrupted() {
		return a2a.SendMessageRequest{}, false
	}
	return ResumeRequest(r.TaskID, r.ContextID, messageID, parts...), true
}

// ResumeRequestForTask builds a continuation for a task read back with GetTask
// rather than observed on a stream. It is the path a client takes after a
// restart, when the stream that carried the interruption is long gone. The
// second return is false when the task is not parked on an interruption.
func ResumeRequestForTask(task *a2a.Task, messageID string, parts ...a2a.Part) (a2a.SendMessageRequest, bool) {
	if task == nil || strings.TrimSpace(task.ID) == "" || !task.Status.State.IsInterrupted() {
		return a2a.SendMessageRequest{}, false
	}
	return ResumeRequest(task.ID, task.ContextID, messageID, parts...), true
}
