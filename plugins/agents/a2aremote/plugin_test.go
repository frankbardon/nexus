package a2aremote

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/posture"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

// ---- Harness helpers ----

func boot(t *testing.T, cfg map[string]any) *contract.ContractHarness {
	t.Helper()
	return contract.NewContract(t, New, contract.WithPluginConfig(cfg))
}

func oneAgent(baseURL string, extra map[string]any) map[string]any {
	agent := map[string]any{"name": "researcher", "base_url": baseURL}
	for k, v := range extra {
		agent[k] = v
	}
	return map[string]any{"agents": []any{agent}}
}

// invoke drives one tool call and waits for the NEW tool.result it produces.
// It counts results rather than matching on name, because a test that calls the
// same tool twice must not be handed the first call's answer again.
func invoke(t *testing.T, h *contract.ContractHarness, toolName string, args map[string]any) events.ToolResult {
	t.Helper()
	before := len(toolResults(h))
	h.Inject("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "call-" + toolName,
		Name:          toolName,
		Arguments:     args,
		TurnID:        "turn-1",
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if results := toolResults(h); len(results) > before {
			return results[len(results)-1]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no new tool.result for %q within the deadline", toolName)
	return events.ToolResult{}
}

// toolResults returns every tool.result the plugin emitted, in order. The
// vetoable before:tool.result carries a *ToolResult and is skipped.
func toolResults(h *contract.ContractHarness) []events.ToolResult {
	var out []events.ToolResult
	for _, ev := range h.PluginEmissions() {
		if ev.Type != "tool.result" {
			continue
		}
		if res, ok := ev.Payload.(events.ToolResult); ok {
			out = append(out, res)
		}
	}
	return out
}

func countEmitted(h *contract.ContractHarness, eventType string) int {
	n := 0
	for _, ev := range h.PluginEmissions() {
		if ev.Type == eventType {
			n++
		}
	}
	return n
}

// toolDefs returns every tool.register payload the plugin emitted, in order.
func toolDefs(h *contract.ContractHarness) []events.ToolDef {
	var out []events.ToolDef
	for _, ev := range h.PluginEmissions() {
		if td, ok := ev.Payload.(events.ToolDef); ok {
			out = append(out, td)
		}
	}
	return out
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// deadURL returns a URL nothing is listening on.
func deadURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(nil)
	url := srv.URL
	srv.Close()
	return url
}

// ---- Registration ----

func TestReadyRegistersOneToolPerAgent(t *testing.T) {
	h := boot(t, map[string]any{
		"agents": []any{
			map[string]any{"name": "Deep Research", "base_url": "https://a.internal"},
			map[string]any{"name": "legal", "base_url": "https://b.internal", "tool_name": "ask_legal"},
		},
	})

	defs := toolDefs(h)
	if len(defs) != 2 {
		t.Fatalf("registered %d tools, want 2", len(defs))
	}
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
		if d.Class != toolClass {
			t.Errorf("tool %q class = %q, want %q", d.Name, d.Class, toolClass)
		}
	}
	if !names["delegate_a2a_deep_research"] || !names["ask_legal"] {
		t.Errorf("registered tool names = %v", names)
	}
}

func TestToolSchemaExposesNoEndpointParameter(t *testing.T) {
	h := boot(t, oneAgent("https://a.internal", nil))
	defs := toolDefs(h)
	if len(defs) != 1 {
		t.Fatalf("registered %d tools, want 1", len(defs))
	}
	props, ok := defs[0].Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("tool parameters carry no properties object: %#v", defs[0].Parameters)
	}
	for _, forbidden := range []string{"url", "base_url", "endpoint", "host", "agent_url"} {
		if _, present := props[forbidden]; present {
			t.Errorf("tool schema exposes %q; remotes must come from config only", forbidden)
		}
	}
	if _, present := props["task"]; !present {
		t.Error("tool schema is missing the task parameter")
	}
}

func TestFallbackDescriptionIsUsedBeforeDiscovery(t *testing.T) {
	h := boot(t, oneAgent("https://a.internal", map[string]any{
		"description": "the operator's own words",
	}))
	defs := toolDefs(h)
	if got := defs[0].Description; got != "the operator's own words" {
		t.Errorf("description = %q, want the configured fallback", got)
	}
}

func TestGenericDescriptionWhenNoneConfigured(t *testing.T) {
	h := boot(t, oneAgent("https://a.internal", nil))
	got := toolDefs(h)[0].Description
	if !strings.Contains(got, "researcher") {
		t.Errorf("generic description should name the agent: %q", got)
	}
	if !strings.Contains(got, "has not been contacted yet") {
		t.Errorf("generic description should say the card is unresolved: %q", got)
	}
}

// ---- Lazy discovery ----

func TestAgentCardIsNotFetchedAtBoot(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{})
	boot(t, oneAgent(agent.URL(), nil))

	// Give a hypothetical boot-time fetch a chance to land before asserting it
	// did not happen.
	time.Sleep(50 * time.Millisecond)
	if cards, _ := agent.counts(); cards != 0 {
		t.Fatalf("the agent card was fetched %d times at boot; it must be lazy", cards)
	}
}

func TestUnreachableRemoteDoesNotFailBoot(t *testing.T) {
	// A base URL nothing answers on. Init and Ready must both succeed —
	// NewContract fails the test if either returns an error.
	h := boot(t, oneAgent(deadURL(t), nil))
	if len(toolDefs(h)) != 1 {
		t.Fatal("an unreachable remote should still register its tool")
	}
}

func TestFirstCallResolvesCardAndRefreshesDescription(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{})
	h := boot(t, oneAgent(agent.URL(), map[string]any{"description": "placeholder"}))

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "find things out"})
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}
	if !strings.Contains(res.Output, "the remote's answer") {
		t.Errorf("tool output missing the remote's artifact:\n%s", res.Output)
	}

	waitFor(t, "the refreshed tool description", func() bool {
		return len(toolDefs(h)) >= 2
	})
	defs := toolDefs(h)
	refreshed := defs[len(defs)-1]
	if !strings.Contains(refreshed.Description, "Test Remote") {
		t.Errorf("refreshed description should use the card's name: %q", refreshed.Description)
	}
	if !strings.Contains(refreshed.Description, "deep research") {
		t.Errorf("refreshed description should list the card's skills: %q", refreshed.Description)
	}
	if !strings.Contains(refreshed.Description, "2.1.0") {
		t.Errorf("refreshed description should carry the card's version: %q", refreshed.Description)
	}
	if cards, _ := agent.counts(); cards != 1 {
		t.Errorf("card fetched %d times, want exactly 1", cards)
	}
}

func TestDescriptionIsRefreshedOnlyOnce(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{})
	h := boot(t, map[string]any{
		"cache":  false,
		"agents": []any{map[string]any{"name": "researcher", "base_url": agent.URL()}},
	})

	for i := 0; i < 3; i++ {
		if res := invoke(t, h, "delegate_a2a_researcher", map[string]any{
			"task": "call " + string(rune('a'+i)),
		}); res.Error != "" {
			t.Fatalf("call %d: %s", i, res.Error)
		}
	}
	waitFor(t, "the refreshed description", func() bool { return len(toolDefs(h)) >= 2 })
	time.Sleep(50 * time.Millisecond)
	if got := len(toolDefs(h)); got != 2 {
		t.Errorf("tool registered %d times, want 2 (boot + one refresh)", got)
	}
}

// ---- Failure shapes ----

func TestUnreachableRemoteProducesCleanToolError(t *testing.T) {
	h := boot(t, oneAgent(deadURL(t), nil))
	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})

	if res.Error == "" {
		t.Fatal("an unreachable remote must produce a tool error")
	}
	for _, want := range []string{"researcher", "unreachable", "agent card"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("tool error should mention %q: %s", want, res.Error)
		}
	}
	if strings.Contains(res.Error, "\n") {
		t.Errorf("tool error should be a single actionable sentence: %q", res.Error)
	}
}

func TestCardHTTPErrorIsACleanToolError(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{cardStatus: 503})
	h := boot(t, oneAgent(agent.URL(), nil))
	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})
	if res.Error == "" {
		t.Fatal("a 503 on the card must produce a tool error")
	}
	if !strings.Contains(res.Error, "unreachable") {
		t.Errorf("tool error = %q", res.Error)
	}
}

func TestFailedRemoteTaskIsAToolError(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{
		frames: func(*a2a.SendMessageRequest) []a2a.StreamResponse {
			return failedRun("t1", "c1", "the source database was offline")
		},
	})
	h := boot(t, oneAgent(agent.URL(), nil))
	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})

	if res.Error == "" {
		t.Fatal("a FAILED task must produce a tool error")
	}
	if !strings.Contains(res.Error, "TASK_STATE_FAILED") {
		t.Errorf("tool error should name the terminal state: %s", res.Error)
	}
	if !strings.Contains(res.Error, "source database was offline") {
		t.Errorf("tool error should carry the remote's explanation: %s", res.Error)
	}
}

func TestInterruptedRemoteTaskIsACleanToolError(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{
		frames: func(*a2a.SendMessageRequest) []a2a.StreamResponse {
			return interruptedRun("t1", "c1", "which fiscal year?")
		},
	})
	h := boot(t, oneAgent(agent.URL(), nil))
	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})

	if res.Error == "" {
		t.Fatal("a parked task must produce a tool error the caller can act on")
	}
	if !strings.Contains(res.Error, "which fiscal year?") {
		t.Errorf("tool error should carry the remote's question: %s", res.Error)
	}
	if !strings.Contains(res.Error, "Re-delegate") {
		t.Errorf("tool error should tell the model what to do next: %s", res.Error)
	}
}

func TestMissingTaskArgumentIsACleanToolError(t *testing.T) {
	h := boot(t, oneAgent("https://a.internal", nil))
	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{})
	if !strings.Contains(res.Error, "task is required") {
		t.Errorf("tool error = %q", res.Error)
	}
}

func TestUnknownToolIsIgnored(t *testing.T) {
	h := boot(t, oneAgent("https://a.internal", nil))
	h.Inject("tool.invoke", events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		ID:            "x",
		Name:          "some_other_tool",
		Arguments:     map[string]any{"task": "hi"},
	})
	time.Sleep(50 * time.Millisecond)
	if n := countEmitted(h, "tool.result"); n != 0 {
		t.Errorf("plugin answered a tool it does not own (%d results)", n)
	}
}

// ---- Caching ----

func TestSuccessfulCallsAreCachedAndFailuresAreNot(t *testing.T) {
	fail := true
	agent := newTestAgent(t, testAgentConfig{
		frames: func(*a2a.SendMessageRequest) []a2a.StreamResponse {
			if fail {
				return failedRun("t1", "c1", "transient")
			}
			return completedRun("t1", "c1", "eventual answer")
		},
	})
	h := boot(t, oneAgent(agent.URL(), nil))
	args := map[string]any{"task": "the same task"}

	if res := invoke(t, h, "delegate_a2a_researcher", args); res.Error == "" {
		t.Fatal("expected the first call to fail")
	}
	_, sendsAfterFailure := agent.counts()

	// A failure must NOT be cached: the retry has to reach the remote again.
	fail = false
	res := invoke(t, h, "delegate_a2a_researcher", args)
	if res.Error != "" {
		t.Fatalf("the retry should have succeeded: %s", res.Error)
	}
	_, sendsAfterRetry := agent.counts()
	if sendsAfterRetry <= sendsAfterFailure {
		t.Fatal("a failed outcome was cached; the retry never reached the remote")
	}

	// The success must be cached: an identical third call does not.
	if res := invoke(t, h, "delegate_a2a_researcher", args); res.Error != "" {
		t.Fatalf("third call: %s", res.Error)
	}
	_, sendsAfterCacheHit := agent.counts()
	if sendsAfterCacheHit != sendsAfterRetry {
		t.Errorf("a repeated successful call re-hit the remote (%d -> %d)",
			sendsAfterRetry, sendsAfterCacheHit)
	}
}

func TestCacheCanBeDisabled(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{})
	h := boot(t, map[string]any{
		"cache":  false,
		"agents": []any{map[string]any{"name": "researcher", "base_url": agent.URL()}},
	})
	args := map[string]any{"task": "the same task"}

	invoke(t, h, "delegate_a2a_researcher", args)
	_, first := agent.counts()
	invoke(t, h, "delegate_a2a_researcher", args)
	_, second := agent.counts()

	if second <= first {
		t.Error("cache: false should send every call to the remote")
	}
}

func TestDifferentContextsAreDifferentCacheKeys(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{})
	h := boot(t, oneAgent(agent.URL(), nil))

	invoke(t, h, "delegate_a2a_researcher", map[string]any{
		"task": "same", "context": map[string]any{"year": float64(2024)},
	})
	_, first := agent.counts()
	invoke(t, h, "delegate_a2a_researcher", map[string]any{
		"task": "same", "context": map[string]any{"year": float64(2025)},
	})
	_, second := agent.counts()

	if second <= first {
		t.Error("a different context must miss the cache")
	}
}

// ---- Transport selection ----

func TestNonStreamingCardFallsBackToBlockingSend(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{noStreaming: true})
	h := boot(t, oneAgent(agent.URL(), nil))

	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})
	if res.Error != "" {
		t.Fatalf("unexpected tool error: %s", res.Error)
	}
	if !strings.Contains(res.Output, "blocking reply") {
		t.Errorf("expected the blocking SendMessage reply:\n%s", res.Output)
	}
	if !strings.Contains(string(agent.lastBody()), a2a.MethodSendMessage) {
		t.Errorf("expected a %s call, body was: %s", a2a.MethodSendMessage, agent.lastBody())
	}
}

func TestContextRidesInsideAnXMLBoundary(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{})
	h := boot(t, oneAgent(agent.URL(), nil))

	invoke(t, h, "delegate_a2a_researcher", map[string]any{
		"task":    "summarize",
		"context": map[string]any{"doc": "quarterly report"},
	})

	call, protoErr := a2a.DecodeCall(agent.lastBody())
	if protoErr != nil {
		t.Fatalf("decode outbound call: %v", protoErr)
	}
	req, ok := call.Params.(*a2a.SendMessageRequest)
	if !ok {
		t.Fatalf("outbound call params = %T", call.Params)
	}
	text, _ := req.Message.Parts[0].TextValue()
	for _, want := range []string{"<delegate_context>", "</delegate_context>", "<task>", "quarterly report"} {
		if !strings.Contains(text, want) {
			t.Errorf("outbound message missing %q:\n%s", want, text)
		}
	}
}

// ---- Budgets and depth ----

func TestDepthLimitProducesACleanToolError(t *testing.T) {
	h := boot(t, map[string]any{
		"max_depth": 2,
		"agents":    []any{map[string]any{"name": "researcher", "base_url": "https://a.internal"}},
	})
	p := h.Plugin().(*Plugin)
	out := p.runRemote(context.Background(), p.remotes["delegate_a2a_researcher"], invocation{
		task:        "anything",
		parentDepth: 2,
	})
	if out.err == "" {
		t.Fatal("exceeding max_depth must produce an error")
	}
	if !strings.Contains(out.err, "depth limit") {
		t.Errorf("error = %q", out.err)
	}
}

func TestPostureSuppliesTimeoutAndNarrowsDepth(t *testing.T) {
	h := boot(t, map[string]any{
		"max_depth": 5,
		"agents": []any{map[string]any{
			"name": "researcher", "base_url": "https://a.internal", "posture": "careful",
		}},
	})
	p := h.Plugin().(*Plugin)

	reg := posture.NewRegistry()
	if err := reg.Register(posture.AgentPosture{
		Name:              "careful",
		DefaultBudget:     posture.ResourceBudget{Timeout: 42 * time.Second},
		MaxRecursionDepth: 2,
	}); err != nil {
		t.Fatalf("register posture: %v", err)
	}
	p.postures = reg

	bud, err := p.resolveBudget(p.remotes["delegate_a2a_researcher"])
	if err != nil {
		t.Fatalf("resolveBudget: %v", err)
	}
	if bud.timeout != 42*time.Second {
		t.Errorf("timeout = %s, want the posture's 42s", bud.timeout)
	}
	if bud.maxDepth != 2 {
		t.Errorf("maxDepth = %d, want the posture's 2", bud.maxDepth)
	}
	if bud.postureVer == "" {
		t.Error("the posture version must reach the cache key")
	}
}

func TestPostureWithUnenforceableBudgetIsRefused(t *testing.T) {
	h := boot(t, map[string]any{
		"agents": []any{map[string]any{
			"name": "researcher", "base_url": "https://a.internal", "posture": "spendy",
		}},
	})
	p := h.Plugin().(*Plugin)

	reg := posture.NewRegistry()
	if err := reg.Register(posture.AgentPosture{
		Name:          "spendy",
		DefaultBudget: posture.ResourceBudget{MaxTokens: 1000},
	}); err != nil {
		t.Fatalf("register posture: %v", err)
	}
	p.postures = reg

	if _, err := p.resolveBudget(p.remotes["delegate_a2a_researcher"]); err == nil {
		t.Fatal("a posture bounding tokens must be refused, not silently ignored")
	} else if !strings.Contains(err.Error(), "max_tokens") {
		t.Errorf("error = %q", err)
	}
}

func TestMissingPostureRegistryFailsTheCallNotTheBoot(t *testing.T) {
	// The contract harness supplies no capability providers, so the registry is
	// absent. Init and Ready still succeed.
	h := boot(t, oneAgent("https://a.internal", map[string]any{"posture": "careful"}))
	res := invoke(t, h, "delegate_a2a_researcher", map[string]any{"task": "anything"})
	if res.Error == "" {
		t.Fatal("naming an unavailable posture must fail the call")
	}
	if !strings.Contains(res.Error, "nexus.agent.postures") {
		t.Errorf("the error should name the plugin to activate: %s", res.Error)
	}
}

func TestPerCallTimeoutOverridesTheBudget(t *testing.T) {
	agent := newTestAgent(t, testAgentConfig{frameDelay: 300 * time.Millisecond})
	h := boot(t, oneAgent(agent.URL(), nil))
	p := h.Plugin().(*Plugin)

	out := p.runRemote(context.Background(), p.remotes["delegate_a2a_researcher"], invocation{
		task:    "slow work",
		timeout: 150 * time.Millisecond,
	})
	if out.err == "" {
		t.Fatal("a call that outran its budget must report it")
	}
	if !strings.Contains(out.err, "150ms") && !strings.Contains(out.err, "cancelled") {
		t.Errorf("error should name the budget that fired: %q", out.err)
	}
}
