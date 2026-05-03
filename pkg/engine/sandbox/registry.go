package sandbox

import (
	"fmt"
	"slices"
	"sync"
)

// Factory builds a sandbox from a config map. The map is the contents of the
// per-plugin `sandbox:` YAML block, with all keys other than `backend`
// passed through to the factory.
type Factory func(cfg map[string]any) (Sandbox, error)

var (
	registryMu sync.RWMutex
	registry   = map[Backend]Factory{}
)

// Register installs a factory for the named backend. Calling Register twice
// for the same name overwrites the prior entry (use cases: tests, alternate
// implementations gated by build tag).
func Register(name Backend, factory Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = factory
}

// New resolves the configured backend name and invokes its factory. Returns
// ErrUnknownBackend when the name is not registered.
func New(name Backend, cfg map[string]any) (Sandbox, error) {
	if name == "" {
		return nil, ErrBackendNotConfigured
	}
	registryMu.RLock()
	factory, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownBackend, name)
	}
	return factory(cfg)
}

// Registered returns the sorted list of currently-registered backend names.
// Useful for diagnostics and error messages that list the legal alternatives.
func Registered() []Backend {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]Backend, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// FromPluginConfig extracts the sandbox config block from a plugin config
// map and returns the resolved backend name plus the inner config. When the
// plugin has no `sandbox:` block, the second return is nil and the first is
// the supplied fallback.
//
// Resolution rule: a plugin's `sandbox.backend` wins; otherwise fallback;
// otherwise empty (boot fails downstream with ErrBackendNotConfigured).
func FromPluginConfig(pluginCfg map[string]any, fallback Backend) (Backend, map[string]any) {
	if pluginCfg == nil {
		return fallback, nil
	}
	raw, ok := pluginCfg["sandbox"]
	if !ok {
		return fallback, nil
	}
	block, ok := raw.(map[string]any)
	if !ok {
		return fallback, nil
	}
	name := fallback
	if s, ok := block["backend"].(string); ok && s != "" {
		name = Backend(s)
	}
	return name, block
}
