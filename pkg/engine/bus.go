package engine

import (
	"context"
	"sort"
	"sync"
	"time"
)

// EventBus is the central event dispatch system.
type EventBus interface {
	// Emit dispatches an event to all matching handlers synchronously.
	Emit(eventType string, payload any) error
	// EmitEvent dispatches a pre-built event.
	EmitEvent(event Event[any]) error
	// EmitAsync dispatches an event asynchronously, returning immediately.
	// Handlers run in a separate goroutine. The returned channel receives
	// nil on success or an error, then is closed.
	EmitAsync(eventType string, payload any) <-chan error
	// Subscribe registers a handler for an event type.
	// Returns an unsubscribe function.
	Subscribe(eventType string, handler HandlerFunc, opts ...SubscribeOption) (unsubscribe func())
	// SubscribeAll registers a wildcard handler that receives all events
	// emitted from this point forward. Pre-subscription events are not seen.
	SubscribeAll(handler HandlerFunc) (unsubscribe func())
	// SubscribeAllReplay is SubscribeAll plus a replay of every event
	// currently held in the bus's ring buffer. The replay runs before live
	// dispatch begins for the new subscriber, so observers that init after
	// other plugins have already emitted still see those events. Replay and
	// live dispatch are serialized: no event is delivered twice, none is
	// dropped at the boundary.
	SubscribeAllReplay(handler HandlerFunc) (unsubscribe func())
	// EmitVetoable dispatches a before:* event. Handlers may veto by setting
	// the VetoResult on the payload. Returns the result.
	EmitVetoable(eventType string, payload any) (VetoResult, error)
	// Drain waits for all in-flight events to complete within the given context.
	Drain(ctx context.Context) error
}

// HandlerFunc is the callback signature for event handlers.
type HandlerFunc func(Event[any])

// SubscribeOption configures a subscription.
type SubscribeOption func(*subscribeConfig)

type subscribeConfig struct {
	priority int
	filter   EventFilter
	source   string
}

// WithPriority sets the handler priority (lower = earlier execution).
func WithPriority(p int) SubscribeOption {
	return func(c *subscribeConfig) {
		c.priority = p
	}
}

// WithFilter sets an event filter on the subscription.
func WithFilter(f EventFilter) SubscribeOption {
	return func(c *subscribeConfig) {
		c.filter = f
	}
}

// WithSource tags the subscription with a plugin ID.
func WithSource(pluginID string) SubscribeOption {
	return func(c *subscribeConfig) {
		c.source = pluginID
	}
}

type subscription struct {
	id       uint64
	handler  HandlerFunc
	priority int
	filter   EventFilter
	source   string
}

type eventBus struct {
	mu        sync.RWMutex
	handlers  map[string][]*subscription
	wildcards []*subscription
	nextID    uint64
	inflight  sync.WaitGroup
	ring      *eventRing
}

// NewEventBus creates a new in-process EventBus with a default-sized ring
// buffer (DefaultLogRingSize). Pass NewEventBusWithRingSize to override.
func NewEventBus() EventBus {
	return NewEventBusWithRingSize(DefaultLogRingSize)
}

// NewEventBusWithRingSize builds a bus whose replay ring holds up to capacity
// events. capacity <= 0 falls back to DefaultLogRingSize.
func NewEventBusWithRingSize(capacity int) EventBus {
	if capacity <= 0 {
		capacity = DefaultLogRingSize
	}
	return &eventBus{
		handlers: make(map[string][]*subscription),
		ring:     newEventRing(capacity),
	}
}

func (b *eventBus) nextSubID() uint64 {
	b.nextID++
	return b.nextID
}

func (b *eventBus) Subscribe(eventType string, handler HandlerFunc, opts ...SubscribeOption) func() {
	cfg := &subscribeConfig{}
	for _, o := range opts {
		o(cfg)
	}

	b.mu.Lock()
	sub := &subscription{
		id:       b.nextSubID(),
		handler:  handler,
		priority: cfg.priority,
		filter:   cfg.filter,
		source:   cfg.source,
	}
	b.handlers[eventType] = append(b.handlers[eventType], sub)
	sort.Slice(b.handlers[eventType], func(i, j int) bool {
		return b.handlers[eventType][i].priority < b.handlers[eventType][j].priority
	})
	subID := sub.id
	b.mu.Unlock()

	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		subs := b.handlers[eventType]
		for i, s := range subs {
			if s.id == subID {
				b.handlers[eventType] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
	}
}

func (b *eventBus) SubscribeAll(handler HandlerFunc) func() {
	b.mu.Lock()
	sub := &subscription{
		id:      b.nextSubID(),
		handler: handler,
	}
	b.wildcards = append(b.wildcards, sub)
	subID := sub.id
	b.mu.Unlock()

	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, s := range b.wildcards {
			if s.id == subID {
				b.wildcards = append(b.wildcards[:i], b.wildcards[i+1:]...)
				break
			}
		}
	}
}

func (b *eventBus) SubscribeAllReplay(handler HandlerFunc) func() {
	b.mu.Lock()
	// Snapshot the ring and append the subscription under the same lock so
	// no event can slip into the gap between snapshot and registration. See
	// EmitEvent for the matching half of the protocol.
	replay := b.ring.snapshot()
	sub := &subscription{
		id:      b.nextSubID(),
		handler: handler,
	}
	b.wildcards = append(b.wildcards, sub)
	subID := sub.id
	b.mu.Unlock()

	// Replay buffered events outside the lock so a slow handler does not
	// stall concurrent emitters. Live events emitted from this point already
	// see the new wildcard and are dispatched normally.
	for _, e := range replay {
		handler(e)
	}

	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, s := range b.wildcards {
			if s.id == subID {
				b.wildcards = append(b.wildcards[:i], b.wildcards[i+1:]...)
				break
			}
		}
	}
}

func (b *eventBus) Emit(eventType string, payload any) error {
	event := Event[any]{
		Type:      eventType,
		ID:        GenerateID(),
		Timestamp: time.Now(),
		Payload:   payload,
	}
	return b.EmitEvent(event)
}

func (b *eventBus) EmitEvent(event Event[any]) error {
	b.inflight.Add(1)
	defer b.inflight.Done()

	meta := event.Meta()

	// Append to the replay ring and snapshot handlers under a single write
	// lock. Serializing this block with SubscribeAllReplay is what guarantees
	// every event is delivered to every subscriber exactly once — either in
	// the replay snapshot (if it entered the ring before the subscriber was
	// added to wildcards) or in live dispatch (if it was emitted after).
	b.mu.Lock()
	b.ring.append(event)
	typed := make([]*subscription, len(b.handlers[event.Type]))
	copy(typed, b.handlers[event.Type])
	wilds := make([]*subscription, len(b.wildcards))
	copy(wilds, b.wildcards)
	b.mu.Unlock()

	for _, sub := range typed {
		if sub.filter != nil && !sub.filter(meta) {
			continue
		}
		sub.handler(event)
	}

	for _, sub := range wilds {
		if sub.filter != nil && !sub.filter(meta) {
			continue
		}
		sub.handler(event)
	}

	return nil
}

func (b *eventBus) EmitAsync(eventType string, payload any) <-chan error {
	ch := make(chan error, 1)
	go func() {
		ch <- b.Emit(eventType, payload)
		close(ch)
	}()
	return ch
}

func (b *eventBus) EmitVetoable(eventType string, payload any) (VetoResult, error) {
	b.inflight.Add(1)
	defer b.inflight.Done()

	// Wrap in VetoablePayload so handlers can set Veto via the shared pointer.
	// Handlers receive the event by value, but VetoablePayload is a pointer —
	// mutations to vp.Veto propagate back here.
	vp := &VetoablePayload{Original: payload}
	event := Event[any]{
		Type:      eventType,
		ID:        GenerateID(),
		Timestamp: time.Now(),
		Payload:   vp,
	}
	meta := event.Meta()

	b.mu.Lock()
	b.ring.append(event)
	typed := make([]*subscription, len(b.handlers[eventType]))
	copy(typed, b.handlers[eventType])
	b.mu.Unlock()

	for _, sub := range typed {
		if sub.filter != nil && !sub.filter(meta) {
			continue
		}
		sub.handler(event)

		if vp.Veto.Vetoed {
			return vp.Veto, nil
		}
	}

	return VetoResult{}, nil
}

func (b *eventBus) Drain(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		b.inflight.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// eventRing is a fixed-capacity ring of Events used to replay pre-subscription
// events to late-arriving SubscribeAllReplay callers. Not safe for concurrent
// use; the bus mutex serializes access.
type eventRing struct {
	buf  []Event[any]
	head int
	size int
	cap  int
}

func newEventRing(capacity int) *eventRing {
	return &eventRing{
		buf: make([]Event[any], capacity),
		cap: capacity,
	}
}

func (r *eventRing) append(e Event[any]) {
	if r.size == r.cap {
		r.head = (r.head + 1) % r.cap
		r.size--
	}
	idx := (r.head + r.size) % r.cap
	r.buf[idx] = e
	r.size++
}

func (r *eventRing) snapshot() []Event[any] {
	out := make([]Event[any], r.size)
	for i := 0; i < r.size; i++ {
		out[i] = r.buf[(r.head+i)%r.cap]
	}
	return out
}
