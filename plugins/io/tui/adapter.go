package tui

import (
	"context"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/ui"
)

// Adapter implements ui.UIAdapter using a BubbleTea program.
type Adapter struct {
	program *tea.Program
	session *engine.SessionWorkspace

	mu              sync.Mutex
	inputHandler    func(ui.InputMessage)
	approvalHandler func(ui.ApprovalResponseMessage)
	cancelHandler   func()
	resumeHandler   func()
	approvalCh      chan ui.ApprovalResponseMessage

	askCh chan ui.AskUserResponseMessage

	done chan struct{}
}

// NewAdapter creates a TUI adapter backed by BubbleTea.
func NewAdapter(session *engine.SessionWorkspace) *Adapter {
	return &Adapter{
		session:    session,
		approvalCh: make(chan ui.ApprovalResponseMessage, 1),
		askCh:      make(chan ui.AskUserResponseMessage, 1),
		done:       make(chan struct{}),
	}
}

// Start creates and runs the BubbleTea program.
func (a *Adapter) Start(ctx context.Context) error {
	m := newModel(a)
	a.program = tea.NewProgram(m,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

	go func() {
		defer close(a.done)
		_, _ = a.program.Run()
	}()

	return nil
}

// Stop signals the BubbleTea program to quit and waits for it to fully
// tear down (restoring the terminal from alt screen mode).
func (a *Adapter) Stop(_ context.Context) error {
	if a.program != nil {
		a.program.Send(quitMsg{})
		a.Wait()
	}
	return nil
}

// Wait blocks until the BubbleTea program exits.
func (a *Adapter) Wait() {
	<-a.done
}

// SendOutput sends a complete output message to the TUI.
func (a *Adapter) SendOutput(msg ui.OutputMessage) error {
	if a.program != nil {
		a.program.Send(outputMsg{msg})
	}
	return nil
}

// SendStreamChunk sends a streaming chunk to the TUI.
func (a *Adapter) SendStreamChunk(msg ui.StreamChunkMessage) error {
	if a.program != nil {
		a.program.Send(streamChunkMsg{msg})
	}
	return nil
}

// SendStreamEnd signals the end of a stream.
func (a *Adapter) SendStreamEnd(msg ui.StreamEndMessage) error {
	if a.program != nil {
		a.program.Send(streamEndMsg{msg})
	}
	return nil
}

// SendStatus sends a status update to the TUI.
func (a *Adapter) SendStatus(msg ui.StatusMessage) error {
	if a.program != nil {
		a.program.Send(statusMsg{msg})
	}
	return nil
}

// SendThinking sends a thinking step to the TUI.
func (a *Adapter) SendThinking(msg ui.ThinkingMessage) error {
	if a.program != nil {
		a.program.Send(thinkingMsg{msg})
	}
	return nil
}

// SendPlanDisplay sends a plan display to the TUI.
func (a *Adapter) SendPlanDisplay(msg ui.PlanDisplayMessage) error {
	if a.program != nil {
		a.program.Send(planDisplayMsg{msg})
	}
	return nil
}

// SendPlanUpdate sends an updated plan with step statuses to the TUI.
func (a *Adapter) SendPlanUpdate(turnID string, steps []planUpdateStep) error {
	if a.program != nil {
		a.program.Send(planUpdateMsg{TurnID: turnID, Steps: steps})
	}
	return nil
}

// SendFileChanged notifies the TUI that a session file changed.
func (a *Adapter) SendFileChanged(path, action string) error {
	if a.program != nil {
		a.program.Send(fileChangedMsg{Path: path, Action: action})
	}
	return nil
}

// RequestApproval sends an approval request and blocks until the user responds.
func (a *Adapter) RequestApproval(msg ui.ApprovalRequestMessage) (ui.ApprovalResponseMessage, error) {
	if a.program != nil {
		a.program.Send(approvalRequestMsg{msg})
	}

	resp := <-a.approvalCh

	return ui.ApprovalResponseMessage{
		PromptID: msg.PromptID,
		Approved: resp.Approved,
		Always:   resp.Always,
	}, nil
}

// RequestInput sends a question to the user and blocks until they respond with text.
func (a *Adapter) RequestInput(msg ui.AskUserMessage) (ui.AskUserResponseMessage, error) {
	if a.program != nil {
		a.program.Send(askUserMsg{msg})
	}
	resp := <-a.askCh
	return resp, nil
}

// OnInput registers the callback for user input.
func (a *Adapter) OnInput(handler func(ui.InputMessage)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inputHandler = handler
}

// OnApprovalResponse registers the callback for approval responses.
func (a *Adapter) OnApprovalResponse(handler func(ui.ApprovalResponseMessage)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.approvalHandler = handler
}

// OnCancel registers the callback for cancel requests.
func (a *Adapter) OnCancel(handler func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cancelHandler = handler
}

// OnResume registers the callback for resume requests.
func (a *Adapter) OnResume(handler func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resumeHandler = handler
}

// SendCancelComplete notifies the TUI that a cancellation completed.
func (a *Adapter) SendCancelComplete(turnID string, resumable bool) error {
	if a.program != nil {
		a.program.Send(cancelCompleteMsg{TurnID: turnID, Resumable: resumable})
	}
	return nil
}

// Sessions returns info about the current terminal session.
func (a *Adapter) Sessions() []ui.SessionInfo {
	return []ui.SessionInfo{
		{
			Transport: "tui",
			UserAgent: "nexus-tui/2.0",
		},
	}
}
