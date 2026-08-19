package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// This file drives one A2A turn across the broker: it decodes the inbound
// message, acquires an instance, sends the `input` payload the instance's
// nexus.io.broker plugin turns into io.input, and renders whatever comes back.
//
// THE SOLE-WRITER RULE. pumpStream and blockOnTask each run on the HTTP
// goroutine and are the only writers of their response. Instance payloads reach
// them exclusively through a2aStream's buffered channel, filled by the
// mapping's deliver path on the read-pump goroutine. No other goroutine touches
// an SSEWriter or a ResponseWriter, which is what makes the race detector clean
// rather than lucky.

// The ErrorInfo reasons the broker's turn mapping originates. They ride the
// google.rpc.ErrorInfo metadata so a client can branch on a stable token rather
// than on prose.
const (
	a2aReasonTaskTerminal    = "TASK_ALREADY_TERMINAL"
	a2aReasonTaskNotAwaiting = "TASK_NOT_AWAITING_INPUT"
	a2aReasonContextMismatch = "TASK_CONTEXT_MISMATCH"
)

// a2aTextMediaType is the media type this ingress speaks, matching the modes a
// stock profile card advertises.
const a2aTextMediaType = "text/plain"

// a2aParkedKeepalive is how often an SSE comment is written to a stream parked
// at INPUT_REQUIRED.
//
// A parked stream is deliberately silent: the task is waiting for a human and
// nothing has happened. Proxies read that silence as a dead connection and
// close it, so an SSE comment keeps the socket warm without emitting a protocol
// frame that says nothing happened. Twenty seconds sits under the common 30s
// and 60s idle timeouts with room to spare. It is a constant rather than a
// config key because it is a property of the transport, not a deployment
// choice.
const a2aParkedKeepalive = 20 * time.Second

// a2aBinding selects the wire framing a response is rendered in. The zero value
// is the REST binding; jsonrpc set means the JSON-RPC envelope, and id is the
// request id every envelope repeats.
type a2aBinding struct {
	jsonrpc bool
	id      json.RawMessage
}

// a2aTurnInput is a decoded SendMessage reduced to what the instance needs.
type a2aTurnInput struct {
	// contextID is the client's requested context, empty when it named none.
	contextID string
	// text is the concatenated text content of the inbound message.
	text string
	// messageID is the client's message id, carried for logging only.
	messageID string
	// taskID is the task this message continues, empty for a message that
	// starts a new one (specification section 3.4).
	taskID string
}

// ---- starting and continuing a turn ----

// startTask mints a task, leases an instance and sends it the client's message.
//
// The observer is attached BEFORE the input is sent, so the requesting client
// cannot miss a frame produced between the instance being handed the message
// and the handler getting around to subscribing.
//
// Every refusal happens before a single frame is emitted, so a client that is
// turned away gets an ordinary error response rather than a 200 stream whose
// only content is a failure.
func (s *A2AServer) startTask(ctx context.Context, card *servedAgentCard, in a2aTurnInput, caller nexusauth.Principal) (*a2aTask, *a2aStream, a2a.Task, *a2a.Error) {
	contextID := strings.TrimSpace(in.contextID)
	if contextID == "" {
		// A broker is multi-context by construction — one instance per task —
		// so a client that named no context is simply given one, rather than
		// being bound to a process-wide session the way a standalone serving
		// instance is.
		contextID = newA2AContextID()
	}

	task := newA2ATask(a2aTaskConfig{
		taskID:    newA2ATaskID(),
		contextID: contextID,
		profile:   card.profile,
		owner:     caller,
		logger:    s.logger,
	})
	// Wired after construction because the callback closes over the task it
	// belongs to. Both halves are one-shot from the task's side: onTerminal runs
	// inside terminate's sync.Once.
	task.onTerminal = func() {
		s.tasks.remove(task)
		if task.instance != nil {
			task.instance.Release()
		}
	}

	// Attached first: from here on every frame the task emits is delivered to
	// this observer, and the snapshot it opens on accounts for everything before.
	sub, opening := task.attach()

	instance, err := s.leases.Acquire(ctx, s.profiles[card.profile], card.profile, a2aInstanceHooks{
		Deliver: task.deliver,
		Gone:    func(reason string) { task.instanceGone(reason) },
	})
	if err != nil {
		task.detach(sub)
		return nil, nil, a2a.Task{}, errLeaseUnavailable(card.profile, err)
	}
	task.instance = instance
	s.tasks.add(task)

	if err := task.send(brokerIOMessage{Type: ioTypeInput, Content: in.text}); err != nil {
		// The message never reached the agent, so there is no turn to report on.
		// The task is removed and the lease released rather than left as a
		// SUBMITTED husk a client would have to cancel.
		s.tasks.remove(task)
		instance.Release()
		task.detach(sub)
		return nil, nil, a2a.Task{}, errSendFailed(card.profile, err)
	}

	s.logger.Debug("a2a task started",
		"profile", card.profile, "task_id", task.taskID,
		"context_id", contextID, "message_id", in.messageID)
	return task, sub, opening, nil
}

// resumeTask routes a message naming a task onto the question that task is
// parked on, and returns the SAME task with a fresh observer attached.
//
// This is A2A's own resume mechanism (specification section 3.4): an
// interrupted task is continued by sending a new message carrying the same
// taskId and contextId. It is emphatically NOT a new turn — no second `input`
// payload is sent, no second task is created, and the agent loop that asked the
// question simply stops blocking.
func (s *A2AServer) resumeTask(card *servedAgentCard, in a2aTurnInput, caller nexusauth.Principal) (*a2aTask, *a2aStream, a2a.Task, *a2a.Error) {
	task, found := s.tasks.get(caller, in.taskID)
	if !found {
		return nil, nil, a2a.Task{}, a2a.ErrTaskNotFound(in.taskID)
	}
	if task.terminated() {
		return nil, nil, a2a.Task{}, a2a.Errorf(a2a.ErrorTypeUnsupportedOperation,
			"task %q has ended and accepts no further messages; send a message with no taskId to start a new task",
			in.taskID).
			WithMetadata("taskId", in.taskID).
			WithMetadata("detail", a2aReasonTaskTerminal)
	}
	// Section 3.4 requires the SAME contextId alongside the taskId. A client
	// that omits it is taken to mean the task's own context; one that names a
	// different context is refused rather than quietly corrected, because the
	// two readings — "continue this task" and "start a conversation elsewhere" —
	// have nothing in common.
	if in.contextID != "" && in.contextID != task.contextID {
		return nil, nil, a2a.Task{}, a2a.Errorf(a2a.ErrorTypeUnsupportedOperation,
			"message names task %q but context %q; a task is continued by naming the context it belongs to, %q",
			in.taskID, in.contextID, task.contextID).
			WithMetadata("taskId", in.taskID).
			WithMetadata("contextId", task.contextID).
			WithMetadata("detail", a2aReasonContextMismatch)
	}
	parked, ok := task.pending()
	if !ok {
		return nil, nil, a2a.Task{}, a2a.Errorf(a2a.ErrorTypeUnsupportedOperation,
			"task %q is not waiting for input; a message may only continue a task that reported %s",
			in.taskID, a2a.TaskStateInputRequired.String()).
			WithMetadata("taskId", in.taskID).
			WithMetadata("detail", a2aReasonTaskNotAwaiting)
	}

	// Attached BEFORE anything moves, so the transitions this call causes reach
	// this request rather than being raced past it.
	sub, opening := task.attach()

	// The task returns to WORKING BEFORE the answer reaches the instance, and
	// the ordering is load-bearing rather than tidy: the moment the answer lands
	// the agent loop unblocks and may run to completion, so a WORKING transition
	// emitted afterwards would race a terminal one and be dropped. Moving first
	// makes the sequence the client sees — INPUT_REQUIRED, WORKING, then
	// whatever the turn does next — deterministic.
	if !task.resume(parked.requestID) {
		task.detach(sub)
		return nil, nil, a2a.Task{}, a2a.Errorf(a2a.ErrorTypeUnsupportedOperation,
			"task %q stopped waiting for input before this answer arrived", in.taskID).
			WithMetadata("taskId", in.taskID).
			WithMetadata("detail", a2aReasonTaskNotAwaiting)
	}

	answer := brokerIOMessage{Type: ioTypeHITLResponse, RequestID: parked.requestID}
	// A multiple-choice question is answered by echoing an option id. Anything
	// else is free text, which is what a free-text question expects and what a
	// choices-only question will be told is invalid by the plugin that asked.
	if id, matched := matchA2AChoice(parked.choices, in.text); matched {
		answer.ChoiceID = id
	} else {
		answer.FreeText = in.text
	}
	if err := task.send(answer); err != nil {
		// The answer did not reach the agent. The task is already back at
		// WORKING, so the honest move is to fail it rather than leave a client
		// waiting on a turn that will never be unblocked.
		task.fail(fmt.Sprintf("the answer could not be delivered to the agent instance: %v", err))
		task.detach(sub)
		return nil, nil, a2a.Task{}, errSendFailed(card.profile, err)
	}

	s.logger.Debug("a2a task resumed",
		"profile", card.profile, "task_id", task.taskID, "hitl_request_id", parked.requestID)
	return task, sub, opening, nil
}

// cancelTask settles one of the caller's tasks at TASK_STATE_CANCELED.
//
// The A2A task is settled SYNCHRONOUSLY and the instance told afterwards, in
// that order: the stream must not be able to carry a frame produced by the
// teardown after the terminal frame that closes it. Settling first makes every
// later payload a no-op, and only then is the cancellation forwarded.
//
// It does not wait for the instance's cancel.complete. Waiting would make the
// response time depend on an agent that has just been asked to stop, and would
// hang if it never answered; a cancel.complete that arrives later finds the
// task already terminal and is ignored. An instance-initiated cancellation —
// one this ingress did not ask for — is still rendered, which is what
// cancel.complete does when it arrives first.
//
// Cancelling an ALREADY-TERMINAL task is refused with TaskNotCancelableError
// (specification section 3.3.2): a terminal state is final, so "cancel" on one
// is a well-defined client mistake rather than an instruction to rewrite
// history.
func (s *A2AServer) cancelTask(caller nexusauth.Principal, taskID string) (a2a.Task, *a2a.Error) {
	task, found := s.tasks.get(caller, taskID)
	if !found {
		return a2a.Task{}, a2a.ErrTaskNotFound(taskID)
	}
	if task.terminated() {
		return a2a.Task{}, a2a.ErrTaskNotCancelable(taskID, task.snapshotTask().Status.State)
	}

	settled := task.cancel(a2aCancelReason)
	if !settled {
		// A cancel racing the turn's own ending loses. The task is answered as
		// it actually settled rather than as the client asked for.
		s.logger.Debug("a2a cancel raced the task's own ending",
			"profile", task.profile, "task_id", taskID)
		return task.snapshotTask(), nil
	}

	// Told after the fact, and a failure here is recorded rather than returned:
	// the client's task IS canceled — that is what the returned Task says — and
	// reporting an error would suggest otherwise.
	if err := task.send(brokerIOMessage{
		Type:   ioTypeCancel,
		TurnID: task.boundTurn(),
		Source: ioCancelSource,
	}); err != nil {
		s.logger.Warn("a2a task was canceled but the instance could not be told",
			"profile", task.profile, "task_id", taskID, "error", err)
	}
	s.logger.Info("a2a task canceled", "profile", task.profile, "task_id", taskID)
	return task.snapshotTask(), nil
}

// ---- rendering ----

// handleSendMessage drives one turn and renders it, blocking or streaming, in
// whichever binding the request arrived on.
func (s *A2AServer) handleSendMessage(w http.ResponseWriter, r *http.Request, card *servedAgentCard, b a2aBinding, req *a2a.SendMessageRequest, streaming bool) {
	in, protoErr := translateA2ASendMessage(req)
	if protoErr != nil {
		s.writeA2AError(w, card.profile, b, protoErr)
		return
	}

	caller := callerPrincipal(r)
	var (
		task    *a2aTask
		sub     *a2aStream
		opening a2a.Task
	)
	// A message naming a task CONTINUES that task rather than starting one, so
	// a resumed task never becomes a second turn — and never a second instance.
	if in.taskID != "" {
		task, sub, opening, protoErr = s.resumeTask(card, in, caller)
	} else {
		task, sub, opening, protoErr = s.startTask(r.Context(), card, in, caller)
	}
	if protoErr != nil {
		s.writeA2AError(w, card.profile, b, protoErr)
		return
	}
	// Detaching does NOT end the task: its lifetime is the turn's, not this
	// request's, so a client that disconnects mid-turn leaves the turn running.
	defer task.detach(sub)

	// configuration.returnImmediately (specification section 3.2.2): answer with
	// the task as it stands and let the client follow it by other means.
	// Streaming ignores it, because a stream IS the follow-up it asks for.
	if !streaming && req.Configuration != nil && req.Configuration.ReturnImmediately {
		s.writeA2AResult(w, card.profile, b, a2a.TaskResponse(opening))
		return
	}

	if streaming {
		s.pumpStream(r.Context(), w, card.profile, b, sub, opening)
		return
	}
	s.blockOnTask(r.Context(), w, card.profile, b, sub, task, opening)
}

// handleCancelTask settles a task and answers with it.
func (s *A2AServer) handleCancelTask(w http.ResponseWriter, r *http.Request, card *servedAgentCard, b a2aBinding, req *a2a.CancelTaskRequest) {
	task, protoErr := s.cancelTask(callerPrincipal(r), req.ID)
	if protoErr != nil {
		s.writeA2AError(w, card.profile, b, protoErr)
		return
	}
	s.writeA2AResult(w, card.profile, b, task)
}

// pumpStream renders one observer as a text/event-stream: the opening Task
// snapshot, then one frame per record until a frame reports a terminal state
// and closes the stream.
//
// THIS GOROUTINE IS THE SOLE WRITER of this response. Instance payloads never
// touch it; they only push onto sub.frames.
func (s *A2AServer) pumpStream(ctx context.Context, w http.ResponseWriter, profile string, b a2aBinding, sub *a2aStream, opening a2a.Task) {
	a2a.WriteSSEHeaders(w.Header())
	w.WriteHeader(http.StatusOK)

	var sse *a2a.SSEWriter
	if b.jsonrpc {
		sse = a2a.NewJSONRPCSSEWriter(w, b.id)
	} else {
		sse = a2a.NewSSEWriter(w)
	}

	if err := sse.WriteTask(opening); err != nil {
		s.logger.Debug("a2a sse open failed", "profile", profile, "task_id", opening.ID, "error", err)
		return
	}
	// A snapshot already terminal closes the stream on the opening frame.
	if sse.Closed() || sub == nil {
		return
	}

	// The keepalive only ever fires on a parked stream; a working task produces
	// frames of its own and a comment between them would be noise.
	keepalive := time.NewTicker(a2aParkedKeepalive)
	defer keepalive.Stop()

	for {
		select {
		case <-keepalive.C:
			if !sse.Interrupted() {
				continue
			}
			if err := sse.Ping(); err != nil {
				s.logger.Debug("a2a sse keepalive failed", "profile", profile, "task_id", opening.ID, "error", err)
				return
			}
		case frame := <-sub.frames:
			if err := sse.Write(frame); err != nil {
				// The transport is gone, or the turn produced a frame the stream
				// contract refuses. Either way nothing more can be delivered on
				// this connection — but the task is unaffected.
				s.logger.Debug("a2a sse write failed", "profile", profile, "task_id", opening.ID, "error", err)
				return
			}
			if sse.Closed() {
				return
			}
		case <-sub.dropped:
			// This observer fell behind and stopped being fed. Ending the
			// response is the only honest move: the alternative is a gapped
			// sequence a conforming client would reject anyway.
			return
		case <-ctx.Done():
			return
		}
	}
}

// blockOnTask renders the turn as a single Task reply once it reaches a state
// the client has to act on. Blocking is A2A's default for SendMessage
// (specification section 3.2.2): the call returns when the work is done, not
// when it was accepted.
//
// "Done" includes INTERRUPTED, not only terminal. A task at INPUT_REQUIRED is
// waiting for the caller, so continuing to block would be waiting for a client
// that is itself waiting on this response.
//
// It consumes exactly the frames the streaming path writes, folding each into
// the snapshot, so the two bindings cannot report different outcomes for one
// turn.
func (s *A2AServer) blockOnTask(ctx context.Context, w http.ResponseWriter, profile string, b a2aBinding, sub *a2aStream, task *a2aTask, opening a2a.Task) {
	snapshot := opening
	for {
		select {
		case frame := <-sub.frames:
			applyA2AFrame(&snapshot, frame)
			if state := snapshot.Status.State; state.IsTerminal() || state.IsInterrupted() {
				s.writeA2AResult(w, profile, b, a2a.TaskResponse(snapshot))
				return
			}
		case <-sub.dropped:
			// The terminal frame will never arrive on this channel. This request
			// cannot report the outcome, and must not invent one: the task as
			// last seen is returned, which is what a later read would answer.
			s.logger.Warn("a2a blocking request fell behind its own task",
				"profile", profile, "task_id", task.taskID)
			s.writeA2AResult(w, profile, b, a2a.TaskResponse(task.snapshotTask()))
			return
		case <-ctx.Done():
			// The caller is gone; there is nobody left to answer.
			return
		}
	}
}

// writeA2AResult renders a successful operation result in the request's
// binding.
func (s *A2AServer) writeA2AResult(w http.ResponseWriter, profile string, b a2aBinding, result any) {
	var (
		data []byte
		err  error
	)
	if b.jsonrpc {
		var resp *a2a.Response
		resp, err = a2a.NewResultResponse(b.id, result)
		if err == nil {
			data, err = resp.Encode()
		}
	} else {
		data, err = a2a.Encode(result)
	}
	if err != nil {
		s.writeA2AError(w, profile, b, a2a.Errorf(a2a.ErrorTypeInternal, "encoding the response: %v", err))
		return
	}

	w.Header().Set("Content-Type", a2a.ContentTypeJSON)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		s.logger.Debug("a2a result write failed", "profile", profile, "error", err)
	}
}

// writeA2AError renders a protocol error in the request's binding.
func (s *A2AServer) writeA2AError(w http.ResponseWriter, profile string, b a2aBinding, protoErr *a2a.Error) {
	if b.jsonrpc {
		s.writeJSONRPCError(w, profile, b.id, protoErr)
		return
	}
	s.writeRESTError(w, profile, protoErr)
}

// ---- inbound translation ----

// translateA2ASendMessage reduces a SendMessage request to what the instance IO
// envelope can carry.
func translateA2ASendMessage(req *a2a.SendMessageRequest) (a2aTurnInput, *a2a.Error) {
	if req == nil {
		return a2aTurnInput{}, a2a.ErrInvalidParams(a2a.FieldViolation{
			Field: "message", Description: "a message is required",
		})
	}
	msg := req.Message

	if msg.Role != a2a.RoleUser {
		return a2aTurnInput{}, a2a.ErrInvalidParams(a2a.FieldViolation{
			Field:       "message.role",
			Description: fmt.Sprintf("an inbound message must carry %s, got %q", a2a.RoleUser, string(msg.Role)),
		})
	}

	text, protoErr := a2aTextFromParts(msg.Parts)
	if protoErr != nil {
		return a2aTurnInput{}, protoErr
	}

	if cfg := req.Configuration; cfg != nil {
		if cfg.TaskPushNotificationConfig != nil {
			return a2aTurnInput{}, a2a.Errorf(a2a.ErrorTypePushNotificationNotSupported,
				"this broker does not deliver push notifications; the agent card declares capabilities.pushNotifications as false")
		}
		if protoErr := checkA2AOutputModes(cfg.AcceptedOutputModes); protoErr != nil {
			return a2aTurnInput{}, protoErr
		}
	}

	return a2aTurnInput{
		contextID: strings.TrimSpace(msg.ContextID),
		text:      text,
		messageID: msg.MessageID,
		taskID:    strings.TrimSpace(msg.TaskID),
	}, nil
}

// a2aTextFromParts renders the inbound message as the plain text the instance
// IO envelope carries. Several text parts are joined with a blank line, which
// is how the parts read as one prompt.
//
// A non-text part is REFUSED rather than dropped: the envelope's `input`
// payload is a single string, so there is nowhere for a file part to go, and
// silently ignoring one would answer a question the client did not ask.
func a2aTextFromParts(parts []a2a.Part) (string, *a2a.Error) {
	var chunks []string
	for i, part := range parts {
		text, ok := part.TextValue()
		if !ok {
			return "", a2a.Errorf(a2a.ErrorTypeContentTypeNotSupported,
				"message.parts[%d] is a %s part; this agent accepts text parts only", i, string(part.Kind()))
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		chunks = append(chunks, text)
	}
	if len(chunks) == 0 {
		return "", a2a.ErrInvalidParams(a2a.FieldViolation{
			Field:       "message.parts",
			Description: "at least one part must carry non-empty text",
		})
	}
	return strings.Join(chunks, "\n\n"), nil
}

// checkA2AOutputModes refuses a client that cannot accept anything this ingress
// produces. An empty list means "no client-imposed restriction".
//
// Unlike nexus.io.a2a there is no textOnly degradation to apply: the instance
// IO envelope carries no files and no structured output, so everything this
// mapping can publish is text already.
func checkA2AOutputModes(modes []string) *a2a.Error {
	if len(modes) == 0 {
		return nil
	}
	for _, mode := range modes {
		switch strings.ToLower(strings.TrimSpace(mode)) {
		case a2aTextMediaType, "text/*", "*/*", "text":
			return nil
		}
	}
	return a2a.Errorf(a2a.ErrorTypeContentTypeNotSupported,
		"this agent produces %s; the request accepts only %s", a2aTextMediaType, strings.Join(modes, ", "))
}
