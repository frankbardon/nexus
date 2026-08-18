package a2a

import (
	"fmt"
	"strings"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// ErrorInfo reasons for the refusals the turn mapping originates. They ride the
// google.rpc.ErrorInfo metadata so a client can branch on a stable token rather
// than on prose.
const (
	reasonForeignContext  = "CONTEXT_NOT_SERVED"
	reasonConcurrentTask  = "TASK_ALREADY_IN_FLIGHT"
	reasonTaskNotRetained = "TASK_NOT_RETAINED"
	reasonBlockingOnly    = "BLOCKING_ONLY"
)

// textMediaType is the media type this transport speaks. It matches the input
// and output modes a stock card advertises.
const textMediaType = "text/plain"

// turnInput is a decoded SendMessage reduced to what the bus needs: the text of
// the turn and the context it belongs to. It decouples the bridge from the
// transport, mirroring nexus.io.agui's runInput.
type turnInput struct {
	// contextID is the client's requested context, empty when it did not name
	// one and expects the server to assign it.
	contextID string
	// text is the concatenated text content of the inbound message.
	text string
	// messageID is the client's message id, carried for logging only.
	messageID string
}

// --- bridge: inbound (server -> bus) and run lifecycle ---

// startTurn binds the context, registers the single active run and emits the
// inbound io.input. It returns the run, or the protocol error the caller must
// answer with.
func (p *Plugin) startTurn(in turnInput) (*run, *a2a.Error) {
	p.mu.Lock()
	if p.active != nil {
		p.mu.Unlock()
		return nil, errConcurrentTask()
	}
	contextID, protoErr := p.bindContextLocked(in.contextID)
	if protoErr != nil {
		p.mu.Unlock()
		return nil, protoErr
	}
	r := newRun(newTaskID(), contextID)
	p.active = r
	p.mu.Unlock()

	ui := events.UserInput{
		SchemaVersion: events.UserInputVersion,
		Content:       in.text,
		SessionID:     p.sessionID,
	}

	// Emitted from a goroutine, exactly as nexus.io.agui does it: the whole turn
	// runs inside this dispatch, and the HTTP goroutine must already be draining
	// frames by then or a streaming client would receive nothing until the turn
	// was over — which is not streaming at all.
	go func() {
		if veto, err := p.bus.EmitVetoable("before:io.input", &ui); err == nil && veto.Vetoed {
			reason := veto.Reason
			if reason == "" {
				reason = "input rejected"
			}
			r.fail("input rejected before it reached the agent: " + reason)
			return
		}
		if err := p.bus.Emit("io.input", ui); err != nil {
			r.fail(fmt.Sprintf("emitting io.input: %v", err))
		}
	}()

	p.logger.Debug("a2a task started",
		"task_id", r.taskID,
		"context_id", contextID,
		"message_id", in.messageID,
	)
	return r, nil
}

// endTurn releases the active-run slot if it still points at r.
func (p *Plugin) endTurn(r *run) {
	p.mu.Lock()
	if p.active == r {
		p.active = nil
	}
	p.mu.Unlock()
}

// currentRun returns the active run, or nil.
func (p *Plugin) currentRun() *run {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active
}

// bindContextLocked resolves an A2A contextId to this listener's Nexus session.
// The caller holds p.mu.
//
// THE MAPPING, and the constraint that shapes it:
//
// An A2A context is a conversation; a Nexus session is a conversation. They are
// the same thing, so contextId maps onto the session — but a Nexus process owns
// exactly ONE session and one memory.history buffer, fixed at boot. There is no
// bus primitive that starts a second session, or that resets history, and
// inventing one here would be a cross-cutting change to every memory plugin.
//
// So the binding is:
//
//   - The first turn claims the session. A client that names no contextId gets
//     the session id as its context; a client that names one has that name
//     recorded. Either way the conversation starts fresh, because the process's
//     session is fresh.
//   - Later turns naming the same context continue it, and the history is
//     intact for free: memory.history persists across turns within a session.
//   - A DIFFERENT contextId is refused. Accepting it would hand the caller a
//     conversation already carrying another context's history while calling it
//     new, which is the one outcome worse than an error. The refusal names the
//     bound context so the fix is obvious: dial a second instance (one process
//     per context), which is exactly what the session broker exists to
//     automate.
func (p *Plugin) bindContextLocked(requested string) (string, *a2a.Error) {
	requested = strings.TrimSpace(requested)
	if p.contextID == "" {
		if requested == "" {
			requested = p.defaultContextID()
		}
		p.contextID = requested
		return p.contextID, nil
	}
	if requested == "" || requested == p.contextID {
		return p.contextID, nil
	}
	return "", errForeignContext(requested, p.contextID)
}

// defaultContextID is the context a client that named none is given. The Nexus
// session id is the honest choice: it is the identity of the conversation the
// client just joined, and it is stable for the life of the process, so echoing
// it back gives the client something it can keep using.
func (p *Plugin) defaultContextID() string {
	if p.sessionID != "" {
		return p.sessionID
	}
	return "ctx-" + engine.GenerateID()
}

// --- bus handlers (engine -> run channel). Never touch the response writer. ---

func (p *Plugin) handleTurnStart(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	t, ok := e.Payload.(events.TurnInfo)
	if !ok {
		return
	}
	r.onTurnStart(t)
}

func (p *Plugin) handleTurnEnd(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	t, ok := e.Payload.(events.TurnInfo)
	if !ok {
		return
	}
	r.onTurnEnd(t)
}

func (p *Plugin) handleLLMResponse(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	resp, ok := e.Payload.(events.LLMResponse)
	if !ok {
		return
	}
	r.onLLMResponse(resp)
}

func (p *Plugin) handleOutput(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	o, ok := e.Payload.(events.AgentOutput)
	if !ok {
		return
	}
	r.onOutput(o)
}

// handleError fails the task on an error nobody is going to recover from.
//
// The filter matters. A retryable error whose retries are NOT exhausted is a
// provider about to try again, and failing the task there would abandon a turn
// that is still going to answer. A fatal error, or one whose retries ARE
// exhausted, has no further Nexus event coming: no llm.response, no
// agent.turn.end. Left alone it would park the client on an open stream
// forever, so it becomes a FAILED status and the stream closes.
func (p *Plugin) handleError(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	info, ok := e.Payload.(events.ErrorInfo)
	if !ok {
		return
	}
	if !info.Fatal && !info.RetriesExhausted {
		return
	}
	reason := "the agent turn failed"
	if info.Err != nil {
		reason = info.Err.Error()
	}
	p.logger.Warn("a2a task failed", "task_id", r.taskID, "source", info.Source, "error", info.Err)
	r.fail(reason)
}

// --- inbound translation ---

// translateSendMessage reduces a SendMessageRequest to a turnInput, or reports
// the protocol error that refuses it.
//
// Every refusal here names a capability this agent genuinely does not have yet.
// None of them is a placeholder: each maps to the error type the specification
// reserves for that condition, so a client already knows how to handle it.
func translateSendMessage(req *a2a.SendMessageRequest) (turnInput, *a2a.Error) {
	if req == nil {
		return turnInput{}, a2a.ErrInvalidParams(a2a.FieldViolation{
			Field: "message", Description: "a message is required",
		})
	}
	msg := req.Message

	// A message naming a task is a continuation of that task (specification
	// section 3.4). Tasks are not retained between calls yet, so the honest
	// answer is that the task is unknown — not a silently fresh turn under a
	// task id the client believes it is resuming.
	if msg.TaskID != "" {
		return turnInput{}, a2a.Errorf(a2a.ErrorTypeTaskNotFound,
			"task %q is not known to this agent: tasks are not retained between calls yet, so a task cannot be continued",
			msg.TaskID).
			WithMetadata("detail", reasonTaskNotRetained)
	}
	if msg.Role != a2a.RoleUser {
		return turnInput{}, a2a.ErrInvalidParams(a2a.FieldViolation{
			Field:       "message.role",
			Description: fmt.Sprintf("an inbound message must carry %s, got %q", a2a.RoleUser, string(msg.Role)),
		})
	}

	text, protoErr := textFromParts(msg.Parts)
	if protoErr != nil {
		return turnInput{}, protoErr
	}

	if cfg := req.Configuration; cfg != nil {
		if cfg.TaskPushNotificationConfig != nil {
			return turnInput{}, a2a.Errorf(a2a.ErrorTypePushNotificationNotSupported,
				"this agent does not deliver push notifications; the agent card declares capabilities.pushNotifications as false")
		}
		if cfg.ReturnImmediately {
			// Returning as soon as the task exists is only useful if the client
			// can then observe it, and GetTask / SubscribeToTask are not wired.
			// A non-blocking return would hand back a task id that can never be
			// resolved, so the call is refused instead.
			return turnInput{}, a2a.Errorf(a2a.ErrorTypeUnsupportedOperation,
				"configuration.returnImmediately is not supported: this agent cannot yet be polled for a task it returned early; use the blocking call or SendStreamingMessage").
				WithMetadata("detail", reasonBlockingOnly)
		}
		if protoErr := checkAcceptedOutputModes(cfg.AcceptedOutputModes); protoErr != nil {
			return turnInput{}, protoErr
		}
	}

	return turnInput{
		contextID: msg.ContextID,
		text:      text,
		messageID: msg.MessageID,
	}, nil
}

// textFromParts renders the inbound message content as the plain text a Nexus
// io.input carries. Several text parts are joined with a blank line, which is
// how the parts read as one prompt.
//
// A non-text part is refused rather than dropped. ContentTypeNotSupportedError
// is the error section 3.3.4 defines for exactly this, and the agent card backs
// it up by declaring text/plain input modes: a client is told what this agent
// accepts before it sends anything, and gets a matching refusal if it sends
// something else. Silently ignoring a file part would answer a question the
// client did not ask.
func textFromParts(parts []a2a.Part) (string, *a2a.Error) {
	var chunks []string
	for i, part := range parts {
		text, ok := part.TextValue()
		if !ok {
			return "", a2a.Errorf(a2a.ErrorTypeContentTypeNotSupported,
				"message.parts[%d] is a %s part; this agent accepts text parts only", i, string(part.Kind()))
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		chunks = append(chunks, text)
	}
	if len(chunks) == 0 {
		return "", a2a.ErrInvalidParams(a2a.FieldViolation{
			Field:       "message.parts",
			Description: "at least one part must carry non-empty text",
		})
	}
	return strings.Join(chunks, "\n\n"), nil
}

// checkAcceptedOutputModes refuses a client that cannot accept anything this
// agent produces. An empty list means "no client-imposed restriction".
func checkAcceptedOutputModes(modes []string) *a2a.Error {
	if len(modes) == 0 {
		return nil
	}
	for _, mode := range modes {
		switch strings.ToLower(strings.TrimSpace(mode)) {
		case textMediaType, "text/*", "*/*", "text":
			return nil
		}
	}
	return a2a.Errorf(a2a.ErrorTypeContentTypeNotSupported,
		"this agent produces %s; the request accepts only %s", textMediaType, strings.Join(modes, ", "))
}

// --- refusals this bridge originates ---

// errConcurrentTask refuses a second task while one is in flight.
//
// The listener fronts one engine with one agent loop and one memory buffer;
// two turns in flight would interleave on the same bus and corrupt both
// conversations. UnsupportedOperationError maps to FAILED_PRECONDITION, which
// is the accurate reading: the operation is fine, the current state forbids it.
func errConcurrentTask() *a2a.Error {
	return a2a.Errorf(a2a.ErrorTypeUnsupportedOperation,
		"a task is already in flight on this agent: it serves one Nexus session and runs one task at a time").
		WithMetadata("detail", reasonConcurrentTask)
}

// errForeignContext refuses a contextId other than the one this process serves.
// See bindContextLocked for the reasoning.
func errForeignContext(requested, bound string) *a2a.Error {
	return a2a.Errorf(a2a.ErrorTypeUnsupportedOperation,
		"context %q is not served by this agent: it is bound to context %q for the life of its Nexus session, so run one instance per context",
		requested, bound).
		WithMetadata("detail", reasonForeignContext).
		WithMetadata("contextId", bound)
}
