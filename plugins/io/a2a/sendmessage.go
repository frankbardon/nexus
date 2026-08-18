package a2a

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// binding selects the wire framing a response is rendered in. The zero value is
// the REST binding; jsonrpc set means the JSON-RPC envelope, and id is the
// request id every envelope repeats.
//
// It exists so the two bindings share one handler: SendMessage differs between
// them only in how a result and an error are wrapped, and duplicating the turn
// logic to express that would be two implementations to keep in step.
type binding struct {
	jsonrpc bool
	id      json.RawMessage
}

// handleSendMessage drives one Nexus agent turn and renders it, blocking or
// streaming, in whichever binding the request arrived on.
//
// caller is the Principal the request authenticated as; the bridge files the
// created task under it, which is what makes every later read of that task
// scoped. It is a required parameter rather than something recovered from the
// request, so a new call site cannot reach the bus without deciding whose task
// it is creating.
//
// This goroutine is the SOLE reader of the run's frame channel and the SOLE
// writer to the response. Bus handlers only ever push onto that channel — see
// the concurrency note on run — so neither the SSEWriter nor the ResponseWriter
// is ever touched concurrently.
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request, caller nexusauth.Principal, b binding, req *a2a.SendMessageRequest, streaming bool) {
	in, protoErr := translateSendMessage(req)
	if protoErr != nil {
		s.writeError(w, b, protoErr)
		return
	}
	if s.cfg.bridge == nil {
		// Defensive: a Server constructed without a bridge cannot drive the bus.
		s.writeError(w, b, errNoBridge())
		return
	}

	// Refusals are answered BEFORE any stream is opened. A client that is turned
	// away gets an ordinary error response it can read from the status and the
	// body, rather than a 200 SSE stream whose only record is a failure.
	run, sub, protoErr := s.cfg.bridge.startTurn(in, caller)
	if protoErr != nil {
		s.writeError(w, b, protoErr)
		return
	}
	defer s.cfg.bridge.endTurn(run)
	// The subscription is attached by startTurn, before the turn can emit
	// anything; releasing it here is the other half. Detaching does not end the
	// task — a SubscribeToTask stream may still be watching it — it only says
	// this response is finished with it.
	defer run.detach(sub)

	// Client disconnect: fail the task so the drain loop stops promptly and the
	// active-task slot is released for the next caller. The stop channel keeps
	// this watcher from outliving the request.
	ctx := r.Context()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			run.fail("the client disconnected before the task finished")
		case <-stop:
		}
	}()

	if streaming {
		// A write failure on THIS connection fails the task: it is the request
		// that started the turn, and there is nowhere else for the answer to go.
		// A SubscribeToTask stream passes no such callback — a passive observer
		// losing its socket must not kill somebody else's task.
		s.pumpStream(ctx, w, b, sub, openingTask(run), run.fail)
		return
	}
	s.blockOnTask(ctx, w, b, sub, run)
}

// openingTask is the snapshot a freshly started turn opens on: SUBMITTED, no
// artifacts. A2A requires a task to exist before any update event may name it,
// and SUBMITTED is the state a task is created in (specification section 4.1).
func openingTask(r *run) a2a.Task { return a2a.NewTask(r.taskID, r.contextID) }

// pumpStream renders one attached observer as a text/event-stream: the opening
// Task snapshot, then one StreamResponse per record until a frame reports a
// terminal state and closes the stream.
//
// It is the SINGLE SSE writer path, shared by SendStreamingMessage and
// SubscribeToTask, and this goroutine is the sole writer of this response. Bus
// handlers never touch it; they only push onto sub.frames.
//
// A nil sub means there is nothing live to follow: the snapshot is written and
// the stream ends. onBroken, when supplied, is called if this connection cannot
// carry the stream — see the call sites for why only one of them supplies it.
func (s *Server) pumpStream(ctx context.Context, w http.ResponseWriter, b binding, sub *stream, opening a2a.Task, onBroken func(string)) {
	a2a.WriteSSEHeaders(w.Header())
	w.WriteHeader(http.StatusOK)

	var sse *a2a.SSEWriter
	if b.jsonrpc {
		sse = a2a.NewJSONRPCSSEWriter(w, b.id)
	} else {
		sse = a2a.NewSSEWriter(w)
	}

	if err := sse.WriteTask(opening); err != nil {
		s.cfg.logger.Debug("a2a sse open failed", "task_id", opening.ID, "error", err)
		if onBroken != nil {
			onBroken("the response stream could not be opened")
		}
		return
	}
	// A snapshot already in a terminal state closes the stream on the opening
	// frame: subscribing to a finished task answers and hangs up.
	if sse.Closed() || sub == nil {
		return
	}

	for {
		select {
		case frame := <-sub.frames:
			if err := sse.Write(frame); err != nil {
				// The transport is gone, or the agent produced a frame the stream
				// contract refuses. Either way nothing more can be delivered on
				// this connection.
				s.cfg.logger.Debug("a2a sse write failed", "task_id", opening.ID, "error", err)
				if onBroken != nil {
					onBroken("the response stream could not be written")
				}
				return
			}
			if sse.Closed() {
				return
			}
		case <-sub.dropped:
			// This observer fell behind and the run stopped feeding it. Ending
			// the response is the only honest move: the alternative is a gapped
			// frame sequence a conforming client would reject anyway.
			return
		case <-ctx.Done():
			return
		}
	}
}

// blockOnTask renders the run as a single Task reply once it reaches a terminal
// state. Blocking is A2A's default for SendMessage (specification section
// 3.2.2): the call returns when the work is done, not when it was accepted.
//
// It consumes exactly the frames the streaming path writes, folding each into
// the Task snapshot, so the two bindings cannot report different outcomes for
// the same turn.
func (s *Server) blockOnTask(ctx context.Context, w http.ResponseWriter, b binding, sub *stream, run *run) {
	task := openingTask(run)
	for {
		select {
		case frame := <-sub.frames:
			if applyFrame(&task, frame) {
				s.writeResult(w, b, a2a.TaskResponse(task))
				return
			}
		case <-sub.dropped:
			// The observer was dropped, so the terminal frame will never arrive
			// on this channel. The task is failed and the failure is rendered
			// here rather than awaited, since awaiting it would be waiting on a
			// channel that is no longer being written to.
			const reason = "the response could not be assembled: this request fell behind its own task"
			run.fail(reason)
			task.Status = a2a.NewTaskStatus(a2a.TaskStateFailed).WithMessage(
				a2a.NewAgentMessage(newMessageID(), reason).
					InContext(run.contextID).
					ForTask(run.taskID))
			s.writeResult(w, b, a2a.TaskResponse(task))
			return
		case <-ctx.Done():
			// The caller is gone; there is nobody left to answer.
			return
		}
	}
}

// writeResult renders a successful operation result in the request's binding.
func (s *Server) writeResult(w http.ResponseWriter, b binding, result any) {
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
		s.writeError(w, b, a2a.Errorf(a2a.ErrorTypeInternal, "encoding the response: %v", err))
		return
	}

	w.Header().Set("Content-Type", a2a.ContentTypeJSON)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		s.cfg.logger.Debug("a2a result write failed", "error", err)
	}
}

// writeError renders a protocol error in the request's binding: a JSON-RPC
// error object at HTTP 200, or the section 11.6 google.rpc.Status body with the
// error's mapped status.
func (s *Server) writeError(w http.ResponseWriter, b binding, protoErr *a2a.Error) {
	if b.jsonrpc {
		s.writeJSONRPCError(w, b.id, protoErr)
		return
	}
	s.writeRESTError(w, protoErr)
}
