package agui

import (
	"encoding/json"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// handleCatalogQuery appends the active run's client-executed (frontend) tools
// to the tool-catalog snapshot the agent assembles for its LLM request. It runs
// at a priority after nexus.tool.catalog has filled the base list, and it only
// appends when a run with client tools is active — so the tools are scoped to
// exactly the run that advertised them and never leak into later runs or other
// listeners (unlike a global tool.register, which the catalog has no way to
// retract). A client tool that shadows a server tool by name is dropped: the
// server-side catalog entry wins so an in-process tool is never masked by a
// same-named frontend tool.
func (p *Plugin) handleCatalogQuery(e engine.Event[any]) {
	q, ok := e.Payload.(*events.ToolCatalogQuery)
	if !ok {
		return
	}
	r := p.currentRun()
	if r == nil || len(r.clientTools) == 0 {
		return
	}

	existing := make(map[string]struct{}, len(q.Tools))
	for _, td := range q.Tools {
		existing[td.Name] = struct{}{}
	}
	for name, td := range r.clientTools {
		if _, dup := existing[name]; dup {
			// A server-side tool already owns this name; do not mask it.
			continue
		}
		q.Tools = append(q.Tools, td)
	}
}

// isClientTool reports whether name identifies a client-executed tool advertised
// for run r. The map is populated once at run construction and read-only after,
// so no lock is needed.
func (p *Plugin) isClientTool(r *run, name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.clientTools[name]
	return ok
}

// suspendForClientTool ends the current run interrupt-style because the agent
// called a client-executed tool: there is no in-process handler to produce the
// tool.result, so the run must yield to the client, which runs the tool and
// resumes with a ToolCallResult. It reuses the E2-S1 suspend machinery: emit a
// StateSnapshot + MessagesSnapshot then a RunFinished(interrupt) whose payload
// names the tool call awaiting a result, and record a pending client-tool
// interrupt so the resume path can feed the result back to the parked agent.
func (p *Plugin) suspendForClientTool(r *run, tc events.ToolCall) {
	interruptID := newInterruptID()

	// Record the correlation BEFORE ending the stream so a fast resume POST can
	// never race ahead of the mapping being visible.
	p.pendingMu.Lock()
	p.pending[interruptID] = pendingInterrupt{
		Kind:        interruptClientTool,
		InterruptID: interruptID,
		SessionID:   r.threadID,
		TurnID:      tc.TurnID,
		ThreadID:    r.threadID,
		RunID:       r.runID,
		ToolCallID:  tc.ID,
		ToolName:    tc.Name,
	}
	p.pendingMu.Unlock()

	snapshot := p.buildClientToolState(r, interruptID, tc)
	payload := buildClientToolInterrupt(interruptID, tc)
	r.interrupt(snapshot, r.snapshotMessages(), payload)
	p.endRun(r)

	p.logger.Info("agui run interrupted for client tool",
		"interrupt_id", interruptID,
		"tool", tc.Name,
		"tool_call_id", tc.ID,
		"thread_id", r.threadID,
		"run_id", r.runID,
	)
}

// buildClientToolState assembles the JSON state handoff for a client-tool
// interrupt. It mirrors buildStateSnapshot but under a "toolCall" anchor so a
// client that restores from state alone still knows which call it must execute.
func (p *Plugin) buildClientToolState(r *run, interruptID string, tc events.ToolCall) []byte {
	state := map[string]any{
		"threadId": r.threadID,
		"runId":    r.runID,
		"toolCall": map[string]any{
			"interruptId": interruptID,
			"toolCallId":  tc.ID,
			"toolName":    tc.Name,
			"arguments":   tc.Arguments,
		},
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

// buildClientToolInterrupt maps a client-tool call onto the AG-UI Interrupt
// payload. Mode is free_text (the client returns an opaque tool result rather
// than choosing among options); the tool call identity travels in the state
// snapshot and via the ToolCall* events already streamed, so the interrupt
// payload only needs the interruptId as the resume anchor plus a rendered hint.
func buildClientToolInterrupt(interruptID string, tc events.ToolCall) agui.Interrupt {
	return agui.Interrupt{
		InterruptID: interruptID,
		Prompt:      "Execute client tool: " + tc.Name,
		Mode:        agui.InterruptModeFreeText,
	}
}
