//go:build integration

package integration

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestToolResultClear_FiresAfterTwoTurns drives the agent through two user
// messages: the first triggers a tool call whose result is recorded; the
// second pushes the prior tool result past the age threshold so
// nexus.memory.tool_result_clear replaces its body with the envelope marker
// before the second turn's LLM request goes out.
//
// Asserts that:
//   - memory.tool_result_cleared event fires exactly once
//   - memory.curated envelope event fires for the same layer
//   - the cleared message in the second turn's request carries the
//     <tool_result … cleared="true" …/> marker rather than the raw body
func TestToolResultClear_FiresAfterTwoTurns(t *testing.T) {
	h := testharness.New(t, "configs/test-tool-result-clear.yaml", testharness.WithTimeout(30*time.Second))

	var (
		mu              sync.Mutex
		clearedEvents   []events.MemoryToolResultCleared
		curatedEvents   []events.MemoryCurated
		toolMessageBody []string // tool-role message contents per before:llm.request
	)

	h.Bus().Subscribe("memory.tool_result_cleared", func(e engine.Event[any]) {
		if v, ok := e.Payload.(events.MemoryToolResultCleared); ok {
			mu.Lock()
			clearedEvents = append(clearedEvents, v)
			mu.Unlock()
		}
	})

	h.Bus().Subscribe("memory.curated", func(e engine.Event[any]) {
		if v, ok := e.Payload.(events.MemoryCurated); ok {
			mu.Lock()
			curatedEvents = append(curatedEvents, v)
			mu.Unlock()
		}
	})

	// Capture every tool-role message body across requests so we can verify
	// the envelope marker replaces the original payload on the second turn.
	h.Bus().Subscribe("before:llm.request", func(e engine.Event[any]) {
		vp, ok := e.Payload.(*engine.VetoablePayload)
		if !ok {
			return
		}
		req, ok := vp.Original.(*events.LLMRequest)
		if !ok {
			return
		}
		// Only the agent's main loop carries tool messages; planner /
		// summariser / classifier sub-flows don't.
		if kind, _ := req.Metadata["task_kind"].(string); kind != "react_main" {
			return
		}
		mu.Lock()
		for _, m := range req.Messages {
			if m.Role == "tool" {
				toolMessageBody = append(toolMessageBody, m.Content)
			}
		}
		mu.Unlock()
	}, engine.WithPriority(15)) // after tool_result_clear (priority 12)

	h.Run()

	mu.Lock()
	defer mu.Unlock()

	if len(clearedEvents) != 1 {
		t.Fatalf("expected 1 memory.tool_result_cleared, got %d", len(clearedEvents))
	}
	if got := clearedEvents[0].Tool; got != "shell" {
		t.Errorf("expected cleared tool=shell, got %q", got)
	}

	// memory.curated should fire for the tool_result_clear layer.
	foundCuratedLayer := false
	for _, ev := range curatedEvents {
		if ev.Layer == "tool_result_clear" {
			foundCuratedLayer = true
			break
		}
	}
	if !foundCuratedLayer {
		t.Errorf("expected memory.curated event with Layer=\"tool_result_clear\"; got events: %+v", curatedEvents)
	}

	// One of the captured tool-message bodies must contain the envelope.
	envelopeSeen := false
	for _, body := range toolMessageBody {
		if strings.Contains(body, `cleared="true"`) {
			envelopeSeen = true
			break
		}
	}
	if !envelopeSeen {
		t.Errorf("expected envelope marker in tool-role message body; bodies seen:\n%s",
			strings.Join(toolMessageBody, "\n---\n"))
	}
}
