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

// TestAGUIServe_HITLInterruptResume_Live drives the interrupt/resume cycle
// against a REAL LLM. It skips cleanly when ANTHROPIC_API_KEY is absent so the
// mock suite stays green in CI without a key. The system prompt strongly steers
// the agent to call ask_user, so run 1 ends with an interrupt; the test resumes
// with a choice and asserts run 2 completes on the SAME thread with a NEW runId.
func TestAGUIServe_HITLInterruptResume_Live(t *testing.T) {
	requireAnthropic(t)

	// Strip the mock_responses block and point the provider at the real key.
	cfg := copyConfig(t, "configs/test-agui-hitl.yaml", map[string]any{
		"nexus.io.test": map[string]any{
			"inputs":  []any{},
			"timeout": "90s",
		},
		"nexus.llm.anthropic": map[string]any{
			"api_key_env": "ANTHROPIC_API_KEY",
		},
		"nexus.io.agui": map[string]any{
			"bind": aguiBindAddr,
		},
		"nexus.agent.react": map[string]any{
			"system_prompt": "You are a deployment assistant. You MUST call the " +
				"ask_user tool with mode=\"choices\" to confirm the target environment " +
				"BEFORE doing anything else. Offer exactly two choices: id \"staging\" " +
				"(label \"Staging\") and id \"prod\" (label \"Production\"). Do not " +
				"answer in prose until the user has chosen.",
		},
	})

	bootEngine(t, cfg)
	waitForListener(t, aguiBindAddr)

	c := aguiclient.New("http://" + aguiBindAddr + "/agui")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const threadID = "thread-hitl-live"

	res1, err := c.Run(ctx, aguiclient.UserMessage(threadID, "run-1", "Deploy the app for me."))
	if err != nil {
		t.Fatalf("live run 1: %v", err)
	}
	if res1.StatusCode != http.StatusOK {
		t.Fatalf("live run 1 status = %d, want 200", res1.StatusCode)
	}

	// The agent should have called ask_user, interrupting the run.
	interrupt, ok := res1.Interrupt()
	if !ok {
		t.Fatalf("live run 1 did not interrupt (outcome=%q, events=%v); the model "+
			"did not call ask_user", res1.Outcome(), res1.Types())
	}
	if interrupt.InterruptID == "" {
		t.Fatal("live run 1 interrupt missing interruptId")
	}

	// Resume the SAME thread with a NEW runId, choosing staging.
	res2, err := c.Run(ctx, aguiclient.ResumeInput(threadID, "run-2",
		aguiclient.ResolveChoice(interrupt.InterruptID, "staging", ""),
	))
	if err != nil {
		t.Fatalf("live run 2 (resume): %v", err)
	}
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("live run 2 status = %d, want 200", res2.StatusCode)
	}
	types2 := res2.Types()
	if len(types2) == 0 || types2[len(types2)-1] != agui.EventRunFinished {
		t.Fatalf("live run 2 not terminated by RunFinished: %v", types2)
	}
	if got := res2.Outcome(); got == agui.OutcomeInterrupt {
		t.Fatalf("live run 2 re-interrupted, want a normal finish (events=%v)", types2)
	}
}
