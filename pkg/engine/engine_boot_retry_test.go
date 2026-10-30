package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// failOnceInitPlugin fails its first Init and succeeds on every later one, so
// Lifecycle.Boot fails after Boot has already installed the journal wildcard,
// the tool cache and the tag seeder — and a retried Boot on the same engine
// then succeeds.
type failOnceInitPlugin struct{ inits int }

func (p *failOnceInitPlugin) ID() string                         { return "nexus.test.fail_once_init" }
func (p *failOnceInitPlugin) Name() string                       { return p.ID() }
func (p *failOnceInitPlugin) Version() string                    { return "test" }
func (p *failOnceInitPlugin) Dependencies() []string             { return nil }
func (p *failOnceInitPlugin) Requires() []Requirement            { return nil }
func (p *failOnceInitPlugin) Capabilities() []Capability         { return nil }
func (p *failOnceInitPlugin) Subscriptions() []EventSubscription { return nil }
func (p *failOnceInitPlugin) Emissions() []string                { return nil }
func (p *failOnceInitPlugin) Ready() error                       { return nil }
func (p *failOnceInitPlugin) Shutdown(context.Context) error     { return nil }
func (p *failOnceInitPlugin) Init(PluginContext) error {
	p.inits++
	if p.inits == 1 {
		return errors.New("forced init failure")
	}
	return nil
}

// busSubscriptionCounts snapshots the bus's handler table: typed subscribers
// per event type, plus the wildcard count under "*".
func busSubscriptionCounts(t *testing.T, bus EventBus) map[string]int {
	t.Helper()
	b, ok := bus.(*eventBus)
	if !ok {
		t.Fatalf("bus is %T, want *eventBus", bus)
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	counts := map[string]int{"*": len(b.wildcards)}
	for typ, subs := range b.handlers {
		if len(subs) > 0 {
			counts[typ] = len(subs)
		}
	}
	return counts
}

// countSourceSubs counts the subscriptions on eventType tagged with source.
// Every one of them runs on each dispatch of that event.
func countSourceSubs(t *testing.T, bus EventBus, eventType, source string) int {
	t.Helper()
	b := bus.(*eventBus)
	b.mu.RLock()
	defer b.mu.RUnlock()
	n := 0
	for _, s := range b.handlers[eventType] {
		if s.source == source {
			n++
		}
	}
	return n
}

func newBootRetryEngine(t *testing.T, p Plugin) *Engine {
	t.Helper()
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = filepath.Join(root, "sessions")
	cfg.Core.Storage.Root = root
	cfg.Plugins.Active = []string{p.ID()}
	eng := newFromConfig(cfg)
	eng.Registry.Register(p.ID(), func() Plugin { return p })
	return eng
}

// A Boot that fails after run-scoped subscriptions are installed removes them
// all, so a retried Boot on the same engine registers each exactly once (#206):
// the handler table after fail-then-succeed equals the one after a single
// clean Boot, and the tag seeder runs once per before:llm.request.
func TestBoot_FailureTearsDownRunScopedSubscriptions(t *testing.T) {
	const seederSource = "nexus.engine.tag_seeder"

	eng := newBootRetryEngine(t, &failOnceInitPlugin{})
	before := busSubscriptionCounts(t, eng.Bus)

	if err := eng.Boot(context.Background()); err == nil {
		t.Fatal("first Boot succeeded, want forced init failure")
	}
	if len(eng.runUnsubs) != 0 {
		t.Fatalf("runUnsubs after failed Boot = %d, want 0", len(eng.runUnsubs))
	}
	if got := busSubscriptionCounts(t, eng.Bus); !equalCounts(got, before) {
		t.Fatalf("subscriptions after failed Boot = %v, want pre-Boot %v", got, before)
	}

	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("retried Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	if n := countSourceSubs(t, eng.Bus, "before:llm.request", seederSource); n != 1 {
		t.Fatalf("tag seeder subscriptions after retry = %d, want 1 (fires once per event)", n)
	}

	// Reference: the same config booted cleanly once. Plugins add nothing
	// here, so any difference is a leaked run-scoped handler.
	ref := newBootRetryEngine(t, &failOnceInitPlugin{inits: 1})
	if err := ref.Boot(context.Background()); err != nil {
		t.Fatalf("reference Boot: %v", err)
	}
	t.Cleanup(func() { _ = ref.Stop(context.Background()) })

	got, want := busSubscriptionCounts(t, eng.Bus), busSubscriptionCounts(t, ref.Bus)
	if !equalCounts(got, want) {
		t.Fatalf("subscriptions after fail-then-succeed = %v, want single-Boot %v", got, want)
	}
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
