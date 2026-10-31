package contract

import (
	"context"
	"strings"
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

// priorityStub declares its subscriptions at one set of priorities and
// registers them at another, so tests can drive the declared-vs-registered
// priority check NewContract runs automatically.
type priorityStub struct {
	declared   []engine.EventSubscription
	registered []engine.EventSubscription
	source     string // WithSource tag; "" means the stub's ID, "-" means none
	unsubs     []func()
}

func (p *priorityStub) ID() string                        { return "nexus.test.priority" }
func (p *priorityStub) Name() string                      { return "Priority Stub" }
func (p *priorityStub) Version() string                   { return "0.1.0" }
func (p *priorityStub) Dependencies() []string            { return nil }
func (p *priorityStub) Requires() []engine.Requirement    { return nil }
func (p *priorityStub) Capabilities() []engine.Capability { return nil }
func (p *priorityStub) Subscriptions() []engine.EventSubscription {
	return p.declared
}
func (p *priorityStub) Emissions() []string { return nil }
func (p *priorityStub) Init(ctx engine.PluginContext) error {
	opts := func(prio int) []engine.SubscribeOption {
		o := []engine.SubscribeOption{engine.WithPriority(prio)}
		switch p.source {
		case "":
			o = append(o, engine.WithSource(p.ID()))
		case "-":
		default:
			o = append(o, engine.WithSource(p.source))
		}
		return o
	}
	for _, s := range p.registered {
		p.unsubs = append(p.unsubs, ctx.Bus.Subscribe(s.EventType, func(engine.Event[any]) {}, opts(s.Priority)...))
	}
	return nil
}
func (p *priorityStub) Ready() error { return nil }
func (p *priorityStub) Shutdown(_ context.Context) error {
	for _, u := range p.unsubs {
		u()
	}
	return nil
}

func TestContract_SubscriptionPriority(t *testing.T) {
	cases := []struct {
		name       string
		declared   []engine.EventSubscription
		registered []engine.EventSubscription
		wantFail   bool
	}{
		{
			// The #213 shape: declared priority 2, subscribed with no
			// WithPriority (0). before:llm.request ordering silently moves.
			name:       "declared priority not registered",
			declared:   []engine.EventSubscription{{EventType: "before:llm.request", Priority: 2}},
			registered: []engine.EventSubscription{{EventType: "before:llm.request", Priority: 0}},
			wantFail:   true,
		},
		{
			name:       "matching priority",
			declared:   []engine.EventSubscription{{EventType: "before:llm.request", Priority: 2}},
			registered: []engine.EventSubscription{{EventType: "before:llm.request", Priority: 2}},
		},
		{
			name: "two handlers on one type, both declared",
			declared: []engine.EventSubscription{
				{EventType: "llm.response", Priority: 5},
				{EventType: "llm.response", Priority: 99},
			},
			registered: []engine.EventSubscription{
				{EventType: "llm.response", Priority: 99},
				{EventType: "llm.response", Priority: 5},
			},
		},
		{
			name:       "second handler on a declared type left undeclared",
			declared:   []engine.EventSubscription{{EventType: "llm.response", Priority: 99}},
			registered: []engine.EventSubscription{{EventType: "llm.response", Priority: 5}, {EventType: "llm.response", Priority: 99}},
			wantFail:   true,
		},
		{
			// Config-conditional subscriptions: declared but not wired in
			// this config is not a priority mismatch.
			name:     "declared but never registered",
			declared: []engine.EventSubscription{{EventType: "before:io.output", Priority: 8}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subT := &testing.T{}
			NewContract(subT, func() engine.Plugin {
				return &priorityStub{declared: tc.declared, registered: tc.registered}
			})
			if subT.Failed() != tc.wantFail {
				t.Errorf("NewContract failed = %v, want %v", subT.Failed(), tc.wantFail)
			}
		})
	}
}

func TestContract_RegisteredSubscriptionsExcludesTestSubscriptions(t *testing.T) {
	h := NewContract(t, newStubPlugin)
	h.Bus().Subscribe("stub.input", func(engine.Event[any]) {}, engine.WithPriority(7))

	got := h.RegisteredSubscriptions()
	if len(got) != 1 {
		t.Fatalf("RegisteredSubscriptions() = %+v, want only the plugin's own subscription", got)
	}
	want := engine.SubscriptionInfo{EventType: "stub.input", Priority: 0, Source: "nexus.test.contract.stub"}
	if got[0] != want {
		t.Errorf("RegisteredSubscriptions()[0] = %+v, want %+v", got[0], want)
	}
}

func TestContract_UndeclaredSubscription(t *testing.T) {
	cases := []struct {
		name       string
		declared   []engine.EventSubscription
		registered []engine.EventSubscription
		source     string // WithSource tag on every registration; "-" means none
		wantFail   bool
	}{
		{
			// The output_length shape: a shared helper registers on the
			// plugin's bus for a type the plugin never declared.
			name:       "registered type absent from declarations",
			declared:   []engine.EventSubscription{{EventType: "before:io.output", Priority: 12}},
			registered: []engine.EventSubscription{{EventType: "before:io.output", Priority: 12}, {EventType: "llm.response", Priority: 5}},
			wantFail:   true,
		},
		{
			name:       "untagged registration is the plugin's",
			declared:   []engine.EventSubscription{{EventType: "before:io.output", Priority: 12}},
			registered: []engine.EventSubscription{{EventType: "llm.response", Priority: 5}},
			source:     "-",
			wantFail:   true,
		},
		{
			name:       "every registered type declared",
			declared:   []engine.EventSubscription{{EventType: "before:io.output", Priority: 12}, {EventType: "llm.response", Priority: 5}},
			registered: []engine.EventSubscription{{EventType: "before:io.output", Priority: 12}, {EventType: "llm.response", Priority: 5}},
		},
		{
			// Tagged with another plugin's ID: that plugin's contract.
			name:       "registration attributed to a different plugin",
			registered: []engine.EventSubscription{{EventType: "llm.response", Priority: 5}},
			source:     "nexus.test.other",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subT := &testing.T{}
			NewContract(subT, func() engine.Plugin {
				return &priorityStub{declared: tc.declared, registered: tc.registered, source: tc.source}
			})
			if subT.Failed() != tc.wantFail {
				t.Errorf("NewContract failed = %v, want %v", subT.Failed(), tc.wantFail)
			}
		})
	}
}

func TestUndeclaredSubscriptionsMessage(t *testing.T) {
	msg := undeclaredSubscriptions("nexus.gate.output_length", "nexus.gate.output_length",
		[]engine.EventSubscription{{EventType: "before:io.output", Priority: 12}},
		[]engine.SubscriptionInfo{{EventType: "llm.response", Priority: 5, Source: "nexus.gate.output_length"}})
	for _, want := range []string{`"nexus.gate.output_length"`, `"llm.response"`, "priority 5"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not name %s", msg, want)
		}
	}
}
