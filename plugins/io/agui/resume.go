package agui

import (
	"encoding/json"
	"fmt"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/events"
)

// resumeRun implements the resume (continuation) half of the virtual-run model.
// It is called for a RunAgentInput that carries resume[] — the terminal-run
// model's way of answering the interrupt(s) that ended a prior run on the same
// thread. It:
//
//  1. Validates the resume against the pending interrupts recorded on this
//     thread: every resume item must correlate to a known interrupt, and ALL
//     open interrupts for the thread must be addressed in this one request
//     (AG-UI forbids partial resumes).
//  2. Registers a fresh run as the single active run so the continuation's bus
//     events stream to the new SSE. This happens BEFORE emitting hitl.responded
//     so the unblocked agent's very first event lands on the new run.
//  3. Emits hitl.responded for each item (choice / free-text / edited payload
//     for "resolved"; Cancelled for "cancelled") to unblock the in-process
//     agent, and deletes each resolved entry from the pending map.
//
// A single interrupted Nexus turn thus spans two AG-UI runs: the run that ended
// with the interrupt and this continuation (same threadID, new runID).
//
// On any validation failure it returns an error and registers no run, so the
// server can write a clean RunError terminal stream and the still-blocked agent
// is left untouched for a corrected resume.
func (p *Plugin) resumeRun(input runInput) (*run, error) {
	// Correlate every resume item to a pending interrupt on this thread and
	// ensure all open interrupts on the thread are addressed in one request.
	items, err := p.matchResume(input.threadID, input.resume)
	if err != nil {
		return nil, err
	}

	// Register the continuation run BEFORE unblocking the agent: the agent's
	// first bus event after hitl.responded must land on this run, not be dropped
	// for want of an active slot.
	p.mu.Lock()
	if p.active != nil {
		p.mu.Unlock()
		return nil, fmt.Errorf("a run is already in flight on this listener")
	}
	r := newRun(input.threadID, input.runID, input.tools)
	p.active = r
	p.mu.Unlock()

	r.markStarted()
	r.queue(newRunStarted(input.threadID, input.runID))

	// Resolve each interrupt, then drop the pending mapping. A HITL interrupt
	// emits hitl.responded to unblock the waiter; a client-tool interrupt emits
	// the tool.result the parked agent is waiting on. Emission is asynchronous so
	// this handler returns promptly and the HTTP goroutine can start draining the
	// run channel.
	go func() {
		for _, m := range items {
			switch m.pending.Kind {
			case interruptClientTool:
				result := buildToolResult(m.pending, m.item)
				if err := p.bus.Emit("tool.result", result); err != nil {
					p.logger.Warn("emit tool.result on client-tool resume failed",
						"error", err,
						"tool", m.pending.ToolName,
						"tool_call_id", m.pending.ToolCallID,
						"interrupt_id", m.pending.InterruptID,
					)
				}
			default:
				resp := buildHITLResponse(m.pending, m.item)
				if err := p.bus.Emit("hitl.responded", resp); err != nil {
					p.logger.Warn("emit hitl.responded on resume failed",
						"error", err,
						"request_id", m.pending.RequestID,
						"interrupt_id", m.pending.InterruptID,
					)
				}
			}
			p.pendingMu.Lock()
			delete(p.pending, m.pending.InterruptID)
			p.pendingMu.Unlock()

			p.logger.Info("agui interrupt resumed",
				"interrupt_id", m.pending.InterruptID,
				"request_id", m.pending.RequestID,
				"tool_call_id", m.pending.ToolCallID,
				"thread_id", input.threadID,
				"run_id", input.runID,
				"status", string(m.item.Status),
			)
		}
	}()

	return r, nil
}

// toolResultPayload is the JSON shape a client sends in a ResumeItem.Payload for
// a resolved client-executed tool call. It mirrors the fields of ToolResult the
// agent consumes; all are optional. An empty payload resolves the call with an
// empty output (the agent still advances past the call).
type toolResultPayload struct {
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// buildToolResult maps a client's resume item onto the ToolResult the parked
// agent awaits. A "cancelled" status yields an error result so the agent's tool
// loop advances rather than hanging; a "resolved" status decodes the client's
// tool output. The TurnID is carried through from the pending interrupt so the
// ReAct agent matches the result to the turn that issued the call.
func buildToolResult(pi pendingInterrupt, item agui.ResumeItem) events.ToolResult {
	result := events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            pi.ToolCallID,
		Name:          pi.ToolName,
		TurnID:        pi.TurnID,
	}
	if item.Status == agui.ResumeCancelled {
		result.Error = "client tool call cancelled"
		return result
	}
	var payload toolResultPayload
	if len(item.Payload) > 0 {
		// A malformed payload is not fatal: fall through with an empty output
		// rather than rejecting a resolve the client considers complete.
		_ = json.Unmarshal(item.Payload, &payload)
	}
	result.Output = payload.Output
	result.Error = payload.Error
	return result
}

// matchedResume pairs a resume item with the pending interrupt it resolves.
type matchedResume struct {
	item    agui.ResumeItem
	pending pendingInterrupt
}

// matchResume validates resume[] against the pending interrupts recorded for
// threadID and returns the matched pairs. It enforces the two AG-UI rules:
// every item must correlate to a known (unexpired) interrupt on this thread, and
// every open interrupt on the thread must be addressed (no partial resume). The
// pending map is read under pendingMu; nothing is mutated here so a validation
// failure leaves the agent blocked for a corrected retry.
func (p *Plugin) matchResume(threadID string, resume []agui.ResumeItem) ([]matchedResume, error) {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()

	// Collect the open interrupts for this thread so we can assert full coverage.
	open := make(map[string]struct{})
	for id, pi := range p.pending {
		if pi.ThreadID == threadID {
			open[id] = struct{}{}
		}
	}

	matched := make([]matchedResume, 0, len(resume))
	seen := make(map[string]struct{}, len(resume))
	for _, item := range resume {
		if item.InterruptID == "" {
			return nil, fmt.Errorf("resume item missing interruptId")
		}
		if _, dup := seen[item.InterruptID]; dup {
			return nil, fmt.Errorf("resume addresses interrupt %q more than once", item.InterruptID)
		}
		seen[item.InterruptID] = struct{}{}

		pi, ok := p.pending[item.InterruptID]
		if !ok {
			return nil, fmt.Errorf("unknown or expired interrupt %q", item.InterruptID)
		}
		if pi.ThreadID != threadID {
			return nil, fmt.Errorf("interrupt %q does not belong to thread %q", item.InterruptID, threadID)
		}
		switch item.Status {
		case agui.ResumeResolved, agui.ResumeCancelled:
		default:
			return nil, fmt.Errorf("interrupt %q has invalid resume status %q", item.InterruptID, item.Status)
		}
		matched = append(matched, matchedResume{item: item, pending: pi})
	}

	// All open interrupts on this thread must be addressed in one request.
	if len(matched) != len(open) {
		return nil, fmt.Errorf("partial resume: %d of %d open interrupts addressed for thread %q", len(matched), len(open), threadID)
	}
	return matched, nil
}

// resumePayload is the JSON shape a client sends in a ResumeItem.Payload for a
// resolved interrupt. Fields mirror the HITLResponse answer shape so a client
// can supply a choice id, free text, and/or an edited payload per the
// interrupt's Mode. All fields are optional; an empty payload resolves the
// interrupt with no answer (equivalent to accepting the default).
type resumePayload struct {
	ChoiceID      string         `json:"choiceId,omitempty"`
	FreeText      string         `json:"freeText,omitempty"`
	EditedPayload map[string]any `json:"editedPayload,omitempty"`
}

// buildHITLResponse maps a resume item onto the HITLResponse the in-process
// waiter expects. A "cancelled" status yields Cancelled:true so the waiter
// unblocks with an abandonment; a "resolved" status decodes the payload into
// choice / free-text / edited-payload. The interrupt's Mode is advisory here —
// the waiter's requesting plugin owns final validation — but a choices-only
// interrupt drops any stray free text to keep the response shape honest.
func buildHITLResponse(pi pendingInterrupt, item agui.ResumeItem) events.HITLResponse {
	resp := events.HITLResponse{
		SchemaVersion: events.HITLResponseVersion,
		RequestID:     pi.RequestID,
	}
	if item.Status == agui.ResumeCancelled {
		resp.Cancelled = true
		resp.CancelReason = "resumed as cancelled"
		return resp
	}

	var payload resumePayload
	if len(item.Payload) > 0 {
		// A malformed payload is not fatal: fall through with an empty answer
		// rather than rejecting a resolve the client considers complete.
		_ = json.Unmarshal(item.Payload, &payload)
	}
	resp.ChoiceID = payload.ChoiceID
	resp.FreeText = payload.FreeText
	resp.EditedPayload = payload.EditedPayload

	// Honor the interrupt's response shape: a choices-only interrupt has no free
	// text field, so drop any the client sent to avoid an ambiguous response.
	if pi.Mode == events.HITLModeChoices {
		resp.FreeText = ""
	}
	return resp
}
