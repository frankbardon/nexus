package nexuscreds

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// registry holds the name → Factory table. It is guarded by a mutex rather than
// written once at init for two reasons: tests register fakes and swap them out
// (see Unregister), and nothing forbids an embedder from registering a Source
// later than package init — from a plugin's own init, or from an explicit setup
// call in main.
var registry = struct {
	mu        sync.RWMutex
	factories map[string]Factory
}{factories: make(map[string]Factory)}

// Register makes a credential source selectable by name. It is the
// database/sql driver pattern, chosen for exactly the reason sql.Register uses
// it: an embedder adds a source to their build with a blank import and one
// config key, and neither the engine nor the plugin that consumes the
// credential ever learns that the source exists.
//
//	import _ "github.com/frankbardon/nexus/pkg/nexuscreds/googleadc"
//
// Register panics on an empty name, a nil factory, or a duplicate name.
// Panicking is correct here — every call happens in package init, so each of
// those is a programming error in the build, visible at process start, and not
// a runtime condition any caller could recover from. A deployment that silently
// dropped a duplicate registration would authenticate with whichever source won
// the race, which is a worse failure than the panic.
func Register(name string, f Factory) {
	if name == "" {
		panic("nexuscreds: Register called with an empty source name")
	}
	if f == nil {
		panic("nexuscreds: Register called with a nil Factory for source " + name)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, dup := registry.factories[name]; dup {
		panic("nexuscreds: Register called twice for source " + name)
	}
	registry.factories[name] = f
}

// Registered reports whether a source is registered under name. Config
// validation uses it to turn "the package was never blank-imported" into a
// failure at config load rather than a surprise on the first outbound request.
func Registered(name string) bool {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	_, ok := registry.factories[name]
	return ok
}

// Sources returns the registered source names in sorted order. Exported for
// diagnostics and for embedders that want to surface the available choices.
func Sources() []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	names := make([]string, 0, len(registry.factories))
	for name := range registry.factories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// registeredList renders Sources() for an error message, distinguishing "none
// at all" (the overwhelmingly likely cause: a missing blank import) from "some,
// but not that one" (a typo in the config).
func registeredList() string {
	names := Sources()
	if len(names) == 0 {
		return "none — no credential-source package is imported into this build"
	}
	return strings.Join(names, ", ")
}

// Open resolves name and constructs its Source from cfg.
//
// The error for an unknown name names the likely cause outright. The failure
// mode this package invites is a config that is entirely correct paired with a
// binary that never blank-imported the source, and "unknown credential source"
// on its own sends an operator hunting through YAML for a typo that is not
// there. Saying what is actually wrong, and listing what the build does have,
// costs one line here and saves the hunt.
//
// cfg may be nil; it is passed through untouched, and a Factory is required to
// treat nil as "every default".
func Open(name string, cfg map[string]any) (Source, error) {
	if name == "" {
		return nil, fmt.Errorf("nexuscreds: no credential source name given (registered: %s)", registeredList())
	}
	registry.mu.RLock()
	f, ok := registry.factories[name]
	registry.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("credential source %q is not registered — the binary must blank-import the package that registers it (registered: %s)", name, registeredList())
	}
	s, err := f(cfg)
	if err != nil {
		return nil, fmt.Errorf("opening credential source %q: %w", name, err)
	}
	if s == nil {
		return nil, fmt.Errorf("credential source %q returned a nil Source with no error", name)
	}
	return s, nil
}

// Unregister removes a source, if present. It exists for tests: the registry is
// process-global, so a test that registers a fake and does not remove it makes
// every later test in the binary order-dependent, and Register panics on the
// duplicate a second registration would produce.
//
// It is EXPORTED for the same reason objectstore.Unregister is: an unexported
// hook only isolates tests inside this package, and the tests that need to
// substitute a credential source live elsewhere — in the googleadc subpackage,
// in the provider plugin that consumes a Source, and in any out-of-tree source
// an embedder writes.
//
// Nothing in Nexus calls it outside a test, and nothing should: unregistering a
// source a live config names turns a working deployment into a boot failure.
func Unregister(name string) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	delete(registry.factories, name)
}
