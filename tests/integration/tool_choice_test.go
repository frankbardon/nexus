//go:build integration

package integration

import (
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestToolChoice_Boot validates the ReAct agent boots with a tool_choice
// sequence config attached.
func TestToolChoice_Boot(t *testing.T) {
	h := testharness.New(t, "configs/test-tool-choice.yaml", testharness.WithTimeout(30*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.agent.react",
		"nexus.tool.shell",
		"nexus.tool.file",
	)
}

// TestToolChoice_SequenceCyclesAcrossIterations validates that the ReAct agent
// rotates ToolChoice through the configured sequence on successive iterations:
//
//	iter 1: mode=required
//	iter 2: mode=tool, name=read_file
//	iter 3+: mode=auto (last entry repeats)
func TestToolChoice_SequenceCyclesAcrossIterations(t *testing.T) {
	h := testharness.New(t, "configs/test-tool-choice.yaml", testharness.WithTimeout(30*time.Second))

	var (
		mu      sync.Mutex
		choices []*events.ToolChoice
	)
	unsub := h.Bus().Subscribe("before:llm.request", func(e engine.Event[any]) {
		vp, ok := e.Payload.(*engine.VetoablePayload)
		if !ok {
			return
		}
		req, ok := vp.Original.(*events.LLMRequest)
		if !ok {
			return
		}
		// Skip non-agent requests (planner / compaction) — react agent requests
		// are the only ones carrying the configured tool_choice sequence.
		if src, _ := req.Metadata["_source"].(string); src != "" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if req.ToolChoice == nil {
			choices = append(choices, nil)
			return
		}
		cp := *req.ToolChoice
		choices = append(choices, &cp)
	}, engine.WithPriority(15))
	defer unsub()

	h.Run()

	mu.Lock()
	defer mu.Unlock()

	if len(choices) < 3 {
		t.Fatalf("expected at least 3 LLM requests with tool_choice, got %d: %+v", len(choices), choices)
	}

	// Iter 1: mode=required.
	if choices[0] == nil || choices[0].Mode != "required" {
		t.Errorf("iter 1: expected mode=required, got %+v", choices[0])
	}

	// Iter 2: mode=tool, name=read_file.
	if choices[1] == nil || choices[1].Mode != "tool" || choices[1].Name != "read_file" {
		t.Errorf("iter 2: expected mode=tool name=read_file, got %+v", choices[1])
	}

	// Iter 3+: mode=auto (last entry repeats).
	for i := 2; i < len(choices); i++ {
		if choices[i] == nil || choices[i].Mode != "auto" {
			t.Errorf("iter %d: expected mode=auto, got %+v", i+1, choices[i])
		}
	}
}
