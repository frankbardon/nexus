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

// TestAGUIServe_LiveStream drives the AG-UI serve endpoint against a REAL LLM.
// It skips cleanly when ANTHROPIC_API_KEY is absent so the mock suite stays
// green in CI without a key. When a key is present it POSTs a real question and
// asserts a well-bracketed AG-UI stream (RunStarted ... RunFinished) with actual
// assistant text.
func TestAGUIServe_LiveStream(t *testing.T) {
	requireAnthropic(t)

	// Strip the mock_responses block and point the provider at the real key
	// (api_key_env resolves ANTHROPIC_API_KEY). Reuse the same fixed bind port.
	cfg := copyConfig(t, "configs/test-agui-serve.yaml", map[string]any{
		"nexus.io.test": map[string]any{
			"inputs":  []any{},
			"timeout": "60s",
		},
		"nexus.llm.anthropic": map[string]any{
			"api_key_env": "ANTHROPIC_API_KEY",
		},
		"nexus.io.agui": map[string]any{
			"bind": aguiBindAddr,
		},
	})

	bootEngine(t, cfg)
	waitForListener(t, aguiBindAddr)

	c := aguiclient.New("http://" + aguiBindAddr + "/agui")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := c.Run(ctx, aguiclient.UserMessage("thread-live", "run-live",
		"Reply with a single short sentence greeting."))
	if err != nil {
		t.Fatalf("agui live run: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	types := res.Types()
	if len(types) == 0 {
		t.Fatal("no AG-UI events decoded from live stream")
	}
	if types[0] != agui.EventRunStarted {
		t.Fatalf("event[0] = %s, want RunStarted", types[0])
	}
	if last := types[len(types)-1]; last != agui.EventRunFinished {
		t.Fatalf("last event = %s, want RunFinished", last)
	}

	// A real turn must produce assistant text somewhere in the stream, either as
	// a streamed TextMessage triple or a self-contained one.
	var text string
	for _, e := range res.Events {
		if tc, ok := e.(*agui.TextMessageContentEvent); ok {
			text += tc.Delta
		}
	}
	if text == "" {
		t.Fatalf("live stream produced no assistant text: %v", types)
	}
}
