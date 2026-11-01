package agui

import (
	"github.com/frankbardon/nexus/pkg/events"
)

// A parked turn is a turn whose run ended interrupt-style (a client-tool
// suspend or a HITL park) while the agent is still working through the
// same LLM response. One response may carry several tool calls; the agent
// dispatches them back to back, so the first client tool (or approval
// question) parks the run and every later one arrives with no run in the
// slot. Dropped, a later client tool was answered "unknown tool" by
// nexus.tool.catalog (its definition left with the run), and a later
// question was never rendered, so the gate that asked it blocked forever.
//
// Instead they are deferred onto the parked turn, in arrival order, and
// delivered one per continuation run: when the client resumes the
// interrupt that parked the turn, the resume's answers are emitted, then
// the next deferred item suspends the continuation run exactly as if it
// had arrived on it. The AG-UI wire is unchanged: every run still ends
// with a single interrupt, and a client that handles one client tool at a
// time handles several. A resume that cancels gives every deferred item a
// cancellation instead (the person stopped the turn).
//
// A deferred item is not a pending interrupt: matchResume's "every open
// interrupt must be addressed" rule only sees what the client was shown.

// parkedTurn is the turn parked by the last interrupt on this listener.
type parkedTurn struct {
	// run is the run the turn suspended; its advertised client tools stay
	// offered (handleCatalogQuery) so a deferred call is never "unknown".
	run      *run
	threadID string
	turnID   string
	queue    []deferredItem
}

// deferredItem is one client-tool call or HITL request held for a later
// continuation run. Exactly one field is set.
type deferredItem struct {
	tool *events.ToolCall
	hitl *events.HITLRequest
}

// releaseRun frees the active slot as endRun does and, when park is set,
// records the turn as parked under the same lock, so no event can land in
// between and meet neither an active run nor a parked turn.
func (p *Plugin) releaseRun(r *run, park *parkedTurn) {
	// Read the run's turn before taking p.mu: r.boundTurn takes r's own lock,
	// and this package never nests the two.
	var retired string
	if r != nil {
		retired = r.boundTurn()
	}

	p.mu.Lock()
	if p.active == r {
		p.active = nil
	}
	// Only a run that actually bound a turn overwrites the record. A run
	// released without one (an input vetoed before any agent ran, a refused
	// resume) has no trailing events to disown, and letting it clear the field
	// would forget a predecessor whose turn is still live.
	if retired != "" {
		p.retiredTurn = retired
	}
	if park != nil {
		p.parked = park
	}
	p.mu.Unlock()
}

// parkFor builds the parked-turn record for run r suspending turnID,
// carrying the items still to deliver.
func parkFor(r *run, turnID string, carry []deferredItem) *parkedTurn {
	return &parkedTurn{run: r, threadID: r.threadID, turnID: turnID, queue: carry}
}

// deferToolInvoke holds tc on the parked turn when it is a top-level call of
// a client tool the parked run advertised, made by that turn while no run is
// active. It reports whether tc was deferred.
func (p *Plugin) deferToolInvoke(tc events.ToolCall) bool {
	if tc.TurnID == "" || tc.ParentCallID != "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pt := p.parked
	if p.active != nil || pt == nil || pt.turnID != tc.TurnID {
		return false
	}
	if _, ok := pt.run.clientTools[tc.Name]; !ok {
		return false
	}
	pt.queue = append(pt.queue, deferredItem{tool: &tc})
	return true
}

// deferHITL holds req on the parked turn when that turn asked it while no
// run is active. It reports whether req was deferred.
func (p *Plugin) deferHITL(req events.HITLRequest) bool {
	if req.TurnID == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pt := p.parked
	if p.active != nil || pt == nil || pt.turnID != req.TurnID {
		return false
	}
	pt.queue = append(pt.queue, deferredItem{hitl: &req})
	return true
}

// dropDeferredHITL removes a deferred request a hitl.cancel retracted before
// it was ever shown. It reports whether one was removed.
func (p *Plugin) dropDeferredHITL(requestID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	pt := p.parked
	if pt == nil {
		return false
	}
	for i, d := range pt.queue {
		if d.hitl != nil && d.hitl.ID == requestID {
			pt.queue = append(pt.queue[:i:i], pt.queue[i+1:]...)
			return true
		}
	}
	return false
}

// parkedRun is the parked turn's run when no run is active, for the client
// tools it still offers.
func (p *Plugin) parkedRun() *run {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active != nil || p.parked == nil {
		return nil
	}
	return p.parked.run
}

// takeParked removes and returns the parked turn of threadID, if it is
// turnID's: the resume about to answer it owns the deferred items now.
func (p *Plugin) takeParked(threadID, turnID string) *parkedTurn {
	p.mu.Lock()
	defer p.mu.Unlock()
	pt := p.parked
	if pt == nil || pt.threadID != threadID || pt.turnID != turnID {
		return nil
	}
	p.parked = nil
	return pt
}

// dropParked forgets the parked turn once that turn has ended: nothing it
// deferred can be answered any more.
func (p *Plugin) dropParked(turnID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.parked != nil && p.parked.turnID == turnID {
		p.parked = nil
	}
}

// deliverDeferred runs on a resume after its answers were emitted: the next
// deferred item suspends the continuation run r, carrying the rest, or,
// when the resume cancelled, every deferred item is cancelled so the agent
// and any gate waiting on one move on.
func (p *Plugin) deliverDeferred(r *run, queue []deferredItem, cancelled bool) {
	if len(queue) == 0 {
		return
	}
	if cancelled {
		for _, d := range queue {
			p.cancelDeferred(d)
		}
		return
	}
	// The answers just emitted may already have let the agent park r on its
	// own (a gate unblocked by an approval invoking its client tool). The
	// queue then waits behind that interrupt, in order.
	p.mu.Lock()
	if p.active != r {
		if p.parked != nil && p.parked.threadID == r.threadID {
			p.parked.queue = append(append([]deferredItem(nil), queue...), p.parked.queue...)
		}
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	next, rest := queue[0], append([]deferredItem(nil), queue[1:]...)
	if next.tool != nil {
		// Its ToolCall* sequence never streamed (it arrived with no run):
		// stream it on the run that now carries it.
		r.onToolCall(*next.tool)
		p.suspendForClientTool(r, *next.tool, rest)
		return
	}
	p.interruptForHITL(r, *next.hitl, rest)
}

// cancelDeferred answers one deferred item as cancelled: an error result for
// a client tool, a cancelled response for a question.
func (p *Plugin) cancelDeferred(d deferredItem) {
	if d.tool != nil {
		result := events.ToolResult{
			SchemaVersion: events.ToolResultVersion,
			ID:            d.tool.ID,
			Name:          d.tool.Name,
			TurnID:        d.tool.TurnID,
			Error:         "client tool call cancelled",
		}
		if err := p.bus.Emit("tool.result", result); err != nil {
			p.logger.Warn("emit tool.result for a cancelled deferred client tool failed",
				"error", err, "tool", d.tool.Name, "tool_call_id", d.tool.ID)
		}
		return
	}
	resp := events.HITLResponse{
		SchemaVersion: events.HITLResponseVersion,
		RequestID:     d.hitl.ID,
		Cancelled:     true,
		CancelReason:  "resumed as cancelled",
	}
	if err := p.bus.Emit("hitl.responded", resp); err != nil {
		p.logger.Warn("emit hitl.responded for a cancelled deferred question failed",
			"error", err, "request_id", d.hitl.ID)
	}
}
