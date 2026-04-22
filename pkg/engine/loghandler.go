package engine

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
)

// DefaultLogRingSize is the default capacity of the in-engine log and event
// ring buffers.
const DefaultLogRingSize = 4096

// LoggingHost is the engine-side surface a plugin uses to become a log sink.
// It is threaded onto PluginContext so the logger plugin (and only the logger
// plugin, in practice) can register its slog.Handler and receive both the
// buffered pre-init records and every live record from that point forward.
//
// The "capture, not alter" contract: registering a sink does not transform
// records, strip attrs, or fan out to any implicit sink. The sink sees exactly
// what the engine logger emits. When no sink is registered, records accumulate
// in a bounded ring until one registers or they are evicted.
type LoggingHost interface {
	// AddLogSink registers a slog.Handler. On registration, every record
	// currently in the engine's ring buffer is replayed to the sink in
	// emission order; subsequent records are dispatched live. The returned
	// function deregisters the sink.
	AddLogSink(h slog.Handler) (remove func())
}

// FanoutHandler is the slog.Handler installed as the engine's root logger. It
// writes every record into a bounded ring buffer and to every currently
// registered sink. It has no implicit output target — absent a registered
// sink, records only live in the ring. This keeps log output from leaking to
// stdout/stderr when a visual IO plugin owns the terminal.
//
// FanoutHandler supports WithAttrs for per-plugin scoping (lifecycle attaches
// a "plugin" attr per plugin logger). WithGroup is not supported and panics
// if called — no plugin uses it today and proper retroactive grouping across
// dynamic sinks is not trivial to implement. Revisit if a consumer appears.
type FanoutHandler struct {
	core  *fanoutCore
	attrs []slog.Attr
}

// fanoutCore is the shared state across all handlers derived via WithAttrs.
// Every derivation points at the same core so sink changes are observed by
// every logger.
type fanoutCore struct {
	mu       sync.RWMutex
	sinks    []*sinkEntry
	ring     *logRing
	level    slog.Level
	nextID   uint64
	overflow atomic.Uint64
}

type sinkEntry struct {
	id      uint64
	handler slog.Handler
}

// NewFanoutHandler builds a FanoutHandler with a ring buffer of the given
// capacity. Capacity <= 0 falls back to DefaultLogRingSize. The level gates
// Enabled for the whole handler; sinks may apply additional filtering.
func NewFanoutHandler(capacity int, level slog.Level) *FanoutHandler {
	if capacity <= 0 {
		capacity = DefaultLogRingSize
	}
	return &FanoutHandler{
		core: &fanoutCore{
			ring:  newLogRing(capacity),
			level: level,
		},
	}
}

// Enabled reports whether records at the given level should be processed.
// Sinks that want stricter filtering should implement their own Enabled.
func (h *FanoutHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.core.level
}

// Handle applies any accumulated WithAttrs state to the record, appends it to
// the ring buffer, and dispatches a clone to every currently-registered sink.
func (h *FanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	if len(h.attrs) > 0 {
		r.AddAttrs(h.attrs...)
	}

	h.core.mu.Lock()
	h.core.ring.append(&h.core.overflow, r.Clone())
	sinks := make([]*sinkEntry, len(h.core.sinks))
	copy(sinks, h.core.sinks)
	h.core.mu.Unlock()

	for _, s := range sinks {
		if !s.handler.Enabled(ctx, r.Level) {
			continue
		}
		_ = s.handler.Handle(ctx, r.Clone())
	}
	return nil
}

// WithAttrs returns a derived handler that appends attrs to every record it
// processes. The derivation shares the underlying core, so sinks added after
// derivation still see records flowing through the child.
func (h *FanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	next := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(next, h.attrs)
	copy(next[len(h.attrs):], attrs)
	return &FanoutHandler{core: h.core, attrs: next}
}

// WithGroup is not supported. Nexus plugin code does not use groups today; the
// retroactive-group semantics across dynamically registered sinks are not
// worth the complexity until a concrete consumer exists.
func (h *FanoutHandler) WithGroup(_ string) slog.Handler {
	panic("engine.FanoutHandler: WithGroup is not supported")
}

// AddLogSink registers a sink. Records already buffered in the ring are
// replayed to the sink in emission order before live records flow, so a
// late-arriving sink never misses pre-init output. The returned remove
// function is idempotent — calling it more than once is a no-op.
func (h *FanoutHandler) AddLogSink(sink slog.Handler) (remove func()) {
	h.core.mu.Lock()
	h.core.nextID++
	entry := &sinkEntry{id: h.core.nextID, handler: sink}
	h.core.sinks = append(h.core.sinks, entry)
	replay := h.core.ring.snapshot()
	h.core.mu.Unlock()

	// Replay outside the lock so a slow sink does not stall other emitters.
	// Records emitted concurrently with this replay will also be dispatched
	// live — the sink may see a brief window of interleaving at boundary,
	// but never loses records and never sees them out of emission order
	// within each class (replay vs. live).
	ctx := context.Background()
	for _, r := range replay {
		if !sink.Enabled(ctx, r.Level) {
			continue
		}
		_ = sink.Handle(ctx, r.Clone())
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			h.core.mu.Lock()
			defer h.core.mu.Unlock()
			for i, s := range h.core.sinks {
				if s.id == entry.id {
					h.core.sinks = append(h.core.sinks[:i], h.core.sinks[i+1:]...)
					return
				}
			}
		})
	}
}

// RingStats returns the current number of records in the ring and the total
// count of records evicted since construction. Useful for tests and
// observability around the bootstrap phase.
func (h *FanoutHandler) RingStats() (size int, overflow uint64) {
	h.core.mu.RLock()
	size = h.core.ring.size
	h.core.mu.RUnlock()
	overflow = h.core.overflow.Load()
	return size, overflow
}

// logRing is a fixed-capacity ring of slog.Records. append evicts the oldest
// entry when full and bumps the provided overflow counter. Not safe for
// concurrent use; callers hold the fanoutCore mutex.
type logRing struct {
	buf  []slog.Record
	head int
	size int
	cap  int
}

func newLogRing(capacity int) *logRing {
	return &logRing{
		buf: make([]slog.Record, capacity),
		cap: capacity,
	}
}

func (r *logRing) append(overflow *atomic.Uint64, rec slog.Record) {
	if r.size == r.cap {
		overflow.Add(1)
		r.head = (r.head + 1) % r.cap
		r.size--
	}
	idx := (r.head + r.size) % r.cap
	r.buf[idx] = rec
	r.size++
}

func (r *logRing) snapshot() []slog.Record {
	out := make([]slog.Record, r.size)
	for i := 0; i < r.size; i++ {
		out[i] = r.buf[(r.head+i)%r.cap]
	}
	return out
}
