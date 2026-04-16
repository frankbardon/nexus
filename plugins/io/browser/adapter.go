package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/ui"
)

// Adapter implements a WebSocket-based UI adapter.
type Adapter struct {
	hub       *Hub
	sessionID string

	mu              sync.Mutex
	inputHandler    func(ui.InputMessage)
	approvalHandler func(ui.ApprovalResponseMessage)
	cancelHandler   func()
	resumeHandler   func()
	approvalCh      chan ui.ApprovalResponseMessage
	askCh           chan ui.AskUserResponseMessage
}

// NewAdapter creates a browser UI adapter backed by the given hub.
func NewAdapter(hub *Hub, sessionID string) *Adapter {
	a := &Adapter{
		hub:        hub,
		sessionID:  sessionID,
		approvalCh: make(chan ui.ApprovalResponseMessage, 1),
		askCh:      make(chan ui.AskUserResponseMessage, 1),
	}

	hub.OnMessage(a.handleInbound)
	return a
}

// Start is a no-op for the browser adapter; the HTTP server is started separately.
func (a *Adapter) Start(_ context.Context) error { return nil }

// Stop is a no-op; shutdown is handled by the server.
func (a *Adapter) Stop(_ context.Context) error { return nil }

// SendOutput sends a complete output message to all clients.
func (a *Adapter) SendOutput(msg ui.OutputMessage) error {
	return a.broadcast(ui.TypeOutput, msg)
}

// SendStreamChunk sends a streaming chunk to all clients.
func (a *Adapter) SendStreamChunk(msg ui.StreamChunkMessage) error {
	return a.broadcast(ui.TypeStreamChunk, msg)
}

// SendStreamEnd signals the end of a stream to all clients.
func (a *Adapter) SendStreamEnd(msg ui.StreamEndMessage) error {
	return a.broadcast(ui.TypeStreamEnd, msg)
}

// SendStatus sends a status update to all clients.
func (a *Adapter) SendStatus(msg ui.StatusMessage) error {
	return a.broadcast(ui.TypeStatus, msg)
}

// SendThinking sends an intermediate thinking step to all clients.
func (a *Adapter) SendThinking(msg ui.ThinkingMessage) error {
	return a.broadcast("thinking", msg)
}

// SendPlanDisplay sends a plan overview to all clients.
func (a *Adapter) SendPlanDisplay(msg ui.PlanDisplayMessage) error {
	return a.broadcast("plan", msg)
}

// SendPlanUpdate sends an updated plan with current step statuses to all clients.
func (a *Adapter) SendPlanUpdate(msg ui.PlanDisplayMessage) error {
	return a.broadcast("plan", msg)
}

// RequestApproval sends an approval request and blocks until the user responds.
func (a *Adapter) RequestApproval(msg ui.ApprovalRequestMessage) (ui.ApprovalResponseMessage, error) {
	if err := a.broadcast(ui.TypeApprovalRequest, msg); err != nil {
		return ui.ApprovalResponseMessage{}, fmt.Errorf("broadcasting approval request: %w", err)
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
	if err := a.broadcast(ui.TypeAskRequest, msg); err != nil {
		return ui.AskUserResponseMessage{}, fmt.Errorf("broadcasting ask request: %w", err)
	}
	resp := <-a.askCh
	return resp, nil
}

// OnInput registers the callback for user input messages.
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

// SendCancelComplete notifies clients that a cancellation completed.
func (a *Adapter) SendCancelComplete(turnID string, resumable bool) error {
	return a.broadcast(ui.TypeCancelComplete, map[string]any{
		"turn_id":   turnID,
		"resumable": resumable,
	})
}

// Sessions returns info about connected browser sessions.
func (a *Adapter) Sessions() []ui.SessionInfo {
	return a.hub.Sessions()
}

// SendFileChanged notifies clients that a session file was created or updated.
func (a *Adapter) SendFileChanged(path string, action string) error {
	return a.broadcast(ui.TypeFileChanged, map[string]string{
		"path":   path,
		"action": action,
	})
}

func (a *Adapter) broadcast(msgType string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling payload: %w", err)
	}

	env := ui.Envelope{
		Type:      msgType,
		ID:        generateEnvelopeID(),
		SessionID: a.sessionID,
		Timestamp: time.Now(),
		Payload:   raw,
	}

	return a.hub.BroadcastEnvelope(env)
}

func (a *Adapter) handleInbound(_ string, env ui.Envelope) {
	switch env.Type {
	case ui.TypeInput:
		var msg ui.InputMessage
		if err := json.Unmarshal(env.Payload, &msg); err != nil {
			return
		}
		a.mu.Lock()
		handler := a.inputHandler
		a.mu.Unlock()
		if handler != nil {
			handler(msg)
		}

	case ui.TypeApprovalResponse:
		var msg ui.ApprovalResponseMessage
		if err := json.Unmarshal(env.Payload, &msg); err != nil {
			return
		}
		select {
		case a.approvalCh <- msg:
		default:
		}
		a.mu.Lock()
		handler := a.approvalHandler
		a.mu.Unlock()
		if handler != nil {
			handler(msg)
		}

	case ui.TypeAskResponse:
		var msg ui.AskUserResponseMessage
		if err := json.Unmarshal(env.Payload, &msg); err != nil {
			return
		}
		select {
		case a.askCh <- msg:
		default:
		}

	case ui.TypeCancelRequest:
		a.mu.Lock()
		handler := a.cancelHandler
		a.mu.Unlock()
		if handler != nil {
			handler()
		}

	case ui.TypeResumeRequest:
		a.mu.Lock()
		handler := a.resumeHandler
		a.mu.Unlock()
		if handler != nil {
			handler()
		}

	case ui.TypePing:
		_ = a.broadcast(ui.TypePong, nil)
	}
}

var envelopeCounter uint64
var envelopeMu sync.Mutex

func generateEnvelopeID() string {
	envelopeMu.Lock()
	envelopeCounter++
	id := envelopeCounter
	envelopeMu.Unlock()
	return fmt.Sprintf("env-%d", id)
}
