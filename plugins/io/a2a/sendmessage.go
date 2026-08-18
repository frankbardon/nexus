package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

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
	//
	// A message naming a task CONTINUES that task rather than starting one: the
	// answer is routed to the question the task is parked on and the same run is
	// followed, so a resumed task never becomes a second turn.
	var (
		run     *run
		sub     *stream
		opening a2a.Task
	)
	// The snapshot comes back with the stream because the two are taken under
	// one lock: it accounts for exactly the frames emitted before the stream
	// attached, and the channel carries exactly those emitted after. A new task
	// opens on SUBMITTED; a resumed one opens on the INPUT_REQUIRED it was
	// parked at, so the transitions the answer causes arrive as updates the
	// client can follow rather than being folded into the opening frame.
	if in.taskID != "" {
		run, sub, opening, protoErr = s.cfg.bridge.resumeTurn(in, caller)
	} else {
		run, sub, opening, protoErr = s.cfg.bridge.startTurn(in, caller)
	}
	if protoErr != nil {
		s.writeError(w, b, protoErr)
		return
	}
	// The subscription is released when this response is finished with it.
	// Detaching does NOT end the task: the run's lifetime is the task's, not
	// this request's, so a SubscribeToTask stream may still be watching it and
	// the turn carries on either way.
	defer run.detach(sub)

	// A client that vanishes mid-turn no longer fails its own task. The run
	// outlives the request that started it, so the honest response to a dropped
	// connection is to stop writing to it — the task keeps running, GetTask
	// still answers, and SubscribeToTask reattaches to exactly where it got to.
	// CancelTask is how a client that has genuinely given up says so.
	ctx := r.Context()

	// configuration.returnImmediately (specification section 3.2.2): answer with
	// the task as it stands and let the client follow it by other means.
	// Streaming ignores it, because a stream IS the follow-up it asks for.
	if !streaming && req.Configuration != nil && req.Configuration.ReturnImmediately {
		run.detach(sub)
		s.writeResult(w, b, a2a.TaskResponse(opening))
		return
	}

	if streaming {
		// A write failure on THIS connection no longer fails the task either,
		// for the same reason: the answer outlives the socket that asked for it.
		s.pumpStream(ctx, w, b, sub, opening, nil)
		return
	}
	s.blockOnTask(ctx, w, b, sub, run, opening)
}

// parkedKeepaliveInterval is how often an SSE comment is written to a stream
// parked on TASK_STATE_INPUT_REQUIRED.
//
// A parked stream is deliberately silent: the task is waiting for a human, and
// nothing has happened. Proxies and load balancers read that silence as a dead
// connection and close it, so pkg/a2a exposes SSEWriter.Ping for exactly this —
// an SSE comment record keeps the socket warm without emitting a protocol frame
// that says nothing happened. Twenty seconds sits under the common 30s and 60s
// idle timeouts with room to spare.
const parkedKeepaliveInterval = 20 * time.Second

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

	// The keepalive only ever fires on a parked stream; a working task produces
	// frames of its own, and a comment between them would be noise.
	keepalive := time.NewTicker(parkedKeepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case <-keepalive.C:
			if !sse.Interrupted() {
				continue
			}
			if err := sse.Ping(); err != nil {
				s.cfg.logger.Debug("a2a sse keepalive failed", "task_id", opening.ID, "error", err)
				return
			}
		case frame := <-sub.frames:
			if err := sse.Write(frame); err != nil {
				// The transport is gone, or the agent produced a frame the stream
				// contract refuses. Either way nothing more can be delivered on
				// this connection — but the task is unaffected: it is followed by
				// reattaching, not by holding one socket open.
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

// blockOnTask renders the run as a single Task reply once it reaches a state
// the client has to act on. Blocking is A2A's default for SendMessage
// (specification section 3.2.2): the call returns when the work is done, not
// when it was accepted.
//
// "Done" includes INTERRUPTED, not only terminal. A task that reaches
// INPUT_REQUIRED is waiting for the caller, so continuing to block would be
// waiting for a client that is itself waiting on this response — the deadlock
// the interrupted states exist to avoid. The returned Task carries the question
// on its status message, and the client answers by sending a new message naming
// the same taskId.
//
// It consumes exactly the frames the streaming path writes, folding each into
// the Task snapshot, so the two bindings cannot report different outcomes for
// the same turn.
func (s *Server) blockOnTask(ctx context.Context, w http.ResponseWriter, b binding, sub *stream, run *run, opening a2a.Task) {
	task := opening
	for {
		select {
		case frame := <-sub.frames:
			applyFrame(&task, frame)
			if state := task.Status.State; state.IsTerminal() || state.IsInterrupted() {
				s.writeResult(w, b, a2a.TaskResponse(task))
				return
			}
		case <-sub.dropped:
			// The observer was dropped, so the terminal frame will never arrive
			// on this channel. This request cannot report the outcome — but it
			// must not invent one either, and it no longer fails the task to
			// produce something to say: the task is still running and the client
			// holds its id. The task as last seen is returned, which is exactly
			// what GetTask would answer a moment later.
			s.cfg.logger.Warn("a2a blocking request fell behind its own task",
				"task_id", run.taskID)
			s.writeResult(w, b, a2a.TaskResponse(run.snapshotTask()))
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
