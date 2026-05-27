// Package streamtool is the channel-aware tool primitive — the contract a
// long-running tool implements when it wants to publish intermediate output
// while it runs. A standard tool is request-response: emit tool.invoke, wait
// for tool.result. A channel-aware tool returns a chan ToolEvent the runtime
// drains, auto-bridging each event to the bus as tool.stream.* so other
// consumers (UIs, observability collectors, sub-agents that need to know
// about the work in progress) see streaming output without the tool author
// wiring up the bus by hand.
//
// Tools should be channel-aware when they take more than a few seconds,
// produce intermediate output a consumer might use, and can be cancelled
// cleanly. Quick request-response operations should stay on the standard
// tool interface.
package streamtool

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// Kind classifies a single emission from a ChannelTool.
type Kind string

const (
	// KindProgress carries status with no data; consumers use it to update
	// progress bars or heartbeat counters.
	KindProgress Kind = "progress"
	// KindPartial carries incremental data the consumer can render.
	KindPartial Kind = "partial"
	// KindComplete carries the final result and closes the channel.
	KindComplete Kind = "complete"
	// KindError carries a terminal failure and closes the channel.
	KindError Kind = "error"
)

// ToolEvent is one item on a ChannelTool's output channel. Sequence is
// monotonic per Stream invocation, starts at 1, and is opaque to consumers
// other than for ordering. Progress is 0.0–1.0 when known, -1 otherwise.
type ToolEvent struct {
	Kind     Kind
	Sequence int
	Payload  any
	Progress float64
	Err      error
}

// ChannelTool is the streaming tool contract. Stream must return a channel
// the runtime will drain to completion (KindComplete or KindError), and
// must close that channel when ctx is cancelled or the work is done. The
// channel may be buffered; Bridge does not require a specific buffer size.
type ChannelTool interface {
	// Name returns the tool name as it appears in the catalog.
	Name() string
	// Stream begins an asynchronous invocation. The returned channel
	// receives ToolEvents in order; the runtime closes nothing — the
	// implementation owns the channel's lifetime.
	Stream(ctx context.Context, input map[string]any) (<-chan ToolEvent, error)
}

// Bridge consumes a ChannelTool's stream and projects each event onto the
// bus. While the stream runs, every ToolEvent emits as a tool.stream.*
// event whose Causation.ParentID is the originating tool.invoke event ID
// (the bus's per-goroutine dispatch context handles that automatically).
// When the stream completes, the function emits a single tool.result with
// the final payload — the parent agent sees the same contract as for a
// standard tool, plus a stream of side-channel updates other consumers
// can watch.
//
// Bridge blocks until the channel closes or ctx is cancelled. It does not
// return the tool's payload; the payload is on the bus. Returns nil on
// graceful completion, an error from the tool only if KindError fires.
func Bridge(ctx context.Context, bus engine.EventBus, tool ChannelTool, call events.ToolCall) error {
	ch, err := tool.Stream(ctx, call.Arguments)
	if err != nil {
		emitError(bus, call, err)
		return err
	}

	var (
		lastPartial any
		streamErr   error
	)
	for {
		select {
		case <-ctx.Done():
			emitError(bus, call, ctx.Err())
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				// Channel closed without a terminal event. Treat as
				// complete with the last partial payload — common for
				// well-behaved tools that emit Partial then close.
				emitFinalResult(bus, call, lastPartial, nil)
				return nil
			}
			switch ev.Kind {
			case KindProgress:
				_ = bus.Emit("tool.stream.progress", map[string]any{
					"tool_name": tool.Name(),
					"tool_id":   call.ID,
					"turn_id":   call.TurnID,
					"sequence":  ev.Sequence,
					"progress":  ev.Progress,
					"payload":   ev.Payload,
				})
			case KindPartial:
				lastPartial = ev.Payload
				_ = bus.Emit("tool.stream.partial", map[string]any{
					"tool_name": tool.Name(),
					"tool_id":   call.ID,
					"turn_id":   call.TurnID,
					"sequence":  ev.Sequence,
					"payload":   ev.Payload,
				})
			case KindComplete:
				emitFinalResult(bus, call, ev.Payload, nil)
				return nil
			case KindError:
				streamErr = ev.Err
				if streamErr == nil {
					streamErr = errors.New("stream error")
				}
				emitError(bus, call, streamErr)
				return streamErr
			}
		}
	}
}

func emitFinalResult(bus engine.EventBus, call events.ToolCall, payload any, _ error) {
	output := ""
	if payload != nil {
		if s, ok := payload.(string); ok {
			output = s
		} else if data, err := json.Marshal(payload); err == nil {
			output = string(data)
		}
	}
	res := events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            call.ID,
		Name:          call.Name,
		Output:        output,
		TurnID:        call.TurnID,
	}
	if veto, vErr := bus.EmitVetoable("before:tool.result", &res); vErr == nil && veto.Vetoed {
		return
	}
	_ = bus.Emit("tool.result", res)
}

func emitError(bus engine.EventBus, call events.ToolCall, err error) {
	res := events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            call.ID,
		Name:          call.Name,
		Error:         err.Error(),
		TurnID:        call.TurnID,
	}
	if veto, vErr := bus.EmitVetoable("before:tool.result", &res); vErr == nil && veto.Vetoed {
		return
	}
	_ = bus.Emit("tool.result", res)
}
