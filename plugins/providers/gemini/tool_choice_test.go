package gemini

import (
	"reflect"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

func TestApplyToolFilter_NilFilter(t *testing.T) {
	tools := []events.ToolDef{{Name: "a"}, {Name: "b"}}
	got := applyToolFilter(tools, nil)
	if !reflect.DeepEqual(got, tools) {
		t.Fatalf("nil filter should pass through, got %+v", got)
	}
}

func TestApplyToolFilter_Include(t *testing.T) {
	tools := []events.ToolDef{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	got := applyToolFilter(tools, &events.ToolFilter{Include: []string{"a", "c"}})
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Fatalf("Include=[a,c] expected [a,c], got %+v", got)
	}
}

func TestApplyToolFilter_Exclude(t *testing.T) {
	tools := []events.ToolDef{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	got := applyToolFilter(tools, &events.ToolFilter{Exclude: []string{"b"}})
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Fatalf("Exclude=[b] expected [a,c], got %+v", got)
	}
}

func TestApplyToolFilter_IncludeWinsOverExclude(t *testing.T) {
	tools := []events.ToolDef{{Name: "a"}, {Name: "b"}}
	got := applyToolFilter(tools, &events.ToolFilter{Include: []string{"a"}, Exclude: []string{"a"}})
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("Include should take precedence; got %+v", got)
	}
}

func TestResolveToolChoice_Nil(t *testing.T) {
	if resolveToolChoice(nil, nil) != nil {
		t.Fatal("nil tc should return nil")
	}
}

func TestResolveToolChoice_Auto(t *testing.T) {
	got := resolveToolChoice(&events.ToolChoice{Mode: "auto"}, nil)
	want := map[string]any{"function_calling_config": map[string]any{"mode": "AUTO"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("auto: got %+v want %+v", got, want)
	}
}

func TestResolveToolChoice_Required(t *testing.T) {
	got := resolveToolChoice(&events.ToolChoice{Mode: "required"}, nil)
	want := map[string]any{"function_calling_config": map[string]any{"mode": "ANY"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("required: got %+v want %+v", got, want)
	}
}

func TestResolveToolChoice_None(t *testing.T) {
	got := resolveToolChoice(&events.ToolChoice{Mode: "none"}, nil)
	want := map[string]any{"function_calling_config": map[string]any{"mode": "NONE"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("none: got %+v want %+v", got, want)
	}
}

func TestResolveToolChoice_SpecificTool(t *testing.T) {
	tools := []events.ToolDef{{Name: "search"}, {Name: "fetch"}}
	got := resolveToolChoice(&events.ToolChoice{Mode: "tool", Name: "search"}, tools)
	cfg, ok := got["function_calling_config"].(map[string]any)
	if !ok {
		t.Fatalf("missing function_calling_config: %+v", got)
	}
	if cfg["mode"] != "ANY" {
		t.Fatalf("expected mode ANY, got %v", cfg["mode"])
	}
	allowed, ok := cfg["allowed_function_names"].([]string)
	if !ok || len(allowed) != 1 || allowed[0] != "search" {
		t.Fatalf("expected allowed_function_names=[search], got %+v", cfg["allowed_function_names"])
	}
}

func TestResolveToolChoice_SpecificToolNotInSet(t *testing.T) {
	tools := []events.ToolDef{{Name: "search"}}
	got := resolveToolChoice(&events.ToolChoice{Mode: "tool", Name: "missing"}, tools)
	cfg := got["function_calling_config"].(map[string]any)
	if cfg["mode"] != "ANY" {
		t.Fatalf("expected fallback mode ANY, got %v", cfg["mode"])
	}
	if _, ok := cfg["allowed_function_names"]; ok {
		t.Fatalf("should not set allowed_function_names when tool absent")
	}
}

func TestResolveToolChoice_ToolWithoutName(t *testing.T) {
	got := resolveToolChoice(&events.ToolChoice{Mode: "tool"}, nil)
	cfg := got["function_calling_config"].(map[string]any)
	if cfg["mode"] != "ANY" {
		t.Fatalf("expected mode ANY when Name empty, got %v", cfg["mode"])
	}
}
