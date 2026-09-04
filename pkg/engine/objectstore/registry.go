package objectstore

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Factory constructs a Backend from a validated Config. It is called once, at
// engine boot, after the config has been validated and paths expanded.
//
// The ctx passed in is the boot context: a factory may dial, resolve
// credentials or probe the bucket, and must honour cancellation. It must not
// retain the ctx for the lifetime of the Backend.
type Factory func(ctx context.Context, cfg Config) (Backend, error)

// registry holds the name → Factory table. Guarded by a mutex rather than
// written once at init because tests register and swap fakes, and because
// nothing forbids an embedder from registering a backend later than package
// init.
var registry = struct {
	mu        sync.RWMutex
	factories map[string]Factory
}{factories: make(map[string]Factory)}

// Register makes a backend selectable by name. It is the database/sql driver
// pattern, chosen for exactly the reason sql.Register uses it: an embedder
// adds a backend to their build with a blank import and one config key, and
// core never learns that the backend exists.
//
//	import _ "github.com/frankbardon/nexus/modules/objectstore-s3"
//
// Register panics on a nil factory, an empty name, or a duplicate name.
// Panicking is correct here — every call happens in package init, so the
// failure is a programming error visible at process start rather than a
// runtime condition anyone could recover from.
func Register(name string, f Factory) {
	if name == "" {
		panic("objectstore: Register called with an empty backend name")
	}
	if f == nil {
		panic("objectstore: Register called with a nil Factory for backend " + name)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, dup := registry.factories[name]; dup {
		panic("objectstore: Register called twice for backend " + name)
	}
	registry.factories[name] = f
}

// Registered reports whether a backend is registered under name. Config
// validation uses it to turn "the module was never imported" into a boot
// failure instead of a surprise at first write.
func Registered(name string) bool {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	_, ok := registry.factories[name]
	return ok
}

// Backends returns the registered backend names in sorted order. Exported for
// diagnostics and for embedders that want to surface the available choices.
func Backends() []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	names := make([]string, 0, len(registry.factories))
	for name := range registry.factories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// registeredList renders Backends() for an error message, distinguishing "none
// at all" (the overwhelmingly likely cause: a missing blank import) from "some,
// but not that one" (a typo).
func registeredList() string {
	names := Backends()
	if len(names) == 0 {
		return "none — no backend module is imported into this build"
	}
	return strings.Join(names, ", ")
}

// Open resolves cfg.BackendName and constructs its Backend. It returns a nil
// Backend and a nil error when cfg is disabled, so the caller's zero-impact
// path is a single nil check rather than a branch on config internals.
//
// Open assumes cfg has already been through Validate — the engine validates at
// config load, well before boot reaches here — but re-checks registration so a
// direct caller cannot get a confusing nil-map panic.
func Open(ctx context.Context, cfg Config) (Backend, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	registry.mu.RLock()
	f, ok := registry.factories[cfg.BackendName]
	registry.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("object-store backend %q is not registered (registered: %s)", cfg.BackendName, registeredList())
	}
	b, err := f(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("opening object-store backend %q: %w", cfg.BackendName, err)
	}
	if b == nil {
		return nil, fmt.Errorf("object-store backend %q returned a nil Backend with no error", cfg.BackendName)
	}
	return b, nil
}

// Unregister removes a backend, if present. It exists for tests: the registry
// is process-global, so a test that registers a fake and does not remove it
// makes every later test in the binary order-dependent, and Register panics on
// the duplicate the second run would produce.
//
// It started out unexported, on the argument that production code has no
// business removing a driver. That argument still holds — but an unexported
// hook only isolates tests inside *this* package, and the callers that need
// isolation are elsewhere: pkg/engine's seam tests, the shared contract suite,
// and any out-of-tree backend module that wants to exercise its own
// registration. Those had to resort to registering under a name derived from
// the test name and leaking it. Exporting one clearly-labelled function is the
// smaller cost.
//
// Nothing in Nexus calls it outside a test, and nothing should: unregistering
// a backend a live Config names would turn a valid config into a boot failure.
func Unregister(name string) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	delete(registry.factories, name)
}
