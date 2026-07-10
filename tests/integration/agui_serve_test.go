//go:build integration

package integration

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/agui/aguiclient"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/allplugins"
)

// aguiBindAddr is the loopback address configured in configs/test-agui-serve.yaml
// (and its live-mode override). Integration tests run sequentially, so a fixed
// high port is safe.
const aguiBindAddr = "127.0.0.1:18190"

// bootEngine boots a real engine from configPath with every plugin registered
// (the same registry cmd/nexus and the test harness use) and returns it with a
// cleanup that stops it. Unlike testharness.Run it does NOT block on the test IO
// plugin's Done channel — the AG-UI POST is the driver here, not scripted io.test
// inputs, so the caller controls the lifecycle.
func bootEngine(t *testing.T, configPath string) *engine.Engine {
	t.Helper()

	abs := configPath
	if !filepath.IsAbs(configPath) {
		abs = filepath.Join(findRoot(t), configPath)
	}
	eng, err := engine.New(abs)
	if err != nil {
		t.Fatalf("engine.New(%s): %v", configPath, err)
	}
	allplugins.RegisterAll(eng.Registry)

	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("engine.Boot: %v", err)
	}
	t.Cleanup(func() {
		_ = eng.Stop(context.Background())
	})
	return eng
}

// waitForListener blocks until addr accepts a TCP connection or the deadline
// elapses.
func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("agui listener %s did not come up", addr)
}

// assertOrderedSubsequence fails when want does not appear as an ordered
// subsequence of got (other event types in between are allowed).
func assertOrderedSubsequence(t *testing.T, got []agui.EventType, want ...agui.EventType) {
	t.Helper()
	idx := 0
	for _, g := range got {
		if idx < len(want) && g == want[idx] {
			idx++
		}
	}
	if idx != len(want) {
		t.Fatalf("event stream %v does not contain ordered subsequence %v (matched %d/%d)",
			got, want, idx, len(want))
	}
}

// TestAGUIServe_MockStream drives the AG-UI serve endpoint end to end with
// mocked LLM responses (no API key). It POSTs a RunAgentInput through the pure-Go
// conformance client and asserts the ordered canonical AG-UI event sequence:
// RunStarted first, a step opens, the agent's text output rides a TextMessage
// triple, a tool result surfaces, the step closes, and RunFinished terminates
// the stream.
func TestAGUIServe_MockStream(t *testing.T) {
	bootEngine(t, "configs/test-agui-serve.yaml")
	waitForListener(t, aguiBindAddr)

	c := aguiclient.New("http://" + aguiBindAddr + "/agui")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := c.Run(ctx, aguiclient.UserMessage("thread-mock", "run-mock", "What time is it?"))
	if err != nil {
		t.Fatalf("agui run: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != agui.ContentType {
		t.Fatalf("content-type = %q, want %q", ct, agui.ContentType)
	}

	types := res.Types()
	if len(types) == 0 {
		t.Fatal("no AG-UI events decoded")
	}

	// The stream must be well-bracketed.
	if types[0] != agui.EventRunStarted {
		t.Fatalf("event[0] = %s, want RunStarted", types[0])
	}
	if last := types[len(types)-1]; last != agui.EventRunFinished {
		t.Fatalf("last event = %s, want RunFinished", last)
	}

	// Ordered canonical lifecycle: run opens, a step opens, text streams as a
	// start/content/end triple, and the run finishes.
	assertOrderedSubsequence(t, types,
		agui.EventRunStarted,
		agui.EventStepStarted,
		agui.EventTextMessageStart,
		agui.EventTextMessageContent,
		agui.EventTextMessageEnd,
		agui.EventStepFinished,
		agui.EventRunFinished,
	)

	// The agent's tool call (mock get_time) surfaces its result. The react loop
	// emits tool.result even when the tool is unregistered, so the AG-UI
	// ToolCallResult event is deterministic without a real tool backend.
	if res.Count(agui.EventToolCallResult) == 0 {
		t.Fatalf("no ToolCallResult in stream: %v", types)
	}

	// The final assistant text must carry the mock response content.
	var finalText string
	for _, e := range res.Events {
		if tc, ok := e.(*agui.TextMessageContentEvent); ok {
			finalText += tc.Delta
		}
	}
	if finalText == "" {
		t.Fatal("no TextMessageContent delta captured")
	}
	if !contains(finalText, "high noon") {
		t.Fatalf("final text = %q, want it to include the mock content", finalText)
	}
}

// TestAGUIServe_MockBearerRejected asserts the serve endpoint rejects a missing
// bearer token when auth is configured, exercised through the conformance
// client. A rejected request yields a 401 and no SSE event stream.
func TestAGUIServe_MockBearerRejected(t *testing.T) {
	cfg := copyConfig(t, "configs/test-agui-serve.yaml", map[string]any{
		"nexus.io.agui": map[string]any{
			"bind":         aguiBindAddr,
			"bearer_token": "s3cret",
		},
	})
	bootEngine(t, cfg)
	waitForListener(t, aguiBindAddr)

	url := "http://" + aguiBindAddr + "/agui"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Missing token -> 401, no events.
	res, err := aguiclient.New(url).Run(ctx, aguiclient.UserMessage("t", "r", "hi"))
	if err != nil {
		t.Fatalf("run (no token): %v", err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", res.StatusCode)
	}
	if res.Events != nil {
		t.Fatalf("no-token events = %v, want none", res.Types())
	}

	// Wrong token -> 401.
	res, err = aguiclient.New(url, aguiclient.WithBearer("wrong")).Run(ctx, aguiclient.UserMessage("t", "r", "hi"))
	if err != nil {
		t.Fatalf("run (wrong token): %v", err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-token status = %d, want 401", res.StatusCode)
	}

	// Correct token -> 200 with a terminated stream.
	res, err = aguiclient.New(url, aguiclient.WithBearer("s3cret")).Run(ctx, aguiclient.UserMessage("t", "r", "hi"))
	if err != nil {
		t.Fatalf("run (valid token): %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("valid-token status = %d, want 200", res.StatusCode)
	}
	if res.First(agui.EventRunFinished) == nil && res.First(agui.EventRunError) == nil {
		t.Fatalf("authorized stream not terminated: %v", res.Types())
	}
}

// contains is a tiny substring helper (avoids importing strings just for one
// use in table tests).
func contains(haystack, needle string) bool {
	return len(needle) == 0 || indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
