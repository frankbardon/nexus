package engine

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/journal"
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

// subscriberPlugin subscribes to one event in Init and unsubscribes in
// Shutdown, counting both, so a test can see whether a failed Boot shut it
// down and whether its subscription survived.
type subscriberPlugin struct {
	id        string
	deps      []string
	event     string
	inits     int
	shutdowns int
	unsub     func()
	journal   *journal.Writer
	// failInit / failReady fail the first Init / Ready call.
	failInit  bool
	failReady bool
	readies   int
}

func (p *subscriberPlugin) ID() string                         { return p.id }
func (p *subscriberPlugin) Name() string                       { return p.id }
func (p *subscriberPlugin) Version() string                    { return "test" }
func (p *subscriberPlugin) Dependencies() []string             { return p.deps }
func (p *subscriberPlugin) Requires() []Requirement            { return nil }
func (p *subscriberPlugin) Capabilities() []Capability         { return nil }
func (p *subscriberPlugin) Subscriptions() []EventSubscription { return nil }
func (p *subscriberPlugin) Emissions() []string                { return nil }
func (p *subscriberPlugin) Init(ctx PluginContext) error {
	p.inits++
	p.journal = ctx.Journal
	if p.failInit && p.inits == 1 {
		return errors.New("forced init failure")
	}
	if p.event != "" {
		p.unsub = ctx.Bus.Subscribe(p.event, func(Event[any]) {})
	}
	return nil
}
func (p *subscriberPlugin) Ready() error {
	p.readies++
	if p.failReady && p.readies == 1 {
		return errors.New("forced ready failure")
	}
	return nil
}
func (p *subscriberPlugin) Shutdown(context.Context) error {
	p.shutdowns++
	if p.unsub != nil {
		p.unsub()
		p.unsub = nil
	}
	return nil
}

func newBootRetryEngineMulti(t *testing.T, plugins ...Plugin) *Engine {
	t.Helper()
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = filepath.Join(root, "sessions")
	cfg.Core.Storage.Root = root
	eng := newFromConfig(cfg)
	for _, p := range plugins {
		cfg.Plugins.Active = append(cfg.Plugins.Active, p.ID())
		eng.Registry.Register(p.ID(), func() Plugin { return p })
	}
	return eng
}

// A failed Boot closes the journal writer it started — drain goroutine and
// segment file included — and detaches it from the engine.
func TestBoot_FailureClosesJournal(t *testing.T) {
	p := &subscriberPlugin{id: "nexus.test.journal_probe", failInit: true}
	eng := newBootRetryEngineMulti(t, p)

	if err := eng.Boot(context.Background()); err == nil {
		t.Fatal("first Boot succeeded, want forced init failure")
	}
	if p.journal == nil {
		t.Fatal("plugin saw no journal writer; Boot failed before startJournal")
	}
	first := p.journal
	if !first.Closed() {
		t.Fatal("journal writer still open after failed Boot")
	}
	if eng.Journal != nil || eng.Lifecycle.journal != nil {
		t.Fatal("journal writer still attached after failed Boot")
	}

	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("retried Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	if eng.Journal == nil || eng.Journal.Closed() {
		t.Fatal("retried Boot has no open journal writer")
	}
	if eng.Journal == first {
		t.Fatal("retried Boot reused the closed writer")
	}
}

// Plugin A Inits, plugin B's Init fails: Boot shuts A down before returning,
// A's subscription is gone, B (never Init'd) is not shut down, and a retried
// Boot registers A's handler exactly once.
func TestBoot_InitFailureShutsDownInitializedPlugins(t *testing.T) {
	const evt = "test.boot_teardown.a"
	a := &subscriberPlugin{id: "nexus.test.a", event: evt}
	b := &subscriberPlugin{id: "nexus.test.b", deps: []string{"nexus.test.a"}, failInit: true}
	eng := newBootRetryEngineMulti(t, a, b)
	before := busSubscriptionCounts(t, eng.Bus)

	err := eng.Boot(context.Background())
	if err == nil || !strings.Contains(err.Error(), "forced init failure") {
		t.Fatalf("first Boot err = %v, want the forced init failure", err)
	}
	if a.shutdowns != 1 {
		t.Fatalf("A.Shutdown calls = %d, want 1", a.shutdowns)
	}
	if b.shutdowns != 0 {
		t.Fatalf("B.Shutdown calls = %d, want 0 (its Init failed)", b.shutdowns)
	}
	if got := busSubscriptionCounts(t, eng.Bus); !equalCounts(got, before) {
		t.Fatalf("subscriptions after failed Boot = %v, want pre-Boot %v", got, before)
	}
	if n := len(eng.Lifecycle.Plugins()); n != 0 {
		t.Fatalf("Lifecycle.Plugins() after failed Boot = %d, want 0", n)
	}

	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("retried Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	if n := busSubscriptionCounts(t, eng.Bus)[evt]; n != 1 {
		t.Fatalf("A's subscriptions after retry = %d, want 1", n)
	}
}

// A Ready failure happens after every plugin Init'd, so every plugin —
// the failing one included — is shut down, in reverse init order.
func TestBoot_ReadyFailureShutsDownAllPlugins(t *testing.T) {
	var order []string
	a := &orderedShutdown{subscriberPlugin: subscriberPlugin{id: "nexus.test.ra", event: "test.boot_teardown.ra"}, order: &order}
	b := &orderedShutdown{subscriberPlugin: subscriberPlugin{id: "nexus.test.rb", deps: []string{"nexus.test.ra"}, event: "test.boot_teardown.rb", failReady: true}, order: &order}
	eng := newBootRetryEngineMulti(t, a, b)
	before := busSubscriptionCounts(t, eng.Bus)

	if err := eng.Boot(context.Background()); err == nil || !strings.Contains(err.Error(), "forced ready failure") {
		t.Fatalf("Boot err = %v, want the forced ready failure", err)
	}
	if want := []string{"nexus.test.rb", "nexus.test.ra"}; !slices.Equal(order, want) {
		t.Fatalf("shutdown order = %v, want %v", order, want)
	}
	if got := busSubscriptionCounts(t, eng.Bus); !equalCounts(got, before) {
		t.Fatalf("subscriptions after failed Boot = %v, want pre-Boot %v", got, before)
	}
}

type orderedShutdown struct {
	subscriberPlugin
	order *[]string
}

func (p *orderedShutdown) Shutdown(ctx context.Context) error {
	*p.order = append(*p.order, p.id)
	return p.subscriberPlugin.Shutdown(ctx)
}

// A Shutdown that errors or panics during failed-Boot teardown is logged, and
// the caller still gets the original Boot error.
func TestBoot_TeardownShutdownErrorsDoNotMaskBootError(t *testing.T) {
	a := &panickyShutdown{subscriberPlugin: subscriberPlugin{id: "nexus.test.pa"}}
	b := &subscriberPlugin{id: "nexus.test.pb", deps: []string{"nexus.test.pa"}, failInit: true}
	eng := newBootRetryEngineMulti(t, a, b)

	err := eng.Boot(context.Background())
	if err == nil || !strings.Contains(err.Error(), "forced init failure") {
		t.Fatalf("Boot err = %v, want the forced init failure", err)
	}
	if !a.called {
		t.Fatal("A.Shutdown was not called")
	}
}

type panickyShutdown struct {
	subscriberPlugin
	called bool
}

func (p *panickyShutdown) Shutdown(context.Context) error {
	p.called = true
	panic("shutdown before ready")
}

// partialInitPlugin subscribes typed and wildcard handlers, then fails its
// own Init. It is never shut down, so only the engine can remove them.
type partialInitPlugin struct {
	subscriberPlugin
	fired *int
}

func (p *partialInitPlugin) Init(ctx PluginContext) error {
	ctx.Bus.Subscribe(p.event, func(Event[any]) { *p.fired++ })
	ctx.Bus.SubscribeAll(func(e Event[any]) {
		if e.Type == p.event {
			*p.fired++
		}
	})
	return errors.New("forced init failure after subscribing")
}

// A plugin whose own Init fails after subscribing part-way has those
// subscriptions removed by the engine (#222): after the Boot error, emitting
// the event reaches none of its handlers and the handler table is back to
// its pre-Boot shape. A plugin Init'd before it keeps working until its own
// Shutdown, which teardownFailedBoot runs.
func TestBoot_OwnInitFailureRemovesItsSubscriptions(t *testing.T) {
	const evt = "test.boot_teardown.partial"
	fired := 0
	a := &subscriberPlugin{id: "nexus.test.pre"}
	p := &partialInitPlugin{
		subscriberPlugin: subscriberPlugin{id: "nexus.test.partial", deps: []string{"nexus.test.pre"}, event: evt},
		fired:            &fired,
	}
	eng := newBootRetryEngineMulti(t, a, p)
	before := busSubscriptionCounts(t, eng.Bus)

	err := eng.Boot(context.Background())
	if err == nil || !strings.Contains(err.Error(), "forced init failure after subscribing") {
		t.Fatalf("Boot err = %v, want the forced init failure", err)
	}
	if p.shutdowns != 0 {
		t.Fatalf("failing plugin Shutdown calls = %d, want 0", p.shutdowns)
	}
	if a.shutdowns != 1 {
		t.Fatalf("earlier plugin Shutdown calls = %d, want 1", a.shutdowns)
	}

	_ = eng.Bus.Emit(evt, nil)
	if fired != 0 {
		t.Fatalf("handlers of the plugin whose Init failed fired %d times after Boot error, want 0", fired)
	}
	if got := busSubscriptionCounts(t, eng.Bus); !equalCounts(got, before) {
		t.Fatalf("subscriptions after failed Boot = %v, want pre-Boot %v", got, before)
	}
}

// Subscriptions made during a successful Init are left alone: the capture
// window only ever undoes a failed Init.
func TestBoot_SuccessfulInitKeepsSubscriptions(t *testing.T) {
	const evt = "test.boot_teardown.kept"
	p := &subscriberPlugin{id: "nexus.test.kept", event: evt}
	eng := newBootRetryEngineMulti(t, p)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	if n := busSubscriptionCounts(t, eng.Bus)[evt]; n != 1 {
		t.Fatalf("subscriptions on %s after Boot = %d, want 1", evt, n)
	}
}
