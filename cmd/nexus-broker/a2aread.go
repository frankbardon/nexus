package main

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
// SubscribeToTask — onto the broker's durable store. Both HTTP bindings share
// every handler here; they differ only in how a result or an error is framed,
// which a2aBinding already abstracts.
//
// # The one rule these operations exist under
//
// A task belonging to another principal must be INDISTINGUISHABLE from a task
// that does not exist. Not a different error, not a different status, not a
// different latency. The store makes this structural rather than careful: a task
// is not reachable by its id at all — the fold is keyed by owner first — so every
// handler below takes exactly ONE code path for "unknown" and "not yours",
// because it cannot tell them apart in the first place. There is no branch here
// to get wrong and no second lookup that could leak the difference in timing.
//
// This is the same reasoning as unknownLeaseError one directory over: a distinct
// "exists but is not yours" answer is an existence oracle for ids the caller was
// never told.
//
// # Why the store rather than the live task
//
// The store is the source of truth even while a task is running. Every frame is
// persisted before it is delivered (see a2aTask.emit), so the record is never
// behind what a client has already been told, and reading the live task instead
// would introduce a second, differently-fresh answer to one question. It would
// also answer nothing at all for the case these operations exist for: a task
// whose instance was released hours ago, or which ran on a previous broker
// process.

// ---- GetTask ----

// handleGetTask answers a task read from the caller's own tasks.
func (s *A2AServer) handleGetTask(w http.ResponseWriter, r *http.Request, card *servedAgentCard, b a2aBinding, req *a2a.GetTaskRequest) {
	rec, protoErr := s.lookupTask(callerPrincipal(r), card.profile, req.ID)
	if protoErr != nil {
		s.writeA2AError(w, card.profile, b, protoErr)
		return
	}
	s.writeA2AResult(w, card.profile, b, rec.task(a2aRenderOptions{historyLength: req.HistoryLength}))
}

// ---- ListTasks ----

// handleListTasks answers a page of the caller's own tasks.
//
// Artifacts are OFF by default (specification section 3.2: includeArtifacts
// "defaults to false to keep payloads small"), history is capped by the request,
// and the page is bounded whether or not the client asked for a bound — an
// unpaginated list of every retained task is not an answer this endpoint offers.
func (s *A2AServer) handleListTasks(w http.ResponseWriter, r *http.Request, card *servedAgentCard, b a2aBinding, req *a2a.ListTasksRequest) {
	pageSize := resolveA2APageSize(req.PageSize)
	q := a2aTaskQuery{
		contextID: strings.TrimSpace(req.ContextID),
		state:     req.Status,
		limit:     pageSize,
	}
	if req.StatusTimestampAfter != nil {
		q.changedAfter = req.StatusTimestampAfter.Time
	}
	if req.PageToken != "" {
		cursor, protoErr := decodeA2APageToken(req.PageToken)
		if protoErr != nil {
			s.writeA2AError(w, card.profile, b, protoErr)
			return
		}
		q.after = cursor
	}

	recs, next, total := s.store.For(callerPrincipal(r), card.profile).Query(q)

	opt := a2aRenderOptions{
		historyLength: req.HistoryLength,
		omitArtifacts: req.IncludeArtifacts == nil || !*req.IncludeArtifacts,
	}
	// Non-nil even when empty: ListTasksResponse.tasks is a required field, and a
	// client that has to distinguish null from [] has been handed a needless
	// special case.
	tasks := make([]a2a.Task, 0, len(recs))
	for _, rec := range recs {
		tasks = append(tasks, rec.task(opt))
	}

	s.writeA2AResult(w, card.profile, b, a2a.ListTasksResponse{
		Tasks:         tasks,
		NextPageToken: encodeA2APageToken(next),
		PageSize:      pageSize,
		TotalSize:     total,
	})
}

// resolveA2APageSize applies the server's default and clamps to the
// specification's bounds. The codec already validates a client-supplied value,
// so the clamp covers a caller that reached this handler another way rather than
// a request path that can produce one.
func resolveA2APageSize(requested *int) int {
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
//   - The task is LIVE: the subscriber joins the task's fan-out and receives
//     exactly the frames every other attached stream receives, from the point it
//     attached. Attaching takes the snapshot and registers the channel under one
//     lock, so no frame lands in neither. That covers a task still queued at
//     SUBMITTED as well as one mid-turn — a queued task's stream is simply quiet
//     until it is promoted.
//   - The task is TERMINAL: the opening snapshot reports a terminal state, which
//     closes the stream by the same rule that closes a completed
//     SendStreamingMessage. The client gets the outcome and an EOF rather than an
//     open socket that will never carry anything.
//   - The task is neither live nor terminal — a task some previous broker process
//     was serving: the snapshot is written and the stream closed. Nothing will
//     ever update that task again, so parking the connection would be a promise
//     this broker cannot keep. In practice the store settles those to FAILED when
//     it opens, so this is the residual race rather than the normal path.
func (s *A2AServer) handleSubscribeToTask(ctx context.Context, w http.ResponseWriter, r *http.Request, card *servedAgentCard, b a2aBinding, req *a2a.SubscribeToTaskRequest) {
	caller := callerPrincipal(r)
	rec, protoErr := s.lookupTask(caller, card.profile, req.ID)
	if protoErr != nil {
		// Refused BEFORE any stream is opened, so the client reads the refusal off
		// the status and the body rather than out of a 200 SSE stream whose only
		// record is an error.
		s.writeA2AError(w, card.profile, b, protoErr)
		return
	}

	opening := rec.task(a2aRenderOptions{})
	task, live := s.tasks.get(caller, req.ID)
	if !live || task.terminated() || rec.status().State.IsTerminal() {
		s.pumpStream(ctx, w, card.profile, b, nil, opening)
		return
	}

	sub, snapshot := task.attach()
	defer task.detach(sub)
	// The live task's snapshot wins for status and artifacts: it is atomic with
	// the subscription, whereas the record was read a moment earlier and may
	// already be one frame behind the channel. The stored History is carried
	// across, because the live task tracks frames and not the message trail.
	snapshot.History = opening.History
	s.pumpStream(ctx, w, card.profile, b, sub, snapshot)
}

// ---- shared plumbing ----

// lookupTask resolves a task id to the caller's own record, or to the error the
// client is answered with.
//
// TaskNotFoundError is the answer for BOTH an id nobody ever minted and an id
// belonging to somebody else — or to another profile — and the three are one
// code path here because the store already collapsed them.
func (s *A2AServer) lookupTask(caller nexusauth.Principal, profile, taskID string) (a2aTaskRecord, *a2a.Error) {
	rec, found := s.store.For(caller, profile).Get(strings.TrimSpace(taskID))
	if !found {
		return a2aTaskRecord{}, a2a.ErrTaskNotFound(taskID)
	}
	return rec, nil
}

// ---- page tokens ----

// a2aPageTokenSeparator joins the two halves of an encoded cursor.
const a2aPageTokenSeparator = "."

// encodeA2APageToken renders a cursor as the opaque nextPageToken a client
// echoes back. An unset cursor renders as the empty string, which is how
// ListTasksResponse spells "this was the last page".
//
// The encoding is base64 of the keyset tuple: opaque enough that a client is not
// tempted to construct one, cheap enough that the broker holds no cursor state.
// Nothing secret is in it — it is an ordering position for rows the caller can
// already read — so it is encoded, not signed.
func encodeA2APageToken(c a2aListCursor) string {
	if !c.set {
		return ""
	}
	raw := strconv.FormatInt(c.createdAt, 10) + a2aPageTokenSeparator + strconv.FormatInt(c.seq, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeA2APageToken parses a token a client echoed back. A token this broker
// did not mint is an InvalidParamsError rather than an empty page: silently
// restarting from the top would make a client's pagination loop repeat for ever.
func decodeA2APageToken(token string) (a2aListCursor, *a2a.Error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return a2aListCursor{}, errA2ABadPageToken()
	}
	createdAt, seq, ok := strings.Cut(string(raw), a2aPageTokenSeparator)
	if !ok {
		return a2aListCursor{}, errA2ABadPageToken()
	}
	created, err := strconv.ParseInt(createdAt, 10, 64)
	if err != nil {
		return a2aListCursor{}, errA2ABadPageToken()
	}
	order, err := strconv.ParseInt(seq, 10, 64)
	if err != nil {
		return a2aListCursor{}, errA2ABadPageToken()
	}
	return a2aListCursor{createdAt: created, seq: order, set: true}, nil
}

func errA2ABadPageToken() *a2a.Error {
	return a2a.ErrInvalidParams(a2a.FieldViolation{
		Field:       "pageToken",
		Description: fmt.Sprintf("must be a nextPageToken returned by a previous %s call", a2a.MethodListTasks),
	})
}
