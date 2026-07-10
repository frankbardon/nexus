//go:build integration

package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/agui/aguiclient"
)

// TestAGUIServe_HITLInterruptResume proves the virtual-run interrupt/resume model
// end to end through the pure-Go conformance client, with mocked LLM responses
// (no API key). ONE Nexus turn spans TWO AG-UI runs:
//
//	Run 1 (same threadId): the mock agent calls ask_user. The nexus.control.hitl
//	plugin emits hitl.requested and parks the in-process agent. The AG-UI plugin
//	ends run 1 per the terminal-run model — StateSnapshot + MessagesSnapshot +
//	RunFinished(interrupt) — WITHOUT unblocking the agent.
//
//	Run 2 (same threadId, NEW runId): a continuation RunAgentInput carrying
//	resume[] (status resolved, choice payload) resolves the interrupt. The plugin
//	emits hitl.responded, the parked agent unblocks, consumes the next mock
//	response, and run 2 completes normally.
func TestAGUIServe_HITLInterruptResume(t *testing.T) {
	bootEngine(t, "configs/test-agui-hitl.yaml")
	waitForListener(t, aguiBindAddr)

	c := aguiclient.New("http://" + aguiBindAddr + "/agui")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const threadID = "thread-hitl"

	// --- Run 1: ask_user interrupts the run. ---
	res1, err := c.Run(ctx, aguiclient.UserMessage(threadID, "run-1", "Deploy the app."))
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if res1.StatusCode != http.StatusOK {
		t.Fatalf("run 1 status = %d, want 200", res1.StatusCode)
	}

	types1 := res1.Types()
	if len(types1) == 0 {
		t.Fatal("run 1 produced no events")
	}
	if types1[0] != agui.EventRunStarted {
		t.Fatalf("run 1 event[0] = %s, want RunStarted", types1[0])
	}

	// The terminal-run interrupt model: StateSnapshot then MessagesSnapshot then
	// RunFinished, in that order, ending the stream.
	assertOrderedSubsequence(t, types1,
		agui.EventStateSnapshot,
		agui.EventMessagesSnapshot,
		agui.EventRunFinished,
	)
	if last := types1[len(types1)-1]; last != agui.EventRunFinished {
		t.Fatalf("run 1 last event = %s, want RunFinished", last)
	}

	// Run 1 must end on an interrupt outcome carrying the anchor for the resume.
	if got := res1.Outcome(); got != agui.OutcomeInterrupt {
		t.Fatalf("run 1 outcome = %q, want %q", got, agui.OutcomeInterrupt)
	}
	interrupt, ok := res1.Interrupt()
	if !ok {
		t.Fatal("run 1 RunFinished did not carry an interrupt payload")
	}
	if interrupt.InterruptID == "" {
		t.Fatal("run 1 interrupt missing interruptId")
	}
	if interrupt.Mode != agui.InterruptModeChoices {
		t.Errorf("interrupt mode = %q, want choices", interrupt.Mode)
	}
	if len(interrupt.Choices) != 2 {
		t.Errorf("interrupt choices = %d, want 2 (staging, prod)", len(interrupt.Choices))
	}

	// --- Run 2: resume the SAME thread with a NEW runId. ---
	res2, err := c.Run(ctx, aguiclient.ResumeInput(threadID, "run-2",
		aguiclient.ResolveChoice(interrupt.InterruptID, "staging", ""),
	))
	if err != nil {
		t.Fatalf("run 2 (resume): %v", err)
	}
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("run 2 status = %d, want 200", res2.StatusCode)
	}

	types2 := res2.Types()
	if len(types2) == 0 {
		t.Fatal("run 2 produced no events")
	}
	if types2[0] != agui.EventRunStarted {
		t.Fatalf("run 2 event[0] = %s, want RunStarted", types2[0])
	}
	if last := types2[len(types2)-1]; last != agui.EventRunFinished {
		t.Fatalf("run 2 last event = %s, want RunFinished", last)
	}
	// Run 2 completes normally — it must NOT re-interrupt.
	if got := res2.Outcome(); got == agui.OutcomeInterrupt {
		t.Fatalf("run 2 outcome = interrupt, want a normal finish (events=%v)", types2)
	}

	// The continuation's final assistant text must carry the post-resume mock
	// content, proving the parked agent unblocked and finished the turn.
	if !streamText(res2, "Deploying to staging") {
		t.Fatalf("run 2 did not stream the continuation content (events=%v)", types2)
	}
}

// TestAGUIServe_HITLResumeCancelled covers the cancel path: the client abandons
// the ask_user interrupt with status "cancelled". The parked agent unblocks with
// a cancellation (not an answer) and the continuation run still completes — the
// turn does not hang on the retracted interrupt.
func TestAGUIServe_HITLResumeCancelled(t *testing.T) {
	bootEngine(t, "configs/test-agui-hitl.yaml")
	waitForListener(t, aguiBindAddr)

	c := aguiclient.New("http://" + aguiBindAddr + "/agui")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const threadID = "thread-hitl-cancel"

	res1, err := c.Run(ctx, aguiclient.UserMessage(threadID, "run-1", "Deploy the app."))
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if got := res1.Outcome(); got != agui.OutcomeInterrupt {
		t.Fatalf("run 1 outcome = %q, want interrupt", got)
	}
	interrupt, ok := res1.Interrupt()
	if !ok || interrupt.InterruptID == "" {
		t.Fatalf("run 1 missing interrupt anchor (ok=%v)", ok)
	}

	// Resume with a cancellation rather than an answer.
	res2, err := c.Run(ctx, aguiclient.ResumeInput(threadID, "run-2",
		aguiclient.Cancel(interrupt.InterruptID),
	))
	if err != nil {
		t.Fatalf("run 2 (cancel resume): %v", err)
	}
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("run 2 status = %d, want 200", res2.StatusCode)
	}
	types2 := res2.Types()
	if last := types2[len(types2)-1]; last != agui.EventRunFinished {
		t.Fatalf("run 2 last event = %s, want RunFinished", last)
	}
	// The continuation must resolve (not re-interrupt) so the turn never hangs on
	// the retracted interrupt.
	if got := res2.Outcome(); got == agui.OutcomeInterrupt {
		t.Fatalf("run 2 outcome = interrupt on a cancel resume, want a terminal finish (events=%v)", types2)
	}
}

// TestAGUIServe_ClientToolRoundTrip proves the client-executed (frontend) tool
// round-trip end to end. A RunAgentInput advertises a client tool; the mock
// agent calls it; run 1 ends interrupt-style with the ToolCall* sequence (there
// is no in-process handler to produce the result); the client resumes with a
// ToolCallResult and run 2 completes.
func TestAGUIServe_ClientToolRoundTrip(t *testing.T) {
	bootEngine(t, "configs/test-agui-clienttool.yaml")
	waitForListener(t, aguiBindAddr)

	c := aguiclient.New("http://" + aguiBindAddr + "/agui")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const threadID = "thread-clienttool"

	// --- Run 1: advertise the client tool; the agent calls it and the run
	// suspends awaiting the client's result. ---
	input := agui.RunAgentInput{
		ThreadID: threadID,
		RunID:    "run-1",
		Messages: []agui.Message{{ID: "m1", Role: "user", Content: "What is the weather in Paris?"}},
		Tools: []agui.Tool{{
			Name:        "get_weather",
			Description: "Get the current weather for a city.",
			Parameters:  []byte(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	}
	res1, err := c.Run(ctx, input)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if res1.StatusCode != http.StatusOK {
		t.Fatalf("run 1 status = %d, want 200", res1.StatusCode)
	}

	types1 := res1.Types()
	// The client tool's ToolCall* sequence must have streamed before the
	// interrupt, so the client knows which call to execute.
	assertOrderedSubsequence(t, types1,
		agui.EventToolCallStart,
		agui.EventToolCallArgs,
		agui.EventToolCallEnd,
		agui.EventRunFinished,
	)
	if got := res1.Outcome(); got != agui.OutcomeInterrupt {
		t.Fatalf("run 1 outcome = %q, want interrupt (events=%v)", got, types1)
	}
	interrupt, ok := res1.Interrupt()
	if !ok || interrupt.InterruptID == "" {
		t.Fatalf("run 1 missing client-tool interrupt anchor (ok=%v)", ok)
	}

	// --- Run 2: resume with the client's ToolCallResult; the agent continues. ---
	res2, err := c.Run(ctx, aguiclient.ResumeInput(threadID, "run-2",
		aguiclient.ResolveToolResult(interrupt.InterruptID, "sunny, 24C", ""),
	))
	if err != nil {
		t.Fatalf("run 2 (tool-result resume): %v", err)
	}
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("run 2 status = %d, want 200", res2.StatusCode)
	}
	types2 := res2.Types()
	if last := types2[len(types2)-1]; last != agui.EventRunFinished {
		t.Fatalf("run 2 last event = %s, want RunFinished", last)
	}
	if got := res2.Outcome(); got == agui.OutcomeInterrupt {
		t.Fatalf("run 2 re-interrupted, want a normal finish (events=%v)", types2)
	}
	if !streamText(res2, "sunny in Paris") {
		t.Fatalf("run 2 did not stream the continuation content (events=%v)", types2)
	}
}

// streamText reports whether the concatenated TextMessageContent deltas of the
// result contain the given substring.
func streamText(res aguiclient.Result, want string) bool {
	var text string
	for _, e := range res.Events {
		if tc, ok := e.(*agui.TextMessageContentEvent); ok {
			text += tc.Delta
		}
	}
	return contains(text, want)
}
