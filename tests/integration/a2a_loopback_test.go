//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// Nexus -> Nexus loopback: both halves of the A2A integration, run against each
// other through two real engines.
//
// Topology, for every test in this file:
//
//	caller engine                              callee engine
//	nexus.io.test (mock LLM, scripted input)   nexus.io.test (mock LLM only)
//	nexus.agent.react                          nexus.agent.react
//	nexus.agent.a2a_remote  --- A2A wire --->  nexus.io.a2a  (127.0.0.1:18192)
//	                            bearer token
//
// This is the effort's conformance backstop, and its limits are worth stating
// plainly: it proves the two Nexus mappings are self-consistent, NOT that either
// interoperates with a third-party implementation. No external counterparty and
// no A2A TCK is in this test path. The frame-level expectations that are not
// self-referential live in pkg/a2a/a2aconform, which nexus.io.a2a is driven
// against by plugins/io/a2a/conformance_test.go.
//
// Both engines run under mocked LLM responses, so the whole file needs no API
// key and no network beyond loopback.

const (
	// a2aLoopbackAddr is the listener configured by both loopback server
	// configs. It is deliberately NOT a2aBindAddr (18191, owned by
	// configs/test-a2a-serve.yaml) so the two suites can never contend.
	a2aLoopbackAddr = "127.0.0.1:18192"

	// a2aLoopbackToken is the shared bearer token: presented by the caller's
	// credentials block, validated by the callee's listener.
	a2aLoopbackToken = "test-a2a-loopback-token"

	// a2aLoopbackTool is the tool nexus.agent.a2a_remote registers for the
	// remote named "peer" in configs/test-a2a-loopback-caller.yaml.
	a2aLoopbackTool = "delegate_a2a_peer"

	// a2aRemotePluginID is the RequesterPlugin a chained question carries, and
	// is what separates the remote's question from any other hitl.requested on
	// the caller's bus.
	a2aRemotePluginID = "nexus.agent.a2a_remote"

	// a2aLoopbackParkDwell is how long a test watches a parked callee before
	// answering. It is the discriminating window: a callee that settled its own
	// question would leave INPUT_REQUIRED inside it.
	a2aLoopbackParkDwell = 250 * time.Millisecond

	// a2aLoopbackCalleeInputTimeout is what the deadline test narrows the
	// callee's tasks.input_timeout to, so its own deadline fires first.
	a2aLoopbackCalleeInputTimeout = "300ms"

	// a2aLoopbackLateAnswer is how long that test then waits before answering:
	// comfortably past the callee's deadline, so the callee is unambiguously
	// the one that gave up, and comfortably inside a2aLoopbackStallCutoff so
	// the caller is still running when the answer lands.
	a2aLoopbackLateAnswer = 1200 * time.Millisecond

	// a2aLoopbackStallCutoff is nexus.io.test's hard-coded stalled-turn
	// detector: three seconds after the last scripted input, a turn still in
	// flight ends the session. Every deadline in this file is chosen to land
	// well inside it, because a session ended by the stall detector tears the
	// engine down — and engine shutdown abandons live remote tasks the same way
	// a cancellation does, which would make several assertions below pass for
	// the wrong reason.
	a2aLoopbackStallCutoff = 3 * time.Second
)

// a2aLoopbackFetchTask reads one task back from the callee over the REST
// binding, as an ordinary authenticated A2A client would.
//
// It reports errors rather than failing the test, because several of the
// assertions below have to observe the callee from a bus handler's goroutine —
// where t.Fatal is not allowed — while the caller is still running.
func a2aLoopbackFetchTask(taskID string) (a2a.Task, int, error) {
	req, err := http.NewRequest(http.MethodGet, "http://"+a2aLoopbackAddr+"/a2a/v1/tasks/"+taskID, nil)
	if err != nil {
		return a2a.Task{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a2aLoopbackToken)
	req.Header.Set(a2a.HeaderVersion, a2a.ProtocolVersion)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return a2a.Task{}, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return a2a.Task{}, resp.StatusCode, nil
	}
	var task a2a.Task
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		return a2a.Task{}, resp.StatusCode, err
	}
	return task, resp.StatusCode, nil
}

// a2aLoopbackState returns the state the callee currently reports for taskID,
// empty when it cannot be read.
func a2aLoopbackState(taskID string) a2a.TaskState {
	task, status, err := a2aLoopbackFetchTask(taskID)
	if err != nil || status != http.StatusOK {
		return ""
	}
	return task.Status.State
}

// a2aLoopbackGetTask is the main-goroutine wrapper that fails the test on a
// transport error.
func a2aLoopbackGetTask(t *testing.T, taskID string) (a2a.Task, int) {
	t.Helper()
	task, status, err := a2aLoopbackFetchTask(taskID)
	if err != nil {
		t.Fatalf("GetTask %s: %v", taskID, err)
	}
	return task, status
}

// a2aLoopbackAwaitState polls the callee until taskID reports want, and returns
// the last state it saw plus whether it got there inside the deadline.
func a2aLoopbackAwaitState(taskID string, want a2a.TaskState, within time.Duration) (a2a.TaskState, bool) {
	deadline := time.Now().Add(within)
	var last a2a.TaskState
	for time.Now().Before(deadline) {
		if state := a2aLoopbackState(taskID); state != "" {
			last = state
			if last == want {
				return last, true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return last, false
}

// a2aLoopbackToolResult returns the first tool.result the caller collected for
// the delegate tool.
func a2aLoopbackToolResult(t *testing.T, h *testharness.Harness) events.ToolResult {
	t.Helper()
	tr, ok := collectedToolResult(h, a2aLoopbackTool)
	if !ok {
		t.Fatalf("no tool.result collected for %q — the delegation never returned", a2aLoopbackTool)
	}
	return tr
}

// a2aLoopbackToolDescriptions returns, in order, every description the caller
// published for the delegate tool.
func a2aLoopbackToolDescriptions(h *testharness.Harness) []string {
	var out []string
	for _, e := range h.Events() {
		if e.Type != "tool.register" {
			continue
		}
		def, ok := e.Payload.(events.ToolDef)
		if !ok || def.Name != a2aLoopbackTool {
			continue
		}
		out = append(out, def.Description)
	}
	return out
}

// TestA2ALoopback_StreamingDelegation is the flagship proof: one Nexus engine
// delegates to another over A2A and gets the finished task back.
//
// It covers, in one delegation:
//
//   - Card fetch. The tool is registered pre-discovery with the operator's
//     configured description and re-registered once with a description built
//     from the callee's own Agent Card.
//   - Bearer auth end to end. The callee's listener validates the token the
//     caller's credentials block presents; without it every operation is a 401
//     (see TestA2ALoopback_BearerRejected).
//   - A streaming run to COMPLETED, folded into the tool result.
//   - Artifact return: the callee's answer artifact arrives inside the folded
//     <artifacts> element the calling model reads.
func TestA2ALoopback_StreamingDelegation(t *testing.T) {
	bootEngine(t, "configs/test-a2a-loopback-server.yaml")
	waitForListener(t, a2aLoopbackAddr)

	h := testharness.New(t, "configs/test-a2a-loopback-caller.yaml", testharness.WithTimeout(60*time.Second))
	h.Run()

	// The caller's agent chose to delegate, and the delegation returned.
	h.AssertToolCalled(a2aLoopbackTool)
	tr := a2aLoopbackToolResult(t, h)
	if tr.Error != "" {
		t.Fatalf("delegation failed: %s", tr.Error)
	}

	// The remote task ran to COMPLETED and the fold names it.
	if !strings.Contains(tr.Output, `state="`+string(a2a.TaskStateCompleted)+`"`) {
		t.Errorf("tool.result does not report a COMPLETED remote task:\n%s", tr.Output)
	}
	if !strings.Contains(tr.Output, `<remote_agent name="peer"`) {
		t.Errorf("tool.result is not the XML-folded remote answer:\n%s", tr.Output)
	}

	// The callee's answer came back as an ARTIFACT, not only as status text:
	// A2A puts task output in artifacts (section 3.7) and the fold keeps them
	// in their own element.
	artifacts := tr.Output
	if idx := strings.Index(artifacts, "<artifacts"); idx >= 0 {
		artifacts = artifacts[idx:]
	} else {
		t.Fatalf("tool.result carries no <artifacts> element:\n%s", tr.Output)
	}
	if !strings.Contains(artifacts, "The remote Nexus agent completed the delegated task.") {
		t.Errorf("the remote's answer did not arrive as an artifact:\n%s", artifacts)
	}

	// Discovery: registered once from configuration, re-registered once from
	// the card. The card-derived description names the remote's own identity
	// and its advertised skills, neither of which the caller's config knows.
	descriptions := a2aLoopbackToolDescriptions(h)
	if len(descriptions) < 2 {
		t.Fatalf("tool.register fired %d time(s) for %q; want the pre-discovery registration plus one card refresh",
			len(descriptions), a2aLoopbackTool)
	}
	if descriptions[0] != "A second Nexus instance reachable over A2A." {
		t.Errorf("first description = %q, want the operator's configured one (the card is fetched lazily)", descriptions[0])
	}
	final := descriptions[len(descriptions)-1]
	for _, want := range []string{"nexus-loopback-agent", "Conversational turn"} {
		if !strings.Contains(final, want) {
			t.Errorf("card-derived description is missing %q:\n%s", want, final)
		}
	}

	// The delegation is bracketed on the caller's bus like any other subagent.
	h.AssertEventEmitted("subagent.started")
	h.AssertEventEmitted("subagent.complete")

	// And the caller's own turn closed on top of the remote's answer, which is
	// what proves the ReAct loop consumed the tool result.
	h.AssertOutputContains("The remote Nexus agent handled the release.")
	h.AssertNoSystemOutput()
}

// TestA2ALoopback_BearerRejected is the other half of the auth proof: the
// callee genuinely validates. The caller presents a token the listener does not
// know, and the delegation fails cleanly with the 401 rather than hanging or
// taking the engine down.
func TestA2ALoopback_BearerRejected(t *testing.T) {
	bootEngine(t, "configs/test-a2a-loopback-server.yaml")
	waitForListener(t, a2aLoopbackAddr)

	cfg := copyConfig(t, "configs/test-a2a-loopback-caller.yaml", map[string]any{
		"nexus.agent.a2a_remote": map[string]any{
			"cache":   false,
			"timeout": "20s",
			"agents": []any{
				map[string]any{
					"name":        "peer",
					"base_url":    "http://" + a2aLoopbackAddr,
					"description": "A second Nexus instance reachable over A2A.",
					"credentials": map[string]any{
						"type":  "bearer",
						"token": "not-the-configured-token",
					},
				},
			},
		},
	})
	h := testharness.New(t, cfg, testharness.WithTimeout(60*time.Second))
	h.Run()

	h.AssertToolCalled(a2aLoopbackTool)
	tr := a2aLoopbackToolResult(t, h)
	if tr.Error == "" {
		t.Fatalf("a wrong bearer token was accepted; output was:\n%s", tr.Output)
	}
	if !strings.Contains(tr.Error, "401") {
		t.Errorf("delegation error = %q, want it to report the HTTP 401 refusal", tr.Error)
	}
}

// TestA2ALoopback_ChainedHITL proves the interruption composes across the two
// mappings: a question raised INSIDE the callee's turn is answered by the human
// driving the CALLER's session.
//
//	callee: ask_user -> hitl.requested -> nexus.io.a2a parks at INPUT_REQUIRED
//	caller: a2a_remote sees INPUT_REQUIRED -> hitl.requested on the CALLER bus
//	caller: the human answers -> a message with the SAME taskId and contextId
//	callee: nexus.io.a2a routes it to hitl.responded -> the parked turn resumes
//
// The answer is supplied by this test rather than by the caller's scripted
// hitl_responses, and only after the callee has been observed sitting at
// INPUT_REQUIRED for a while. That is what makes the assertion discriminating:
// a callee that answered its own question would have run to COMPLETED during
// the wait, and the delegation would be a coincidence rather than a chain.
//
// It runs over the BLOCKING binding (stream: false). That is not incidental:
// nexus.io.a2a holds a streaming connection open across an INPUT_REQUIRED park
// (keep-alive comments, no terminal frame), while nexus.agent.a2a_remote drains
// a stream to its end before it acts on what it read — so over SSE the two
// deadlock until a deadline fires and the question never reaches a human. Both
// behaviours are individually specification-legal; composed they are not. See
// the caveat in docs/src/plugins/agents/a2a-remote.md.
func TestA2ALoopback_ChainedHITL(t *testing.T) {
	bootEngine(t, "configs/test-a2a-loopback-hitl-server.yaml")
	waitForListener(t, a2aLoopbackAddr)

	cfg := copyConfig(t, "configs/test-a2a-loopback-caller.yaml", map[string]any{
		"nexus.io.test":          a2aLoopbackCallerIO(nil),
		"nexus.agent.a2a_remote": a2aLoopbackRemote(map[string]any{"stream": false}),
	})
	h := testharness.New(t, cfg, testharness.WithTimeout(60*time.Second))

	// The human at the top of the chain, answering only once the callee has
	// been seen genuinely waiting.
	type parkObservation struct {
		taskID    string
		onArrival a2a.TaskState
		afterWait a2a.TaskState
	}
	observed := make(chan parkObservation, 1)
	h.Bus().Subscribe("hitl.requested", func(e engine.Event[any]) {
		req, ok := e.Payload.(events.HITLRequest)
		if !ok || req.RequesterPlugin != a2aRemotePluginID {
			return
		}
		taskID, _ := req.ActionRef["task_id"].(string)
		go func() {
			obs := parkObservation{taskID: taskID, onArrival: a2aLoopbackState(taskID)}
			time.Sleep(a2aLoopbackParkDwell)
			obs.afterWait = a2aLoopbackState(taskID)
			select {
			case observed <- obs:
			default:
			}
			_ = h.Bus().Emit("hitl.responded", events.HITLResponse{
				SchemaVersion: events.HITLResponseVersion,
				RequestID:     req.ID,
				FreeText:      "staging",
			})
		}()
	})

	h.Run()

	// The question surfaced on the CALLER's bus, attributed to the remote and
	// carrying the remote task it belongs to.
	var asked events.HITLRequest
	for _, e := range h.Events() {
		if e.Type != "hitl.requested" {
			continue
		}
		req, ok := e.Payload.(events.HITLRequest)
		if ok && req.RequesterPlugin == a2aRemotePluginID {
			asked = req
			break
		}
	}
	if asked.ID == "" {
		t.Fatal("the remote's question never reached the caller's bus as a hitl.requested")
	}
	if !strings.Contains(asked.Prompt, "Which environment should I deploy to?") {
		t.Errorf("caller-side prompt = %q, want the remote agent's own question", asked.Prompt)
	}

	var obs parkObservation
	select {
	case obs = <-observed:
	default:
		t.Fatal("the callee was never observed while the question was outstanding")
	}
	if obs.taskID == "" {
		t.Fatal("the caller-side question does not name the remote task it belongs to")
	}
	// The callee was parked when the question arrived, and STAYED parked: its
	// turn is blocked inside ask_user waiting for an answer only the caller's
	// human can give.
	if obs.onArrival != a2a.TaskStateInputRequired {
		t.Fatalf("callee task %s = %s when the question surfaced, want INPUT_REQUIRED", obs.taskID, obs.onArrival)
	}
	if obs.afterWait != a2a.TaskStateInputRequired {
		t.Fatalf("callee task %s moved to %s after %s with the question unanswered; its turn was not actually blocked on the caller",
			obs.taskID, obs.afterWait, a2aLoopbackParkDwell)
	}

	// The answer resumed the SAME remote task, which then finished.
	tr := a2aLoopbackToolResult(t, h)
	if tr.Error != "" {
		t.Fatalf("the resumed delegation failed: %s", tr.Error)
	}
	if !strings.Contains(tr.Output, "The remote Nexus agent deployed to the environment you named.") {
		t.Errorf("tool.result does not carry the post-resume answer:\n%s", tr.Output)
	}

	task, status := a2aLoopbackGetTask(t, obs.taskID)
	if status != http.StatusOK {
		t.Fatalf("GetTask %s on the callee = %d, want 200", obs.taskID, status)
	}
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("callee task %s = %s, want COMPLETED", obs.taskID, task.Status.State)
	}
	// One task for the whole exchange: a resume is a continuation, not a new
	// conversation, so the answer is in THIS task's history.
	if !a2aLoopbackHistoryContains(task, "staging") {
		t.Errorf("the callee's task history does not record the answer:\n%+v", task.History)
	}

	h.AssertOutputContains("The remote Nexus agent handled the release.")
}

// TestA2ALoopback_ChainedHITLInputTimeout exercises the interaction the two
// input deadlines have with each other, which only a two-engine loopback can
// reach: nexus.io.a2a's tasks.input_timeout expires the parked task on the
// CALLEE while nexus.agent.a2a_remote is still holding the question in front of
// the human on the CALLER, whose own hitl.input_timeout has not yet fired.
//
// The callee wins the race, so the answer arrives at a task that is already
// terminal. The delegation must then fail cleanly — the caller is told the
// remote would not take the answer — rather than hanging on a task nobody is
// going to resume.
func TestA2ALoopback_ChainedHITLInputTimeout(t *testing.T) {
	serverCfg := copyConfig(t, "configs/test-a2a-loopback-hitl-server.yaml", map[string]any{
		"nexus.io.a2a": a2aLoopbackListener(map[string]any{
			"tasks": map[string]any{"input_timeout": a2aLoopbackCalleeInputTimeout},
		}),
	})
	bootEngine(t, serverCfg)
	waitForListener(t, a2aLoopbackAddr)

	cfg := copyConfig(t, "configs/test-a2a-loopback-caller.yaml", map[string]any{
		"nexus.io.test": a2aLoopbackCallerIO(nil),
		// The caller's own question deadline is far looser than the callee's, so
		// the callee is unambiguously the one that gives up first.
		"nexus.agent.a2a_remote": a2aLoopbackRemote(map[string]any{
			"stream": false,
			"hitl":   map[string]any{"input_timeout": "20s"},
		}),
	})

	h := testharness.New(t, cfg, testharness.WithTimeout(60*time.Second))

	// Answer well after the callee's own deadline has already expired the task.
	taskIDs := make(chan string, 1)
	h.Bus().Subscribe("hitl.requested", func(e engine.Event[any]) {
		req, ok := e.Payload.(events.HITLRequest)
		if !ok || req.RequesterPlugin != a2aRemotePluginID {
			return
		}
		id, _ := req.ActionRef["task_id"].(string)
		select {
		case taskIDs <- id:
		default:
		}
		go func() {
			time.Sleep(a2aLoopbackLateAnswer)
			_ = h.Bus().Emit("hitl.responded", events.HITLResponse{
				SchemaVersion: events.HITLResponseVersion,
				RequestID:     req.ID,
				FreeText:      "staging",
			})
		}()
	})

	h.Run()

	var taskID string
	select {
	case taskID = <-taskIDs:
	default:
		t.Fatal("the remote's question never reached the caller's bus")
	}

	// The callee failed the task when its own input deadline elapsed.
	state, reached := a2aLoopbackAwaitState(taskID, a2a.TaskStateFailed, 2*time.Second)
	if !reached {
		t.Fatalf("callee task %s = %s, want FAILED once tasks.input_timeout elapsed", taskID, state)
	}

	// And the caller's delegation ended as a clean tool error rather than a
	// hang: the late answer was sent, and the callee refused it because the
	// task it named is already terminal.
	tr := a2aLoopbackToolResult(t, h)
	if tr.Error == "" {
		t.Fatalf("the delegation reported success against a task the remote had already failed:\n%s", tr.Output)
	}
	if !strings.Contains(tr.Error, "refused the request") {
		t.Errorf("delegation error = %q, want the callee's refusal of the late answer", tr.Error)
	}
	h.AssertOutputContains("The remote Nexus agent handled the release.")
}

// TestA2ALoopback_Cancellation proves cancellation crosses the boundary: the
// user cancels the CALLER's turn while the delegation is parked on a question,
// and the CALLEE's task is settled at CANCELED rather than left running for a
// caller that has gone away.
func TestA2ALoopback_Cancellation(t *testing.T) {
	bootEngine(t, "configs/test-a2a-loopback-hitl-server.yaml")
	waitForListener(t, a2aLoopbackAddr)

	cfg := copyConfig(t, "configs/test-a2a-loopback-caller.yaml", map[string]any{
		// Nobody answers: the turn is cancelled with the question still up.
		"nexus.io.test":          a2aLoopbackCallerIO(nil),
		"nexus.agent.a2a_remote": a2aLoopbackRemote(map[string]any{"stream": false}),
	})

	h := testharness.New(t, cfg, testharness.WithTimeout(60*time.Second))

	// Cancel the caller's turn the moment the remote's question surfaces, and
	// watch the callee from here — the observation has to happen while the
	// caller is still running, because engine shutdown abandons live remote
	// tasks the same way and would otherwise be an equally good explanation for
	// a CANCELED task.
	canceled := make(chan bool, 1)
	h.Bus().Subscribe("hitl.requested", func(e engine.Event[any]) {
		req, ok := e.Payload.(events.HITLRequest)
		if !ok || req.RequesterPlugin != a2aRemotePluginID {
			return
		}
		taskID, _ := req.ActionRef["task_id"].(string)
		go func() {
			// cancel.request is control.cancel's own entry point — the same one
			// the TUI's interrupt key and the A2A CancelTask operation use.
			_ = h.Bus().Emit("cancel.request", events.CancelRequest{
				SchemaVersion: events.CancelRequestVersion,
				Source:        "test",
			})
			// Deliberately shorter than a2aLoopbackStallCutoff: past it the
			// engine is being torn down, and shutdown abandons live remote
			// tasks too, so a CANCELED observed after it would prove nothing.
			_, reached := a2aLoopbackAwaitState(taskID, a2a.TaskStateCanceled, 2*time.Second)
			canceled <- reached
		}()
	})

	h.Run()

	select {
	case reached := <-canceled:
		if !reached {
			t.Fatal("cancelling the caller's turn did not cancel the remote task")
		}
	case <-time.After(a2aLoopbackStallCutoff + 5*time.Second):
		t.Fatal("the remote's question never reached the caller's bus, so nothing was cancelled")
	}

	// The delegation itself ends as a clean tool error naming the abandoned
	// question, not as a hang.
	tr := a2aLoopbackToolResult(t, h)
	if tr.Error == "" {
		t.Fatalf("a cancelled delegation reported success:\n%s", tr.Output)
	}
	if !strings.Contains(tr.Error, "do NOT answer it on their behalf") {
		t.Errorf("delegation error = %q, want the unanswered-question guidance", tr.Error)
	}
}

// a2aLoopbackCallerIO renders the caller's nexus.io.test block with overrides
// merged in. copyConfig replaces a plugin block wholesale, so the unchanged
// keys have to be restated.
//
// hitl_auto_respond is off in every chained-question test: the test itself
// plays the human, so it controls WHEN the answer arrives, which is what the
// deadline and cancellation legs turn on.
func a2aLoopbackCallerIO(overrides map[string]any) map[string]any {
	block := map[string]any{
		"inputs":            []any{"Ask the remote Nexus agent to handle the release."},
		"input_delay":       "100ms",
		"approval_mode":     "approve",
		"timeout":           "30s",
		"hitl_auto_respond": false,
		"mock_responses": []any{
			map[string]any{"tool_calls": []any{map[string]any{
				"name":      a2aLoopbackTool,
				"arguments": `{"task":"Handle the release for the loopback test."}`,
			}}},
			map[string]any{"content": "The remote Nexus agent handled the release."},
		},
	}
	for k, v := range overrides {
		block[k] = v
	}
	return block
}

// a2aLoopbackRemote renders the caller's nexus.agent.a2a_remote block with the
// given per-agent overrides merged in, so a test states only what it changes.
func a2aLoopbackRemote(overrides map[string]any) map[string]any {
	agent := map[string]any{
		"name":        "peer",
		"base_url":    "http://" + a2aLoopbackAddr,
		"description": "A second Nexus instance reachable over A2A.",
		"credentials": map[string]any{
			"type":  "bearer",
			"token": a2aLoopbackToken,
		},
	}
	for k, v := range overrides {
		agent[k] = v
	}
	return map[string]any{
		"cache":   false,
		"timeout": "60s",
		"agents":  []any{agent},
	}
}

// a2aLoopbackListener renders the callee's nexus.io.a2a block with overrides
// merged in. copyConfig replaces a plugin block wholesale, so the unchanged
// keys have to be restated.
func a2aLoopbackListener(overrides map[string]any) map[string]any {
	block := map[string]any{
		"bind":         a2aLoopbackAddr,
		"public_url":   "http://" + a2aLoopbackAddr,
		"bearer_token": a2aLoopbackToken,
		"card": map[string]any{
			"name":        "nexus-loopback-agent",
			"description": "A Nexus instance serving itself over A2A for loopback interop testing.",
			"version":     "0.1.0",
			"skills": []any{map[string]any{
				"id":          "chat",
				"name":        "Conversational turn",
				"description": "Run a single conversational turn against the configured agent loop.",
			}},
		},
	}
	for k, v := range overrides {
		block[k] = v
	}
	return block
}

// a2aLoopbackHistoryContains reports whether any message in the task's history
// carries text.
func a2aLoopbackHistoryContains(task a2a.Task, text string) bool {
	for _, m := range task.History {
		for _, part := range m.Parts {
			if s, ok := part.TextValue(); ok && strings.Contains(s, text) {
				return true
			}
		}
	}
	return false
}
