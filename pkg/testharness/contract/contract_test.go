package contract

import (
	"context"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
)

// stubPlugin is a minimal Plugin used to self-test the contract harness
// without depending on any real plugin. It declares one subscription and
// one emission, mutates a counter on every input event, and emits a
// "stub.echo" event in response so emit-flow assertions exercise both
// directions.
type stubPlugin struct {
	bus     engine.EventBus
	unsubs  []func()
	echoes  int
	echoTag string
}

func newStubPlugin() engine.Plugin { return &stubPlugin{echoTag: "ok"} }

func (s *stubPlugin) ID() string                        { return "nexus.test.contract.stub" }
func (s *stubPlugin) Name() string                      { return "Contract Stub" }
func (s *stubPlugin) Version() string                   { return "0.1.0" }
func (s *stubPlugin) Dependencies() []string            { return nil }
func (s *stubPlugin) Requires() []engine.Requirement    { return nil }
func (s *stubPlugin) Capabilities() []engine.Capability { return nil }
func (s *stubPlugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{{EventType: "stub.input"}}
}
func (s *stubPlugin) Emissions() []string { return []string{"stub.echo"} }

func (s *stubPlugin) Init(ctx engine.PluginContext) error {
	s.bus = ctx.Bus
	s.unsubs = append(s.unsubs, s.bus.Subscribe("stub.input", func(ev engine.Event[any]) {
		s.echoes++
		ev2 := engine.Event[any]{Type: "stub.echo", ID: engine.GenerateID(), Source: s.ID(), Payload: s.echoTag}
		_ = s.bus.EmitEvent(ev2)
	}, engine.WithSource(s.ID())))
	return nil
}
func (s *stubPlugin) Ready() error { return nil }
func (s *stubPlugin) Shutdown(_ context.Context) error {
	for _, u := range s.unsubs {
		u()
	}
	return nil
}

func TestContract_SubscribeAndEmit(t *testing.T) {
	h := NewContract(t, newStubPlugin)

	h.AssertSubscribesTo("stub.input")
	h.Inject("stub.input", "ping")
	h.AssertEmitted("stub.echo")
	h.AssertEmittedInOrder("stub.echo")
	h.AssertNoUndeclaredEmissions()
}

func TestContract_AssertEmittedFailsWhenSilent(t *testing.T) {
	subT := &testing.T{}
	h := NewContract(subT, newStubPlugin)
	h.AssertEmitted("never.fired")
	if !subT.Failed() {
		t.Error("expected AssertEmitted to fail when nothing was emitted")
	}
}

func TestContract_NoUndeclaredEmissions_FailsWhenPluginExceedsContract(t *testing.T) {
	// A plugin that emits a type not in its Emissions() list violates the
	// contract; the assertion must catch it.
	bad := func() engine.Plugin {
		return &leakyStub{}
	}
	subT := &testing.T{}
	h := NewContract(subT, bad)
	h.Inject("stub.input", "ping")
	h.AssertNoUndeclaredEmissions()
	if !subT.Failed() {
		t.Error("expected AssertNoUndeclaredEmissions to flag undeclared emit")
	}
}

type leakyStub struct {
	bus    engine.EventBus
	unsubs []func()
}

func (l *leakyStub) ID() string                        { return "nexus.test.leaky" }
func (l *leakyStub) Name() string                      { return "Leaky" }
func (l *leakyStub) Version() string                   { return "0.1.0" }
func (l *leakyStub) Dependencies() []string            { return nil }
func (l *leakyStub) Requires() []engine.Requirement    { return nil }
func (l *leakyStub) Capabilities() []engine.Capability { return nil }
func (l *leakyStub) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{{EventType: "stub.input"}}
}
func (l *leakyStub) Emissions() []string { return []string{"declared.only"} }
func (l *leakyStub) Init(ctx engine.PluginContext) error {
	l.bus = ctx.Bus
	l.unsubs = append(l.unsubs, l.bus.Subscribe("stub.input", func(ev engine.Event[any]) {
		_ = l.bus.EmitEvent(engine.Event[any]{Type: "undeclared.leak", ID: engine.GenerateID(), Source: l.ID()})
	}, engine.WithSource(l.ID())))
	return nil
}
func (l *leakyStub) Ready() error { return nil }
func (l *leakyStub) Shutdown(_ context.Context) error {
	for _, u := range l.unsubs {
		u()
	}
	return nil
}

func TestContract_InjectVetoableReturnsResult(t *testing.T) {
	h := NewContract(t, newStubPlugin)
	h.Bus().Subscribe("before:test.action", func(ev engine.Event[any]) {
		vp, ok := ev.Payload.(*engine.VetoablePayload)
		if !ok {
			return
		}
		vp.Veto = engine.VetoResult{Vetoed: true, Reason: "blocked by test"}
	})
	res := h.InjectVetoable("before:test.action", &struct{}{})
	if !res.Vetoed {
		t.Error("expected veto, got pass")
	}
	if res.Reason != "blocked by test" {
		t.Errorf("reason = %q", res.Reason)
	}
}
