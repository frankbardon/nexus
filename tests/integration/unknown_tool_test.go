//go:build integration

package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/agui/aguiclient"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestUnknownTool_ReactRecovers pins issue #203: a tool call naming a tool no
// plugin handles must not hang the ReAct turn. nexus.tool.catalog answers the
// orphaned tool.invoke with an error tool.result, which is fed back to the model
// on the next request, and the turn completes. Mock mode; no API key.
func TestUnknownTool_ReactRecovers(t *testing.T) {
	h := testharness.New(t, "configs/test-unknown-tool.yaml", testharness.WithTimeout(30*time.Second))
	h.Run()

	h.AssertToolCalled("get_weather")
	h.AssertEventCount("tool.result", 1, 1)

	var result events.ToolResult
	var found bool
	for _, e := range h.Events() {
		if e.Type != "tool.result" {
			continue
		}
		if r, ok := e.Payload.(events.ToolResult); ok {
			result, found = r, true
		}
	}
	if !found {
		t.Fatal("no tool.result payload collected")
	}
	if result.Name != "get_weather" || result.Error != "unknown tool: get_weather" {
		t.Fatalf("tool.result = {Name:%q Error:%q}, want get_weather / unknown tool: get_weather",
			result.Name, result.Error)
	}

	// The error must reach the model on the follow-up request. The mock
	// provider answers at before:llm.request, so that is where the outbound
	// request is observable.
	var fedBack bool
	for _, e := range h.Events() {
		if e.Type != "before:llm.request" {
			continue
		}
		vp, ok := e.Payload.(*engine.VetoablePayload)
		if !ok {
			continue
		}
		req, ok := vp.Original.(*events.LLMRequest)
		if !ok {
			continue
		}
		for _, m := range req.Messages {
			if m.Role == "tool" && m.ToolCallID == result.ID && strings.Contains(m.Content, "unknown tool: get_weather") {
				fedBack = true
			}
		}
	}
	if !fedBack {
		t.Fatal("unknown-tool error was not fed back to the model in a later llm.request")
	}

	h.AssertOutputContains("could not look up the weather")
}

// TestUnknownTool_AGUIRunFinishes pins the AG-UI side of issue #203: a run that
// advertises no client tools, whose agent calls an unhandled tool, must still
// reach RunFinished (with the error tool result streamed) instead of leaving the
// SSE stream open after ToolCallEnd. It reuses the client-tool config, whose
// mock calls get_weather, and simply does not advertise the tool.
func TestUnknownTool_AGUIRunFinishes(t *testing.T) {
	bootEngine(t, "configs/test-agui-clienttool.yaml")
	waitForListener(t, aguiAddr(t))

	c := aguiclient.New("http://" + aguiAddr(t) + "/agui")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := c.Run(ctx, aguiclient.UserMessage("thread-unknown-tool", "run-1", "What is the weather in Paris?"))
	if err != nil {
		t.Fatalf("agui run: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	types := res.Types()
	if len(types) == 0 || types[len(types)-1] != agui.EventRunFinished {
		t.Fatalf("stream did not end at RunFinished: %v", types)
	}
	if got := res.Outcome(); got == agui.OutcomeInterrupt {
		t.Fatalf("run interrupted for a tool it never advertised (events=%v)", types)
	}
	assertOrderedSubsequence(t, types,
		agui.EventToolCallStart,
		agui.EventToolCallEnd,
		agui.EventToolCallResult,
		agui.EventRunFinished,
	)
	var sawError bool
	for _, e := range res.Events {
		if r, ok := e.(*agui.ToolCallResultEvent); ok && strings.Contains(r.Content, "unknown tool: get_weather") {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("ToolCallResult did not carry the unknown-tool error (events=%v)", types)
	}
	if !streamText(res, "sunny in Paris") {
		t.Fatalf("run did not continue to the model's follow-up content (events=%v)", types)
	}
}
