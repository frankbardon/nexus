package a2a

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/frankbardon/nexus/pkg/a2a"
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
// This goroutine is the SOLE reader of the run's frame channel and the SOLE
// writer to the response. Bus handlers only ever push onto that channel — see
// the concurrency note on run — so neither the SSEWriter nor the ResponseWriter
// is ever touched concurrently.
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request, b binding, req *a2a.SendMessageRequest, streaming bool) {
	in, protoErr := translateSendMessage(req)
	if protoErr != nil {
		s.writeError(w, b, protoErr)
		return
	}
	if s.cfg.bridge == nil {
		// Defensive: a Server constructed without a bridge cannot drive the bus.
		s.writeError(w, b, a2a.Errorf(a2a.ErrorTypeInternal,
			"the a2a serve transport is not wired to the event bus"))
		return
	}

	// Refusals are answered BEFORE any stream is opened. A client that is turned
	// away gets an ordinary error response it can read from the status and the
	// body, rather than a 200 SSE stream whose only record is a failure.
	run, protoErr := s.cfg.bridge.startTurn(in)
	if protoErr != nil {
		s.writeError(w, b, protoErr)
		return
	}
	defer s.cfg.bridge.endTurn(run)

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
		s.streamTask(ctx, w, b, run)
		return
	}
	s.blockOnTask(ctx, w, b, run)
}

// streamTask renders the run as a text/event-stream, one StreamResponse per
// record, until a frame reports a terminal state and closes the stream.
//
// The opening Task frame is written here rather than queued, because A2A
// requires a stream to open with a Task or a Message (specification section
// 11.7) and writing it inline guarantees it precedes anything a bus handler has
// already produced.
func (s *Server) streamTask(ctx context.Context, w http.ResponseWriter, b binding, run *run) {
	a2a.WriteSSEHeaders(w.Header())
	w.WriteHeader(http.StatusOK)

	var sse *a2a.SSEWriter
	if b.jsonrpc {
		sse = a2a.NewJSONRPCSSEWriter(w, b.id)
	} else {
		sse = a2a.NewSSEWriter(w)
	}

	if err := sse.WriteTask(run.openingTask()); err != nil {
		s.cfg.logger.Debug("a2a sse open failed", "task_id", run.taskID, "error", err)
		run.fail("the response stream could not be opened")
		return
	}

	for {
		select {
		case frame := <-run.out:
			if err := sse.Write(frame); err != nil {
				// The transport is gone, or the agent produced a frame the stream
				// contract refuses. Either way nothing more can be delivered on
				// this connection, so the task is failed and the slot released.
				s.cfg.logger.Debug("a2a sse write failed", "task_id", run.taskID, "error", err)
				run.fail("the response stream could not be written")
				return
			}
			if sse.Closed() {
				return
			}
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
func (s *Server) blockOnTask(ctx context.Context, w http.ResponseWriter, b binding, run *run) {
	task := run.openingTask()
	for {
		select {
		case frame := <-run.out:
			if applyFrame(&task, frame) {
				s.writeResult(w, b, a2a.TaskResponse(task))
				return
			}
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
