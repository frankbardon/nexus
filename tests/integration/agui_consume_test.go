//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// aguiConsumeToolName is the delegate tool nexus.agent.agui_remote registers for
// the "remote" agent configured in configs/test-agui-consume.yaml.
const aguiConsumeToolName = "delegate_agui_remote"

// collectedToolResult scans harness events for the first tool.result matching
// name and returns its payload.
func collectedToolResult(h *testharness.Harness, name string) (events.ToolResult, bool) {
	for _, e := range h.Events() {
		if e.Type != "tool.result" {
			continue
		}
		tr, ok := e.Payload.(events.ToolResult)
		if ok && tr.Name == name {
			return tr, true
		}
	}
	return events.ToolResult{}, false
}

// TestAGUIConsume_LoopbackServeConsume is the flagship end-to-end proof of the
// consume path: a CALLER Nexus engine whose ReAct agent delegates to a REMOTE
// AG-UI agent that is itself a loopback nexus.io.agui SERVE instance.
//
// Topology:
//
//	caller engine (mock LLM) --delegate_agui_remote--> aguiclient (AG-UI wire)
//	    --> loopback serve engine (mock LLM) --> RunFinished
//
// Both engines run under mock LLM responses, so the test needs no API key and is
// deterministic sub-second. The serve engine is stood up first on the fixed
// loopback port; the caller's ReAct loop is driven (via a scripted mock
// tool_call) to invoke the delegate tool. The assertion is that the remote run's
// terminal outcome ("high noon" mock text) rides back to the caller as the
// delegate tool.result, and that the remote run's mapped observability events
// (subagent.started/complete, io.output) appear on the CALLER's bus.
func TestAGUIConsume_LoopbackServeConsume(t *testing.T) {
	// 1. Stand up the loopback SERVE engine (the "remote" AG-UI agent).
	bootEngine(t, "configs/test-agui-serve.yaml")
	waitForListener(t, aguiBindAddr)

	// 2. Boot the CALLER engine whose react agent delegates to that endpoint.
	cfg := copyConfig(t, "configs/test-agui-consume.yaml", map[string]any{
		"nexus.agent.agui_remote": map[string]any{
			"cache": false,
			"agents": []any{
				map[string]any{
					"name":     "remote",
					"endpoint": "http://" + aguiBindAddr + "/agui",
				},
			},
		},
	})
	h := testharness.New(t, cfg, testharness.WithTimeout(30*time.Second))
	h.Run()

	// The caller's LLM chose to delegate, so the delegate tool was invoked.
	h.AssertToolCalled(aguiConsumeToolName)

	// The remote run's terminal outcome came back as the delegate tool.result.
	tr, ok := collectedToolResult(h, aguiConsumeToolName)
	if !ok {
		t.Fatalf("no tool.result collected for %q", aguiConsumeToolName)
	}
	if tr.Error != "" {
		t.Fatalf("delegate tool.result carried error: %q", tr.Error)
	}
	if !strings.Contains(tr.Output, "high noon") {
		t.Fatalf("delegate tool.result output = %q, want it to include the remote agent's mock text (\"high noon\")", tr.Output)
	}

	// The remote run's mapped observability events appear on the CALLER's bus.
	h.AssertEventEmitted("subagent.started")
	h.AssertEventEmitted("subagent.complete")

	// subagent.complete for this remote run carries the same terminal result,
	// and no error.
	var sawCompleteWithResult bool
	for _, e := range h.Events() {
		if e.Type != "subagent.complete" {
			continue
		}
		sc, ok := e.Payload.(events.SubagentComplete)
		if !ok {
			continue
		}
		if sc.Error == "" && strings.Contains(sc.Result, "high noon") {
			sawCompleteWithResult = true
		}
	}
	if !sawCompleteWithResult {
		t.Fatalf("no subagent.complete carrying the remote result without error")
	}

	// The caller's turn closed with its own final content (proving the react
	// loop consumed the delegate result and continued).
	h.AssertOutputContains("The remote agent reported the time.")
	h.AssertNoSystemOutput()
}

// TestAGUIConsume_UnreachableEndpoint proves the error path through the full
// engine: when the remote AG-UI endpoint is unreachable, the delegate returns a
// clean tool.result carrying an error (not a hang or panic), and the caller's
// react loop still terminates normally.
func TestAGUIConsume_UnreachableEndpoint(t *testing.T) {
	// Point at a loopback port with nothing listening. A short per-call timeout
	// keeps the test fast even though the connection refusal is immediate.
	cfg := copyConfig(t, "configs/test-agui-consume.yaml", map[string]any{
		"nexus.agent.agui_remote": map[string]any{
			"cache":           false,
			"timeout_seconds": 5,
			"agents": []any{
				map[string]any{
					"name":     "remote",
					"endpoint": "http://127.0.0.1:18191/agui", // nothing bound here
				},
			},
		},
	})
	h := testharness.New(t, cfg, testharness.WithTimeout(30*time.Second))
	h.Run()

	h.AssertToolCalled(aguiConsumeToolName)

	tr, ok := collectedToolResult(h, aguiConsumeToolName)
	if !ok {
		t.Fatalf("no tool.result collected for %q — delegate hung instead of failing cleanly", aguiConsumeToolName)
	}
	if tr.Error == "" {
		t.Fatalf("expected a clean delegate error for an unreachable endpoint, got output=%q", tr.Output)
	}
	if !strings.Contains(tr.Error, "agui") {
		t.Fatalf("delegate error = %q, want it to identify the AG-UI transport failure", tr.Error)
	}

	// The failure is surfaced as a subagent.complete carrying the error too.
	var sawErrComplete bool
	for _, e := range h.Events() {
		if e.Type != "subagent.complete" {
			continue
		}
		if sc, ok := e.Payload.(events.SubagentComplete); ok && sc.Error != "" {
			sawErrComplete = true
		}
	}
	if !sawErrComplete {
		t.Fatalf("expected a subagent.complete carrying the transport error")
	}
}

// TestAGUIConsume_BearerRejected proves auth handling end to end: when the
// loopback serve requires a bearer token the caller does not present, the remote
// rejects with 401 and the delegate surfaces a clean error rather than hanging.
func TestAGUIConsume_BearerRejected(t *testing.T) {
	// Serve engine with bearer auth enabled.
	serveCfg := copyConfig(t, "configs/test-agui-serve.yaml", map[string]any{
		"nexus.io.agui": map[string]any{
			"bind":         aguiBindAddr,
			"bearer_token": "s3cret",
		},
	})
	bootEngine(t, serveCfg)
	waitForListener(t, aguiBindAddr)

	// Caller delegates with NO bearer token -> the serve returns 401.
	cfg := copyConfig(t, "configs/test-agui-consume.yaml", map[string]any{
		"nexus.agent.agui_remote": map[string]any{
			"cache": false,
			"agents": []any{
				map[string]any{
					"name":     "remote",
					"endpoint": "http://" + aguiBindAddr + "/agui",
				},
			},
		},
	})
	h := testharness.New(t, cfg, testharness.WithTimeout(30*time.Second))
	h.Run()

	h.AssertToolCalled(aguiConsumeToolName)

	tr, ok := collectedToolResult(h, aguiConsumeToolName)
	if !ok {
		t.Fatalf("no tool.result collected for %q — delegate hung on a 401", aguiConsumeToolName)
	}
	if tr.Error == "" {
		t.Fatalf("expected a clean delegate error for a 401, got output=%q", tr.Output)
	}
	if !strings.Contains(tr.Error, "401") {
		t.Fatalf("delegate error = %q, want it to report the HTTP 401 rejection", tr.Error)
	}
}
