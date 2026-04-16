package openai

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
	result := applyToolFilter(tools, &events.ToolFilter{Include: []string{"b"}})
	if len(result) != 1 || result[0].Name != "b" {
		t.Errorf("expected [b], got %+v", result)
	}
}

func TestApplyToolFilter_Exclude(t *testing.T) {
	tools := []events.ToolDef{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	result := applyToolFilter(tools, &events.ToolFilter{Exclude: []string{"a", "c"}})
	if len(result) != 1 || result[0].Name != "b" {
		t.Errorf("expected [b], got %+v", result)
	}
}

func TestResolveToolChoice_Auto(t *testing.T) {
	tc := resolveToolChoice(&events.ToolChoice{Mode: "auto"}, nil)
	if tc != "auto" {
		t.Errorf("expected \"auto\", got %v", tc)
	}
}

func TestResolveToolChoice_Required(t *testing.T) {
	tc := resolveToolChoice(&events.ToolChoice{Mode: "required"}, nil)
	if tc != "required" {
		t.Errorf("expected \"required\", got %v", tc)
	}
}

func TestResolveToolChoice_None(t *testing.T) {
	tc := resolveToolChoice(&events.ToolChoice{Mode: "none"}, nil)
	if tc != "none" {
		t.Errorf("expected \"none\", got %v", tc)
	}
}

func TestResolveToolChoice_SpecificTool(t *testing.T) {
	tools := []events.ToolDef{{Name: "shell"}}
	tc := resolveToolChoice(&events.ToolChoice{Mode: "tool", Name: "shell"}, tools)
	m, ok := tc.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", tc)
	}
	if m["type"] != "function" {
		t.Errorf("expected type=function, got %v", m["type"])
	}
	fn, ok := m["function"].(map[string]any)
	if !ok {
		t.Fatal("expected function map")
	}
	if fn["name"] != "shell" {
		t.Errorf("expected name=shell, got %v", fn["name"])
	}
}

func TestResolveToolChoice_MissingToolFallback(t *testing.T) {
	tools := []events.ToolDef{{Name: "file"}}
	tc := resolveToolChoice(&events.ToolChoice{Mode: "tool", Name: "shell"}, tools)
	if tc != "required" {
		t.Errorf("expected fallback \"required\", got %v", tc)
	}
}

func TestResolveToolChoice_Nil(t *testing.T) {
	tc := resolveToolChoice(nil, nil)
	if tc != nil {
		t.Errorf("expected nil, got %v", tc)
	}
}
