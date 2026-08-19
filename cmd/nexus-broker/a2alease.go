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

// a2aLeaseRequest is everything a provider is told about the turn it must
// produce an instance for.
//
// It is a struct rather than a parameter list for the reason a2aTaskConfig is:
// three of its fields are strings, and a positional call site would let two of
// them be transposed without the compiler noticing — a bug that would route one
// caller's conversation to another's, silently.
type a2aLeaseRequest struct {
	// profile is the resolved `agents:` entry: which config to boot and which
	// binary registry entry to boot it with.
	profile AgentProfile

	// name is the profile's name, which is also its route namespace and the
	// label every log line carries.
	name string

	// contextID is the A2A conversation this turn belongs to. It is THE
	// continuity key: a provider that keeps state maps it to an engine session,
	// so a second message on the same context reaches the same history.
	//
	// It is never empty by the time a provider sees it — the ingress mints one
	// for a client that named none — so a provider need not invent a fallback.
	contextID string

	// owner is the principal the A2A request authenticated as. It scopes the
	// context mapping (see a2aContextRecord.OwnerID) and is stamped on the lease
	// the same way a claim's principal is, so an A2A-created instance is owned,
	// listed and released exactly like a claimed one.
	owner nexusauth.Principal

	// hooks is how the leased instance reports back.
	hooks a2aInstanceHooks
}

// a2aLeaseProvider hands the A2A ingress an instance to run one task on.
//
// Acquire is the whole of the lifecycle contract: given a profile and a
// conversation, return something that speaks the IO envelope and route its
// replies to hooks. How that instance comes to exist — cold spawn, reuse of the
// one already serving that context, resuming a persisted session — is entirely
// the provider's business, and the translation above it never learns which
// happened.
type a2aLeaseProvider interface {
	Acquire(ctx context.Context, req a2aLeaseRequest) (a2aInstance, error)
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

func (unwiredLeaseProvider) Acquire(context.Context, a2aLeaseRequest) (a2aInstance, error) {
	return nil, errNoLeaseProvider
}

// a2aSpawnError classifies a failure to produce an instance onto the A2A TASK
// STATE the client is answered with, rather than onto a protocol error.
//
// The distinction is the point. A protocol error says "this request was not
// processed"; a terminal task state says "this request was processed and here is
// how it ended". A client that asked an agent a question and could not be given
// one deserves the second: it gets a task id, a terminal state and a status
// message explaining what happened, in the same shape it would have got if the
// agent had run and failed. It never has to learn that Nexus has leases, or that
// starting one can fail.
//
//   - REJECTED is for a request this broker REFUSED: the profile names a binary
//     registry entry that is gone, or the context's session was created by a
//     different build (see resolveSpawnBinary). Nothing was attempted, and
//     retrying the same message will fail the same way — an operator has to
//     change something.
//   - FAILED is for a spawn that was ATTEMPTED and did not produce a live
//     instance: the process died booting, it never signalled ready inside the
//     timeout, or the broker is at capacity. A retry may well succeed.
//
// Anything not carrying this classification stays a protocol error (see
// errLeaseUnavailable), because "the broker has no way to run agents at all" is
// not a property of the task.
type a2aSpawnError struct {
	// state is the terminal state the task settles in. It is always one of
	// TaskStateRejected or TaskStateFailed; the constructors are the only way to
	// build one, so no third state can leak in.
	state a2a.TaskState

	// reason is the operator-and-client-facing prose put on the terminal status
	// message. It names what could not be started and why, without naming leases.
	reason string

	// err is the underlying cause, kept for logs and errors.Is.
	err error
}

func (e *a2aSpawnError) Error() string { return e.reason }

func (e *a2aSpawnError) Unwrap() error { return e.err }

// a2aRejectedSpawn builds the refusal for a request this broker will not act on.
func a2aRejectedSpawn(reason string, err error) *a2aSpawnError {
	return &a2aSpawnError{state: a2a.TaskStateRejected, reason: reason, err: err}
}

// a2aFailedSpawn builds the failure for a spawn that was attempted and did not
// produce a live instance.
func a2aFailedSpawn(reason string, err error) *a2aSpawnError {
	return &a2aSpawnError{state: a2a.TaskStateFailed, reason: reason, err: err}
}

// a2aSpawnOutcome reports the terminal task state an acquisition failure should
// settle at, and whether the failure carried a classification at all.
//
// ok=false means "not a spawn outcome" — the unwired provider, or any error a
// future provider returns without classifying it — and the caller answers with a
// protocol error instead. Defaulting an unclassified error to FAILED was
// rejected: it would silently turn "this broker cannot run agents" into "your
// task failed", which points an integrator at their own request.
func a2aSpawnOutcome(err error) (a2a.TaskState, string, bool) {
	var spawnErr *a2aSpawnError
	if errors.As(err, &spawnErr) {
		return spawnErr.state, spawnErr.reason, true
	}
	return "", "", false
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
