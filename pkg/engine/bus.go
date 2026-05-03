package engine

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// SeqController is implemented by buses that expose their dispatch seq
// counter. The engine uses it on session recall to continue numbering past
// the existing journal's LastSeq instead of resetting to 1.
type SeqController interface {
	SetSeqFloor(seq uint64)
}

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
	// drainMu serializes inflight.Add against inflight.Wait. Without it, a
	// race window exists where Wait wakes up because the counter hit zero
	// and is about to return — if Add(positive) lands during that window,
	// the runtime panics with "WaitGroup is reused before previous Wait
	// has returned." Holding drainMu around every Add and across the
	// entire Wait closes that window. Done is left lock-free; it can
	// never trigger the panic.
	drainMu sync.Mutex
	ring    *eventRing

	// seqCounter is the per-bus monotonic dispatch counter. Assigned at
	// EmitEvent / EmitVetoable entry, before typed handlers run, so a
	// nested emit always gets a higher seq than the event whose handler
	// triggered it. The journal writer reads this via CurrentSeq.
	seqCounter atomic.Uint64

	// dispatchMu guards dispatchStacks. Held only briefly during push/pop;
	// not the same lock as mu so dispatch stack reads don't contend with
	// handler-snapshot reads.
	dispatchMu     sync.Mutex
	dispatchStacks map[int64][]uint64
}

// DefaultEventRingSize is the default capacity of the bus's in-memory
// replay ring. Sized for the boot-time pre-subscription gap only — durable
// event history lives in the per-session journal at
// <session>/journal/events.jsonl. Pre-Phase-1 callers used the bus ring as
// a poor-man's event log; that role is gone.
//
// Bumping this does not buy you more durable history — use the journal or
// register a SubscribeAll handler at engine construction time.
const DefaultEventRingSize = 64

// NewEventBus creates a new in-process EventBus with the default-sized
// replay ring (DefaultEventRingSize). Pass NewEventBusWithRingSize to
// override — typical callers do not.
func NewEventBus() EventBus {
	return NewEventBusWithRingSize(DefaultEventRingSize)
}

// NewEventBusWithRingSize builds a bus whose replay ring holds up to capacity
// events. capacity <= 0 falls back to DefaultEventRingSize.
func NewEventBusWithRingSize(capacity int) EventBus {
	if capacity <= 0 {
		capacity = DefaultEventRingSize
	}
	return &eventBus{
		handlers:       make(map[string][]*subscription),
		ring:           newEventRing(capacity),
		dispatchStacks: make(map[int64][]uint64),
	}
}

func (b *eventBus) nextSubID() uint64 {
	b.nextID++
	return b.nextID
}

// pushDispatch records the seq for the goroutine that is about to run an
// event's handlers. Returns a pop function the caller defers.
func (b *eventBus) pushDispatch(seq uint64) func() {
	gid := goroutineID()
	b.dispatchMu.Lock()
	b.dispatchStacks[gid] = append(b.dispatchStacks[gid], seq)
	b.dispatchMu.Unlock()
	return func() {
		b.dispatchMu.Lock()
		stack := b.dispatchStacks[gid]
		if n := len(stack); n > 0 {
			stack = stack[:n-1]
			if len(stack) == 0 {
				delete(b.dispatchStacks, gid)
			} else {
				b.dispatchStacks[gid] = stack
			}
		}
		b.dispatchMu.Unlock()
	}
}

// SetSeqFloor advances the dispatch seq counter to at least floor. No-op if
// the counter is already higher. Used by the engine on session recall so
// freshly-dispatched events do not collide with journal entries from the
// prior run.
func (b *eventBus) SetSeqFloor(floor uint64) {
	for {
		cur := b.seqCounter.Load()
		if floor <= cur {
			return
		}
		if b.seqCounter.CompareAndSwap(cur, floor) {
			return
		}
	}
}

// CurrentSeq returns the seq of the event whose handlers are currently
// running on the calling goroutine, or 0 if no dispatch is in flight.
// Implements journal.SeqSource.
func (b *eventBus) CurrentSeq() uint64 {
	gid := goroutineID()
	b.dispatchMu.Lock()
	defer b.dispatchMu.Unlock()
	stack := b.dispatchStacks[gid]
	if len(stack) == 0 {
		return 0
	}
	return stack[len(stack)-1]
}

// ParentSeq returns the seq of the event whose handler triggered the current
// dispatch (the entry below the top of the goroutine's stack), or 0 if the
// current dispatch has no detectable parent. Implements journal.SeqSource.
func (b *eventBus) ParentSeq() uint64 {
	gid := goroutineID()
	b.dispatchMu.Lock()
	defer b.dispatchMu.Unlock()
	stack := b.dispatchStacks[gid]
	if len(stack) < 2 {
		return 0
	}
	return stack[len(stack)-2]
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
	b.drainMu.Lock()
	b.inflight.Add(1)
	b.drainMu.Unlock()
	defer b.inflight.Done()

	// Assign the dispatch seq before any handler runs so the journal sees
	// this event's seq in its causally-correct slot — a nested emit from a
	// typed handler will see this seq as its ParentSeq.
	seq := b.seqCounter.Add(1)
	pop := b.pushDispatch(seq)
	defer pop()

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
	b.drainMu.Lock()
	b.inflight.Add(1)
	b.drainMu.Unlock()
	defer b.inflight.Done()

	seq := b.seqCounter.Add(1)
	pop := b.pushDispatch(seq)
	defer pop()

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
	wilds := make([]*subscription, len(b.wildcards))
	copy(wilds, b.wildcards)
	b.mu.Unlock()

	var result VetoResult
	for _, sub := range typed {
		if sub.filter != nil && !sub.filter(meta) {
			continue
		}
		sub.handler(event)

		if vp.Veto.Vetoed {
			result = vp.Veto
			break
		}
	}

	// Wildcard dispatch always runs, even on veto, so the journal records
	// the event with vetoed=true. Wildcards see the same VetoablePayload
	// pointer the typed handlers saw, so they can detect veto state.
	for _, sub := range wilds {
		if sub.filter != nil && !sub.filter(meta) {
			continue
		}
		sub.handler(event)
	}

	return result, nil
}

func (b *eventBus) Drain(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		// Hold drainMu across the entire Wait. Any concurrent Add must
		// acquire drainMu first, so an Add cannot land in the brief
		// window between the WaitGroup counter hitting zero and Wait
		// returning — which is the only window that triggers the
		// "WaitGroup is reused before previous Wait has returned"
		// runtime panic.
		b.drainMu.Lock()
		b.inflight.Wait()
		b.drainMu.Unlock()
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
