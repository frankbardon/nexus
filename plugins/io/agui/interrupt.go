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
//
// It is TURN-SCOPED, by the content rule rather than the terminating one. A
// question belonging to a turn this run does not carry must not be rendered
// onto it, and must certainly not PARK it: the successor would be handed an
// interrupt for a question its client never asked, its own turn would be left
// running with no stream, and the p.pending mapping would be recorded against
// the wrong thread/run so a resume POST would answer the departed turn's
// question. That is reachable on exactly the window handleTurnEnd guards — a
// disconnect frees the slot and only then asks the agent to stop, so the
// abandoned turn is still alive and still hitting gates when the next POST
// takes the slot.
//
// runAcceptsTurnEvent rather than runOwnsTurnEnd, because a park is not a
// termination and the vocabularies differ. HITLRequest.TurnID is a genuine
// agent-loop turn where it is set at all — nexus.control.hitl copies the
// asking tool call's own TurnID verbatim, and nexus.agent.a2aremote and the
// ICM workflow both set the spawning loop's turn — but two of the five
// emitters set NO TurnID at all (nexus.gate.approval_policy carries the turn
// in its ActionRef metadata instead, and plugins/memory's approval helper
// names none), so a strict equality match would silently drop a question the
// agent is blocked on and park it forever with nobody able to answer.
func (p *Plugin) handleHITLRequested(e engine.Event[any]) {
	req, ok := e.Payload.(events.HITLRequest)
	if !ok {
		return
	}
	r := p.currentRun()
	if r == nil {
		return
	}
	if !p.runAcceptsTurnEvent(r, req.TurnID) {
		p.logger.Debug("agui dropping a question from a turn this run does not carry",
			"turn_id", req.TurnID,
			"run_turn_id", r.boundTurn(),
			"request_id", req.ID,
			"thread_id", r.threadID,
			"run_id", r.runID,
		)
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
//
// ⚠ events.HITLCancel carries NO TurnID — only a RequestID and a Reason — so
// the discriminator its sibling handler reads off the payload does not exist
// here and had to be RECOVERED. p.pending is the recovery: every request this
// plugin actually rendered was recorded there with the turn it suspended, so a
// cancel naming one of those can be scoped exactly as a turn-carrying event
// is. That matters because cancelTerminal is the most destructive verb on this
// path — it closes the SSE of whatever holds the slot — and a retracted
// question from a departed turn was terminating the successor's stream with a
// cancelled outcome it had no part in.
//
// The two halves are deliberately independent:
//
//   - The mapping is dropped UNCONDITIONALLY. The request is retracted
//     whoever holds the slot, so leaving the correlation behind would let a
//     resume POST answer a question that no longer exists.
//   - The TERMINATION is gated on runAcceptsTurnEvent, against the turn
//     recovered from the dropped mapping. Its own turn still wins, which is
//     what keeps a continuation run that adopted the parked turn terminable by
//     the retraction of its own question.
//
// A cancel this plugin cannot correlate at all — no pending mapping (the
// retraction arrived before the request ever reached a run, which is the case
// the original comment names) or a mapping whose request carried no turn —
// terminates the current run exactly as it always did. That is the same
// asymmetry runOwnsTurnEnd sits on: leaving a client on a stream that never
// ends is worse than ending one early.
func (p *Plugin) handleHITLCancel(e engine.Event[any]) {
	c, ok := e.Payload.(events.HITLCancel)
	if !ok {
		return
	}

	// Drop any pending interrupt mapping recorded for this request, and recover
	// the turn it suspended on the way past. RequestID is unique, so at most one
	// entry matches in practice; the first turn found is taken, and an entry
	// recorded for a request that named no turn recovers nothing.
	var cancelledTurn string
	p.pendingMu.Lock()
	for id, pi := range p.pending {
		if pi.RequestID == c.RequestID {
			if cancelledTurn == "" {
				cancelledTurn = pi.TurnID
			}
			delete(p.pending, id)
		}
	}
	p.pendingMu.Unlock()

	// If a run is still in flight (cancel arrived before the request), end it
	// cleanly with a cancelled outcome so the client's SSE terminates.
	r := p.currentRun()
	if r == nil {
		return
	}
	if !p.runAcceptsTurnEvent(r, cancelledTurn) {
		p.logger.Debug("agui ignoring a hitl cancel for a turn this run does not carry",
			"turn_id", cancelledTurn,
			"run_turn_id", r.boundTurn(),
			"request_id", c.RequestID,
			"thread_id", r.threadID,
			"run_id", r.runID,
		)
		return
	}
	r.cancelTerminal()
	p.endRun(r)
	p.logger.Info("agui run cancelled for hitl", "request_id", c.RequestID, "reason", c.Reason)
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
