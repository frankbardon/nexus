package main

import (
	"context"
	"errors"
	"sync"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// This file is the seam between the A2A mapping and the broker's instance
// lifecycle. The mapping needs exactly two things from a lease — a way to send
// an IO payload, and a way to be told what comes back — and nothing else about
// spawning, claiming, idling or releasing. Naming that in an interface is what
// lets the mapping be tested against a real conformance corpus without a
// process, and lets the lifecycle story replace the provider without touching a
// line of translation.

// a2aInstance is the leased Nexus instance one A2A task drives.
type a2aInstance interface {
	// SendIO delivers one IO payload to the instance. It must not block on the
	// network: the broker's existing wsConn.queue semantics (enqueue or report
	// a full buffer) are exactly the contract wanted here.
	SendIO(msg brokerIOMessage) error
	// Release relinquishes the lease. It is called exactly once, from the
	// task's terminal sequence, and must be safe on an instance that is
	// already gone.
	Release()
}

// a2aInstanceHooks is how a leased instance reports back. Both are called from
// whatever goroutine reads the instance connection, and neither may block: the
// mapping only ever appends to buffered channels.
type a2aInstanceHooks struct {
	// Deliver receives every SignalIO payload the instance sends.
	Deliver func(brokerIOMessage)
	// Gone reports that the instance is no longer reachable, with a reason fit
	// to put on a FAILED status message. It may be called after Release, and
	// the mapping treats a late call as a no-op.
	Gone func(reason string)
}

// a2aLeaseProvider hands the A2A ingress an instance to run one task on.
//
// Acquire is the whole of the lifecycle contract: given a profile, return
// something that speaks the IO envelope and route its replies to hooks. How
// that instance comes to exist — cold spawn, a warm pool, resuming a persisted
// session — is entirely the provider's business.
type a2aLeaseProvider interface {
	Acquire(ctx context.Context, profile AgentProfile, name string, hooks a2aInstanceHooks) (a2aInstance, error)
}

// errNoLeaseProvider is the refusal an A2A request gets while no provider is
// wired.
//
// It is a distinct sentinel rather than an ad-hoc string so the dispatch can
// answer it with a specific, actionable message instead of a generic internal
// error: the operation IS implemented — decoded, translated and dispatched —
// and what is missing is the machinery that produces an instance to run it on.
var errNoLeaseProvider = errors.New("this broker has no agent instance provider wired, so it cannot start a Nexus instance to run the turn")

// unwiredLeaseProvider is the default provider: it refuses everything.
//
// It exists so a broker built without a lifecycle still ANSWERS — with a clear
// error naming the missing piece — rather than panicking on a nil interface.
// The A2A translation above it is complete and is exercised end to end by the
// shared conformance corpus against a provider supplied by the test.
type unwiredLeaseProvider struct{}

func (unwiredLeaseProvider) Acquire(context.Context, AgentProfile, string, a2aInstanceHooks) (a2aInstance, error) {
	return nil, errNoLeaseProvider
}

// a2aTasks is the ingress's live-task registry.
//
// It holds only LIVE tasks: a task is forgotten the moment it reaches a
// terminal state, because the durable record a finished task is read back from
// is a separate concern. That is why GetTask and ListTasks are not among the
// implemented operations — answering them from this map would answer "unknown
// task" for every task that had finished, which is worse than refusing.
//
// Lookups are PRINCIPAL-SCOPED, and a task belonging to another caller is
// reported exactly as one that does not exist. Anything else is an oracle: a
// caller that could tell "someone else's task" from "no such task" could
// enumerate the ids in flight on a shared broker.
type a2aTasks struct {
	mu   sync.Mutex
	live map[string]*a2aTask
}

func newA2ATasks() *a2aTasks {
	return &a2aTasks{live: make(map[string]*a2aTask)}
}

// add registers a live task.
func (r *a2aTasks) add(t *a2aTask) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.live[t.taskID] = t
}

// remove forgets a task, but only if the map still points at THIS one — a task
// whose id was somehow reused must not be evicted by its predecessor's
// teardown.
func (r *a2aTasks) remove(t *a2aTask) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live[t.taskID] == t {
		delete(r.live, t.taskID)
	}
}

// get resolves a live task for a caller. A task owned by anyone else is
// reported absent.
func (r *a2aTasks) get(caller nexusauth.Principal, taskID string) (*a2aTask, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.live[taskID]
	if !ok || t.owner.ID != caller.ID {
		return nil, false
	}
	return t, true
}

// count reports how many tasks are live, for logging and tests.
func (r *a2aTasks) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.live)
}

// shutdown settles every live task at FAILED so no client is left holding an
// open stream when the broker stops.
//
// A process going away is exactly the condition the crash path renders, and
// rendering it the same way here means a client sees one shape of ending
// whether its instance died or its broker did.
func (r *a2aTasks) shutdown(reason string) {
	r.mu.Lock()
	tasks := make([]*a2aTask, 0, len(r.live))
	for _, t := range r.live {
		tasks = append(tasks, t)
	}
	r.mu.Unlock()
	for _, t := range tasks {
		t.instanceGone(reason)
	}
}

// errLeaseUnavailable renders a failure to acquire an instance as the A2A error
// a client is answered with.
//
// InternalError is the honest type: the request was well-formed and the
// operation is supported, and what failed was this broker's ability to produce
// an agent to run it. The ErrorInfo reason gives a client a stable token to
// branch on, since the prose will change as the lifecycle grows.
func errLeaseUnavailable(profile string, err error) *a2a.Error {
	reason := "INSTANCE_UNAVAILABLE"
	if errors.Is(err, errNoLeaseProvider) {
		reason = "INSTANCE_PROVIDER_NOT_WIRED"
	}
	return a2a.Errorf(a2a.ErrorTypeInternal,
		"no agent instance could be started for profile %q: %v", profile, err).
		WithMetadata("profile", profile).
		WithMetadata("detail", reason)
}

// errSendFailed renders a payload that could not be handed to the instance.
func errSendFailed(profile string, err error) *a2a.Error {
	return a2a.Errorf(a2a.ErrorTypeInternal,
		"the message could not be delivered to the agent instance for profile %q: %v", profile, err).
		WithMetadata("profile", profile).
		WithMetadata("detail", "INSTANCE_UNREACHABLE")
}
