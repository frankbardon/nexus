package tui

import "github.com/frankbardon/nexus/pkg/ui"

// Tea messages bridging ui.* types to BubbleTea.

type outputMsg struct{ ui.OutputMessage }
type streamChunkMsg struct{ ui.StreamChunkMessage }
type streamEndMsg struct{ ui.StreamEndMessage }
type statusMsg struct{ ui.StatusMessage }
type thinkingMsg struct{ ui.ThinkingMessage }
type codeExecStdoutMsg struct{ ui.CodeExecStdoutMessage }
type planDisplayMsg struct{ ui.PlanDisplayMessage }
type approvalRequestMsg struct{ ui.ApprovalRequestMessage }
type askUserMsg struct{ ui.HITLRequestMessage }

// planUpdateMsg carries step status updates from the agent during execution.
type planUpdateMsg struct {
	TurnID string
	Steps  []planUpdateStep
}

type planUpdateStep struct {
	Description string
	Status      string
}

// planStepStatusMsg carries a single-step status delta keyed by step ID,
// the shape emitted by plan.progress events. Distinct from planUpdateMsg
// (which carries the full ordered list from agent.plan) so step status
// can transition without re-sending every other step.
type planStepStatusMsg struct {
	PlanID string
	StepID string
	Status string
	Detail string
}

type fileChangedMsg struct {
	Path   string
	Action string
}

type fileEntry struct {
	Name  string
	Path  string
	IsDir bool
	Size  int64
}

type fileListMsg struct {
	Files []fileEntry
}

type fileContentMsg struct {
	Path    string
	Content string
	Err     error
}

// sessionInfoMsg delivers session metadata to the model.
type sessionInfoMsg struct {
	ID string
}

// pluginListMsg delivers feature flags derived from active plugins.
type pluginListMsg struct {
	Features map[string]bool
}

// cancelCompleteMsg signals that a cancellation finished and resume may be available.
type cancelCompleteMsg struct {
	TurnID    string
	Resumable bool
}

// outputClearMsg tells the chat view to wipe the current partial stream.
type outputClearMsg struct{}

// quitMsg tells the program to exit.
type quitMsg struct{}
