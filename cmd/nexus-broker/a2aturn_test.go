package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// The conformance corpus (a2aconformance_test.go) judges the FRAMES this
// mapping produces. This file covers what a corpus of frame sequences cannot:
// what the broker sends BACK to the instance, the refusals a client meets, the
// wire the frames travel over, and the failure modes — a crashed instance, an
// unreachable one, a broker shutting down — that must never leave a stream
// hanging.

// startConformTask starts one task on a scripted instance and returns it with
// an attached observer. It is the shared setup for every test below and for the
// leniency test in a2aio_contract_test.go.
func startConformTask(t *testing.T, server *A2AServer, _ *conformInstance, text string) (*a2aTask, *a2aStream) {
	t.Helper()
	card := server.card("support")
	if card == nil {
		t.Fatal("the test ingress has no support profile")
	}
	task, sub, _, protoErr := server.startTask(context.Background(), card, a2aTurnInput{
		text:      text,
		messageID: "m-1",
	}, nexusauth.Principal{})
	if protoErr != nil {
		t.Fatalf("startTask: %s", protoErr.Message)
	}
	return task, sub
}

// TestInboundMessageBecomesAnInputPayload is the inbound half of the mapping:
// an A2A message must reach the instance as the `input` payload its
// nexus.io.broker plugin turns into io.input, and as nothing else.
func TestInboundMessageBecomesAnInputPayload(t *testing.T) {
	server, instance := newConformIngress(t)
	task, sub := startConformTask(t, server, instance, "what is the answer?")
	defer task.detach(sub)

	sent := instance.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("the instance was sent %d payload(s), want exactly 1: %+v", len(sent), sent)
	}
	if sent[0].Type != ioTypeInput {
		t.Errorf("payload type = %q, want %q", sent[0].Type, ioTypeInput)
	}
	if sent[0].Content != "what is the answer?" {
		t.Errorf("payload content = %q, want the message text", sent[0].Content)
	}
	// The opening snapshot is SUBMITTED: nothing has come back yet.
	if state := task.snapshotTask().Status.State; state != a2a.TaskStateSubmitted {
		t.Errorf("opening state = %s, want SUBMITTED", state)
	}
}

// TestInputPayloadRidesTheBrokerframeEnvelopeUnchanged proves the claim that
// this story needs no wire change: the payload the A2A ingress sends is carried
// by exactly the SignalIO frame the gateway has always forwarded, at the
// version it has always stamped.
func TestInputPayloadRidesTheBrokerframeEnvelopeUnchanged(t *testing.T) {
	data, err := encodeIOFrame("lease-1", brokerIOMessage{Type: ioTypeInput, Content: "hi"})
	if err != nil {
		t.Fatalf("encodeIOFrame: %v", err)
	}
	var envelope struct {
		Version   int             `json:"version"`
		LeaseID   string          `json:"lease_id"`
		Signal    string          `json:"signal"`
		Secret    string          `json:"secret"`
		SessionID string          `json:"session_id"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("the encoded frame is not JSON: %v", err)
	}
	if envelope.Version != 1 {
		t.Errorf("frame version = %d, want 1: this story must not bump the broker wire version", envelope.Version)
	}
	if envelope.Signal != "io" {
		t.Errorf("signal = %q, want %q", envelope.Signal, "io")
	}
	if envelope.LeaseID != "lease-1" {
		t.Errorf("lease_id = %q, want lease-1", envelope.LeaseID)
	}
	// The spawn secret must never appear on an IO frame: the gateway forwards
	// these verbatim to whatever is on the other end.
	if envelope.Secret != "" {
		t.Errorf("an io frame carried a secret (%q); it must never be populated on one", envelope.Secret)
	}
	var payload brokerIOMessage
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("payload is not an io message: %v", err)
	}
	if payload.Type != ioTypeInput || payload.Content != "hi" {
		t.Errorf("payload = %+v, want the input message", payload)
	}
}

// TestAnswerBecomesAHITLResponsePayload is the resume half: an A2A message
// naming a parked task must reach the instance as `hitl.response` correlated to
// the question, with a recognized option resolved to its canonical id.
func TestAnswerBecomesAHITLResponsePayload(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answer   string
		wantID   string
		wantFree string
	}{
		{name: "an option id", answer: "staging", wantID: "staging"},
		{name: "an option id in another case", answer: "  STAGING ", wantID: "staging"},
		{name: "free text", answer: "somewhere else", wantFree: "somewhere else"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, instance := newConformIngress(t)
			task, sub := startConformTask(t, server, instance, "deploy")
			defer task.detach(sub)

			instance.deliver(brokerIOMessage{
				Type: ioTypeHITLRequest, RequestID: "q-1", Prompt: "Where?", TurnID: "t1",
				Choices: []brokerIOChoice{{ID: "staging", Label: "Staging"}, {ID: "production"}},
			})
			if state := task.snapshotTask().Status.State; state != a2a.TaskStateInputRequired {
				t.Fatalf("state = %s, want INPUT_REQUIRED", state)
			}

			card := server.card("support")
			_, resumeSub, _, protoErr := server.resumeTask(card, a2aTurnInput{
				taskID:    task.taskID,
				contextID: task.contextID,
				text:      tc.answer,
			}, nexusauth.Principal{})
			if protoErr != nil {
				t.Fatalf("resumeTask: %s", protoErr.Message)
			}
			task.detach(resumeSub)

			sent := instance.sentMessages()
			last := sent[len(sent)-1]
			if last.Type != ioTypeHITLResponse {
				t.Fatalf("payload type = %q, want %q", last.Type, ioTypeHITLResponse)
			}
			if last.RequestID != "q-1" {
				t.Errorf("request_id = %q, want q-1", last.RequestID)
			}
			if last.ChoiceID != tc.wantID {
				t.Errorf("choice_id = %q, want %q", last.ChoiceID, tc.wantID)
			}
			if last.FreeText != tc.wantFree {
				t.Errorf("free_text = %q, want %q", last.FreeText, tc.wantFree)
			}
		})
	}
}

// TestParkedQuestionCarriesItsChoices pins the reason nexus.io.broker gained a
// choices field: without it the INPUT_REQUIRED message names a question whose
// options the client cannot see, so it cannot answer by id at all.
func TestParkedQuestionCarriesItsChoices(t *testing.T) {
	server, instance := newConformIngress(t)
	task, sub := startConformTask(t, server, instance, "deploy")
	defer task.detach(sub)

	instance.deliver(brokerIOMessage{
		Type: ioTypeHITLRequest, RequestID: "q-1", Prompt: "Which environment?", TurnID: "t1",
		Mode:    "choices",
		Choices: []brokerIOChoice{{ID: "staging", Label: "Staging"}, {ID: "production", Label: "Production"}},
	})

	status := task.snapshotTask().Status
	if status.Message == nil {
		t.Fatal("the INPUT_REQUIRED status carries no message")
	}
	var text strings.Builder
	for _, p := range status.Message.Parts {
		if v, ok := p.TextValue(); ok {
			text.WriteString(v)
		}
	}
	for _, want := range []string{"Which environment?", "staging", "Staging", "production", "Production"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("the question text %q does not name %q", text.String(), want)
		}
	}
	if got := status.Message.Metadata[a2aMetadataHITLRequestID]; got != "q-1" {
		t.Errorf("metadata[%s] = %v, want q-1", a2aMetadataHITLRequestID, got)
	}
}

// TestCancelSettlesAndTellsTheInstance covers the client-initiated half of
// cancellation: the task settles synchronously and the instance is told
// afterwards, naming the turn it must stop.
func TestCancelSettlesAndTellsTheInstance(t *testing.T) {
	server, instance := newConformIngress(t)
	task, sub := startConformTask(t, server, instance, "reindex")
	defer task.detach(sub)

	instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: "thinking"})
	// The turn id is bound from a payload that genuinely carries one:
	// events.StatusUpdate has no TurnID field, so a status payload never does.
	instance.deliver(brokerIOMessage{Type: ioTypeStreamDelta, Content: "working", TurnID: "turn-7"})

	settled, protoErr := server.cancelTask(nexusauth.Principal{}, "support", task.taskID)
	if protoErr != nil {
		t.Fatalf("cancelTask: %s", protoErr.Message)
	}
	if settled.Status.State != a2a.TaskStateCanceled {
		t.Errorf("state = %s, want CANCELED", settled.Status.State)
	}

	sent := instance.sentMessages()
	last := sent[len(sent)-1]
	if last.Type != ioTypeCancel {
		t.Fatalf("payload type = %q, want %q", last.Type, ioTypeCancel)
	}
	if last.TurnID != "turn-7" {
		t.Errorf("cancel names turn %q, want turn-7: the task binds to the turn its payloads carry", last.TurnID)
	}
	if last.Source != ioCancelSource {
		t.Errorf("cancel source = %q, want %q", last.Source, ioCancelSource)
	}

	// A second cancel is refused rather than rewriting a terminal state, and the
	// task is no longer live.
	if _, protoErr := server.cancelTask(nexusauth.Principal{}, "support", task.taskID); protoErr == nil {
		t.Error("cancelling a settled task was accepted; want TaskNotCancelableError")
	}
	if !instance.wasReleased() {
		t.Error("the lease was not released when the task settled")
	}
}

// TestInstanceInitiatedCancelCompleteSettlesTheTask covers the other half: a
// cancellation this ingress did not ask for — a /cancel typed into another
// transport, a gate stopping the turn — still reaches the A2A client as the
// terminal state it is, rather than as a stream that stops.
func TestInstanceInitiatedCancelCompleteSettlesTheTask(t *testing.T) {
	server, instance := newConformIngress(t)
	task, sub := startConformTask(t, server, instance, "reindex")
	defer task.detach(sub)

	instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: "thinking"})
	instance.deliver(brokerIOMessage{Type: ioTypeStreamDelta, Content: "working", TurnID: "turn-7"})
	resumable := true
	instance.deliver(brokerIOMessage{Type: ioTypeCancelComplete, TurnID: "turn-7", Resumable: &resumable})

	snapshot := task.snapshotTask()
	if snapshot.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("state = %s, want CANCELED", snapshot.Status.State)
	}
	// No `cancel` was sent back: the instance is the one that cancelled.
	for _, msg := range instance.sentMessages() {
		if msg.Type == ioTypeCancel {
			t.Error("the broker echoed a cancel for a cancellation the instance originated")
		}
	}
}

// TestInstanceCrashFailsTheTask is the crash contract: an instance that goes
// away mid-turn produces a FAILED task carrying a reason, and the stream that
// was following it CLOSES rather than hanging.
func TestInstanceCrashFailsTheTask(t *testing.T) {
	server, instance := newConformIngress(t)
	task, sub := startConformTask(t, server, instance, "summarize")

	instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: "thinking"})
	instance.gone("the agent instance exited with status 2")

	snapshot := task.snapshotTask()
	if snapshot.Status.State != a2a.TaskStateFailed {
		t.Fatalf("state = %s, want FAILED", snapshot.Status.State)
	}
	if msg := snapshot.Status.Message; msg == nil {
		t.Fatal("FAILED carries no message; a client would learn nothing about why")
	} else {
		var text strings.Builder
		for _, p := range msg.Parts {
			if v, ok := p.TextValue(); ok {
				text.WriteString(v)
			}
		}
		if !strings.Contains(text.String(), "exited with status 2") {
			t.Errorf("failure message = %q, want it to name the cause", text.String())
		}
	}

	// The frames a follower saw end at the terminal state and nothing follows.
	frames := drainFrames(sub)
	if len(frames) == 0 {
		t.Fatal("no frames were delivered")
	}
	final := frames[len(frames)-1]
	if state, terminal := final.TerminalState(); !terminal || state != a2a.TaskStateFailed {
		t.Errorf("the last frame is %v, want a terminal FAILED", state)
	}
	task.detach(sub)

	// A payload arriving after the crash is ignored, not a second ending.
	instance.deliver(brokerIOMessage{Type: ioTypeOutput, Content: "too late", TurnID: "t1"})
	if got := task.snapshotTask().Status.State; got != a2a.TaskStateFailed {
		t.Errorf("state after a late payload = %s, want it to stay FAILED", got)
	}
}

// TestBrokerShutdownSettlesLiveTasks: a broker going away must end its tasks
// the same way a dying instance does, so no client is left on a socket nothing
// will write to again.
func TestBrokerShutdownSettlesLiveTasks(t *testing.T) {
	server, instance := newConformIngress(t)
	task, sub := startConformTask(t, server, instance, "work")
	defer task.detach(sub)
	instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: "thinking"})

	if server.tasks.count() != 1 {
		t.Fatalf("live tasks = %d, want 1", server.tasks.count())
	}
	server.Shutdown()

	if state := task.snapshotTask().Status.State; state != a2a.TaskStateFailed {
		t.Errorf("state = %s, want FAILED after shutdown", state)
	}
	if server.tasks.count() != 0 {
		t.Errorf("live tasks = %d after shutdown, want 0", server.tasks.count())
	}
}

// TestUndeliverableMessageIsRefusedBeforeAnyFrame: a message that never reached
// the agent must be answered with an error, not with a task stuck at SUBMITTED
// that a client would have to cancel.
func TestUndeliverableMessageIsRefusedBeforeAnyFrame(t *testing.T) {
	server, instance := newConformIngress(t)
	instance.sendErr = errors.New("the instance send buffer is full")

	card := server.card("support")
	_, _, _, protoErr := server.startTask(context.Background(), card, a2aTurnInput{text: "hi"}, nexusauth.Principal{})
	if protoErr == nil {
		t.Fatal("an undeliverable message was accepted")
	}
	if server.tasks.count() != 0 {
		t.Errorf("live tasks = %d, want 0: a refused start must leave nothing behind", server.tasks.count())
	}
	if !instance.wasReleased() {
		t.Error("the lease was not released after a refused start")
	}
}

// TestUnwiredLeaseProviderRefusesClearly: with no lifecycle wired the ingress
// must answer with a diagnosis rather than a nil-pointer panic, and the reason
// must be machine-readable so a client can tell it from a transient failure.
func TestUnwiredLeaseProviderRefusesClearly(t *testing.T) {
	cfg := a2aTestConfig(t, "")
	server, err := NewA2AServer(testLogger(), cfg)
	if err != nil {
		t.Fatalf("NewA2AServer: %v", err)
	}
	_, _, _, protoErr := server.startTask(context.Background(), server.card("support"),
		a2aTurnInput{text: "hi"}, nexusauth.Principal{})
	if protoErr == nil {
		t.Fatal("an ingress with no lease provider accepted a message")
	}
	if protoErr.Type != a2a.ErrorTypeInternal {
		t.Errorf("error type = %s, want InternalError", protoErr.Type)
	}
	if !strings.Contains(protoErr.Message, "no agent instance provider") {
		t.Errorf("error message = %q, want it to name the missing piece", protoErr.Message)
	}
}

// TestTasksAreScopedToTheirOwner: another caller's task is indistinguishable
// from one that does not exist. Anything else is an oracle for enumerating the
// task ids in flight on a shared broker.
func TestTasksAreScopedToTheirOwner(t *testing.T) {
	server, instance := newConformIngress(t)
	card := server.card("support")
	owner := nexusauth.Principal{ID: "alice"}
	task, sub, _, protoErr := server.startTask(context.Background(), card, a2aTurnInput{text: "hi"}, owner)
	if protoErr != nil {
		t.Fatalf("startTask: %s", protoErr.Message)
	}
	defer task.detach(sub)
	instance.deliver(brokerIOMessage{Type: ioTypeHITLRequest, RequestID: "q-1", Prompt: "?", TurnID: "t1"})

	stranger := nexusauth.Principal{ID: "mallory"}
	if _, protoErr := server.cancelTask(stranger, "support", task.taskID); protoErr == nil {
		t.Error("a stranger canceled somebody else's task")
	} else if protoErr.Type != a2a.ErrorTypeTaskNotFound {
		t.Errorf("error type = %s, want TaskNotFoundError (indistinguishable from an unknown id)", protoErr.Type)
	}
	if _, _, _, protoErr := server.resumeTask(card, a2aTurnInput{taskID: task.taskID, text: "yes"}, stranger); protoErr == nil {
		t.Error("a stranger answered somebody else's question")
	}
}

// TestStreamedTurnWithNoOutputPayloadStillPublishesItsAnswer pins the fallback
// that a broker turn actually depends on.
//
// Every shipped Nexus agent loop tags its io.output with metadata
// streamed=true, and nexus.io.broker drops those, so on the ordinary streaming
// path the ONLY text the envelope carries is the deltas. A mapping that
// published only what an `output` payload named would answer every real
// streaming turn with an empty task.
func TestStreamedTurnWithNoOutputPayloadStillPublishesItsAnswer(t *testing.T) {
	server, instance := newConformIngress(t)
	task, sub := startConformTask(t, server, instance, "hi")
	defer task.detach(sub)

	instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: "thinking"})
	instance.deliver(brokerIOMessage{Type: ioTypeStreamDelta, Content: "the answer ", TurnID: "t1"})
	instance.deliver(brokerIOMessage{Type: ioTypeStreamDelta, Content: "is 42", TurnID: "t1"})
	instance.deliver(brokerIOMessage{Type: ioTypeStreamEnd, TurnID: "t1", FinishReason: "end_turn"})
	instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: ioStateIdle})

	snapshot := task.snapshotTask()
	if len(snapshot.Artifacts) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(snapshot.Artifacts))
	}
	text, _ := snapshot.Artifacts[0].Parts[0].TextValue()
	if text != "the answer is 42" {
		t.Errorf("artifact text = %q, want the accumulated deltas", text)
	}
}

// TestOnlyTheLastResponseSegmentBecomesTheAnswer: a tool-using turn streams
// several model responses, and only the last one is the turn's answer. Joining
// them would prefix every tool-using turn's answer with its own reasoning
// preamble.
func TestOnlyTheLastResponseSegmentBecomesTheAnswer(t *testing.T) {
	server, instance := newConformIngress(t)
	task, sub := startConformTask(t, server, instance, "hi")
	defer task.detach(sub)

	instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: "thinking"})
	instance.deliver(brokerIOMessage{Type: ioTypeStreamDelta, Content: "Let me look that up.", TurnID: "t1"})
	instance.deliver(brokerIOMessage{Type: ioTypeStreamEnd, TurnID: "t1", FinishReason: "tool_use"})
	instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: "tool_running"})
	instance.deliver(brokerIOMessage{Type: ioTypeStreamDelta, Content: "It is 42.", TurnID: "t1"})
	instance.deliver(brokerIOMessage{Type: ioTypeStreamEnd, TurnID: "t1", FinishReason: "end_turn"})
	instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: ioStateIdle})

	snapshot := task.snapshotTask()
	if len(snapshot.Artifacts) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(snapshot.Artifacts))
	}
	text, _ := snapshot.Artifacts[0].Parts[0].TextValue()
	if text != "It is 42." {
		t.Errorf("artifact text = %q, want only the last response segment", text)
	}
}

// TestPayloadsFromAnotherTurnAreIgnored: a task is one turn, so a payload
// naming a different turn must not be folded into it.
func TestPayloadsFromAnotherTurnAreIgnored(t *testing.T) {
	server, instance := newConformIngress(t)
	task, sub := startConformTask(t, server, instance, "hi")
	defer task.detach(sub)

	instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: "thinking"})
	instance.deliver(brokerIOMessage{Type: ioTypeStreamDelta, Content: "a", TurnID: "t1"})
	if got := task.boundTurn(); got != "t1" {
		t.Fatalf("bound turn = %q, want t1", got)
	}
	instance.deliver(brokerIOMessage{Type: ioTypeStreamDelta, Content: "other turn", TurnID: "t2"})
	if got := task.boundTurn(); got != "t1" {
		t.Errorf("bound turn = %q after a foreign payload, want it unchanged", got)
	}
}

// TestIdleBeforeAnyWorkDoesNotCompleteTheTask: an instance reporting itself
// quiet before it has done anything is not a turn ending, and settling on it
// would complete a task that never ran.
func TestIdleBeforeAnyWorkDoesNotCompleteTheTask(t *testing.T) {
	server, instance := newConformIngress(t)
	task, sub := startConformTask(t, server, instance, "hi")
	defer task.detach(sub)

	instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: ioStateIdle})
	if task.terminated() {
		t.Fatal("an idle before any work completed the task")
	}
	if state := task.snapshotTask().Status.State; state != a2a.TaskStateSubmitted {
		t.Errorf("state = %s, want SUBMITTED", state)
	}
}

// TestApprovalRequestIsRecordedNotFatal: the envelope carries tool approvals
// this mapping has no A2A vector for. They must be ignored safely, exactly as
// any other unmapped payload is.
func TestApprovalRequestIsRecordedNotFatal(t *testing.T) {
	server, instance := newConformIngress(t)
	task, sub := startConformTask(t, server, instance, "hi")
	defer task.detach(sub)

	instance.deliver(brokerIOMessage{Type: ioTypeStatus, State: "thinking"})
	instance.deliver(brokerIOMessage{
		Type: ioTypeApprovalRequest, PromptID: "p-1", Description: "run rm -rf", Risk: "high",
	})
	if task.terminated() {
		t.Fatal("an approval request failed the task")
	}
	if state := task.snapshotTask().Status.State; state != a2a.TaskStateWorking {
		t.Errorf("state = %s, want WORKING", state)
	}
}

// ---- the wire ----

// TestSendStreamingMessageOverSSE drives one turn over the real JSON-RPC route
// and reads the SSE records off the socket, which is the layer the conformance
// corpus deliberately does not reach.
//
// It is also the sole-writer assertion in practice: the payloads that produce
// the frames arrive on a different goroutine from the one writing the response,
// and `go test -race` is what makes that claim mean something.
func TestSendStreamingMessageOverSSE(t *testing.T) {
	cfg := a2aTestConfig(t, "")
	server, err := NewA2AServer(testLogger(), cfg)
	if err != nil {
		t.Fatalf("NewA2AServer: %v", err)
	}
	instance := &conformInstance{}
	// The turn is scripted from a goroutine as soon as the input lands, which is
	// what an instance does: it answers on its own schedule while the HTTP
	// goroutine is already streaming.
	scripted := &scriptedInstance{conformInstance: instance, script: func(i *conformInstance) {
		i.deliver(brokerIOMessage{Type: ioTypeStatus, State: "thinking"})
		i.deliver(brokerIOMessage{Type: ioTypeStreamDelta, Content: "the answer is 42", TurnID: "t1"})
		i.deliver(brokerIOMessage{Type: ioTypeStreamEnd, TurnID: "t1", FinishReason: "end_turn"})
		i.deliver(brokerIOMessage{Type: ioTypeStatus, State: ioStateIdle})
	}}
	server.useLeaseProvider(&conformLeaseProvider{instance: instance})
	instance.onInput = scripted.run

	ts, _ := newBrokerTestServer(t, cfg, server.Register)

	body := `{"jsonrpc":"2.0","id":9,"method":"SendStreamingMessage","params":{"message":` +
		`{"messageId":"m1","role":"ROLE_USER","contextId":"ctx-1","parts":[{"text":"what is the answer?"}]}}}`
	resp, err := http.Post(ts.URL+agentJSONRPCPath("support"), a2a.ContentTypeJSON, strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST = %d, want 200: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want an SSE stream", ct)
	}

	states := readSSEStates(t, resp.Body)
	want := []string{"TASK_STATE_SUBMITTED", "TASK_STATE_WORKING", "artifact", "TASK_STATE_COMPLETED"}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Errorf("stream carried %v, want %v", states, want)
	}
}

// TestSendMessageBlocksUntilTheTurnEnds covers the non-streaming shape of the
// same turn: one Task reply, produced by folding exactly the frames the stream
// would have carried.
func TestSendMessageBlocksUntilTheTurnEnds(t *testing.T) {
	cfg := a2aTestConfig(t, "")
	server, err := NewA2AServer(testLogger(), cfg)
	if err != nil {
		t.Fatalf("NewA2AServer: %v", err)
	}
	instance := &conformInstance{}
	scripted := &scriptedInstance{conformInstance: instance, script: func(i *conformInstance) {
		i.deliver(brokerIOMessage{Type: ioTypeStatus, State: "thinking"})
		i.deliver(brokerIOMessage{Type: ioTypeOutput, Content: "hello back", Role: "assistant", TurnID: "t1"})
		i.deliver(brokerIOMessage{Type: ioTypeStatus, State: ioStateIdle})
	}}
	server.useLeaseProvider(&conformLeaseProvider{instance: instance})
	instance.onInput = scripted.run

	ts, _ := newBrokerTestServer(t, cfg, server.Register)
	body := `{"jsonrpc":"2.0","id":3,"method":"SendMessage","params":{"message":` +
		`{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hello"}]}}}`
	resp, err := http.Post(ts.URL+agentJSONRPCPath("support"), a2a.ContentTypeJSON, strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)

	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("response is not a JSON-RPC envelope: %v (%s)", err, raw)
	}
	if envelope.Error != nil {
		t.Fatalf("SendMessage was refused: %s", envelope.Error)
	}
	var result a2a.SendMessageResponse
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatalf("result is not a SendMessageResponse: %v (%s)", err, envelope.Result)
	}
	if result.Task == nil {
		t.Fatalf("SendMessage answered with no task: %s", envelope.Result)
	}
	task := *result.Task
	if task.Status.State != a2a.TaskStateCompleted {
		t.Errorf("state = %s, want COMPLETED", task.Status.State)
	}
	if len(task.Artifacts) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(task.Artifacts))
	}
	if text, _ := task.Artifacts[0].Parts[0].TextValue(); text != "hello back" {
		t.Errorf("artifact text = %q, want the published output", text)
	}
}

// scriptedInstance runs a turn script the first time the instance is handed an
// input, from a goroutine — which is what puts the frame producer and the
// response writer on different goroutines for the race detector to judge.
type scriptedInstance struct {
	conformInstance *conformInstance
	script          func(*conformInstance)
	once            sync.Once
}

func (s *scriptedInstance) run() {
	s.once.Do(func() {
		go s.script(s.conformInstance)
	})
}

// readSSEStates reads an SSE stream and renders each record as the state (or
// "artifact") it reports, so a test can assert the sequence without restating
// the codec.
func readSSEStates(t *testing.T, body io.Reader) []string {
	t.Helper()
	var states []string
	scanner := bufio.NewScanner(body)
	deadline := time.Now().Add(5 * time.Second)
	for scanner.Scan() {
		if time.Now().After(deadline) {
			t.Fatal("the SSE stream did not end within 5s")
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var record struct {
			Result struct {
				Task *struct {
					Status struct {
						State string `json:"state"`
					} `json:"status"`
				} `json:"task"`
				StatusUpdate *struct {
					Status struct {
						State string `json:"state"`
					} `json:"status"`
				} `json:"statusUpdate"`
				ArtifactUpdate json.RawMessage `json:"artifactUpdate"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &record); err != nil {
			t.Fatalf("SSE record is not JSON: %v (%s)", err, line)
		}
		switch {
		case record.Result.Task != nil:
			states = append(states, record.Result.Task.Status.State)
		case record.Result.StatusUpdate != nil:
			states = append(states, record.Result.StatusUpdate.Status.State)
		case len(record.Result.ArtifactUpdate) > 0:
			states = append(states, "artifact")
		}
	}
	return states
}

// drainFrames takes everything currently queued on an observer.
func drainFrames(sub *a2aStream) []a2a.StreamResponse {
	var out []a2a.StreamResponse
	for {
		select {
		case frame := <-sub.frames:
			out = append(out, frame)
		default:
			return out
		}
	}
}
