// Package posture defines the AgentPosture configuration type that the
// sub-agent invocation primitive consumes, together with an in-memory
// registry that nexus.agent.postures populates from on-disk YAML.
//
// A posture is a named, versioned configuration describing how a sub-agent
// should run: which system prompt, which subset of tools, which model, and
// what resource budget. Postures are the contract that parent agents
// reference when they delegate work — see Session.Delegate in the engine.
package posture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

// AgentPosture is the registered, named configuration a sub-agent runs under.
// Postures are loaded from YAML at startup (or hot-reloaded via fsnotify) and
// keyed by Name. Two postures with the same Name but different content are a
// version conflict — the registry tracks Version (a content hash) so cache
// keys can include it and stale results invalidate on edits.
type AgentPosture struct {
	// Name is the registry key parent agents reference when delegating.
	Name string `yaml:"name" json:"name"`
	// Description is human-facing copy used for agent introspection prompts.
	Description string `yaml:"description" json:"description"`
	// SystemPrompt is the sub-agent's system prompt — the core of the
	// posture's behavior shaping.
	SystemPrompt string `yaml:"system_prompt" json:"system_prompt"`
	// AllowedTools is the closed list of tool names the sub-agent may
	// invoke. Tools outside this list are filtered before dispatch.
	AllowedTools []string `yaml:"allowed_tools" json:"allowed_tools"`
	// OutputSchema names a registered schema the sub-agent's final output
	// is validated against. Optional — leave empty for free-form output.
	OutputSchema string `yaml:"output_schema,omitempty" json:"output_schema,omitempty"`
	// Model describes which model tier the sub-agent runs on.
	Model ModelConfig `yaml:"model" json:"model"`
	// DefaultBudget is the resource budget applied when the caller omits
	// per-invocation overrides.
	DefaultBudget ResourceBudget `yaml:"default_budget" json:"default_budget"`
	// MaxRecursionDepth caps how deep this posture may be nested. Zero
	// falls back to the registry-wide default.
	MaxRecursionDepth int `yaml:"max_recursion_depth,omitempty" json:"max_recursion_depth,omitempty"`
	// Version is a content-hash assigned by the registry when a posture is
	// installed. Callers should treat it as opaque. Cache keys include this
	// so a posture edit invalidates previously-cached delegate results.
	Version string `yaml:"-" json:"version,omitempty"`
}

// ModelConfig selects the LLM the sub-agent uses. ModelRole resolves through
// the engine's model registry; Provider/Model are an explicit override taking
// precedence when set.
type ModelConfig struct {
	ModelRole   string  `yaml:"model_role,omitempty" json:"model_role,omitempty"`
	Provider    string  `yaml:"provider,omitempty" json:"provider,omitempty"`
	Model       string  `yaml:"model,omitempty" json:"model,omitempty"`
	Temperature float64 `yaml:"temperature,omitempty" json:"temperature,omitempty"`
	MaxTokens   int     `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty"`
}

// ResourceBudget bounds a sub-agent's resource use. Zero fields mean unlimited
// for that dimension — but at least one of Timeout / MaxTokens / MaxToolCalls
// should be set to prevent unbounded loops.
type ResourceBudget struct {
	Timeout      time.Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	MaxTokens    int           `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty"`
	MaxToolCalls int           `yaml:"max_tool_calls,omitempty" json:"max_tool_calls,omitempty"`
}

// ChangeKind classifies registry change notifications.
type ChangeKind int

const (
	ChangeAdded ChangeKind = iota
	ChangeModified
	ChangeRemoved
)

// Change is a registry notification delivered on the channel returned by
// Watch. Posture is nil for ChangeRemoved.
type Change struct {
	Name    string
	Kind    ChangeKind
	Posture *AgentPosture
}

// Registry is the lookup surface sub-agent runtimes use to resolve a posture
// name. Implementations must be safe for concurrent reads from any goroutine.
type Registry interface {
	// Register installs (or replaces) a posture. Returns an error when the
	// posture is invalid (missing Name, conflicting model config).
	Register(p AgentPosture) error
	// Get returns the posture by name. Returns ErrNotFound when absent.
	Get(name string) (*AgentPosture, error)
	// List returns a sorted snapshot of all installed postures.
	List() []AgentPosture
	// Remove deletes the posture by name. Returns ErrNotFound when absent.
	Remove(name string) error
	// Watch returns a channel that receives changes. The returned channel
	// closes when ctx is cancelled. Multiple watchers fan-in to independent
	// channels — buffer is small (8); slow consumers drop events with a log
	// from the registry implementation.
	Watch(ctx context.Context) <-chan Change
}

// ErrNotFound is returned by Get/Remove when the requested posture is absent.
var ErrNotFound = errors.New("posture: not found")

// memoryRegistry is the default Registry implementation. Safe for concurrent
// use; reads acquire a read lock, writes a write lock. Watcher fan-out runs
// under the write lock; non-blocking sends drop on full channels to keep
// hot-reload latency bounded.
type memoryRegistry struct {
	mu       sync.RWMutex
	items    map[string]*AgentPosture
	watchers []chan Change
}

// NewRegistry returns an empty in-memory registry.
func NewRegistry() Registry {
	return &memoryRegistry{
		items: make(map[string]*AgentPosture),
	}
}

// HashPosture computes the version hash for a posture from its content. The
// loader assigns this to Version when installing from YAML; consumers should
// treat it as opaque.
func HashPosture(p AgentPosture) string {
	h := sha256.New()
	h.Write([]byte(p.Name))
	h.Write([]byte{0})
	h.Write([]byte(p.SystemPrompt))
	h.Write([]byte{0})
	for _, t := range p.AllowedTools {
		h.Write([]byte(t))
		h.Write([]byte{0})
	}
	h.Write([]byte(p.OutputSchema))
	h.Write([]byte{0})
	h.Write([]byte(p.Model.ModelRole))
	h.Write([]byte(p.Model.Provider))
	h.Write([]byte(p.Model.Model))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (r *memoryRegistry) Register(p AgentPosture) error {
	if p.Name == "" {
		return errors.New("posture: Name is required")
	}
	if p.Version == "" {
		p.Version = HashPosture(p)
	}
	r.mu.Lock()
	prev, existed := r.items[p.Name]
	stored := p
	r.items[p.Name] = &stored
	kind := ChangeAdded
	if existed && prev != nil && prev.Version != stored.Version {
		kind = ChangeModified
	} else if existed {
		// Same content — skip the fan-out so unchanged reloads are silent.
		r.mu.Unlock()
		return nil
	}
	watchers := append([]chan Change(nil), r.watchers...)
	r.mu.Unlock()

	r.fanout(watchers, Change{Name: p.Name, Kind: kind, Posture: &stored})
	return nil
}

func (r *memoryRegistry) Get(name string) (*AgentPosture, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.items[name]
	if !ok {
		return nil, ErrNotFound
	}
	copy := *p
	return &copy, nil
}

func (r *memoryRegistry) List() []AgentPosture {
	r.mu.RLock()
	out := make([]AgentPosture, 0, len(r.items))
	for _, p := range r.items {
		out = append(out, *p)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *memoryRegistry) Remove(name string) error {
	r.mu.Lock()
	_, ok := r.items[name]
	if !ok {
		r.mu.Unlock()
		return ErrNotFound
	}
	delete(r.items, name)
	watchers := append([]chan Change(nil), r.watchers...)
	r.mu.Unlock()
	r.fanout(watchers, Change{Name: name, Kind: ChangeRemoved})
	return nil
}

func (r *memoryRegistry) Watch(ctx context.Context) <-chan Change {
	ch := make(chan Change, 8)
	r.mu.Lock()
	r.watchers = append(r.watchers, ch)
	r.mu.Unlock()

	go func() {
		<-ctx.Done()
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, w := range r.watchers {
			if w == ch {
				r.watchers = append(r.watchers[:i], r.watchers[i+1:]...)
				break
			}
		}
		close(ch)
	}()
	return ch
}

func (r *memoryRegistry) fanout(watchers []chan Change, change Change) {
	for _, w := range watchers {
		select {
		case w <- change:
		default:
			// drop on slow consumer to keep hot-reload latency bounded
		}
	}
}
