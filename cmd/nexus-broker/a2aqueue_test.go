package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// This file covers the serialization of concurrent tasks on one conversation:
// that a second message waits, that the wait ends when the first task settles
// HOWEVER it settles, and that nothing can wedge the queue permanently.
//
// The instance below is deliberately shared across every task on a conversation,
// which is what a real lease is: one process, one agent loop. That is the whole
// reason the queue exists.

// queueTestIngress builds an A2A ingress over one scripted instance, with a
// durable task store under dir ("" for memory-only) and an input deadline of
// inputTimeout.
func queueTestIngress(t *testing.T, dir string, inputTimeout time.Duration) (*A2AServer, *conformInstance) {
	t.Helper()
	cfg := a2aTestConfig(t, "")
	cfg.StateDir = dir
	cfg.A2AInputTimeout = inputTimeout

	server, err := NewA2AServer(testLogger(), cfg)
	if err != nil {
		t.Fatalf("NewA2AServer: %v", err)
	}
	if dir != "" {
		server.useTaskStore(openTestTaskStore(t, dir, cfg.A2ATaskRetention))
	}
	instance := &conformInstance{}
	server.useLeaseProvider(&conformLeaseProvider{instance: instance})
	return server, instance
}

// startQueuedTask starts one task on a named context, as a named caller.
func startQueuedTask(t *testing.T, server *A2AServer, contextID, text string) (*a2aTask, *a2aStream) {
	t.Helper()
	card := server.card("support")
	if card == nil {
		t.Fatal("the test ingress has no support profile")
	}
	task, sub, _, protoErr := server.startTask(context.Background(), card, a2aTurnInput{
		contextID: contextID,
		text:      text,
		messageID: "m-" + text,
	}, nexusauth.Principal{})
	if protoErr != nil {
		t.Fatalf("startTask: %s", protoErr.Message)
	}
	t.Cleanup(func() { task.detach(sub) })
	return task, sub
}

// waitForState polls a task until it reaches one of the wanted states. Polling
// rather than watching the observer because a promoted task is started on a
// goroutine the queue owns, so the transition is genuinely asynchronous.
func waitForTaskState(t *testing.T, task *a2aTask, want ...a2a.TaskState) a2a.TaskState {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := task.snapshotTask().Status.State
		for _, w := range want {
			if got == w {
				return got
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s stayed at %s, want one of %v", task.taskID, got, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// inputsSent counts the `input` payloads the instance has been handed, which is
// the only thing that can start a turn.
func inputsSent(instance *conformInstance) int {
	n := 0
	for _, msg := range instance.sentMessages() {
		if msg.Type == ioTypeInput {
			n++
		}
	}
	return n
}

// TestConcurrentTasksOnOneContextQueueSerially is the acceptance criterion taken
// literally: the second task sits in SUBMITTED until the first is terminal, then
// moves to WORKING.
//
// The assertion that matters most is the one about the INSTANCE: exactly one
// `input` payload has been sent while the first turn is running. Two would mean
// two turns interleaving in one agent loop, which is the defect this queue
// exists to prevent — and it is invisible from the frames alone, because both
// tasks would happily report the same turn's output as their own.
func TestConcurrentTasksOnOneContextQueueSerially(t *testing.T) {
	server, instance := queueTestIngress(t, t.TempDir(), 0)

	first, _ := startQueuedTask(t, server, "ctx-1", "first")
	instance.deliver(brokerIOMessage{Type: ioTypeStreamDelta, Content: "thinking", TurnID: "t1"})
	waitForTaskState(t, first, a2a.TaskStateWorking)

	second, _ := startQueuedTask(t, server, "ctx-1", "second")

	if state := second.snapshotTask().Status.State; state != a2a.TaskStateSubmitted {
		t.Fatalf("the queued task is at %s, want SUBMITTED", state)
	}
	if got := inputsSent(instance); got != 1 {
		t.Fatalf("the instance was handed %d input payloads while a turn was running, want exactly 1", got)
	}
	if waiting, active := server.queues.depth(a2aContextKey("", "support", "ctx-1")); waiting != 1 || !active {
		t.Fatalf("queue depth = %d waiting / active %v, want 1 waiting behind an active task", waiting, active)
	}

	// The first turn ends. The second must then start on its own.
	instance.deliver(brokerIOMessage{Type: ioTypeOutput, Content: "the first answer", TurnID: "t1"})
	instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: ioStateIdle})
	waitForTaskState(t, first, a2a.TaskStateCompleted)

	waitForTaskState(t, second, a2a.TaskStateWorking)
	if got := inputsSent(instance); got != 2 {
		t.Fatalf("the instance was handed %d input payloads after the first turn ended, want 2", got)
	}

	// And the second task's turn is genuinely its own: a payload on a NEW turn
	// completes it with its own answer, not the first turn's.
	instance.deliver(brokerIOMessage{Type: ioTypeOutput, Content: "the second answer", TurnID: "t2"})
	instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: ioStateIdle})
	waitForTaskState(t, second, a2a.TaskStateCompleted)

	answer := second.snapshotTask()
	if len(answer.Artifacts) != 1 {
		t.Fatalf("the second task published %d artifacts, want 1", len(answer.Artifacts))
	}
	if text, _ := answer.Artifacts[0].Parts[0].TextValue(); text != "the second answer" {
		t.Errorf("the second task answered %q; it observed the first turn's output", text)
	}
	if first.boundTurn() == second.boundTurn() {
		t.Errorf("both tasks bound to turn %q; serialization did not separate the turns", first.boundTurn())
	}
}

// TestTasksOnDifferentContextsDoNotQueue: serialization is per CONVERSATION.
// Two conversations are two instances and two agent loops, so making them wait
// on each other would serialize the whole broker behind its slowest client.
func TestTasksOnDifferentContextsDoNotQueue(t *testing.T) {
	server, instance := queueTestIngress(t, "", 0)

	startQueuedTask(t, server, "ctx-1", "first")
	startQueuedTask(t, server, "ctx-2", "second")

	if got := inputsSent(instance); got != 2 {
		t.Fatalf("%d input payloads were sent, want 2: two conversations must not queue on each other", got)
	}
}

// TestTheQueueSurvivesAnInstanceReleasedMidQueue: the instance behind a
// conversation dies while a task is queued behind the active one.
//
// The queue advances on exactly one event — a task reaching a terminal state —
// and an instance going away is one of the ways that happens. So the active task
// fails, the queued one is promoted, and it acquires a fresh instance, which for
// a real broker means a re-spawn with -recall and a conversation that carries on.
func TestTheQueueSurvivesAnInstanceReleasedMidQueue(t *testing.T) {
	server, instance := queueTestIngress(t, t.TempDir(), 0)

	first, _ := startQueuedTask(t, server, "ctx-1", "first")
	instance.deliver(brokerIOMessage{Type: ioTypeStreamDelta, Content: "working", TurnID: "t1"})
	waitForTaskState(t, first, a2a.TaskStateWorking)

	second, _ := startQueuedTask(t, server, "ctx-1", "second")

	// The dial-back socket drops: the crash path, the idle release path and the
	// broker-shutdown path all arrive here.
	instance.gone("the agent instance stopped")

	if state := waitForTaskState(t, first, a2a.TaskStateFailed); state != a2a.TaskStateFailed {
		t.Fatalf("the active task settled at %s, want FAILED", state)
	}
	waitForTaskState(t, second, a2a.TaskStateWorking)
	if got := inputsSent(instance); got != 2 {
		t.Errorf("%d input payloads were sent; the queued task did not start after the instance went away", got)
	}
}

// TestCancellingAQueuedTaskPromotesTheNext: a task cancelled while it was still
// waiting must leave the queue without disturbing what is ahead of it, and the
// task behind it must still run.
//
// Cancelling a queued task also exercises the one case where there is no
// instance to tell: nothing was ever sent, so there is nothing to stop, and that
// must not be reported as a failure.
func TestCancellingAQueuedTaskPromotesTheNext(t *testing.T) {
	server, instance := queueTestIngress(t, t.TempDir(), 0)

	first, _ := startQueuedTask(t, server, "ctx-1", "first")
	instance.deliver(brokerIOMessage{Type: ioTypeStreamDelta, Content: "working", TurnID: "t1"})
	waitForTaskState(t, first, a2a.TaskStateWorking)

	second, _ := startQueuedTask(t, server, "ctx-1", "second")
	third, _ := startQueuedTask(t, server, "ctx-1", "third")

	if _, protoErr := server.cancelTask(nexusauth.Principal{}, "support", second.taskID); protoErr != nil {
		t.Fatalf("cancelling a queued task: %s", protoErr.Message)
	}
	if state := second.snapshotTask().Status.State; state != a2a.TaskStateCanceled {
		t.Fatalf("the cancelled task is at %s, want CANCELED", state)
	}
	// Still exactly one turn in flight: cancelling a WAITING task must not have
	// started anything.
	if got := inputsSent(instance); got != 1 {
		t.Fatalf("%d input payloads were sent after cancelling a queued task, want 1", got)
	}

	instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: ioStateIdle})
	waitForTaskState(t, first, a2a.TaskStateCompleted)
	// The cancelled task is skipped and the one behind it runs.
	waitForTaskState(t, third, a2a.TaskStateWorking)
	if got := inputsSent(instance); got != 2 {
		t.Errorf("%d input payloads were sent, want 2: the task behind the cancelled one did not start", got)
	}
}

// TestTheQueueDoesNotDeadlockOnAnUnansweredQuestion is the explicit
// deadlock policy, tested.
//
// A task at INPUT_REQUIRED is not terminal and deliberately keeps the queue: the
// agent loop is blocked inside ask_user, so starting the next turn would send
// input to an instance that cannot read it. What stops that being a deadlock is
// a2a.tasks.input_timeout — the parked task is FAILED, which is terminal, which
// advances the queue — and the instance is told to cancel the turn so its loop
// unblocks.
func TestTheQueueDoesNotDeadlockOnAnUnansweredQuestion(t *testing.T) {
	server, instance := queueTestIngress(t, t.TempDir(), 20*time.Millisecond)

	first, _ := startQueuedTask(t, server, "ctx-1", "first")
	instance.deliver(brokerIOMessage{
		Type:      ioTypeHITLRequest,
		TurnID:    "t1",
		RequestID: "req-1",
		Prompt:    "which environment?",
	})
	waitForTaskState(t, first, a2a.TaskStateInputRequired)

	second, _ := startQueuedTask(t, server, "ctx-1", "second")
	if state := second.snapshotTask().Status.State; state != a2a.TaskStateSubmitted {
		t.Fatalf("a task queued behind a parked one is at %s, want SUBMITTED", state)
	}

	// Nobody answers. The deadline expires and the queue moves.
	waitForTaskState(t, first, a2a.TaskStateFailed)
	waitForTaskState(t, second, a2a.TaskStateWorking)

	if msg := first.snapshotTask().Status.Message; msg == nil {
		t.Error("the abandoned task carries no status message saying why")
	} else if text, _ := msg.Parts[0].TextValue(); !strings.Contains(text, "no answer arrived") {
		t.Errorf("status message = %q, want it to name the unanswered question", text)
	}

	// The instance is told to stop waiting, or its agent loop would stay blocked
	// inside ask_user for the life of the process.
	var canceled bool
	for _, msg := range instance.sentMessages() {
		if msg.Type == ioTypeCancel && msg.TurnID == "t1" {
			canceled = true
		}
	}
	if !canceled {
		t.Errorf("the instance was never told to cancel the abandoned turn: %+v", instance.sentMessages())
	}
}

// TestAnAnsweredQuestionDisarmsTheDeadline: the deadline must not fire on a task
// whose answer arrived, and a client that takes its time must not be punished
// for it after the fact.
func TestAnAnsweredQuestionDisarmsTheDeadline(t *testing.T) {
	server, instance := queueTestIngress(t, t.TempDir(), 40*time.Millisecond)
	card := server.card("support")

	task, _ := startQueuedTask(t, server, "ctx-1", "first")
	instance.deliver(brokerIOMessage{
		Type: ioTypeHITLRequest, TurnID: "t1", RequestID: "req-1", Prompt: "which environment?",
	})
	waitForTaskState(t, task, a2a.TaskStateInputRequired)

	resumed, sub, _, protoErr := server.resumeTask(card, a2aTurnInput{
		taskID: task.taskID, contextID: "ctx-1", text: "staging", messageID: "m-answer",
	}, nexusauth.Principal{})
	if protoErr != nil {
		t.Fatalf("resumeTask: %s", protoErr.Message)
	}
	defer resumed.detach(sub)

	// Well past the deadline the parked task was armed with.
	time.Sleep(120 * time.Millisecond)
	if state := task.snapshotTask().Status.State; state != a2a.TaskStateWorking {
		t.Fatalf("the answered task is at %s, want WORKING: the deadline fired after the answer", state)
	}

	// And the answer is in the durable record, so a task read back says what it
	// was told as well as what it said.
	rec, found := server.store.For(nexusauth.Principal{}, "support").Get(task.taskID)
	if !found {
		t.Fatal("the task is not in the store")
	}
	var sawAnswer bool
	for _, msg := range rec.History {
		if msg.Text == "staging" {
			sawAnswer = true
		}
	}
	if !sawAnswer {
		t.Errorf("the answer was not recorded in the task history: %+v", rec.History)
	}
}

// TestConcurrentStartsOnOneContextAreSerialized hammers the queue from several
// goroutines at once. It is the -race assertion for this file: the queue table,
// the live registry and the store are all touched concurrently, and exactly one
// turn may reach the instance.
func TestConcurrentStartsOnOneContextAreSerialized(t *testing.T) {
	server, instance := queueTestIngress(t, t.TempDir(), 0)
	card := server.card("support")

	const callers = 8
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		tasks []*a2aTask
	)
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			task, sub, _, protoErr := server.startTask(context.Background(), card, a2aTurnInput{
				contextID: "ctx-1",
				text:      "message",
				messageID: "m",
			}, nexusauth.Principal{})
			if protoErr != nil {
				t.Errorf("startTask: %s", protoErr.Message)
				return
			}
			mu.Lock()
			tasks = append(tasks, task)
			mu.Unlock()
			task.detach(sub)
		}(i)
	}
	wg.Wait()

	if len(tasks) != callers {
		t.Fatalf("%d tasks were created, want %d", len(tasks), callers)
	}
	if got := inputsSent(instance); got != 1 {
		t.Fatalf("%d input payloads reached the instance, want exactly 1", got)
	}

	// Every task is readable, and exactly one of them is not SUBMITTED.
	view := server.store.For(nexusauth.Principal{}, "support")
	started := 0
	for _, task := range tasks {
		rec, found := view.Get(task.taskID)
		if !found {
			t.Fatalf("task %s is not in the store", task.taskID)
		}
		if rec.State != string(a2a.TaskStateSubmitted) {
			started++
		}
	}
	if started > 1 {
		t.Errorf("%d tasks left SUBMITTED at once, want at most 1", started)
	}

	// Drain the queue: each turn ends and the next starts, until all are done.
	for range callers {
		instance.deliver(brokerIOMessage{Type: ioTypeStreamDelta, Content: "ok", TurnID: "t"})
		instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: ioStateIdle})
		time.Sleep(5 * time.Millisecond)
	}
	for _, task := range tasks {
		waitForTaskState(t, task, a2a.TaskStateCompleted, a2a.TaskStateFailed, a2a.TaskStateCanceled)
	}
	if waiting, active := server.queues.depth(a2aContextKey("", "support", "ctx-1")); waiting != 0 || active {
		t.Errorf("the queue still holds %d waiting / active %v after every task settled", waiting, active)
	}
}

// TestShutdownDoesNotStartQueuedTurns closes a race that only a shutdown can
// produce: settling every live task makes each of them terminal, which is the
// event the queue advances on — so without the queues being closed first, the
// last act of a broker shutting down would be to spawn instances for the
// messages queued behind the tasks it was cancelling.
func TestShutdownDoesNotStartQueuedTurns(t *testing.T) {
	server, instance := queueTestIngress(t, t.TempDir(), 0)

	first, _ := startQueuedTask(t, server, "ctx-1", "first")
	instance.deliver(brokerIOMessage{Type: ioTypeStreamDelta, Content: "working", TurnID: "t1"})
	waitForTaskState(t, first, a2a.TaskStateWorking)

	second, _ := startQueuedTask(t, server, "ctx-1", "second")
	third, _ := startQueuedTask(t, server, "ctx-1", "third")

	server.Shutdown()

	// Every task is settled, and no client is left holding a stream the broker
	// will never write to again.
	for _, task := range []*a2aTask{first, second, third} {
		waitForTaskState(t, task, a2a.TaskStateFailed)
	}
	// Give any errant promotion goroutine a chance to run before asserting.
	time.Sleep(30 * time.Millisecond)
	if got := inputsSent(instance); got != 1 {
		t.Errorf("%d input payloads reached the instance, want 1: shutdown started a queued turn", got)
	}
	if waiting, active := server.queues.depth(a2aContextKey("", "support", "ctx-1")); waiting != 0 || active {
		t.Errorf("the queue still holds %d waiting / active %v after shutdown", waiting, active)
	}
}
