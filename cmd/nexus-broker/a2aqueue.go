package main

import (
	"log/slog"
	"sync"
)

// This file makes concurrent A2A tasks on one conversation run ONE AT A TIME.
//
// # The problem it solves
//
// A Nexus instance runs one agent loop. Two `input` payloads sent to it while a
// turn is in flight do not produce two turns: they interleave into whatever the
// loop does next. The broker's IO envelope cannot express two turns either — a
// payload carries a turn id, but the fan-out from a lease reaches every task
// attached to it, and a task binds to the first turn id it sees. So before this
// file existed, two simultaneous messages on one contextId produced two tasks
// that both watched the SAME turn's output and both claimed it as their answer.
//
// The fix is not smarter routing. It is to stop the second turn from starting:
// a conversation admits one active task, and the next one waits in
// TASK_STATE_SUBMITTED until the active one is terminal. That is a truthful A2A
// rendering of what the agent is actually doing — SUBMITTED means "accepted, not
// yet started" (specification section 3.1.1), which is precisely a queued turn —
// and it makes the turn-id binding correct by construction rather than by luck,
// because there is only ever one turn in flight per instance to bind to.
//
// # What the queue is keyed by
//
// The (owner, profile, contextId) triple: exactly the key a2aLeaseManager files
// its instance under. That is the point — the queue serializes precisely the set
// of tasks that would otherwise share one agent loop. Two principals using the
// same contextId get two instances and therefore two queues, and do not wait on
// each other.
//
// # How it survives things going wrong
//
// The queue advances on ONE event: a task reaching a terminal state. Every way a
// turn can end funnels through there — completion, cancellation, an instance
// released while idle, an instance that crashed, the broker shutting down, and
// the input deadline expiring on a parked task. So an instance dying mid-queue
// does not strand the tasks behind it: the active task is failed by the same
// Gone hook that has always failed it, that is terminal, and the next task is
// promoted and acquires a fresh instance — which, because the conversation's
// session id is durable, resumes the conversation rather than starting a new one.
//
// A task at TASK_STATE_INPUT_REQUIRED is NOT terminal and deliberately keeps the
// queue: the agent loop is blocked inside ask_user, so letting the next turn
// start would send input to an instance that cannot read it. What stops that
// from being a deadlock is a2a.tasks.input_timeout — a parked task that nobody
// answers is failed, which is terminal, which advances the queue. See
// a2aTask.armInputDeadline.
//
// # Concurrency
//
// One mutex for the whole table. Every operation under it is O(1) bookkeeping —
// append to a slice, pop from a slice — and the actual work of starting a turn
// (which spawns processes and waits for a dial-back) is ALWAYS run outside it,
// on a goroutine of its own. Holding this lock across a spawn would serialize
// every conversation in the broker behind one slow boot.

// a2aQueuedTurn is one task waiting for its conversation to be free, together
// with the closure that starts it.
//
// The closure is captured at enqueue time rather than reconstructed at promotion
// because everything it needs — the card, the decoded message, the caller — is in
// hand then and would otherwise have to be stored and threaded through.
type a2aQueuedTurn struct {
	task  *a2aTask
	begin func()
}

// a2aContextQueue is one conversation's serialization state.
type a2aContextQueue struct {
	// active is the task currently permitted to drive the instance. Nil means the
	// conversation is free.
	active *a2aTask
	// waiting is the FIFO of tasks admitted but not yet started, oldest first.
	// FIFO rather than any cleverer ordering: a client that sent two messages
	// expects them answered in the order it sent them.
	waiting []a2aQueuedTurn
}

// a2aContextQueues is the broker-wide table of per-conversation queues.
type a2aContextQueues struct {
	logger *slog.Logger

	mu     sync.Mutex
	queues map[string]*a2aContextQueue
	// stopped latches when the broker is shutting down, after which nothing is
	// promoted. See stop.
	stopped bool
}

func newA2AContextQueues(logger *slog.Logger) *a2aContextQueues {
	if logger == nil {
		logger = slog.Default()
	}
	return &a2aContextQueues{logger: logger, queues: make(map[string]*a2aContextQueue)}
}

// enter admits a task to its conversation's queue, reporting whether it may
// start NOW.
//
// True means the conversation was free and the caller owns starting the turn —
// synchronously, on the request goroutine, so a refusal reaches the client as an
// ordinary error response rather than as a task that fails later.
//
// False means the task is queued. Its begin closure will be run on a goroutine
// of its own when the task ahead of it settles, and until then the task stays in
// TASK_STATE_SUBMITTED with nothing sent to any instance.
func (q *a2aContextQueues) enter(key string, task *a2aTask, begin func()) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	entry, ok := q.queues[key]
	if !ok {
		entry = &a2aContextQueue{}
		q.queues[key] = entry
	}
	if entry.active == nil {
		entry.active = task
		return true
	}
	entry.waiting = append(entry.waiting, a2aQueuedTurn{task: task, begin: begin})
	q.logger.Debug("a2a task queued behind the conversation's active turn",
		"profile", task.profile, "task_id", task.taskID, "context_id", task.contextID,
		"ahead", len(entry.waiting))
	return false
}

// finish records that a task has settled and starts the next one, if any.
//
// It is called from the task's terminal sequence, so every way a turn can end
// reaches it without any caller having to remember. A task that settled while it
// was still WAITING — cancelled before it ever ran, or failed with its instance —
// is simply removed from the queue and promotes nobody, because it was never
// what the others were waiting for.
//
// The promoted turn is started on a NEW goroutine. That is required, not tidy:
// this runs inside the terminal sequence, which is usually on the gateway's
// instance read pump, and starting a turn can spawn a process and wait for it to
// dial back — up to the ready timeout. Doing that on the read pump would stall
// every other lease the broker is serving.
func (q *a2aContextQueues) finish(key string, task *a2aTask) {
	q.mu.Lock()

	entry, ok := q.queues[key]
	if !ok {
		q.mu.Unlock()
		return
	}

	if entry.active != task {
		// The task ended before it was ever promoted.
		for i, w := range entry.waiting {
			if w.task == task {
				entry.waiting = append(entry.waiting[:i:i], entry.waiting[i+1:]...)
				break
			}
		}
		q.dropIfIdleLocked(key, entry)
		q.mu.Unlock()
		return
	}

	entry.active = nil
	var promoted *a2aQueuedTurn
	for len(entry.waiting) > 0 {
		next := entry.waiting[0]
		entry.waiting = entry.waiting[1:]
		if next.task.terminated() {
			// Settled while it waited — by a cancellation, or by the broker
			// shutting down. Skip it rather than "starting" a finished task.
			continue
		}
		entry.active = next.task
		promoted = &next
		break
	}
	if q.stopped {
		// Shutting down: whatever was promoted must not be started. It is settled
		// by the same sweep that settled the task ahead of it.
		promoted = nil
		entry.active = nil
		entry.waiting = nil
	}
	q.dropIfIdleLocked(key, entry)
	q.mu.Unlock()

	if promoted == nil {
		return
	}
	q.logger.Debug("a2a conversation is free; starting the next queued task",
		"profile", promoted.task.profile, "task_id", promoted.task.taskID,
		"context_id", promoted.task.contextID)
	go promoted.begin()
}

// stop latches the table closed so that no further task is promoted.
//
// It exists for one race, and the race is real: settling live tasks at shutdown
// makes each of them terminal, which is exactly the event the queue advances on
// — so without this, the last act of a broker shutting down would be to spawn
// instances for the messages queued behind the tasks it was cancelling. The
// queued tasks are settled by the same sweep, so nothing is lost by refusing to
// start them.
func (q *a2aContextQueues) stop() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.stopped = true
}

// depth reports how many tasks are waiting behind the active one, and whether
// the conversation has an active task at all. It exists for logging and tests;
// nothing routes on it.
func (q *a2aContextQueues) depth(key string) (waiting int, active bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	entry, ok := q.queues[key]
	if !ok {
		return 0, false
	}
	return len(entry.waiting), entry.active != nil
}

// dropIfIdleLocked forgets a queue with nothing in it. Caller MUST hold q.mu.
//
// Without it the table would grow one entry per conversation the broker ever
// serves and never shrink — the same unbounded-key-space problem
// a2aLeaseManager caps its context table against, except that here there is
// nothing to remember once a conversation is quiet, so the entry can simply go.
func (q *a2aContextQueues) dropIfIdleLocked(key string, entry *a2aContextQueue) {
	if entry.active == nil && len(entry.waiting) == 0 {
		delete(q.queues, key)
	}
}
