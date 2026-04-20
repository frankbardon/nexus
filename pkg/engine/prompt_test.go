package engine

import (
	"strings"
	"testing"
)

func TestPromptRegistry_ApplyEmpty(t *testing.T) {
	r := NewPromptRegistry()
	result := r.Apply("base prompt")
	if result != "base prompt" {
		t.Fatalf("expected unchanged prompt, got %q", result)
	}
}

func TestPromptRegistry_ApplyEmptyPrompt(t *testing.T) {
	r := NewPromptRegistry()
	r.Register("test", 0, func() string { return "injected" })
	result := r.Apply("")
	if !strings.Contains(result, "<prompt_section name=\"test\">") {
		t.Fatalf("expected prompt_section tag, got %q", result)
	}
	if !strings.Contains(result, "injected") {
		t.Fatalf("expected content, got %q", result)
	}
	// No system_instructions when base prompt is empty.
	if strings.Contains(result, "<system_instructions>") {
		t.Fatalf("should not have system_instructions for empty base, got %q", result)
	}
}

func TestPromptRegistry_ApplyAppends(t *testing.T) {
	r := NewPromptRegistry()
	r.Register("test", 0, func() string { return "section1" })
	result := r.Apply("base")
	if !strings.Contains(result, "<system_instructions>") {
		t.Fatalf("expected system_instructions wrapper, got %q", result)
	}
	if !strings.Contains(result, "base") {
		t.Fatalf("expected base prompt content, got %q", result)
	}
	if !strings.Contains(result, `<prompt_section name="test">`) {
		t.Fatalf("expected prompt_section tag, got %q", result)
	}
	if !strings.Contains(result, "section1") {
		t.Fatalf("expected section content, got %q", result)
	}
}

func TestPromptRegistry_PriorityOrdering(t *testing.T) {
	r := NewPromptRegistry()
	r.Register("c", 30, func() string { return "C" })
	r.Register("a", 10, func() string { return "A" })
	r.Register("b", 20, func() string { return "B" })

	result := r.Apply("")
	idxA := strings.Index(result, `name="a"`)
	idxB := strings.Index(result, `name="b"`)
	idxC := strings.Index(result, `name="c"`)
	if idxA < 0 || idxB < 0 || idxC < 0 {
		t.Fatalf("expected all three sections, got %q", result)
	}
	if !(idxA < idxB && idxB < idxC) {
		t.Fatalf("expected priority order a < b < c, got %q", result)
	}
}

func TestPromptRegistry_SkipsEmptySections(t *testing.T) {
	r := NewPromptRegistry()
	r.Register("empty", 10, func() string { return "" })
	r.Register("full", 20, func() string { return "content" })

	result := r.Apply("base")
	if strings.Contains(result, `name="empty"`) {
		t.Fatalf("empty section should be skipped, got %q", result)
	}
	if !strings.Contains(result, `name="full"`) {
		t.Fatalf("full section should be present, got %q", result)
	}
}

func TestPromptRegistry_AllEmptySections(t *testing.T) {
	r := NewPromptRegistry()
	r.Register("a", 10, func() string { return "" })
	r.Register("b", 20, func() string { return "" })

	result := r.Apply("base")
	if result != "base" {
		t.Fatalf("expected unchanged prompt when all sections empty, got %q", result)
	}
}

func TestPromptRegistry_ReplaceDuplicate(t *testing.T) {
	r := NewPromptRegistry()
	r.Register("test", 10, func() string { return "old" })
	r.Register("test", 10, func() string { return "new" })

	result := r.Apply("")
	if !strings.Contains(result, "new") {
		t.Fatalf("expected replaced value, got %q", result)
	}
	if strings.Contains(result, "old") {
		t.Fatalf("old value should be replaced, got %q", result)
	}
	// Should still be one section, not two.
	count := strings.Count(result, "<prompt_section")
	if count != 1 {
		t.Fatalf("expected single section, got %d", count)
	}
}

func TestPromptRegistry_Unregister(t *testing.T) {
	r := NewPromptRegistry()
	r.Register("a", 10, func() string { return "A" })
	r.Register("b", 20, func() string { return "B" })

	r.Unregister("a")

	result := r.Apply("")
	if strings.Contains(result, `name="a"`) {
		t.Fatalf("unregistered section should not appear, got %q", result)
	}
	if !strings.Contains(result, `name="b"`) {
		t.Fatalf("remaining section should appear, got %q", result)
	}
}

func TestPromptRegistry_UnregisterNonexistent(t *testing.T) {
	r := NewPromptRegistry()
	r.Register("a", 10, func() string { return "A" })
	r.Unregister("nonexistent") // should not panic

	result := r.Apply("")
	if !strings.Contains(result, "A") {
		t.Fatalf("expected A unchanged, got %q", result)
	}
}
