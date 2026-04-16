package engine

import "sync"

// PluginFactory creates a new instance of a plugin.
type PluginFactory func() Plugin

// PluginRegistry stores plugin factories by ID.
type PluginRegistry struct {
	mu        sync.RWMutex
	factories map[string]PluginFactory
}

// NewPluginRegistry creates an empty plugin registry.
func NewPluginRegistry() *PluginRegistry {
	return &PluginRegistry{
		factories: make(map[string]PluginFactory),
	}
}

// Register adds a plugin factory to the registry.
func (r *PluginRegistry) Register(id string, factory PluginFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[id] = factory
}

// Get retrieves a plugin factory by ID.
func (r *PluginRegistry) Get(id string) (PluginFactory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.factories[id]
	return f, ok
}

// List returns all registered plugin IDs.
func (r *PluginRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.factories))
	for id := range r.factories {
		ids = append(ids, id)
	}
	return ids
}
