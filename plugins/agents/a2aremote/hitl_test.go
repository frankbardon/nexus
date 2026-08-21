package a2aremote

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// ---- Harness helpers ----

// answerHITL plays the human: it watches the bus for the question this plugin
// raises and answers it with the given text.
//
// It stands in for whatever would answer in a real session — a TUI operator, the
// browser, the hitl registry's on-disk queue — and deliberately reaches the
// plugin only through the bus, which is the whole interface the chained path has
// to nexus.control.hitl.
func answerHITL(t *testing.T, h *contract.ContractHarness, answer string) *hitlSpy {
	t.Helper()
	spy := &hitlSpy{answer: answer, respond: true}
	spy.attach(h)
	return spy
}

// watchHITL observes the questions without answering any of them.
func watchHITL(t *testing.T, h *contract.ContractHarness) *hitlSpy {
	t.Helper()
	spy := &hitlSpy{}
	spy.attach(h)
	return spy
}

type hitlSpy struct {
	answer  string
	respond bool
	// cancel makes the spy decline rather than answer.
	cancel       bool
	cancelReason string

	mu   sync.Mutex
	seen []events.HITLRequest
}

func (s *hitlSpy) attach(h *contract.ContractHarness) {
	h.Bus().Subscribe("hitl.requested", func(ev engine.Event[any]) {
		req, ok := ev.Payload.(events.HITLRequest)
		if !ok {
			return
		}
		s.mu.Lock()
		s.seen = append(s.seen, req)
		reply := s.respond
		s.mu.Unlock()
		if !reply {
			return
		}
		// Answering from a fresh goroutine keeps the response off the dispatch
		// stack that delivered the question, which is how a real transport
		// answers: some time later, from its own reader.
		go func() {
			resp := events.HITLResponse{
				SchemaVersion: events.HITLResponseVersion,
				RequestID:     req.ID,
				FreeText:      s.answer,
			}
			if s.cancel {
				resp = events.HITLResponse{
					SchemaVersion: events.HITLResponseVersion,
					RequestID:     req.ID,
					Cancelled:     true,
					CancelReason:  s.cancelReason,
				}
			}
			// Emitted with an explicit source so a test can tell the human's
			// answer from one the plugin might (wrongly) have produced itself.
			_ = h.Bus().EmitEvent(engine.Event[any]{
				Type:    "hitl.responded",
				Source:  humanSource,
				Payload: resp,
			})
		}()
	}, engine.WithSource("testharness.human"))
}

// humanSource tags the answers the test's stand-in human emits.
const humanSource = "testharness.human"

// assertPluginNeverAnswered checks that every hitl.responded on the bus came
// from the stand-in human rather than from the plugin.
//
// This is the hard constraint as an assertion: the plugin asks a question and
// waits, and must never produce the answer. A plain AssertNotEmitted cannot say
// this, because the harness attributes any non-injected event to the plugin and
// the test's own human answers on the same bus.
func assertPluginNeverAnswered(t *testing.T, h *contract.ContractHarness) {
	t.Helper()
	for _, ev := range h.PluginEmissions() {
		if ev.Type == "hitl.responded" && ev.Source != humanSource {
			t.Errorf("the plugin answered a human's question itself: %+v", ev)
		}
	}
}

func (s *hitlSpy) questions() []events.HITLRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]events.HITLRequest(nil), s.seen...)
}

// ---- The round trip ----

// TestRemoteQuestionReachesTheHumanAndResumesTheTask is the story's core claim,
// end to end: a remote parks at INPUT_REQUIRED, the question reaches a human
// through hitl.requested, and the answer resumes the SAME task.
func TestRemoteQuestionReachesTheHumanAndResumesTheTask(t *testing.T) {
	rec := &resumeRecorder{}
	agent := newTestAgent(t, testAgentConfig{
		frames: askThenAnswer("task-77", "ctx-77", "which fiscal year?", rec),
	})
	h := boot(t, oneAgent(agent.URL(), nil))
	human := answerHITL(t, h, "FY2026")

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "summarize the numbers"})

	if res.Error != "" {
		t.Fatalf("the delegation should have completed once the human answered: %s", res.Error)
	}
	if !strings.Contains(res.Output, "the answer was FY2026") {
		t.Errorf("the resumed task's output should reach the model: %s", res.Output)
	}

	questions := human.questions()
	if len(questions) != 1 {
		t.Fatalf("the human was asked %d times, want 1", len(questions))
	}
	q := questions[0]
	if !strings.Contains(q.Prompt, "which fiscal year?") {
		t.Errorf("the human's prompt should carry the remote's own question: %q", q.Prompt)
	}
	if !strings.Contains(q.Prompt, "researcher") {
		t.Errorf("the human's prompt should name the remote that asked: %q", q.Prompt)
	}
	if q.RequesterPlugin != pluginID {
		t.Errorf("requester = %q, want %q", q.RequesterPlugin, pluginID)
	}
	if q.Mode != events.HITLModeFreeText {
		t.Errorf("mode = %q, want free_text", q.Mode)
	}
	if q.ActionKind != hitlActionKind {
		t.Errorf("action kind = %q, want %q", q.ActionKind, hitlActionKind)
	}
	if got, _ := q.ActionRef["task_id"].(string); got != "task-77" {
		t.Errorf("action_ref.task_id = %q, want the parked task", got)
	}

	// The identity is the whole mechanism: A2A has no resume operation, only an
	// ordinary message that names the task it continues (specification 3.4).
	resumes := rec.all()
	if len(resumes) != 1 {
		t.Fatalf("the remote saw %d resuming messages, want 1", len(resumes))
	}
	if resumes[0].taskID != "task-77" {
		t.Errorf("resume taskId = %q, want task-77", resumes[0].taskID)
	}
	if resumes[0].contextID != "ctx-77" {
		t.Errorf("resume contextId = %q, want ctx-77", resumes[0].contextID)
	}
	if !strings.Contains(resumes[0].text, "FY2026") {
		t.Errorf("resume carried %q, want the human's answer", resumes[0].text)
	}
}

// TestTheDelegatingModelNeverGetsToAnswer is the constraint stated as a test.
//
// A model handed a question only a person can settle will answer it, and act on
// the answer. So on the chained path the question must never appear in anything
// the model reads, and this plugin must never put a hitl.responded on the bus.
func TestTheDelegatingModelNeverGetsToAnswer(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{
		frames: askThenAnswer("t1", "c1", "should I delete the production bucket?", &resumeRecorder{}),
	})
	h := boot(t, oneAgent(agent.URL(), nil))
	answerHITL(t, h, "no")

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "tidy up"})

	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}
	if strings.Contains(res.Output, "should I delete the production bucket?") {
		t.Errorf("the remote's question must not reach the delegating model: %s", res.Output)
	}
	if strings.Contains(res.Error, "Re-delegate") {
		t.Errorf("the chained path must not invite the model to re-delegate with an answer: %s", res.Error)
	}
	assertPluginNeverAnswered(t, h)
}

// TestTransitiveQuestionReachesTheHumanTwoHopsUp proves the mapping composes.
//
// The chain is local Nexus -> a middle A2A agent -> a leaf A2A agent. The leaf
// asks; the middle agent mirrors that as its OWN INPUT_REQUIRED (which is
// exactly what nexus.io.a2a does when a Nexus turn asks a human); the local
// human answers; the answer walks back down, each hop resuming its own task with
// its own taskId.
func TestTransitiveQuestionReachesTheHumanTwoHopsUp(t *testing.T) {
	leafRec := &resumeRecorder{}
	leaf := newTestAgent(t, testAgentConfig{
		frames: askThenAnswer("leaf-task", "leaf-ctx", "which region?", leafRec),
	})

	middleRec := &resumeRecorder{}
	middle := newRelayAgent(t, leaf.URL(), middleRec)

	h := boot(t, oneAgent(middle.URL(), nil))
	human := answerHITL(t, h, "eu-west-1")

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "provision it"})

	if res.Error != "" {
		t.Fatalf("the chained delegation should have completed: %s", res.Error)
	}

	questions := human.questions()
	if len(questions) != 1 {
		t.Fatalf("the human was asked %d times, want 1", len(questions))
	}
	if !strings.Contains(questions[0].Prompt, "which region?") {
		t.Errorf("the leaf's question must survive both hops: %q", questions[0].Prompt)
	}

	// Each hop resumed its own task, with its own id.
	if resumes := middleRec.all(); len(resumes) != 1 || resumes[0].taskID != relayTaskID {
		t.Errorf("the middle hop was resumed as %+v, want one resume of %q", resumes, relayTaskID)
	}
	if resumes := leafRec.all(); len(resumes) != 1 || resumes[0].taskID != "leaf-task" {
		t.Errorf("the leaf hop was resumed as %+v, want one resume of leaf-task", resumes)
	}
	if !strings.Contains(res.Output, "eu-west-1") {
		t.Errorf("the human's answer should have reached the leaf: %s", res.Output)
	}
}

// ---- Deadlines ----

// TestUnansweredQuestionExpiresAndFreesTheSession is the "a human who never
// answers must not pin the session" criterion. The dedicated input deadline
// fires, the question is retracted, the remote task is cancelled, and the model
// is told plainly not to answer it itself.
func TestUnansweredQuestionExpiresAndFreesTheSession(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{
		frames: askThenAnswer("task-9", "ctx-9", "which fiscal year?", &resumeRecorder{}),
	})
	h := boot(t, oneAgent(agent.URL(), map[string]any{
		"hitl": map[string]any{"input_timeout": "150ms"},
	}))
	human := watchHITL(t, h)

	start := time.Now()
	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})
	elapsed := time.Since(start)

	if res.Error == "" {
		t.Fatal("an unanswered question must end the delegation as a tool error")
	}
	if elapsed > 5*time.Second {
		t.Errorf("the delegation took %s; the input deadline should have freed it promptly", elapsed)
	}
	if !strings.Contains(res.Error, "no answer arrived within 150ms") {
		t.Errorf("the error should name the deadline that fired: %s", res.Error)
	}
	if !strings.Contains(res.Error, "which fiscal year?") {
		t.Errorf("the error should carry the question so the model can report it: %s", res.Error)
	}
	if !strings.Contains(res.Error, "do NOT answer it on their behalf") {
		t.Errorf("the error must forbid the model answering for the human: %s", res.Error)
	}
	if len(human.questions()) != 1 {
		t.Errorf("the human should have been asked exactly once")
	}

	// The prompt is retracted rather than left sitting in a UI.
	if countEmitted(h, "hitl.cancel") == 0 {
		t.Error("an expired question must be retracted with hitl.cancel")
	}
	// And the remote is told to stop working for a caller that has gone.
	waitFor(t, "CancelTask on the abandoned remote task", func() bool {
		return len(agent.cancelledTasks()) > 0
	})
}

// TestCallBudgetKeepsRunningWhileParked is the other half of the same criterion:
// the whole-call budget does NOT pause for a human. A tight budget ends the
// delegation even though the dedicated input deadline is far away.
func TestCallBudgetKeepsRunningWhileParked(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{
		frames: askThenAnswer("t1", "c1", "which fiscal year?", &resumeRecorder{}),
	})
	h := boot(t, oneAgent(agent.URL(), map[string]any{
		"timeout": "250ms",
		"hitl":    map[string]any{"input_timeout": "10m"},
	}))
	watchHITL(t, h)

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})

	if res.Error == "" {
		t.Fatal("the call budget must end a delegation parked on an unanswered question")
	}
	if !strings.Contains(res.Error, "the call ended before it was answered") {
		t.Errorf("the error should say the CALL ended, not that the input deadline fired: %s", res.Error)
	}
	if !strings.Contains(res.Error, "do NOT answer it on their behalf") {
		t.Errorf("the error must forbid the model answering for the human: %s", res.Error)
	}
}

// TestDeclinedQuestionIsReportedWithoutInvitingAnAnswer covers a human who
// refuses outright.
func TestDeclinedQuestionIsReportedWithoutInvitingAnAnswer(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{
		frames: askThenAnswer("t1", "c1", "approve the spend?", &resumeRecorder{}),
	})
	h := boot(t, oneAgent(agent.URL(), nil))
	spy := &hitlSpy{respond: true, cancel: true, cancelReason: "not my call to make"}
	spy.attach(h)

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})

	if !strings.Contains(res.Error, "declined") {
		t.Errorf("a refused question should be reported as declined: %s", res.Error)
	}
	if !strings.Contains(res.Error, "not my call to make") {
		t.Errorf("the human's stated reason should reach the model: %s", res.Error)
	}
	if !strings.Contains(res.Error, "do NOT answer it on their behalf") {
		t.Errorf("the error must forbid the model answering for the human: %s", res.Error)
	}
}

// TestMaxRoundsStopsAnEndlessInterrogation bounds a remote that answers every
// answer with another question.
func TestMaxRoundsStopsAnEndlessInterrogation(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{
		frames: alwaysAsking("t1", "c1", "and then what?"),
	})
	h := boot(t, oneAgent(agent.URL(), map[string]any{
		"hitl": map[string]any{"max_rounds": 2},
	}))
	human := answerHITL(t, h, "carry on")

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})

	if !strings.Contains(res.Error, "asked for input 2 times") {
		t.Errorf("the error should name the round cap: %s", res.Error)
	}
	if got := len(human.questions()); got != 2 {
		t.Errorf("the human was asked %d times, want 2", got)
	}
}

// TestHITLDisabledKeepsTheOldToolError proves the switch is real: with chaining
// off, a parked task is reported to the calling model exactly as it was before.
func TestHITLDisabledKeepsTheOldToolError(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{
		frames: askThenAnswer("t1", "c1", "which fiscal year?", &resumeRecorder{}),
	})
	h := boot(t, oneAgent(agent.URL(), map[string]any{
		"hitl": map[string]any{"enabled": false},
	}))
	human := watchHITL(t, h)

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})

	if !strings.Contains(res.Error, "waiting for input") {
		t.Errorf("tool error = %q", res.Error)
	}
	if !strings.Contains(res.Error, "Re-delegate") {
		t.Errorf("the unchained path keeps its own advice: %s", res.Error)
	}
	if len(human.questions()) != 0 {
		t.Error("no human should be asked when chaining is disabled")
	}
	h.AssertNotEmitted("hitl.requested")
}

// TestAuthRequiredIsNotRoutedToAHuman: a remote demanding credentials is an
// operator problem, and no answer a person types is one.
func TestAuthRequiredIsNotRoutedToAHuman(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{
		frames: func(*a2a.SendMessageRequest) []a2a.StreamResponse {
			status := a2a.NewTaskStatus(a2a.TaskStateAuthRequired).
				WithMessage(a2a.NewAgentMessage("m-auth", "present a token for scope reports.read"))
			return []a2a.StreamResponse{
				a2a.StreamTask(a2a.NewTask("t1", "c1")),
				a2a.StreamStatusUpdate(a2a.NewStatusUpdate("t1", "c1", status)),
			}
		},
	})
	h := boot(t, oneAgent(agent.URL(), nil))
	human := watchHITL(t, h)

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})

	if !strings.Contains(res.Error, "needs credentials") {
		t.Errorf("tool error = %q", res.Error)
	}
	if !strings.Contains(res.Error, "An operator must configure") {
		t.Errorf("the error should point at configuration, not a person: %s", res.Error)
	}
	if len(human.questions()) != 0 {
		t.Error("AUTH_REQUIRED must not be put in front of a human")
	}
}

// TestHumanAnsweredOutcomesAreNotCached: a person's answer is a decision made at
// a moment, and replaying it for a later identical task would apply that
// decision again without asking.
func TestHumanAnsweredOutcomesAreNotCached(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{
		frames: askThenAnswer("t1", "c1", "which fiscal year?", &resumeRecorder{}),
	})
	h := boot(t, oneAgent(agent.URL(), nil))
	human := answerHITL(t, h, "FY2026")

	args := map[string]any{"task": "identical task"}
	if res := invoke(t, h, "delegate_a2a_researcher", args); res.Error != "" {
		t.Fatalf("first call: %s", res.Error)
	}
	if res := invoke(t, h, "delegate_a2a_researcher", args); res.Error != "" {
		t.Fatalf("second call: %s", res.Error)
	}

	if got := len(human.questions()); got != 2 {
		t.Errorf("the human was asked %d times, want 2 — a human-answered outcome must not be cached", got)
	}
}
