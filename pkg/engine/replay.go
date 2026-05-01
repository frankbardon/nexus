package engine

import (
	"context"
	"sync"
	"sync/atomic"
)

// replayKey is the unexported context key used to mark a context as in
// replay mode. Most replay decisions go through ReplayState (engine-wide
// flag + queues) rather than this context value, but the helpers are kept
// for plugins that prefer ctx-based control flow and for forward-compat
// with a future ctx-threaded bus.
type replayKey struct{}

// WithReplay returns a derived context tagged as a replay context.
func WithReplay(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, replayKey{}, true)
}

// IsReplay reports whether the calling context is a replay context.
func IsReplay(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, ok := ctx.Value(replayKey{}).(bool)
	return ok && v
}

// ReplayState is the engine-wide replay coordination point. It is always
// non-nil on a constructed Engine; plugins that need to short-circuit during
// deterministic replay query it via PluginContext.Replay.
//
// During replay, side-effecting plugins (LLM providers, tools) check
// Active() and pop the next journaled response from the per-event-type FIFO
// queue instead of calling out. Order-based lookup (Nth request consumes
// Nth response) keeps the surface area small — only emitting plugins whose
// outputs were originally journaled need replay awareness.
type ReplayState struct {
	active atomic.Bool

	mu     sync.Mutex
	queues map[string][]any
}

// NewReplayState constructs an idle ReplayState. The coordinator activates
// it and seeds queues; plugins consume.
func NewReplayState() *ReplayState {
	return &ReplayState{
		queues: make(map[string][]any),
	}
}

// Active reports whether the coordinator is currently driving replay. Cheap
// — backed by an atomic flag — safe to call from hot paths.
func (r *ReplayState) Active() bool {
	if r == nil {
		return false
	}
	return r.active.Load()
}

// SetActive flips the replay flag. Coordinator-only.
func (r *ReplayState) SetActive(active bool) {
	if r == nil {
		return
	}
	r.active.Store(active)
}

// Push appends a journaled payload to the queue for an event type. Used by
// the coordinator to seed responses before driving inputs.
func (r *ReplayState) Push(eventType string, payload any) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queues[eventType] = append(r.queues[eventType], payload)
}

// Pop removes and returns the next queued payload for an event type. Returns
// (nil, false) when the queue is empty — replay mismatch the caller should
// surface as a non-deterministic divergence.
func (r *ReplayState) Pop(eventType string) (any, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.queues[eventType]
	if len(q) == 0 {
		return nil, false
	}
	head := q[0]
	r.queues[eventType] = q[1:]
	return head, true
}

// Remaining reports the number of queued payloads for an event type. Used
// by tests asserting the queue was fully drained at end of replay.
func (r *ReplayState) Remaining(eventType string) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.queues[eventType])
}

// Reset clears all queues and deactivates. Coordinator calls this before
// each replay run so prior state cannot leak across runs in the same
// engine instance.
func (r *ReplayState) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queues = make(map[string][]any)
	r.active.Store(false)
}
