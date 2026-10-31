package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/frankbardon/nexus/pkg/events"
)

// ErrToolCallTimedOut is the cancellation cause a tool handler sees, via
// context.Cause on its per-call context, when its call is timed out (a
// tool.timeout event naming its call ID). Match it with errors.Is.
var ErrToolCallTimedOut = errors.New("tool call timed out")

// ErrToolCallsStopped is the cancellation cause for every call still in
// flight when the engine stops.
var ErrToolCallsStopped = errors.New("engine stopped")

// ToolCallContexts is the engine-owned registry of per-call contexts for tool
// dispatch. A tool.invoke handler calls Begin with the call's ID and runs its
// work under the returned context; anything that decides the call is over —
// the engine on a tool.timeout event, the engine on Stop, or any other holder
// of the registry — calls Cancel with that ID, and the handler's context is
// cancelled with a cause it can read back with context.Cause.
//
// Cancellation is cooperative: the registry kills nothing itself. A handler
// that never calls Begin, or ignores the context it gets, keeps running
// exactly as before; its eventual result is the caller's to drop (the
// tool_timeout gate already vetoes a late result for a call it timed out).
//
// The registry is keyed by call ID, which is how every tool lifecycle event
// (tool.invoke, tool.result, tool.timeout) is correlated. A Cancel for an ID
// with no live Begin is a no-op: it does not pre-cancel a later Begin.
//
// The zero value is not usable; build one with NewToolCallContexts. A nil
// *ToolCallContexts is valid and behaves as a registry nothing ever cancels,
// so plugins can use PluginContext.ToolCalls unconditionally even when a
// hand-built test context leaves it unset.
type ToolCallContexts struct {
	mu    sync.Mutex
	calls map[string][]*toolCallEntry
}

type toolCallEntry struct {
	cancel context.CancelCauseFunc
}

// NewToolCallContexts returns an empty registry. The engine builds one per
// Engine and hands it to every plugin as PluginContext.ToolCalls; tests that
// drive a plugin against a bare bus build their own and Attach it.
func NewToolCallContexts() *ToolCallContexts {
	return &ToolCallContexts{calls: map[string][]*toolCallEntry{}}
}

// Begin derives a cancellable context for the call with the given ID from
// parent and registers it. The returned release func unregisters it and
// releases the context's resources; call it (typically deferred) when the
// handler is finished with the call. release is idempotent.
//
// On a nil registry or an empty callID, Begin returns a plain cancellable
// child of parent that nothing else can cancel.
func (r *ToolCallContexts) Begin(parent context.Context, callID string) (context.Context, func()) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancelCause(parent)
	if r == nil || callID == "" {
		return ctx, func() { cancel(nil) }
	}
	e := &toolCallEntry{cancel: cancel}
	r.mu.Lock()
	r.calls[callID] = append(r.calls[callID], e)
	r.mu.Unlock()

	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			r.remove(callID, e)
			cancel(nil)
		})
	}
}

// Cancel cancels every live context registered for callID with cause (nil
// means context.Canceled) and reports whether there was one. Entries stay
// registered until their handler releases them, so a repeated Cancel is a
// harmless no-op on an already-cancelled context.
func (r *ToolCallContexts) Cancel(callID string, cause error) bool {
	if r == nil || callID == "" {
		return false
	}
	r.mu.Lock()
	entries := append([]*toolCallEntry(nil), r.calls[callID]...)
	r.mu.Unlock()
	for _, e := range entries {
		e.cancel(cause)
	}
	return len(entries) > 0
}

// CancelAll cancels every live call with cause. The engine calls it on Stop.
func (r *ToolCallContexts) CancelAll(cause error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	var entries []*toolCallEntry
	for _, es := range r.calls {
		entries = append(entries, es...)
	}
	r.mu.Unlock()
	for _, e := range entries {
		e.cancel(cause)
	}
}

// Live reports whether a context is currently registered for callID.
func (r *ToolCallContexts) Live(callID string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls[callID]) > 0
}

// Attach subscribes the registry to bus so a tool.timeout event cancels the
// timed-out call's context with a cause wrapping ErrToolCallTimedOut. It
// returns the unsubscribe func. The engine attaches its registry at Boot;
// any plugin that emits tool.timeout therefore cancels the dispatch with no
// further wiring.
func (r *ToolCallContexts) Attach(bus EventBus) func() {
	if r == nil || bus == nil {
		return func() {}
	}
	return bus.Subscribe("tool.timeout", func(event Event[any]) {
		var tt events.ToolTimeout
		switch p := event.Payload.(type) {
		case events.ToolTimeout:
			tt = p
		case *events.ToolTimeout:
			if p == nil {
				return
			}
			tt = *p
		default:
			return
		}
		r.Cancel(tt.CallID, fmt.Errorf("%w: %s exceeded %s", ErrToolCallTimedOut, tt.ToolName, tt.Timeout))
	}, WithSource("nexus.engine.toolcalls"))
}

func (r *ToolCallContexts) remove(callID string, e *toolCallEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	es := r.calls[callID]
	for i, x := range es {
		if x == e {
			es = append(es[:i], es[i+1:]...)
			break
		}
	}
	if len(es) == 0 {
		delete(r.calls, callID)
	} else {
		r.calls[callID] = es
	}
}
