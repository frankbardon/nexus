package anthropic

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

func TestApplyToolFilter_Nil(t *testing.T) {
	tools := []events.ToolDef{{Name: "a"}, {Name: "b"}}
	result := applyToolFilter(tools, nil)
	if len(result) != 2 {
		t.Errorf("expected 2 tools, got %d", len(result))
	}
}

func TestApplyToolFilter_Include(t *testing.T) {
	tools := []events.ToolDef{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	result := applyToolFilter(tools, &events.ToolFilter{Include: []string{"a", "c"}})
	if len(result) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(result))
	}
	if result[0].Name != "a" || result[1].Name != "c" {
		t.Errorf("expected [a, c], got [%s, %s]", result[0].Name, result[1].Name)
	}
}

func TestApplyToolFilter_Exclude(t *testing.T) {
	tools := []events.ToolDef{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	result := applyToolFilter(tools, &events.ToolFilter{Exclude: []string{"b"}})
	if len(result) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(result))
	}
	if result[0].Name != "a" || result[1].Name != "c" {
		t.Errorf("expected [a, c], got [%s, %s]", result[0].Name, result[1].Name)
	}
}

func TestApplyToolFilter_IncludePrecedence(t *testing.T) {
	tools := []events.ToolDef{{Name: "a"}, {Name: "b"}}
	result := applyToolFilter(tools, &events.ToolFilter{
		Include: []string{"a"},
		Exclude: []string{"a"},
	})
	// Include takes precedence — "a" should be kept.
	if len(result) != 1 || result[0].Name != "a" {
		t.Errorf("expected [a], got %+v", result)
	}
}

func TestResolveToolChoice_Auto(t *testing.T) {
	tc := resolveToolChoice(&events.ToolChoice{Mode: "auto"}, nil)
	if tc == nil {
		t.Fatal("expected non-nil result")
	}
	if tc["type"] != "auto" {
		t.Errorf("expected type=auto, got %v", tc["type"])
	}
}

func TestResolveToolChoice_Required(t *testing.T) {
	tc := resolveToolChoice(&events.ToolChoice{Mode: "required"}, nil)
	if tc == nil {
		t.Fatal("expected non-nil result")
	}
	if tc["type"] != "any" {
		t.Errorf("expected type=any, got %v", tc["type"])
	}
}

func TestResolveToolChoice_None(t *testing.T) {
	tc := resolveToolChoice(&events.ToolChoice{Mode: "none"}, nil)
	if tc != nil {
		t.Errorf("expected nil for none mode, got %+v", tc)
	}
}

func TestResolveToolChoice_SpecificTool(t *testing.T) {
	tools := []events.ToolDef{{Name: "shell"}, {Name: "file"}}
	tc := resolveToolChoice(&events.ToolChoice{Mode: "tool", Name: "shell"}, tools)
	if tc == nil {
		t.Fatal("expected non-nil result")
	}
	if tc["type"] != "tool" || tc["name"] != "shell" {
		t.Errorf("expected type=tool name=shell, got %+v", tc)
	}
}

func TestResolveToolChoice_MissingToolFallback(t *testing.T) {
	tools := []events.ToolDef{{Name: "file"}}
	tc := resolveToolChoice(&events.ToolChoice{Mode: "tool", Name: "shell"}, tools)
	if tc == nil {
		t.Fatal("expected non-nil result")
	}
	// Should fall back to "any" (required).
	if tc["type"] != "any" {
		t.Errorf("expected fallback type=any, got %v", tc["type"])
	}
}

func TestResolveToolChoice_Nil(t *testing.T) {
	tc := resolveToolChoice(nil, nil)
	if tc != nil {
		t.Errorf("expected nil for nil input, got %+v", tc)
	}
}
