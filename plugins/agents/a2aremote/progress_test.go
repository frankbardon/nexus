package a2aremote

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// republished collects the io.output contents observed on the harness bus.
func republished(h *contract.ContractHarness) []string {
	var out []string
	for _, ev := range h.PluginEmissions() {
		if ev.Type != "io.output" {
			continue
		}
		if o, ok := ev.Payload.(events.AgentOutput); ok {
			out = append(out, o.Content)
		}
	}
	return out
}

// iterations collects the subagent.iteration payloads observed on the bus.
func iterations(h *contract.ContractHarness) []events.SubagentIteration {
	var out []events.SubagentIteration
	for _, ev := range h.PluginEmissions() {
		if ev.Type != "subagent.iteration" {
			continue
		}
		if it, ok := ev.Payload.(events.SubagentIteration); ok {
			out = append(out, it)
		}
	}
	return out
}

// TestRemoteNarrationIsRepublishedAsOutput covers the extension-free progress
// channel: a WORKING status carrying a message is the remote narrating, and it
// becomes local output so a transport can show that something is happening.
func TestRemoteNarrationIsRepublishedAsOutput(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{
		frames: func(*a2a.SendMessageRequest) []a2a.StreamResponse {
			return narratedRun("t1", "c1", "reading 40 sources", "the answer")
		},
	})
	h := boot(t, oneAgent(agent.URL(), nil))

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}

	got := republished(h)
	if len(got) != 1 || !strings.Contains(got[0], "reading 40 sources") {
		t.Fatalf("republished output = %v, want the remote's narration", got)
	}

	// The turn id groups the remote's output separately from the local turn
	// that asked for it.
	for _, ev := range h.PluginEmissions() {
		if ev.Type != "io.output" {
			continue
		}
		o := ev.Payload.(events.AgentOutput)
		if !strings.HasPrefix(o.TurnID, "a2a_remote_") {
			t.Errorf("io.output turn id = %q, want the delegated run's own turn", o.TurnID)
		}
	}
}

// TestTerminalStatusIsNotRepublished: the closing message is the answer, and the
// answer belongs in the tool result, not in the local conversation ahead of it.
func TestTerminalStatusIsNotRepublished(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{
		frames: func(*a2a.SendMessageRequest) []a2a.StreamResponse {
			status := a2a.NewTaskStatus(a2a.TaskStateCompleted).
				WithMessage(a2a.NewAgentMessage("m-done", "here is the final answer"))
			return []a2a.StreamResponse{
				a2a.StreamTask(a2a.NewTask("t1", "c1")),
				a2a.StreamStatusUpdate(a2a.NewStatusUpdate("t1", "c1", status)),
			}
		},
	})
	h := boot(t, oneAgent(agent.URL(), nil))

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})
	if !strings.Contains(res.Output, "here is the final answer") {
		t.Fatalf("the terminal message should reach the model as the result: %s", res.Output)
	}
	if got := republished(h); len(got) != 0 {
		t.Errorf("terminal status republished as output: %v", got)
	}
}

// TestRemoteQuestionIsNotRepublishedAsOutput: an INPUT_REQUIRED message is a
// question for a human, not progress. Showing it as output too would put it in
// front of the delegating model, which is what the chained path exists to avoid.
func TestRemoteQuestionIsNotRepublishedAsOutput(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{
		frames: askThenAnswer("t1", "c1", "which fiscal year?", &resumeRecorder{}),
	})
	h := boot(t, oneAgent(agent.URL(), nil))
	answerHITL(t, h, "FY2026")

	invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})

	for _, content := range republished(h) {
		if strings.Contains(content, "which fiscal year?") {
			t.Errorf("the remote's question was republished as output: %q", content)
		}
	}
}

// TestRemoteTelemetryBecomesSubagentIterations covers the Nexus extension path:
// a remote Nexus instance's own tool calls arrive as extension metadata and are
// republished as subagent iterations, which is what the local transports already
// know how to render.
func TestRemoteTelemetryBecomesSubagentIterations(t *testing.T) {
	event := a2a.ToolCallEvent("t1", "c1", a2a.NexusToolCall{
		CallID:    "call-1",
		Name:      "web_search",
		Arguments: json.RawMessage(`{"query":"nexus a2a"}`),
	}).From("tool.invoke")

	agent := newTestAgent(t, testAgentConfig{
		frames: func(*a2a.SendMessageRequest) []a2a.StreamResponse {
			return telemetryRun(t, "t1", "c1", event)
		},
	})
	h := boot(t, oneAgent(agent.URL(), nil))

	if res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"}); res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}

	its := iterations(h)
	if len(its) != 1 {
		t.Fatalf("republished %d iterations, want 1", len(its))
	}
	if len(its[0].ToolCalls) != 1 || its[0].ToolCalls[0].Name != "web_search" {
		t.Fatalf("iteration did not carry the remote's tool call: %+v", its[0])
	}
	if !strings.Contains(its[0].ToolCalls[0].Arguments, "nexus a2a") {
		t.Errorf("the tool call's arguments should survive: %q", its[0].ToolCalls[0].Arguments)
	}
}

// TestRemoteThinkingIsNotRepublished: reasoning belongs in the remote's own
// transcript, and token accounting is the remote's spend under the remote's
// budget. Surfacing either locally would misattribute it.
func TestRemoteThinkingIsNotRepublished(t *testing.T) {
	event := a2a.ThinkingEvent("t1", "c1", a2a.NexusThinking{
		Step: 1, Content: "the remote's private reasoning",
	}).From("thinking.step")

	agent := newTestAgent(t, testAgentConfig{
		frames: func(*a2a.SendMessageRequest) []a2a.StreamResponse {
			return telemetryRun(t, "t1", "c1", event)
		},
	})
	h := boot(t, oneAgent(agent.URL(), nil))
	invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})

	if got := iterations(h); len(got) != 0 {
		t.Errorf("thinking telemetry was republished: %+v", got)
	}
	for _, content := range republished(h) {
		if strings.Contains(content, "private reasoning") {
			t.Errorf("thinking telemetry reached io.output: %q", content)
		}
	}
}

// TestProgressCanBeSilenced: an operator with a chatty remote can turn the
// republishing off without losing the task identity the cancel and resume paths
// depend on.
func TestProgressCanBeSilenced(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{
		frames: func(*a2a.SendMessageRequest) []a2a.StreamResponse {
			return narratedRun("t1", "c1", "reading 40 sources", "the answer")
		},
	})
	h := boot(t, oneAgent(agent.URL(), map[string]any{"progress": false}))

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}
	h.AssertNotEmitted("io.output")
	if !strings.Contains(res.Output, "the answer") {
		t.Errorf("silencing progress must not change the result: %s", res.Output)
	}
}

// TestNexusExtensionIsRequestedByDefault pins the default this story chose. The
// plugin is now the consumer of a remote Nexus instance's telemetry, and a
// client that does not ask for the extension is handed a stream with none of it.
func TestNexusExtensionIsRequestedByDefault(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{})
	h := boot(t, oneAgent(agent.URL(), nil))
	invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})

	asked := false
	for _, header := range agent.seenHeaders() {
		for _, uri := range a2a.ParseExtensions(header.Values(a2a.HeaderExtensions)...) {
			if uri == a2a.NexusExtensionURI {
				asked = true
			}
		}
	}
	if !asked {
		t.Errorf("the Nexus extension should be requested by default; headers = %v", agent.seenHeaders())
	}
}

// TestExtensionsCanBeCleared: the default is a default, not a policy.
func TestExtensionsCanBeCleared(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{})
	h := boot(t, oneAgent(agent.URL(), map[string]any{"extensions": []any{}}))
	invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})

	for _, header := range agent.seenHeaders() {
		if len(a2a.ParseExtensions(header.Values(a2a.HeaderExtensions)...)) != 0 {
			t.Errorf("an empty extensions list must send none; got %v", header.Values(a2a.HeaderExtensions))
		}
	}
}
