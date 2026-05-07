package toolfilter

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/testharness/contract"
)

func TestContract_SubscribesAndDeclaresNoEmissions(t *testing.T) {
	h := contract.NewContract(t, New)
	h.AssertSubscribesTo("before:llm.request")
	if got := h.Plugin().Emissions(); len(got) != 0 {
		t.Errorf("Emissions() = %v, want empty", got)
	}
}

func TestContract_IncludeMode_AppliesToolFilter(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"include": []any{"shell", "file_read"},
	}))

	req := &events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Tools: []events.ToolDef{
			{Name: "shell"}, {Name: "file_read"}, {Name: "web_search"},
		},
	}
	h.InjectVetoable("before:llm.request", req)

	if req.ToolFilter == nil {
		t.Fatal("ToolFilter not applied")
	}
	if len(req.ToolFilter.Include) != 2 {
		t.Errorf("Include = %v, want 2 entries", req.ToolFilter.Include)
	}
	if len(req.ToolFilter.Exclude) != 0 {
		t.Errorf("Exclude must remain empty in include mode, got %v", req.ToolFilter.Exclude)
	}
}

func TestContract_ExcludeMode_AppliesToolFilter(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"exclude": []any{"shell"},
	}))

	req := &events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Tools:         []events.ToolDef{{Name: "shell"}},
	}
	h.InjectVetoable("before:llm.request", req)

	if req.ToolFilter == nil || len(req.ToolFilter.Exclude) != 1 || req.ToolFilter.Exclude[0] != "shell" {
		t.Errorf("exclude not applied, got %+v", req.ToolFilter)
	}
}

func TestContract_RequestWithoutTools_NoFilterApplied(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"include": []any{"shell"},
	}))

	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion}
	h.InjectVetoable("before:llm.request", req)

	if req.ToolFilter != nil {
		t.Errorf("filter should not be applied when request has no tools, got %+v", req.ToolFilter)
	}
}

func TestContract_PreExistingFilter_NotOverwritten(t *testing.T) {
	h := contract.NewContract(t, New, contract.WithPluginConfig(map[string]any{
		"include": []any{"shell"},
	}))

	preset := &events.ToolFilter{Include: []string{"file_read"}}
	req := &events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Tools:         []events.ToolDef{{Name: "shell"}, {Name: "file_read"}},
		ToolFilter:    preset,
	}
	h.InjectVetoable("before:llm.request", req)

	if req.ToolFilter != preset {
		t.Error("request-level filter should win, gate must not overwrite")
	}
}
