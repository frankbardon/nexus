package a2a

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// This file wires the three task-READ operations — GetTask, ListTasks and
// SubscribeToTask — onto the durable store. Both HTTP bindings share every
// handler here; they differ only in how a result or an error is framed, which
// the binding type already abstracts.
//
// # The one rule these operations exist under
//
// A task belonging to another principal must be INDISTINGUISHABLE from a task
// that does not exist. Not a different error, not a different status, not a
// different latency. The store makes this structural rather than careful: the
// only way to reach a task is through a view bound to one Principal, and that
// view reports a foreign task as absent, so every handler below takes exactly
// one code path for "unknown" and "not yours" because it cannot tell them apart
// in the first place. There is no branch here to get wrong, and no second lookup
// that could leak the difference in timing — the foreign case does the same
// single indexed lookup the unknown case does.
//
// This mirrors the broker's errTicketRejected reasoning: a distinct "exists but
// is not yours" answer is an existence oracle for ids the caller was never told.

// ---- GetTask ----

// handleGetTask answers a task read from the caller's own tasks.
//
// The STORE is the source of truth, even while the task is live. Every frame is
// persisted before it is delivered (see run.record), so the store is never
// behind what a client has been told — reading the in-memory run instead would
// buy nothing and would introduce a second, differently-fresh answer to the same
// question.
func (s *Server) handleGetTask(w http.ResponseWriter, caller nexusauth.Principal, b binding, req *a2a.GetTaskRequest) {
	rec, protoErr := s.lookupTask(caller, req.ID)
	if protoErr != nil {
		s.writeError(w, b, protoErr)
		return
	}
	s.writeResult(w, b, rec.Task(renderOptions{historyLength: req.HistoryLength}))
}

// ---- ListTasks ----

// handleListTasks answers a page of the caller's own tasks.
//
// Artifacts are OFF by default (specification section 3.2: includeArtifacts
// "defaults to false to keep payloads small"), history is capped by the request,
// and the page is bounded whether or not the client asked for a bound — an
// unpaginated list of every retained task is not an answer this endpoint offers.
func (s *Server) handleListTasks(w http.ResponseWriter, caller nexusauth.Principal, b binding, req *a2a.ListTasksRequest) {
	if s.cfg.bridge == nil {
		s.writeError(w, b, errNoBridge())
		return
	}

	pageSize := resolvePageSize(req.PageSize)
	q := taskQuery{
		contextID: req.ContextID,
		state:     req.Status,
		limit:     pageSize,
	}
	if req.StatusTimestampAfter != nil {
		q.changedAfter = req.StatusTimestampAfter.Time
	}
	if req.PageToken != "" {
		cursor, protoErr := decodePageToken(req.PageToken)
		if protoErr != nil {
			s.writeError(w, b, protoErr)
			return
		}
		q.after = cursor
	}

	recs, next, total, err := s.cfg.bridge.queryTasks(caller, q)
	if err != nil {
		s.cfg.logger.Error("a2a ListTasks failed", "error", err)
		s.writeError(w, b, a2a.Errorf(a2a.ErrorTypeInternal, "the task list could not be read"))
		return
	}

	opt := renderOptions{
		historyLength: req.HistoryLength,
		omitArtifacts: req.IncludeArtifacts == nil || !*req.IncludeArtifacts,
	}
	// Non-nil even when empty: ListTasksResponse.tasks is a required field, and
	// a client that has to distinguish null from [] has been handed a needless
	// special case.
	tasks := make([]a2a.Task, 0, len(recs))
	for _, rec := range recs {
		tasks = append(tasks, rec.Task(opt))
	}

	s.writeResult(w, b, a2a.ListTasksResponse{
		Tasks:         tasks,
		NextPageToken: encodePageToken(next),
		PageSize:      pageSize,
		TotalSize:     total,
	})
}

// resolvePageSize applies the server's default and clamps to the specification's
// bounds. The codec already validates a client-supplied value, so the clamp
// covers a caller that reached this handler another way rather than a request
// path that can produce one.
func resolvePageSize(requested *int) int {
	if requested == nil {
		return a2a.DefaultListPageSize
	}
	return min(max(*requested, a2a.MinListPageSize), a2a.MaxListPageSize)
}

// ---- SubscribeToTask ----

// handleSubscribeToTask attaches an SSE stream to an existing task.
//
// The stream ALWAYS opens with the task's current state — section 11.7 requires
// a stream to open with a Task frame, and a subscriber that joined mid-turn has
// no other way to learn what it missed. What follows depends on whether there is
// anything left to say:
//
//   - The task is live: the subscriber joins the run's fan-out and receives
//     exactly the frames every other attached stream receives, from the point it
//     attached. Attaching takes the snapshot and registers the channel under one
//     lock, so there is no frame that lands in neither.
//   - The task is terminal: the opening snapshot reports a terminal state, which
//     closes the stream by the same rule that closes a completed SendStreamingMessage.
//     The client gets the outcome and an EOF rather than an open socket.
//   - The task is neither live nor terminal — a task the process was serving when
//     it last stopped: the snapshot is written and the stream closed. Nothing
//     will ever update that task again, so parking the connection would be a
//     promise this agent cannot keep. GetTask remains the way to read it.
func (s *Server) handleSubscribeToTask(ctx context.Context, w http.ResponseWriter, b binding, caller nexusauth.Principal, req *a2a.SubscribeToTaskRequest, opts streamOptions) {
	rec, protoErr := s.lookupTask(caller, req.ID)
	if protoErr != nil {
		// Refused BEFORE any stream is opened, so the client reads the refusal
		// off the status and the body rather than out of a 200 SSE stream whose
		// only record is an error.
		s.writeError(w, b, protoErr)
		return
	}

	// The store's history is carried onto the opening frame; the live run tracks
	// status and artifacts but not the message trail.
	opening := rec.Task(renderOptions{})

	live := s.cfg.bridge.liveRun(req.ID)
	if live == nil || rec.Status.State.IsTerminal() {
		s.pumpStream(ctx, w, b, nil, nil, opening, nil)
		return
	}

	sub, snapshot := live.attach(opts)
	defer live.detach(sub)
	// The run's snapshot wins for status and artifacts: it is atomic with the
	// subscription, whereas the record was read a moment earlier and may already
	// be one frame behind the channel.
	snapshot.History = opening.History
	s.pumpStream(ctx, w, b, live, sub, snapshot, nil)
}

// ---- CancelTask ----

// handleCancelTask settles one of the caller's tasks at TASK_STATE_CANCELED.
//
// The lookup is the same principal-scoped one every read uses, so a task
// belonging to somebody else is refused exactly as an unknown id is — before
// its state, and therefore whether it was cancelable, can be inferred.
//
// Cancelling an ALREADY-TERMINAL task answers TaskNotCancelableError and writes
// nothing: a terminal state is final (section 3.1.1), so "cancel" on one is a
// well-defined client mistake, not an instruction to rewrite history. The
// distinction matters more than it looks — the alternative, treating it as a
// no-op success, would tell a client its cancel took effect on a task that had
// already completed and whose output it is about to read.
func (s *Server) handleCancelTask(w http.ResponseWriter, caller nexusauth.Principal, b binding, req *a2a.CancelTaskRequest) {
	if s.cfg.bridge == nil {
		s.writeError(w, b, errNoBridge())
		return
	}
	if req == nil || strings.TrimSpace(req.ID) == "" {
		s.writeError(w, b, a2a.ErrInvalidParams(a2a.FieldViolation{
			Field: "id", Description: "task id is required",
		}))
		return
	}
	task, protoErr := s.cfg.bridge.cancelTask(caller, req.ID)
	if protoErr != nil {
		s.writeError(w, b, protoErr)
		return
	}
	s.writeResult(w, b, task)
}

// ---- shared plumbing ----

// lookupTask resolves a task id to the caller's own record, or to the error the
// client is answered with.
//
// TaskNotFoundError is the answer for BOTH an id nobody ever minted and an id
// belonging to somebody else, and the two are the same code path here because
// the store already collapsed them.
func (s *Server) lookupTask(caller nexusauth.Principal, taskID string) (taskRecord, *a2a.Error) {
	if s.cfg.bridge == nil {
		return taskRecord{}, errNoBridge()
	}
	rec, found, err := s.cfg.bridge.lookupTask(caller, taskID)
	if err != nil {
		s.cfg.logger.Error("a2a task lookup failed", "task_id", taskID, "error", err)
		return taskRecord{}, a2a.Errorf(a2a.ErrorTypeInternal, "the task could not be read")
	}
	if !found {
		return taskRecord{}, a2a.ErrTaskNotFound(taskID)
	}
	return rec, nil
}

// errNoBridge is the refusal a Server with no bus bridge answers with. It is
// defensive: a directly-constructed transport-only Server has no store to read.
func errNoBridge() *a2a.Error {
	return a2a.Errorf(a2a.ErrorTypeInternal, "the a2a serve transport is not wired to the event bus")
}

// ---- page tokens ----

// pageTokenSeparator joins the two halves of an encoded cursor.
const pageTokenSeparator = "."

// encodePageToken renders a cursor as the opaque nextPageToken a client echoes
// back. An unset cursor renders as the empty string, which is how
// ListTasksResponse spells "this was the last page".
//
// The encoding is base64 of the keyset tuple: opaque enough that a client is not
// tempted to construct one, cheap enough that the server holds no cursor state.
// Nothing secret is in it — it is two timestamps' worth of ordering position for
// rows the caller can already read — so it is encoded, not signed.
func encodePageToken(c listCursor) string {
	if !c.set {
		return ""
	}
	raw := strconv.FormatInt(c.createdAt, 10) + pageTokenSeparator + strconv.FormatInt(c.rowID, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodePageToken parses a token a client echoed back. A token this server did
// not mint is an InvalidParamsError rather than an empty page: silently
// restarting from the top would make a client's pagination loop repeat forever.
func decodePageToken(token string) (listCursor, *a2a.Error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return listCursor{}, errBadPageToken()
	}
	createdAt, rowID, ok := strings.Cut(string(raw), pageTokenSeparator)
	if !ok {
		return listCursor{}, errBadPageToken()
	}
	created, err := strconv.ParseInt(createdAt, 10, 64)
	if err != nil {
		return listCursor{}, errBadPageToken()
	}
	row, err := strconv.ParseInt(rowID, 10, 64)
	if err != nil {
		return listCursor{}, errBadPageToken()
	}
	return listCursor{createdAt: created, rowID: row, set: true}, nil
}

func errBadPageToken() *a2a.Error {
	return a2a.ErrInvalidParams(a2a.FieldViolation{
		Field:       "pageToken",
		Description: fmt.Sprintf("must be a nextPageToken returned by a previous %s call", a2a.MethodListTasks),
	})
}

// ---- bridge: the plugin's half ----
//
// These are the *Plugin implementations of agentBridge's read methods. They are
// thin on purpose: the scoping they rely on lives in the store, and adding
// filtering or an ownership check HERE would be a second place for it to be
// wrong. All they do is derive the caller's view and ask it.

// lookupTask reads one of the caller's own tasks.
func (p *Plugin) lookupTask(caller nexusauth.Principal, taskID string) (taskRecord, bool, error) {
	return p.tasks.For(caller).Get(taskID)
}

// queryTasks reads one page of the caller's own tasks.
func (p *Plugin) queryTasks(caller nexusauth.Principal, q taskQuery) ([]taskRecord, listCursor, int, error) {
	return p.tasks.For(caller).Query(q)
}
