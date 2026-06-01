package browser

import (
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/ui"
	"github.com/frankbardon/nexus/plugins/workflows/icm/icmtypes"
)

// sendICMThinking funnels every ICM-progress row through the existing
// thinking-step adapter so the browser frontend reuses the same envelope
// type ("thinking") that it already renders.
func (p *Plugin) sendICMThinking(phase, content string) {
	_ = p.adapter.SendThinking(ui.ThinkingMessage{
		Content: content,
		Phase:   phase,
		Source:  "nexus.workflows.icm",
	})
}

func (p *Plugin) handleICMRunStarted(e engine.Event[any]) {
	ev, ok := e.Payload.(icmtypes.ICMRunStarted)
	if !ok {
		return
	}
	p.sendICMThinking(icmtypes.FormatRunStarted(ev))
}

func (p *Plugin) handleICMRunCompleted(e engine.Event[any]) {
	ev, ok := e.Payload.(icmtypes.ICMRunCompleted)
	if !ok {
		return
	}
	p.sendICMThinking(icmtypes.FormatRunCompleted(ev))
}

func (p *Plugin) handleICMRunHalted(e engine.Event[any]) {
	ev, ok := e.Payload.(icmtypes.ICMRunHalted)
	if !ok {
		return
	}
	p.sendICMThinking(icmtypes.FormatRunHalted(ev))
}

func (p *Plugin) handleICMStageStarted(e engine.Event[any]) {
	ev, ok := e.Payload.(icmtypes.ICMStageStarted)
	if !ok {
		return
	}
	p.sendICMThinking(icmtypes.FormatStageStarted(ev))
}

func (p *Plugin) handleICMStageCompleted(e engine.Event[any]) {
	ev, ok := e.Payload.(icmtypes.ICMStageCompleted)
	if !ok {
		return
	}
	p.sendICMThinking(icmtypes.FormatStageCompleted(ev))
}

func (p *Plugin) handleICMStageFailed(e engine.Event[any]) {
	ev, ok := e.Payload.(icmtypes.ICMStageFailed)
	if !ok {
		return
	}
	p.sendICMThinking(icmtypes.FormatStageFailed(ev))
}

func (p *Plugin) handleICMStageIteration(e engine.Event[any]) {
	ev, ok := e.Payload.(icmtypes.ICMStageIteration)
	if !ok {
		return
	}
	p.sendICMThinking(icmtypes.FormatStageIteration(ev))
}

func (p *Plugin) handleICMTurn(e engine.Event[any]) {
	ev, ok := e.Payload.(icmtypes.ICMTurn)
	if !ok {
		return
	}
	p.sendICMThinking(icmtypes.FormatTurn(ev))
}

func (p *Plugin) handleICMFanoutItem(e engine.Event[any]) {
	ev, ok := e.Payload.(icmtypes.ICMFanoutItem)
	if !ok {
		return
	}
	p.sendICMThinking(icmtypes.FormatFanoutItem(ev))
}

func (p *Plugin) handleICMPredicateFailed(e engine.Event[any]) {
	ev, ok := e.Payload.(icmtypes.ICMPredicateFailed)
	if !ok {
		return
	}
	p.sendICMThinking(icmtypes.FormatPredicateFailed(ev))
}

// handleWorkflowProgress projects the engine-generic events.WorkflowProgress
// payload onto ui.WorkflowStatusMessage and broadcasts it. The browser
// frontend renders the latest payload in a sticky indicator above the
// chat panel.
func (p *Plugin) handleWorkflowProgress(e engine.Event[any]) {
	ev, ok := e.Payload.(events.WorkflowProgress)
	if !ok {
		return
	}
	_ = p.adapter.SendWorkflowStatus(ui.WorkflowStatusMessage{
		WorkflowID:    ev.WorkflowID,
		WorkflowName:  ev.WorkflowName,
		RunID:         ev.RunID,
		Stage:         ev.Stage,
		StageLabel:    ev.StageLabel,
		StageIndex:    ev.StageIndex,
		StageTotal:    ev.StageTotal,
		Iteration:     ev.Iteration,
		MaxIterations: ev.MaxIterations,
		Turn:          ev.Turn,
		MaxTurns:      ev.MaxTurns,
		ItemsDone:     ev.ItemsDone,
		ItemsTotal:    ev.ItemsTotal,
		CurrentItem:   ev.CurrentItem,
		Status:        ev.Status,
		Detail:        ev.Detail,
		Failures:      ev.Failures,
	})
}
