package wails

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/ui"
)

// Adapter implements ui.UIAdapter on top of the Wails runtime.
//
// The outbound path (SendOutput, SendStatus, SendStreamChunk, SendStreamEnd)
// is fully wired. Approval and ask-user flows use the same channel plumbing
// as the browser adapter. No code here is Wails-specific — everything funnels
// through the Hub abstraction.
//
// In config-driven mode, the adapter also handles generic inbound events:
// any envelope whose Type matches the acceptList is unmarshaled to
// map[string]any and emitted on the bus.
type Adapter struct {
	hub       *Hub
	sessionID string

	// Generic inbound bridging for config-driven mode.
	bus        engine.EventBus
	acceptList map[string]bool

	mu              sync.Mutex
	inputHandler    func(ui.InputMessage)
	approvalHandler func(ui.ApprovalResponseMessage)
	cancelHandler   func()
	resumeHandler   func()
	approvalCh      chan ui.ApprovalResponseMessage
	askCh           chan ui.AskUserResponseMessage
}

// NewAdapter creates a Wails UI adapter backed by the given hub.
//
// bus and acceptList enable generic inbound event bridging for
// config-driven mode. Pass nil/nil for legacy mode.
func NewAdapter(hub *Hub, sessionID string, bus engine.EventBus, acceptList []string) *Adapter {
	accepted := make(map[string]bool, len(acceptList))
	for _, a := range acceptList {
		accepted[a] = true
	}
	a := &Adapter{
		hub:        hub,
		sessionID:  sessionID,
		bus:        bus,
		acceptList: accepted,
		approvalCh: make(chan ui.ApprovalResponseMessage, 1),
		askCh:      make(chan ui.AskUserResponseMessage, 1),
	}

	hub.OnMessage(a.handleInbound)
	return a
}

// Start is a no-op; the Wails runtime is owned by the host process.
func (a *Adapter) Start(_ context.Context) error { return nil }

// Stop is a no-op for the same reason.
func (a *Adapter) Stop(_ context.Context) error { return nil }

// SendOutput emits a complete output message to the attached webview.
func (a *Adapter) SendOutput(msg ui.OutputMessage) error {
	return a.broadcast(ui.TypeOutput, msg)
}

// SendStreamChunk emits a streaming chunk to the webview.
func (a *Adapter) SendStreamChunk(msg ui.StreamChunkMessage) error {
	return a.broadcast(ui.TypeStreamChunk, msg)
}

// SendStreamEnd signals end of a stream to the webview.
func (a *Adapter) SendStreamEnd(msg ui.StreamEndMessage) error {
	return a.broadcast(ui.TypeStreamEnd, msg)
}

// SendStatus emits a status update to the webview.
func (a *Adapter) SendStatus(msg ui.StatusMessage) error {
	return a.broadcast(ui.TypeStatus, msg)
}

// SendThinking emits an intermediate reasoning step to the webview.
func (a *Adapter) SendThinking(msg ui.ThinkingMessage) error {
	return a.broadcast("thinking", msg)
}

// SendPlanDisplay emits a plan overview to the webview.
func (a *Adapter) SendPlanDisplay(msg ui.PlanDisplayMessage) error {
	return a.broadcast("plan", msg)
}

// SendPlanUpdate emits an updated plan with current step statuses to the webview.
func (a *Adapter) SendPlanUpdate(msg ui.PlanDisplayMessage) error {
	return a.broadcast("plan", msg)
}

// SendFileChanged notifies the webview that a session file was created or updated.
func (a *Adapter) SendFileChanged(path string, action string) error {
	return a.broadcast(ui.TypeFileChanged, map[string]string{
		"path":   path,
		"action": action,
	})
}

// SendCancelComplete notifies the webview that a cancellation has completed.
func (a *Adapter) SendCancelComplete(turnID string, resumable bool) error {
	return a.broadcast(ui.TypeCancelComplete, map[string]any{
		"turn_id":   turnID,
		"resumable": resumable,
	})
}

// RequestInput sends a question to the user and blocks until they respond with text.
func (a *Adapter) RequestInput(msg ui.AskUserMessage) (ui.AskUserResponseMessage, error) {
	if err := a.broadcast(ui.TypeAskRequest, msg); err != nil {
		return ui.AskUserResponseMessage{}, fmt.Errorf("broadcasting ask request: %w", err)
	}
	resp := <-a.askCh
	return resp, nil
}

// RequestApproval emits an approval request and blocks until the user responds.
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

// OnInput registers a callback for user input from the webview.
func (a *Adapter) OnInput(handler func(ui.InputMessage)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inputHandler = handler
}

// OnApprovalResponse registers a callback for approval responses.
func (a *Adapter) OnApprovalResponse(handler func(ui.ApprovalResponseMessage)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.approvalHandler = handler
}

// OnCancel registers a callback for cancel requests.
func (a *Adapter) OnCancel(handler func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cancelHandler = handler
}

// OnResume registers a callback for resume requests.
func (a *Adapter) OnResume(handler func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resumeHandler = handler
}

// Sessions returns info about the (single) attached Wails webview.
func (a *Adapter) Sessions() []ui.SessionInfo {
	return a.hub.Sessions()
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

func (a *Adapter) handleInbound(env ui.Envelope) {
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

	default:
		// Config-driven generic inbound: if the envelope type matches
		// the accept list, unmarshal the payload to map[string]any and
		// emit it on the bus. This is the path domain events take from
		// the frontend (e.g. "match.request", "hello.request").
		if a.bus != nil && a.acceptList[env.Type] {
			var payload map[string]any
			if err := json.Unmarshal(env.Payload, &payload); err != nil {
				return
			}
			go a.bus.Emit(env.Type, payload)
		}
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
