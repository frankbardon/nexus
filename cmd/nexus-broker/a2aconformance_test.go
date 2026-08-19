package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/a2a/a2aconform"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// This file is the broker ingress's side of the shared A2A conformance corpus.
//
// The corpus (pkg/a2a/a2aconform) describes A2A OUTPUT only: it names abstract
// steps ("the agent produced final text", "the agent asked the human a
// question") and pins the exact frame sequence each must produce. nexus.io.a2a
// realizes those steps as ENGINE BUS events; this driver realizes the same
// steps as INSTANCE IO PAYLOADS, which is the only vocabulary a broker has. The
// two mappings share no translation code at all, so the corpus is the only
// thing that keeps them producing the same stream.
//
// The driver drives the ingress through its real surfaces — startTask,
// resumeTask, cancelTask, and the a2aInstanceHooks a lease provider is handed —
// and reads frames off the same attached observer the SSE pump drains, one
// layer below the socket. A vector asserts a frame sequence, and the socket
// contributes nothing to it; the wire is covered by a2aturn_test.go.

// TestA2AConformance replays the whole shared corpus against the broker's
// mapping.
func TestA2AConformance(t *testing.T) {
	a2aconform.Run(t, brokerConformDriver{})
}

// brokerConformDriver is the a2aconform.Driver for the broker's A2A ingress.
type brokerConformDriver struct{}

func (brokerConformDriver) Name() string { return "cmd/nexus-broker a2a ingress" }

// Features declares what this mapping can express — and, just as importantly,
// what it cannot.
//
// PRESENT, each backed by the code the matching vector exercises:
//
//   - turn: an `input` payload starts a turn; stream deltas and `output` carry
//     its text; `status: idle` ends it.
//   - failure: the instance going away settles the task at FAILED. This is the
//     ONLY failure signal a broker has — core.error is not forwarded by
//     nexus.io.broker, so no payload carries an error reason — and it is a real
//     path, not a test affordance: it is what a crashed instance does.
//   - cancel: CancelTask settles the task and forwards `cancel`; an
//     instance-initiated `cancel.complete` settles it too.
//   - hitl: `hitl.request` parks the task at INPUT_REQUIRED carrying the
//     question, and a resuming message sends `hitl.response`.
//
// ABSENT, and the reason is the envelope rather than the effort:
//
//   - tool_artifacts: the IO envelope carries NO tool results. nexus.io.broker
//     does not subscribe to tool.invoke or tool.result and ioMessage has no
//     field for either, so there is nothing to publish an artifact from. This
//     also skips streaming-order-interleaves, which needs a tool result to
//     interleave with.
//   - file_artifacts: files a turn wrote are reported on events.ToolResult,
//     which is the same absence.
//   - artifact_budget: the only artifact this mapping mints is the turn's own
//     answer, which is never charged against a budget, so there is no budget to
//     have.
//
// Declaring an absent feature to make a vector run would be worse than the
// skip: the skip is reported, and a mapping that claimed tool artifacts it
// cannot produce would pass a vector by lying about its transport.
func (brokerConformDriver) Features() []a2aconform.Feature {
	return []a2aconform.Feature{
		a2aconform.FeatureTurn,
		a2aconform.FeatureFailure,
		a2aconform.FeatureCancel,
		a2aconform.FeatureHITL,
	}
}

// Begin builds one ingress per vector over a fake instance, and deliberately
// does not start a task: the vector's first step does that, so the opening
// frame is produced by the mapping.
func (brokerConformDriver) Begin(t *testing.T, v a2aconform.Vector, env a2aconform.Env) (a2aconform.Session, error) {
	server, instance := newConformIngress(t)
	s := &brokerConformSession{server: server, instance: instance, vector: v}
	t.Cleanup(s.Close)
	return s, nil
}

// newConformIngress builds an A2A ingress with one profile, backed by a lease
// provider that hands out one scripted instance.
//
// The instance is fake in exactly one respect — no process is spawned — and
// real in the respect that matters: it speaks the same brokerIOMessage envelope
// a live nexus.io.broker plugin speaks, and its replies reach the mapping
// through the same a2aInstanceHooks a live provider wires.
func newConformIngress(t *testing.T) (*A2AServer, *conformInstance) {
	t.Helper()
	cfg := a2aTestConfig(t, "")
	server, err := NewA2AServer(testLogger(), cfg)
	if err != nil {
		t.Fatalf("NewA2AServer: %v", err)
	}
	instance := &conformInstance{}
	server.useLeaseProvider(&conformLeaseProvider{instance: instance})
	return server, instance
}

// conformLeaseProvider hands every turn the same scripted instance.
type conformLeaseProvider struct{ instance *conformInstance }

func (p *conformLeaseProvider) Acquire(_ context.Context, req a2aLeaseRequest) (a2aInstance, error) {
	p.instance.bind(req.hooks)
	return p.instance, nil
}

// conformInstance is the leased instance a vector scripts.
//
// Outbound payloads are recorded so a test can assert what the instance was
// actually told; inbound ones are pushed through the hooks the mapping
// registered, which is exactly the path the gateway's instance read pump takes.
type conformInstance struct {
	mu       sync.Mutex
	hooks    a2aInstanceHooks
	sent     []brokerIOMessage
	released bool
	// sendErr, when set, makes every SendIO fail, so the "the message never
	// reached the agent" branch is reachable from a test.
	sendErr error
	// onInput, when set, is called after an `input` payload is accepted. It is
	// how a wire-level test scripts a turn from ANOTHER goroutine, which is what
	// puts the frame producer and the response writer on different goroutines
	// for the race detector to judge.
	onInput func()
}

func (i *conformInstance) bind(h a2aInstanceHooks) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.hooks = h
}

func (i *conformInstance) SendIO(msg brokerIOMessage) error {
	i.mu.Lock()
	if i.sendErr != nil {
		err := i.sendErr
		i.mu.Unlock()
		return err
	}
	i.sent = append(i.sent, msg)
	onInput := i.onInput
	i.mu.Unlock()

	if msg.Type == ioTypeInput && onInput != nil {
		onInput()
	}
	return nil
}

func (i *conformInstance) Release() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.released = true
}

// deliver pushes one payload at the mapping, exactly as the gateway's instance
// read pump would. It is synchronous, which is what lets a driver step return
// with every frame it caused already visible.
func (i *conformInstance) deliver(msg brokerIOMessage) {
	i.mu.Lock()
	deliver := i.hooks.Deliver
	i.mu.Unlock()
	if deliver != nil {
		deliver(msg)
	}
}

// gone reports the instance as no longer reachable, the way a dropped dial-back
// socket does.
func (i *conformInstance) gone(reason string) {
	i.mu.Lock()
	gone := i.hooks.Gone
	i.mu.Unlock()
	if gone != nil {
		gone(reason)
	}
}

// sentMessages returns a copy of everything the instance was told.
func (i *conformInstance) sentMessages() []brokerIOMessage {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]brokerIOMessage(nil), i.sent...)
}

// wasReleased reports whether the lease was released.
func (i *conformInstance) wasReleased() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.released
}

// brokerConformSession is one vector's replay against this mapping.
//
// Every driving step either calls an ingress entry point or pushes an IO
// payload, and both are synchronous: the mapping enqueues its frames inside the
// call, so by the time Apply returns every frame the step caused is already in
// the observer's buffer and Frames can see it.
type brokerConformSession struct {
	server   *A2AServer
	instance *conformInstance
	vector   a2aconform.Vector

	task *a2aTask
	sub  *a2aStream
	// frames accumulates what has been drained off the observer, opening
	// snapshot first. It is accumulated rather than re-read because the channel
	// is consuming.
	frames []a2a.StreamResponse
	// turnID is the turn the vector bound to, echoed onto every payload that
	// carries one — which is how a broker anchors a task to a Nexus turn.
	turnID string
}

// Apply realizes one abstract step in the instance IO envelope's vocabulary.
func (s *brokerConformSession) Apply(step a2aconform.Step) error {
	switch step.Kind {
	case a2aconform.StepMessage:
		return s.start(step)

	case a2aconform.StepTurnStart:
		// The envelope carries no turn-start payload: agent.turn.start is not
		// forwarded. What a real instance DOES send as its first sign of life is
		// the io.status every shipped agent loop emits when it picks the input
		// up — plugins/agents/react emits exactly this — and that is what moves
		// the task to WORKING.
		//
		// It carries NO turn id, deliberately: events.StatusUpdate has no TurnID
		// field, so nexus.io.broker cannot put one on a status payload. The task
		// binds its turn from the first payload that genuinely carries one,
		// which is a delta, an output or a question.
		s.turnID = step.TurnID
		return s.deliver(brokerIOMessage{
			Type:   ioTypeStatus,
			State:  "thinking",
			Detail: "Processing input",
		})

	case a2aconform.StepAgentText:
		// The model producing its final text reaches a broker as streamed
		// deltas followed by the stream ending. Neither completes the task —
		// which is the deviation the corpus's assert_active step pins.
		if err := s.deliver(brokerIOMessage{
			Type:    ioTypeStreamDelta,
			Content: step.Text,
			TurnID:  s.turnID,
		}); err != nil {
			return err
		}
		return s.deliver(brokerIOMessage{
			Type:         ioTypeStreamEnd,
			TurnID:       s.turnID,
			FinishReason: "end_turn",
		})

	case a2aconform.StepOutput:
		return s.deliver(brokerIOMessage{
			Type:    ioTypeOutput,
			Content: step.Text,
			Role:    "assistant",
			TurnID:  s.turnID,
		})

	case a2aconform.StepAskUser:
		return s.deliver(brokerIOMessage{
			Type:      ioTypeHITLRequest,
			RequestID: step.RequestID,
			Prompt:    step.Prompt,
			Mode:      conformHITLMode(step.Choices),
			Choices:   conformChoices(step.Choices),
			TurnID:    s.turnID,
		})

	case a2aconform.StepAnswer:
		return s.answer(step)

	case a2aconform.StepTurnEnd:
		// io.status "idle" is the ONLY end-of-turn signal the envelope carries;
		// agent.turn.end is not forwarded. Every shipped agent loop emits it.
		return s.deliver(brokerIOMessage{Type: ioTypeStatus, State: ioStateIdle})

	case a2aconform.StepFailure:
		// A broker learns a turn failed by its instance going away: no payload
		// carries an error, because nexus.io.broker does not forward core.error.
		// This is the crash path, driven with the reason a crash would supply.
		if s.task == nil {
			return errors.New("the vector failed a turn before any task existed")
		}
		s.instance.gone(step.Reason)
		return nil

	case a2aconform.StepCancel:
		return s.cancel()

	case a2aconform.StepThinking:
		return errors.New("the broker io envelope carries no reasoning steps: " +
			"nexus.io.broker does not forward thinking.step")

	case a2aconform.StepToolCall, a2aconform.StepToolResult:
		return errors.New("the broker io envelope carries no tool activity: " +
			"nexus.io.broker forwards neither tool.invoke nor tool.result, which is why " +
			"this driver does not declare the tool_artifacts feature")

	default:
		return fmt.Errorf("the broker a2a ingress cannot realize step kind %q", step.Kind)
	}
}

// start creates the task, exactly as an inbound SendMessage does.
func (s *brokerConformSession) start(step a2aconform.Step) error {
	if s.task != nil {
		return errors.New("the vector started a second task")
	}
	card, ok := s.server.cards["support"]
	if !ok {
		return errors.New("the test ingress has no support profile")
	}
	task, sub, opening, protoErr := s.server.startTask(context.Background(), card, a2aTurnInput{
		contextID: step.ContextID,
		text:      step.Text,
		messageID: step.MessageID,
	}, nexusauth.Principal{})
	if protoErr != nil {
		return fmt.Errorf("startTask refused the message: %s", protoErr.Message)
	}
	s.task, s.sub = task, sub
	// The opening Task snapshot is the first thing an A2A stream carries
	// (specification section 11.7); the SSE pump writes exactly this.
	s.frames = append(s.frames, a2a.StreamTask(opening))
	return nil
}

// answer resumes an interrupted task the way A2A defines it: a new message
// naming the same taskId and contextId (specification section 3.4).
//
// The observer that call attaches is dropped immediately. A real client
// resuming over HTTP wants the rest of the turn on its own connection; here the
// vector is already watching the stream the creating message opened, and a
// second observer would be a second copy of the same frames.
func (s *brokerConformSession) answer(step a2aconform.Step) error {
	if s.task == nil {
		return errors.New("the vector answered a question before any task existed")
	}
	card := s.server.cards["support"]
	_, sub, _, protoErr := s.server.resumeTask(card, a2aTurnInput{
		taskID:    s.task.taskID,
		contextID: s.task.contextID,
		text:      step.Text,
		messageID: step.MessageID,
	}, nexusauth.Principal{})
	if protoErr != nil {
		return fmt.Errorf("resumeTask refused the answer: %s", protoErr.Message)
	}
	s.task.detach(sub)
	return nil
}

// cancel settles the task through the ingress's CancelTask path.
//
// The vector's reason is deliberately not passed through: A2A's CancelTask
// carries no client reason, so this mapping states its own — exactly as
// nexus.io.a2a does. The vector asserts only that the status message says the
// task was canceled, which is the part a client can rely on.
func (s *brokerConformSession) cancel() error {
	if s.task == nil {
		return errors.New("the vector canceled a task that was never created")
	}
	if _, protoErr := s.server.cancelTask(nexusauth.Principal{}, s.task.taskID); protoErr != nil {
		return fmt.Errorf("cancelTask refused: %s", protoErr.Message)
	}
	return nil
}

// deliver pushes one payload from the instance into the mapping.
func (s *brokerConformSession) deliver(msg brokerIOMessage) error {
	if s.task == nil {
		return fmt.Errorf("the vector delivered a %s payload before any task existed", msg.Type)
	}
	s.instance.deliver(msg)
	return nil
}

// Frames drains whatever the task has queued and returns the canonical stream
// so far, opening snapshot included.
func (s *brokerConformSession) Frames() []a2a.StreamResponse {
	if s.sub != nil {
		for draining := true; draining; {
			select {
			case frame := <-s.sub.frames:
				s.frames = append(s.frames, frame)
			default:
				draining = false
			}
		}
	}
	return append([]a2a.StreamResponse(nil), s.frames...)
}

// Task returns the mapping's own task snapshot: the same fold of the same
// frames a blocking SendMessage answers with.
func (s *brokerConformSession) Task() a2a.Task {
	if s.task == nil {
		return a2a.Task{}
	}
	return s.task.snapshotTask()
}

// Close detaches the observer. It is safe on a task that never terminated, and
// safe to call twice.
func (s *brokerConformSession) Close() {
	if s.task != nil && s.sub != nil {
		s.task.detach(s.sub)
		s.sub = nil
	}
}

// conformChoices maps a vector's multiple-choice options onto the IO envelope.
func conformChoices(in []a2aconform.StepChoice) []brokerIOChoice {
	if len(in) == 0 {
		return nil
	}
	out := make([]brokerIOChoice, 0, len(in))
	for _, c := range in {
		out = append(out, brokerIOChoice{ID: c.ID, Label: c.Label})
	}
	return out
}

// conformHITLMode spells the mode nexus.io.broker sends alongside the choices.
func conformHITLMode(choices []a2aconform.StepChoice) string {
	if len(choices) == 0 {
		return "free_text"
	}
	return "choices"
}
