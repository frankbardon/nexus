package aguiclient

import (
	"encoding/json"

	"github.com/frankbardon/nexus/pkg/agui"
)

// Interrupt returns the AG-UI Interrupt payload carried by the run's terminal
// RunFinished event when the run ended with an interrupt outcome, plus true. It
// returns a zero Interrupt and false when the run did not end on an interrupt
// (a normal or cancelled finish, an error, or an undecodable result). It is the
// client's anchor for building the continuation resume[] request.
func (r Result) Interrupt() (agui.Interrupt, bool) {
	fin := r.First(agui.EventRunFinished)
	if fin == nil {
		return agui.Interrupt{}, false
	}
	fe, ok := fin.(*agui.RunFinishedEvent)
	if !ok {
		return agui.Interrupt{}, false
	}
	if fe.Outcome != agui.OutcomeInterrupt || len(fe.Result) == 0 {
		return agui.Interrupt{}, false
	}
	var in agui.Interrupt
	if err := json.Unmarshal(fe.Result, &in); err != nil {
		return agui.Interrupt{}, false
	}
	return in, true
}

// Outcome returns the outcome discriminator of the run's terminal RunFinished
// event ("", "interrupt", or "cancelled"). It returns "" when the run did not
// terminate with a RunFinished (e.g. a RunError stream).
func (r Result) Outcome() string {
	fin := r.First(agui.EventRunFinished)
	if fin == nil {
		return ""
	}
	fe, ok := fin.(*agui.RunFinishedEvent)
	if !ok {
		return ""
	}
	return fe.Outcome
}

// ResumeInput builds a continuation RunAgentInput that resolves the open
// interrupts on a thread. It carries no messages — the resume[] items are the
// payload the server correlates back to its pending interrupts. threadID must
// match the interrupted run's thread; runID is a fresh id for the continuation
// run (the interrupted turn spans two AG-UI runs).
func ResumeInput(threadID, runID string, items ...agui.ResumeItem) agui.RunAgentInput {
	return agui.RunAgentInput{
		ThreadID: threadID,
		RunID:    runID,
		Resume:   items,
	}
}

// ResolveChoice builds a ResumeItem that resolves a choices-mode interrupt by
// selecting a choice id (with optional free text for a "both"-mode interrupt).
func ResolveChoice(interruptID, choiceID, freeText string) agui.ResumeItem {
	payload := map[string]any{}
	if choiceID != "" {
		payload["choiceId"] = choiceID
	}
	if freeText != "" {
		payload["freeText"] = freeText
	}
	return resumeItem(interruptID, agui.ResumeResolved, payload)
}

// ResolveText builds a ResumeItem that resolves a free-text interrupt with the
// user's typed answer.
func ResolveText(interruptID, freeText string) agui.ResumeItem {
	return resumeItem(interruptID, agui.ResumeResolved, map[string]any{"freeText": freeText})
}

// ResolveToolResult builds a ResumeItem that resolves a client-executed
// (frontend) tool interrupt by returning the tool's output. An empty error
// string leaves the tool call successful.
func ResolveToolResult(interruptID, output, errMsg string) agui.ResumeItem {
	payload := map[string]any{}
	if output != "" {
		payload["output"] = output
	}
	if errMsg != "" {
		payload["error"] = errMsg
	}
	return resumeItem(interruptID, agui.ResumeResolved, payload)
}

// Cancel builds a ResumeItem that abandons an interrupt (HITL prompt or client
// tool call). The server unblocks the parked agent with a cancellation rather
// than an answer.
func Cancel(interruptID string) agui.ResumeItem {
	return agui.ResumeItem{InterruptID: interruptID, Status: agui.ResumeCancelled}
}

// resumeItem encodes a resolved resume item, dropping an empty payload so the
// wire carries no "payload" field when there is nothing to send.
func resumeItem(interruptID string, status agui.ResumeStatus, payload map[string]any) agui.ResumeItem {
	item := agui.ResumeItem{InterruptID: interruptID, Status: status}
	if len(payload) > 0 {
		if raw, err := json.Marshal(payload); err == nil {
			item.Payload = raw
		}
	}
	return item
}
