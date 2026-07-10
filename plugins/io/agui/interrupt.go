package agui

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// handleHITLRequested implements the interrupt (suspend) half of the virtual-run
// model. When a HITL request fires during an active AG-UI run, the plugin ends
// the run's SSE stream per AG-UI's terminal-run model: it emits a StateSnapshot
// and a MessagesSnapshot, then a RunFinished carrying an interrupt outcome, and
// closes the stream.
//
// Crucially, this does NOT emit hitl.responded and does NOT unblock the
// in-process agent: the Nexus session stays alive and the agent stays parked on
// the pending hitl. A continuation run resolves it (E2-S2). The interruptId ↔
// HITLRequest mapping is recorded in p.pending so the resume side can correlate.
//
// If no run is in flight the request is ignored here — it will be surfaced by a
// browser/TUI transport or resolved out-of-band; there is no SSE stream to end.
func (p *Plugin) handleHITLRequested(e engine.Event[any]) {
	req, ok := e.Payload.(events.HITLRequest)
	if !ok {
		return
	}
	r := p.currentRun()
	if r == nil {
		return
	}

	interruptID := newInterruptID()

	// Record the correlation BEFORE ending the stream so a fast resume POST can
	// never race ahead of the mapping being visible.
	p.pendingMu.Lock()
	p.pending[interruptID] = pendingInterrupt{
		InterruptID: interruptID,
		RequestID:   req.ID,
		SessionID:   req.SessionID,
		TurnID:      req.TurnID,
		ThreadID:    r.threadID,
		RunID:       r.runID,
		Mode:        req.Mode,
	}
	p.pendingMu.Unlock()

	snapshot := p.buildStateSnapshot(r, interruptID, req)
	payload := buildInterruptPayload(interruptID, req)
	r.interrupt(snapshot, r.snapshotMessages(), payload)
	// Clear the active-run pointer: the stream is done, but the agent remains
	// blocked in-process on the pending hitl.
	p.endRun(r)

	p.logger.Info("agui run interrupted for hitl",
		"interrupt_id", interruptID,
		"request_id", req.ID,
		"thread_id", r.threadID,
		"run_id", r.runID,
		"mode", string(req.Mode),
	)
}

// handleHITLCancel maps a retracted HITL request to a terminal cancelled
// outcome on the active run (if any) and drops any recorded pending interrupt
// for that request. It does not emit hitl.responded — the control/hitl plugin
// owns synthesizing the cancellation response for the blocked in-process agent.
func (p *Plugin) handleHITLCancel(e engine.Event[any]) {
	c, ok := e.Payload.(events.HITLCancel)
	if !ok {
		return
	}

	// Drop any pending interrupt mapping recorded for this request.
	p.pendingMu.Lock()
	for id, pi := range p.pending {
		if pi.RequestID == c.RequestID {
			delete(p.pending, id)
		}
	}
	p.pendingMu.Unlock()

	// If a run is still in flight (cancel arrived before the request), end it
	// cleanly with a cancelled outcome so the client's SSE terminates.
	if r := p.currentRun(); r != nil {
		r.cancelTerminal()
		p.endRun(r)
		p.logger.Info("agui run cancelled for hitl", "request_id", c.RequestID, "reason", c.Reason)
	}
}

// buildStateSnapshot assembles the JSON state handed to the client on interrupt.
// It is intentionally minimal: the thread/run identity plus the pending
// interrupt echoed under "interrupt" so a client that restores from state alone
// (rather than replaying MessagesSnapshot) still has the resume anchor.
func (p *Plugin) buildStateSnapshot(r *run, interruptID string, req events.HITLRequest) []byte {
	state := map[string]any{
		"threadId": r.threadID,
		"runId":    r.runID,
		"interrupt": map[string]any{
			"interruptId": interruptID,
			"requestId":   req.ID,
			"sessionId":   req.SessionID,
			"turnId":      req.TurnID,
			"mode":        string(req.Mode),
		},
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

// buildInterruptPayload maps a HITLRequest onto the AG-UI Interrupt payload the
// client renders. Only the fields a client needs to render a prompt cross the
// boundary (prompt, mode, choices, defaultChoiceId); the interruptId ties the
// eventual resume back to the pending request.
func buildInterruptPayload(interruptID string, req events.HITLRequest) agui.Interrupt {
	in := agui.Interrupt{
		InterruptID:     interruptID,
		Prompt:          req.Prompt,
		Mode:            mapMode(req.Mode),
		DefaultChoiceID: req.DefaultChoiceID,
	}
	for _, c := range req.Choices {
		in.Choices = append(in.Choices, agui.InterruptChoice{
			ID:    c.ID,
			Label: c.Label,
			Kind:  string(c.Kind),
		})
	}
	return in
}

// mapMode translates a Nexus HITL mode onto the AG-UI interrupt mode. A zero
// mode defaults to free_text, matching the HITL semantics.
func mapMode(m events.HITLMode) agui.InterruptMode {
	switch m {
	case events.HITLModeChoices:
		return agui.InterruptModeChoices
	case events.HITLModeBoth:
		return agui.InterruptModeBoth
	default:
		return agui.InterruptModeFreeText
	}
}

// newInterruptID mints a random, collision-resistant interrupt id. It is
// distinct from the HITLRequest.ID so the AG-UI-facing correlator never leaks
// internal request identifiers, and it stays stable for the resume round-trip.
func newInterruptID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should not fail; fall back to a fixed prefix so the run
		// still terminates cleanly rather than panicking on the hot path.
		return "int-fallback"
	}
	return "int-" + hex.EncodeToString(b[:])
}
