package engine

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

// subsPlugin is a testPlugin whose declared subscriptions are what the policy
// reads. Only Subscriptions() and ID() matter here.
type subsPlugin struct {
	*testPlugin
	subs []EventSubscription
}

func (p *subsPlugin) Subscriptions() []EventSubscription { return p.subs }

func newSubsPlugin(id string, eventTypes ...string) *subsPlugin {
	subs := make([]EventSubscription, 0, len(eventTypes))
	for _, et := range eventTypes {
		subs = append(subs, EventSubscription{EventType: et})
	}
	return &subsPlugin{testPlugin: &testPlugin{id: id}, subs: subs}
}

func TestStreamUnsafePlugins(t *testing.T) {
	tests := []struct {
		name    string
		plugins []Plugin
		want    []string
	}{
		{
			name: "response-only gate is unsafe",
			plugins: []Plugin{
				newSubsPlugin("gate.a", "before:llm.response"),
			},
			want: []string{"gate.a"},
		},
		{
			name: "a gate that also handles the stream is safe",
			plugins: []Plugin{
				newSubsPlugin("gate.a", "before:llm.response", EventBeforeStreamChunk),
			},
			want: nil,
		},
		{
			name: "a stream-only handler is not a response gate at all",
			plugins: []Plugin{
				newSubsPlugin("obs.a", EventBeforeStreamChunk),
			},
			want: nil,
		},
		{
			name: "plugins unrelated to either hook are ignored",
			plugins: []Plugin{
				newSubsPlugin("tool.a", "before:tool.invoke", "before:io.output"),
			},
			want: nil,
		},
		{
			name: "one unsafe among safe ones still counts, sorted",
			plugins: []Plugin{
				newSubsPlugin("gate.z", "before:llm.response"),
				newSubsPlugin("gate.a", "before:llm.response", EventBeforeStreamChunk),
				newSubsPlugin("gate.m", "before:llm.response"),
			},
			want: []string{"gate.m", "gate.z"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StreamUnsafePlugins(tt.plugins)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// emitRequest runs a request through before:llm.request the way an agent loop
// does and reports whether Stream survived.
func emitRequest(bus EventBus, stream bool) bool {
	req := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Stream: stream}
	_, _ = bus.EmitVetoable("before:llm.request", &req)
	return req.Stream
}

// TestInstallStreamPolicy_DowngradesForResponseOnlyGate is the hole this
// closes: before it, an operator running a whole-response gate under
// streaming got no disclosure protection and no warning, and the documented
// mitigation ("require stream: false") was not reachable through any config
// key — the agent loops hardcode Stream: true.
func TestInstallStreamPolicy_DowngradesForResponseOnlyGate(t *testing.T) {
	bus := NewEventBus()
	plugins := []Plugin{newSubsPlugin("gate.a", "before:llm.response")}

	unsub := installStreamPolicy(bus, plugins, "", nil)
	if unsub == nil {
		t.Fatal("expected the policy to install for a response-only gate")
	}
	defer unsub()

	if emitRequest(bus, true) {
		t.Error("Stream should have been cleared for a response-only gate")
	}
}

func TestInstallStreamPolicy_NoopWhenEveryGateStreams(t *testing.T) {
	bus := NewEventBus()
	plugins := []Plugin{
		newSubsPlugin("gate.a", "before:llm.response", EventBeforeStreamChunk),
	}

	if unsub := installStreamPolicy(bus, plugins, "", nil); unsub != nil {
		defer unsub()
		t.Error("a gate that handles before:llm.stream.chunk should not cost the deployment streaming")
	}
	if !emitRequest(bus, true) {
		t.Error("Stream should have survived")
	}
}

// TestInstallStreamPolicy_NoopWithNoGates covers the common deployment: no
// output gating at all, so nothing subscribes and streaming is untouched.
func TestInstallStreamPolicy_NoopWithNoGates(t *testing.T) {
	bus := NewEventBus()
	if unsub := installStreamPolicy(bus, nil, "", nil); unsub != nil {
		defer unsub()
		t.Error("no plugins means nothing to downgrade for")
	}
	if !emitRequest(bus, true) {
		t.Error("Stream should have survived")
	}
}

// TestInstallStreamPolicy_AllowLeavesStreamAlone is the operator's escape
// hatch: keep the latency, accept that the veto no longer governs disclosure.
func TestInstallStreamPolicy_AllowLeavesStreamAlone(t *testing.T) {
	bus := NewEventBus()
	plugins := []Plugin{newSubsPlugin("gate.a", "before:llm.response")}

	if unsub := installStreamPolicy(bus, plugins, StreamPolicyAllow, nil); unsub != nil {
		defer unsub()
		t.Error("allow policy must not install a downgrade handler")
	}
	if !emitRequest(bus, true) {
		t.Error("Stream should have survived under the allow policy")
	}
}

// TestInstallStreamPolicy_NeverVetoes pins that downgrading a request is not
// a reason to refuse it.
func TestInstallStreamPolicy_NeverVetoes(t *testing.T) {
	bus := NewEventBus()
	plugins := []Plugin{newSubsPlugin("gate.a", "before:llm.response")}
	unsub := installStreamPolicy(bus, plugins, StreamPolicyDowngrade, nil)
	if unsub == nil {
		t.Fatal("expected the policy to install")
	}
	defer unsub()

	req := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Stream: true}
	veto, err := bus.EmitVetoable("before:llm.request", &req)
	if err != nil {
		t.Fatalf("EmitVetoable: %v", err)
	}
	if veto.Vetoed {
		t.Error("the streaming downgrade must never veto the request")
	}
}

// TestInstallStreamPolicy_WinsOverEarlierHandlers checks the priority choice:
// a plugin that sets Stream on before:llm.request cannot outrank the engine's
// safety decision by accident.
func TestInstallStreamPolicy_WinsOverEarlierHandlers(t *testing.T) {
	bus := NewEventBus()
	plugins := []Plugin{newSubsPlugin("gate.a", "before:llm.response")}
	unsub := installStreamPolicy(bus, plugins, StreamPolicyDowngrade, nil)
	if unsub == nil {
		t.Fatal("expected the policy to install")
	}
	defer unsub()

	// A plugin-shaped handler at a conventional priority that turns streaming
	// back on.
	bus.Subscribe("before:llm.request", func(e Event[any]) {
		vp, ok := e.Payload.(*VetoablePayload)
		if !ok {
			return
		}
		if req, ok := vp.Original.(*events.LLMRequest); ok {
			req.Stream = true
		}
	}, WithPriority(50))

	if emitRequest(bus, false) {
		t.Error("the engine's downgrade must be the final word on Stream")
	}
}
