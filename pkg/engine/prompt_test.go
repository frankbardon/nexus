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
	if result != "injected" {
		t.Fatalf("expected %q, got %q", "injected", result)
	}
}

func TestPromptRegistry_ApplyAppends(t *testing.T) {
	r := NewPromptRegistry()
	r.Register("test", 0, func() string { return "section1" })
	result := r.Apply("base")
	if result != "base\n\nsection1" {
		t.Fatalf("expected %q, got %q", "base\n\nsection1", result)
	}
}

func TestPromptRegistry_PriorityOrdering(t *testing.T) {
	r := NewPromptRegistry()
	r.Register("c", 30, func() string { return "C" })
	r.Register("a", 10, func() string { return "A" })
	r.Register("b", 20, func() string { return "B" })

	result := r.Apply("")
	if result != "A\n\nB\n\nC" {
		t.Fatalf("expected priority order A,B,C, got %q", result)
	}
}

func TestPromptRegistry_SkipsEmptySections(t *testing.T) {
	r := NewPromptRegistry()
	r.Register("empty", 10, func() string { return "" })
	r.Register("full", 20, func() string { return "content" })

	result := r.Apply("base")
	if result != "base\n\ncontent" {
		t.Fatalf("expected empty section skipped, got %q", result)
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
	if result != "new" {
		t.Fatalf("expected replaced value, got %q", result)
	}

	// Should still be one section, not two.
	count := strings.Count(result, "\n\n")
	if count != 0 {
		t.Fatalf("expected single section, got separators: %d", count)
	}
}

func TestPromptRegistry_Unregister(t *testing.T) {
	r := NewPromptRegistry()
	r.Register("a", 10, func() string { return "A" })
	r.Register("b", 20, func() string { return "B" })

	r.Unregister("a")

	result := r.Apply("")
	if result != "B" {
		t.Fatalf("expected only B after unregister, got %q", result)
	}
}

func TestPromptRegistry_UnregisterNonexistent(t *testing.T) {
	r := NewPromptRegistry()
	r.Register("a", 10, func() string { return "A" })
	r.Unregister("nonexistent") // should not panic

	result := r.Apply("")
	if result != "A" {
		t.Fatalf("expected A unchanged, got %q", result)
	}
}
