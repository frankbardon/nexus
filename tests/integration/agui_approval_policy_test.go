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

// TestAGUIServe_ApprovalPolicyResume is the #218 regression: an interrupt
// raised by nexus.gate.approval_policy (not ask_user) must resume to
// RUN_FINISHED over AG-UI.
//
// The gate used to emit its hitl.requested with no TurnID. nexus.io.agui
// records the request's TurnID against the interrupt and the continuation run
// adopts it, so the resumed run carried turn "": the turn's content was
// dropped as foreign and its agent.turn.end never finished the stream, which
// stayed open until the client gave up. Run 2 is bounded by its own deadline,
// so a regression fails fast as a context-deadline error rather than a hang.
func TestAGUIServe_ApprovalPolicyResume(t *testing.T) {
	bootEngine(t, "configs/test-agui-approval-policy.yaml")
	waitForListener(t, aguiBindAddr)

	c := aguiclient.New("http://" + aguiBindAddr + "/agui")
	const threadID = "thread-approval"

	// --- Run 1: the gated shell call interrupts the run. ---
	ctx1, cancel1 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel1()
	res1, err := c.Run(ctx1, aguiclient.UserMessage(threadID, "run-1", "Run the command."))
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if res1.StatusCode != http.StatusOK {
		t.Fatalf("run 1 status = %d, want 200", res1.StatusCode)
	}
	types1 := res1.Types()
	if len(types1) == 0 || types1[len(types1)-1] != agui.EventRunFinished {
		t.Fatalf("run 1 did not end on RunFinished (events=%v)", types1)
	}
	if got := res1.Outcome(); got != agui.OutcomeInterrupt {
		t.Fatalf("run 1 outcome = %q, want %q (events=%v)", got, agui.OutcomeInterrupt, types1)
	}
	interrupt, ok := res1.Interrupt()
	if !ok || interrupt.InterruptID == "" {
		t.Fatalf("run 1 missing interrupt anchor (ok=%v)", ok)
	}
	if interrupt.Mode != agui.InterruptModeChoices {
		t.Errorf("interrupt mode = %q, want choices", interrupt.Mode)
	}

	// --- Run 2: approve; the resumed run must reach RunFinished. ---
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	res2, err := c.Run(ctx2, aguiclient.ResumeInput(threadID, "run-2",
		aguiclient.ResolveChoice(interrupt.InterruptID, "allow", ""),
	))
	if err != nil {
		t.Fatalf("run 2 (resume) did not reach RunFinished: %v", err)
	}
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("run 2 status = %d, want 200", res2.StatusCode)
	}
	types2 := res2.Types()
	if len(types2) == 0 || types2[0] != agui.EventRunStarted {
		t.Fatalf("run 2 did not start with RunStarted (events=%v)", types2)
	}
	if last := types2[len(types2)-1]; last != agui.EventRunFinished {
		t.Fatalf("run 2 last event = %s, want RunFinished (events=%v)", last, types2)
	}
	if got := res2.Outcome(); got == agui.OutcomeInterrupt {
		t.Fatalf("run 2 outcome = interrupt, want a normal finish (events=%v)", types2)
	}

	// The approved tool ran on THIS run and the turn's remaining content
	// streamed onto it — the continuation carried the parked turn.
	if got, ok := toolResultContent(res2); !ok || !contains(got, "approved-run") {
		t.Fatalf("run 2 shell result = %q (present=%v), want the approved command's output (events=%v)", got, ok, types2)
	}
	if !streamText(res2, "The approved command ran.") {
		t.Fatalf("run 2 did not stream the continuation content (events=%v)", types2)
	}
}
