package a2a

import (
	"encoding/json"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file carries the Nexus A2A extension's producing side: the telemetry a
// Nexus turn generates that A2A's canonical data model has no field for.
//
// # What rides here rather than in an artifact
//
// Thinking steps, tool CALLS (as distinct from their results), subagent progress
// and token accounting. None of them is task output — they describe how the
// agent is getting to its output — so putting them in artifacts would overstate
// them, and putting them in messages would put them in the conversation. A2A's
// sanctioned home for exactly this is an extension (section 8.4), and
// pkg/a2a/extension.go defines the typed payload; this file is its first
// consumer.
//
// The carrier is TaskStatusUpdateEvent.metadata, keyed by the extension URI. The
// status those updates carry is the task's CURRENT state, not a hardcoded
// WORKING: a telemetry frame emitted while the task is parked at INPUT_REQUIRED
// must not tell a client the task went back to work. Re-entering an active state
// is legal precisely so an agent can re-emit a state with new information
// attached (specification section 3.1.1), which is what these frames are.
//
// # Opt-in is per stream, and is honoured by not sending
//
// Section 8.4 makes an extension opt-in through the A2A-Extensions service
// parameter, so a client that did not ask for it must not be force-fed it. That
// is enforced at the fan-out: a telemetry frame is copied only into the channels
// of observers that opted in. A client that did not ask sees a stream containing
// nothing but canonical frames — not a filtered one, an unpolluted one.
//
// # Telemetry is NOT persisted
//
// It is the one frame class that does not go through the task store's
// write-through path, and the exception is deliberate rather than an oversight.
// A stored telemetry frame would land in task_status_history as a WORKING
// transition, so GetTask would replay a turn's reasoning as if it were a
// sequence of state changes the task actually made — and a long turn would fill
// the history table with hundreds of them. Telemetry describes a moment; the
// store records what the task IS.

// nexusEventFrame wraps a Nexus extension event as a status-update frame
// carrying the current task state.
//
// It errors when the event does not validate, which is a programming error in a
// translator rather than anything a client did — so the caller logs and drops it
// rather than surfacing a protocol error on a stream that is otherwise fine.
func nexusEventFrame(taskID, contextID string, state a2a.TaskState, event a2a.NexusEvent) (a2a.StreamResponse, error) {
	metadata, err := a2a.NexusEventMetadata(event)
	if err != nil {
		return a2a.StreamResponse{}, err
	}
	update := a2a.NewStatusUpdate(taskID, contextID, a2a.NewTaskStatus(state))
	update.Metadata = metadata
	return a2a.StreamStatusUpdate(update), nil
}

// --- translators: one Nexus bus payload -> one typed extension event ---

// thinkingEvent renders a reasoning step.
func thinkingEvent(taskID, contextID string, step events.ThinkingStep) a2a.NexusEvent {
	event := a2a.ThinkingEvent(taskID, contextID, a2a.NexusThinking{
		Step:    step.Index,
		Content: step.Content,
	}).From("thinking.step")
	if !step.Timestamp.IsZero() {
		event = event.At(step.Timestamp)
	}
	return event
}

// toolCallEvent renders a tool invocation. The arguments travel as the JSON
// object the model produced, so a client sees what the agent asked for rather
// than a rendering of it.
func toolCallEvent(taskID, contextID string, call events.ToolCall) a2a.NexusEvent {
	payload := a2a.NexusToolCall{CallID: call.ID, Name: call.Name}
	if len(call.Arguments) > 0 {
		if raw, err := json.Marshal(call.Arguments); err == nil {
			payload.Arguments = raw
		}
	}
	return a2a.ToolCallEvent(taskID, contextID, payload).From("tool.invoke")
}

// toolResultEvent renders a tool outcome. It duplicates what the tool-result
// ARTIFACT carries, on purpose: the artifact is the durable output a client
// reads back later, and this is the live signal that something just happened —
// a client following a turn should not have to diff artifact lists to notice.
// The output is capped here as it is there, since telemetry is a notification
// rather than a delivery mechanism.
func toolResultEvent(taskID, contextID string, res events.ToolResult, limit int) a2a.NexusEvent {
	output, _ := truncate(res.Output, limit)
	return a2a.ToolResultEvent(taskID, contextID, a2a.NexusToolResult{
		CallID: res.ID,
		Name:   res.Name,
		Output: output,
		Error:  res.Error,
	}).From("tool.result")
}

// subagentStartedEvent renders a delegated run beginning.
func subagentStartedEvent(taskID, contextID string, s events.SubagentStarted) a2a.NexusEvent {
	return a2a.SubagentEvent(taskID, contextID, a2a.NexusSubagentProgress{
		SubagentID: s.SpawnID,
		Phase:      a2a.NexusSubagentStarted,
		Detail:     s.Task,
	}).From("subagent.started")
}

// subagentIterationEvent renders one loop of a delegated run.
func subagentIterationEvent(taskID, contextID string, s events.SubagentIteration) a2a.NexusEvent {
	return a2a.SubagentEvent(taskID, contextID, a2a.NexusSubagentProgress{
		SubagentID: s.SpawnID,
		Phase:      a2a.NexusSubagentIteration,
		Iteration:  s.Iteration,
		Detail:     s.Content,
	}).From("subagent.iteration")
}

// subagentCompleteEvent renders a delegated run ending. A subagent that reported
// an error is FAILED rather than complete: "finished" and "worked" are different
// facts, and a client that renders progress needs to be able to tell them apart.
func subagentCompleteEvent(taskID, contextID string, s events.SubagentComplete) a2a.NexusEvent {
	progress := a2a.NexusSubagentProgress{
		SubagentID: s.SpawnID,
		Phase:      a2a.NexusSubagentComplete,
		Iteration:  s.Iterations,
		Detail:     s.Result,
	}
	if s.Error != "" {
		progress.Phase = a2a.NexusSubagentFailed
		progress.Error = s.Error
	}
	return a2a.SubagentEvent(taskID, contextID, progress).From("subagent.complete")
}

// usageEvent renders one model call's token accounting. Cumulative is false: this
// is the cost of the call that just returned, and a client that wants a session
// total sums what it received rather than being handed two numbers that can
// disagree.
func usageEvent(taskID, contextID string, resp events.LLMResponse) a2a.NexusEvent {
	return a2a.UsageEvent(taskID, contextID, a2a.NexusTokenUsage{
		Model:             resp.Model,
		InputTokens:       resp.Usage.PromptTokens,
		OutputTokens:      resp.Usage.CompletionTokens,
		CachedInputTokens: resp.Usage.CachedTokens,
		ReasoningTokens:   resp.Usage.ReasoningTokens,
		TotalTokens:       resp.Usage.TotalTokens,
	}).From("llm.response")
}

// hasUsage reports whether a response carries token accounting worth reporting.
// A provider that reported nothing produces no usage event, rather than a frame
// full of zeros a client would have to special-case.
func hasUsage(u events.Usage) bool {
	return u.PromptTokens > 0 || u.CompletionTokens > 0 || u.TotalTokens > 0
}
