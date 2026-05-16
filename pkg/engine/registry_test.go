package engine

import (
	"context"
	"sort"
	"sync"
	"testing"
)

// registryStub is a minimal Plugin used by registry tests; no engine boot
// occurs so the methods only need to satisfy the interface.
type registryStub struct{ id string }

func (r *registryStub) ID() string                         { return r.id }
func (r *registryStub) Name() string                       { return r.id }
func (r *registryStub) Version() string                    { return "test" }
func (r *registryStub) Dependencies() []string             { return nil }
func (r *registryStub) Requires() []Requirement            { return nil }
func (r *registryStub) Capabilities() []Capability         { return nil }
func (r *registryStub) Init(PluginContext) error           { return nil }
func (r *registryStub) Ready() error                       { return nil }
func (r *registryStub) Shutdown(context.Context) error     { return nil }
func (r *registryStub) Subscriptions() []EventSubscription { return nil }
func (r *registryStub) Emissions() []string                { return nil }

func TestNewPluginRegistry_Empty(t *testing.T) {
	r := NewPluginRegistry()
	if r == nil {
		t.Fatal("NewPluginRegistry returned nil")
	}
	if got := r.List(); len(got) != 0 {
		t.Fatalf("expected empty list, got %v", got)
	}
	if _, ok := r.Get("missing"); ok {
		t.Fatal("Get on empty registry returned ok=true")
	}
}

func TestPluginRegistry_RegisterAndGet(t *testing.T) {
	r := NewPluginRegistry()
	r.Register("nexus.test.alpha", func() Plugin { return &registryStub{id: "nexus.test.alpha"} })

	factory, ok := r.Get("nexus.test.alpha")
	if !ok {
		t.Fatal("Get returned ok=false for registered ID")
	}
	if factory == nil {
		t.Fatal("Get returned nil factory")
	}
	p := factory()
	if p == nil {
		t.Fatal("factory returned nil plugin")
	}
	if p.ID() != "nexus.test.alpha" {
		t.Fatalf("plugin ID = %q, want %q", p.ID(), "nexus.test.alpha")
	}
}

func TestPluginRegistry_Get_Miss(t *testing.T) {
	r := NewPluginRegistry()
	r.Register("present", func() Plugin { return &registryStub{id: "present"} })

	if _, ok := r.Get("absent"); ok {
		t.Fatal("Get returned ok=true for unregistered ID")
	}
}

func TestPluginRegistry_RegisterOverwrites(t *testing.T) {
	r := NewPluginRegistry()
	r.Register("dup", func() Plugin { return &registryStub{id: "first"} })
	r.Register("dup", func() Plugin { return &registryStub{id: "second"} })

	factory, ok := r.Get("dup")
	if !ok {
		t.Fatal("Get returned ok=false after re-registration")
	}
	if got := factory().ID(); got != "second" {
		t.Fatalf("expected second registration to win, got %q", got)
	}

	if got := r.List(); len(got) != 1 {
		t.Fatalf("expected exactly 1 entry after re-register, got %d (%v)", len(got), got)
	}
}

func TestPluginRegistry_List_Populated(t *testing.T) {
	r := NewPluginRegistry()
	want := []string{"a", "b", "c"}
	for _, id := range want {
		r.Register(id, func() Plugin { return &registryStub{id: id} })
	}

	got := r.List()
	if len(got) != len(want) {
		t.Fatalf("List length = %d, want %d (%v)", len(got), len(want), got)
	}

	// Map iteration order is randomized — sort before comparing.
	sort.Strings(got)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestPluginRegistry_FactoryProducesFreshInstances(t *testing.T) {
	r := NewPluginRegistry()
	r.Register("fresh", func() Plugin { return &registryStub{id: "fresh"} })

	factory, _ := r.Get("fresh")
	a := factory()
	b := factory()
	if a == b {
		t.Fatal("factory returned same pointer twice; expected fresh instance per call")
	}
}

// TestPluginRegistry_Concurrent exercises the RWMutex under concurrent
// Register / Get / List from multiple goroutines. Run under `go test -race`
// to verify lock correctness.
func TestPluginRegistry_Concurrent(t *testing.T) {
	r := NewPluginRegistry()

	const writers = 8
	const readers = 8
	const ops = 200

	var wg sync.WaitGroup
	wg.Add(writers + readers)

	for w := range writers {
		go func() {
			defer wg.Done()
			for i := range ops {
				id := "p" + string(rune('a'+w)) + string(rune('0'+(i%10)))
				r.Register(id, func() Plugin { return &registryStub{id: id} })
			}
		}()
	}

	for range readers {
		go func() {
			defer wg.Done()
			for range ops {
				_ = r.List()
				_, _ = r.Get("pa0")
			}
		}()
	}

	wg.Wait()

	if got := r.List(); len(got) == 0 {
		t.Fatal("expected at least one registered plugin after concurrent writes")
	}
}
