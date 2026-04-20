package engine

import (
	"sort"
	"strings"
	"sync"
)

// PromptSectionFunc returns the content for a named prompt section.
// Returning an empty string means the section is skipped.
type PromptSectionFunc func() string

type promptSection struct {
	name     string
	priority int
	fn       PromptSectionFunc
}

// PromptRegistry collects named prompt sections that are appended
// to the system prompt before LLM requests reach a provider.
type PromptRegistry struct {
	mu       sync.RWMutex
	sections []promptSection
}

// NewPromptRegistry creates an empty PromptRegistry.
func NewPromptRegistry() *PromptRegistry {
	return &PromptRegistry{}
}

// Register adds a named prompt section. Lower priority values execute first.
// If a section with the same name already exists, it is replaced.
func (r *PromptRegistry) Register(name string, priority int, fn PromptSectionFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, s := range r.sections {
		if s.name == name {
			r.sections[i] = promptSection{name: name, priority: priority, fn: fn}
			r.sortLocked()
			return
		}
	}

	r.sections = append(r.sections, promptSection{name: name, priority: priority, fn: fn})
	r.sortLocked()
}

// Unregister removes a named prompt section.
func (r *PromptRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, s := range r.sections {
		if s.name == name {
			r.sections = append(r.sections[:i], r.sections[i+1:]...)
			return
		}
	}
}

// Apply appends all registered sections to the given system prompt.
// Each non-empty section is separated by a double newline.
func (r *PromptRegistry) Apply(systemPrompt string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.sections) == 0 {
		return systemPrompt
	}

	var parts []string
	for _, s := range r.sections {
		content := s.fn()
		if content != "" {
			parts = append(parts, XMLWrap("prompt_section", content, "name", s.name))
		}
	}

	if len(parts) == 0 {
		return systemPrompt
	}

	var result strings.Builder
	if systemPrompt != "" {
		result.WriteString(XMLWrap("system_instructions", systemPrompt))
	}
	for i, wrapped := range parts {
		if i > 0 || systemPrompt != "" {
			result.WriteByte('\n')
		}
		result.WriteString(wrapped)
	}
	return result.String()
}

func (r *PromptRegistry) sortLocked() {
	sort.SliceStable(r.sections, func(i, j int) bool {
		return r.sections[i].priority < r.sections[j].priority
	})
}
