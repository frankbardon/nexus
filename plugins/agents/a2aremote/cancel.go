package a2aremote

import (
	"context"
	"errors"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/a2a/a2aclient"
	"github.com/frankbardon/nexus/pkg/engine"
)

// Cancellation, in both directions it has to travel.
//
// A user who interrupts the local turn has interrupted everything that turn set
// in motion, and a remote A2A agent is the one piece of that which is running in
// somebody else's process. Left alone it would keep working — burning the
// remote's tokens, holding the remote's slot — on an answer nobody is left to
// read, and would keep a question sitting in front of a human who has already
// moved on.
//
// So a local cancellation does three things, in this order:
//
//  1. Retracts any question this delegation put in front of a human, with
//     hitl.cancel, so the prompt disappears rather than outliving the turn.
//  2. Issues CancelTask to the remote for every task this instance knows the id
//     of, which is A2A's own cancellation operation (specification section 3.3).
//  3. Cancels the call's context, which unblocks the stream reader and lets the
//     delegated tool result be published as a cancellation rather than a hang.
//
// cancel.active is the signal, not cancel.request: it is the event
// nexus.control.cancel emits once a cancellation is actually happening, and it
// is what the LLM providers already key their own abort off. Keying off the
// request instead would abort work a veto might still have stopped.
//
// The same abandonment path runs on the ordinary exits too — an exhausted
// budget, a broken stream, a question nobody answered. The rule is one sentence:
// if this instance walks away from a remote task that has not reached a terminal
// state, it tells the remote.

// cancelTaskTimeout bounds the CancelTask call itself. It is deliberately short
// and independent of the delegation's own deadline, because by the time it runs
// that deadline is usually the thing that expired: a cancel sent on a dead
// context would never leave the process.
const cancelTaskTimeout = 15 * time.Second

// trackSession registers an in-flight call so the cancellation path can reach it.
func (p *Plugin) trackSession(s *session) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.live[s.spawnID] = s
}

// untrackSession forgets a finished call.
func (p *Plugin) untrackSession(s *session) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.live, s.spawnID)
}

// liveSessions snapshots the in-flight calls.
func (p *Plugin) liveSessions() []*session {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*session, 0, len(p.live))
	for _, s := range p.live {
		out = append(out, s)
	}
	return out
}

// onCancelActive propagates a local cancellation to every remote in flight.
//
// The work happens off the dispatch goroutine: CancelTask is a network call, and
// the bus dispatches synchronously, so doing it inline would block every other
// subscriber to cancel.active behind somebody else's HTTP round trip.
func (p *Plugin) onCancelActive(engine.Event[any]) {
	sessions := p.liveSessions()
	if len(sessions) == 0 {
		return
	}
	p.spawn(func() {
		for _, s := range sessions {
			s.cancelLocally("the local turn was cancelled")
		}
	})
}

// cancelLocally retracts the session's pending question, cancels the remote task
// and aborts the call.
func (s *session) cancelLocally(reason string) {
	if requestID := s.pendingHITL(); requestID != "" {
		s.p.retract(requestID, reason)
	}
	s.abandon(reason)
	if s.cancel != nil {
		s.cancel()
	}
}

// abandon tells the remote to stop working on a task this instance is walking
// away from.
//
// It is a no-op for a task that reached a terminal state (there is nothing to
// cancel), for a call that never learned a task id (there is nothing to address),
// and on a second call (a cancel arriving twice must not send two CancelTasks).
func (s *session) abandon(reason string) {
	s.mu.Lock()
	if s.terminal || s.aborted || s.taskID == "" {
		s.mu.Unlock()
		return
	}
	s.aborted = true
	taskID := s.taskID
	s.mu.Unlock()

	p := s.p
	agent := s.ra.cfg.name
	client := s.ra.client

	p.spawn(func() {
		ctx, cancel := context.WithTimeout(context.Background(), cancelTaskTimeout)
		defer cancel()

		task, err := client.CancelTask(ctx, a2a.CancelTaskRequest{ID: taskID})
		if err != nil {
			// A task the remote considers finished answers TaskNotCancelable,
			// which is not a failure: it means the race was lost in the harmless
			// direction. Everything else is worth a line, but none of it is
			// worth failing the delegation over — the delegation is already over.
			var protoErr *a2a.Error
			if errors.As(err, &protoErr) && protoErr.Type == a2a.ErrorTypeTaskNotCancelable {
				p.logger.Debug("a2a_remote could not cancel an already-final remote task",
					"agent", agent, "task_id", taskID)
				return
			}
			var httpErr *a2aclient.HTTPError
			if errors.As(err, &httpErr) {
				p.logger.Warn("a2a_remote could not cancel a remote task",
					"agent", agent, "task_id", taskID, "status", httpErr.StatusCode)
				return
			}
			p.logger.Warn("a2a_remote could not cancel a remote task",
				"agent", agent, "task_id", taskID, "error", err)
			return
		}
		state := ""
		if task != nil {
			state = string(task.Status.State)
		}
		p.logger.Info("a2a_remote cancelled a remote task",
			"agent", agent, "task_id", taskID, "state", state, "reason", reason)
	})
}

// spawn runs fn on its own goroutine, tracked so Shutdown waits for it.
//
// The closing flag is what makes the tracking safe: adding to a WaitGroup whose
// counter has already reached zero, concurrently with a Wait, is a data race by
// definition. Shutdown sets the flag under the same lock before it waits, so a
// cancel.active that was already in flight cannot add a goroutine to a group
// nobody will ever wait for again — it is dropped instead.
func (p *Plugin) spawn(fn func()) {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return
	}
	p.wg.Add(1)
	p.mu.Unlock()

	go func() {
		defer p.wg.Done()
		fn()
	}()
}
