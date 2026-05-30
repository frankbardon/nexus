package icm

import (
	"context"
	"io"
	"log/slog"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	icmruntime "github.com/frankbardon/nexus/plugins/workflows/icm/runtime"
)

// recordingBus is a minimal engine.EventBus stand-in for tests in the
// main icm package. It records every Emit call so tests can inspect the
// last event of a given type. Subscribe is best-effort (handlers fire
// synchronously on Emit) — sufficient for the limited tool-result
// surface exercised here.
type recordingBus struct {
	mu     sync.Mutex
	events []recordedEvent
	subs   map[string][]engine.HandlerFunc
}

type recordedEvent struct {
	Type    string
	Payload any
}

func newRecordingBus() *recordingBus {
	return &recordingBus{subs: map[string][]engine.HandlerFunc{}}
}

func (b *recordingBus) Emit(eventType string, payload any) error {
	b.mu.Lock()
	b.events = append(b.events, recordedEvent{Type: eventType, Payload: payload})
	handlers := append([]engine.HandlerFunc(nil), b.subs[eventType]...)
	b.mu.Unlock()
	for _, h := range handlers {
		h(engine.Event[any]{Type: eventType, Payload: payload})
	}
	return nil
}

func (b *recordingBus) EmitEvent(ev engine.Event[any]) error {
	return b.Emit(ev.Type, ev.Payload)
}

func (b *recordingBus) EmitAsync(eventType string, payload any) <-chan error {
	ch := make(chan error, 1)
	ch <- b.Emit(eventType, payload)
	close(ch)
	return ch
}

func (b *recordingBus) Subscribe(eventType string, handler engine.HandlerFunc, _ ...engine.SubscribeOption) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[eventType] = append(b.subs[eventType], handler)
	idx := len(b.subs[eventType]) - 1
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		list := b.subs[eventType]
		if idx < len(list) {
			b.subs[eventType] = append(list[:idx], list[idx+1:]...)
		}
	}
}

func (b *recordingBus) SubscribeAll(_ engine.HandlerFunc) func()       { return func() {} }
func (b *recordingBus) SubscribeAllReplay(_ engine.HandlerFunc) func() { return func() {} }

func (b *recordingBus) EmitVetoable(_ string, _ any) (engine.VetoResult, error) {
	return engine.VetoResult{}, nil
}

func (b *recordingBus) Drain(_ context.Context) error { return nil }

// last returns the most recent payload for an event type, or nil.
func (b *recordingBus) last(eventType string) any {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.events) - 1; i >= 0; i-- {
		if b.events[i].Type == eventType {
			return b.events[i].Payload
		}
	}
	return nil
}

// newTestPlugin returns a minimally-populated Plugin sufficient for
// tests that exercise tool surfaces (validate, configuration) without
// going through Init.
func newTestPlugin(bus engine.EventBus) *Plugin {
	return &Plugin{
		instanceID:    defaultPluginID,
		bus:           bus,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		hitlWait:      map[string]chan events.HITLResponse{},
		orchestrators: map[string]*icmruntime.Orchestrator{},
	}
}
