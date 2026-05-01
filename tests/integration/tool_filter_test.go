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

// TestToolFilter_Boot validates that the tool_filter gate boots alongside the
// shell and file plugins it filters.
func TestToolFilter_Boot(t *testing.T) {
	h := testharness.New(t, "configs/test-tool-filter.yaml", testharness.WithTimeout(20*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.gate.tool_filter",
		"nexus.tool.shell",
		"nexus.tool.file",
		"nexus.agent.react",
	)
}

// TestToolFilter_GatePopulatesToolFilter validates that the gate runs at
// priority 10 on before:llm.request and sets req.ToolFilter.Include with the
// configured allowlist before any downstream handler sees the request.
func TestToolFilter_GatePopulatesToolFilter(t *testing.T) {
	h := testharness.New(t, "configs/test-tool-filter.yaml", testharness.WithTimeout(20*time.Second))

	// Subscribe at priority 15 — after gate (10), before mock interceptor (20)
	// — so we observe the request after the gate has populated ToolFilter.
	var (
		mu        sync.Mutex
		filters   []*events.ToolFilter
		toolNames [][]string
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
		mu.Lock()
		defer mu.Unlock()
		// Copy filter so mutations after this handler don't affect captured value.
		if req.ToolFilter != nil {
			cp := *req.ToolFilter
			filters = append(filters, &cp)
		} else {
			filters = append(filters, nil)
		}
		names := make([]string, len(req.Tools))
		for i, td := range req.Tools {
			names[i] = td.Name
		}
		toolNames = append(toolNames, names)
	}, engine.WithPriority(15))
	defer unsub()

	h.Run()

	mu.Lock()
	defer mu.Unlock()

	if len(filters) == 0 {
		t.Fatal("no before:llm.request events captured")
	}

	first := filters[0]
	if first == nil {
		t.Fatalf("expected gate to populate req.ToolFilter, got nil; captured tool list: %v", toolNames[0])
	}
	wantInclude := []string{"read_file", "write_file", "check_file_size", "list_files"}
	if !equalStringSet(first.Include, wantInclude) {
		t.Errorf("expected ToolFilter.Include=%v, got %v", wantInclude, first.Include)
	}
	if len(first.Exclude) != 0 {
		t.Errorf("expected ToolFilter.Exclude empty, got %v", first.Exclude)
	}

	// The shell tool plugin is registered, so its tools appear in the request's
	// Tools slice; the filter is applied downstream by the provider, not by
	// mutating Tools here. Sanity-check that registration happened.
	if len(toolNames[0]) == 0 {
		t.Error("expected at least one tool registered in request")
	}
}

// TestToolFilter_NoShellInvocation validates that with the filter active and
// the mock LLM only requesting list_files, no shell tool is invoked end-to-end.
func TestToolFilter_NoShellInvocation(t *testing.T) {
	h := testharness.New(t, "configs/test-tool-filter.yaml", testharness.WithTimeout(20*time.Second))
	h.Run()

	h.AssertToolCalled("list_files")
	h.AssertToolNotCalled("shell")
}

func equalStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, s := range a {
		set[s] = struct{}{}
	}
	for _, s := range b {
		if _, ok := set[s]; !ok {
			return false
		}
	}
	return true
}
