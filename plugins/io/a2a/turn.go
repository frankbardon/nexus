package a2a

import (
	"fmt"
	"strings"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// ErrorInfo reasons for the refusals the turn mapping originates. They ride the
// google.rpc.ErrorInfo metadata so a client can branch on a stable token rather
// than on prose.
const (
	reasonForeignContext  = "CONTEXT_NOT_SERVED"
	reasonConcurrentTask  = "TASK_ALREADY_IN_FLIGHT"
	reasonTaskTerminal    = "TASK_ALREADY_TERMINAL"
	reasonTaskNotLive     = "TASK_NO_LONGER_LIVE"
	reasonTaskNotAwaiting = "TASK_NOT_AWAITING_INPUT"
	reasonContextMismatch = "TASK_CONTEXT_MISMATCH"
)

// metadataHITLRequestID is the message-metadata key an INPUT_REQUIRED question
// carries its originating hitl.requested id under. See run.park.
const metadataHITLRequestID = "nexus.hitl.requestId"

// cancelSource is the events.CancelRequest.Source this transport identifies
// itself as on the bus, alongside "tui", "browser" and the rest.
const cancelSource = "a2a"

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
	// taskID is the task this message continues, empty for a message that
	// starts a new one. A2A resumes an interrupted task by naming it here
	// (specification section 3.4); see resumeTurn.
	taskID string
	// textOnly records that the request's acceptedOutputModes named text types
	// only, so this task must publish no JSON or inline-file parts. See
	// runConfig.textOnly.
	textOnly bool
}

// --- bridge: inbound (server -> bus) and run lifecycle ---

// startTurn binds the context, records the new task against the caller,
// registers the single active run and emits the inbound io.input. It returns
// the run and the requesting client's attached stream, or the protocol error the
// caller must answer with.
//
// The stream is attached HERE rather than by the HTTP handler, before anything
// can be emitted, so the requesting client cannot miss a frame produced between
// the run being registered and its handler getting around to subscribing.
//
// caller is the authenticated Principal the request resolved to — the zero
// value when the listener runs with no validator chain. It is the ONLY way a
// task reaches the store, so a task cannot be created without an owner: the
// store's scoped view is derived from it here and handed to the run as a
// write-only sink.
func (p *Plugin) startTurn(in turnInput, caller nexusauth.Principal, opts streamOptions) (*run, *stream, a2a.Task, *a2a.Error) {
	p.mu.Lock()
	// The two refusals below are checked in this order on purpose. Do not swap
	// them.
	//
	// A request can be wrong in two independent ways: it can name a context this
	// process does not serve, or it can arrive while a task is in flight. The
	// context is resolved FIRST because that refusal describes the request
	// itself, while the in-flight refusal describes the listener's momentary
	// state. Checking the slot first told a client presenting a genuinely
	// foreign contextId that a task was already in flight — a transient-sounding
	// reason that invites a retry, for a mistake that is permanent: no amount of
	// waiting makes this instance serve a second context, and the client has to
	// dial a different instance (which is what the session broker automates).
	// Resolving first means a refusal always names the client's actual mistake,
	// and the in-flight refusal keeps the one meaning worth having: your context
	// is right, come back when this turn is over.
	//
	// The binding is NOT committed here, only resolved — see
	// resolveContextLocked. Committing it before the slot check would let a
	// request that is about to be refused claim this process's context on its
	// way out, which is the one outcome worse than the wrong error message.
	contextID, protoErr := p.resolveContextLocked(in.contextID)
	if protoErr != nil {
		p.mu.Unlock()
		return nil, nil, a2a.Task{}, protoErr
	}
	if p.active != nil {
		p.mu.Unlock()
		return nil, nil, a2a.Task{}, errConcurrentTask()
	}
	owner := p.tasks.For(caller)
	var r *run
	r = newRun(runConfig{
		taskID:     newTaskID(),
		contextID:  contextID,
		sink:       owner,
		logger:     p.logger,
		artifacts:  p.cfg.artifacts,
		textOnly:   in.textOnly,
		onTerminal: func() { p.endTurn(r) },
	})
	sub, opening := r.attach(opts)
	// Both claims are committed together, at the one point where the turn is
	// certain to start: this process takes the context and the slot in the same
	// lock hold, so "bound to a context" and "has run at least one turn" cannot
	// disagree. That keeps the invariant resolveContextLocked relies on — an
	// unbound listener has no active run — true by construction.
	p.contextID = contextID
	p.active = r
	p.mu.Unlock()

	// The task is durable BEFORE the turn is allowed to start. Every later
	// transition is an UPDATE against this row, so a task whose creation failed
	// would silently drop its whole history; refusing here is the only reading
	// that keeps the store and the wire in agreement.
	if err := owner.Create(r.taskID, contextID, a2a.NewTaskStatus(a2a.TaskStateSubmitted), messageRef{
		MessageID: in.messageID,
		Role:      a2a.RoleUser,
		Text:      in.text,
	}); err != nil {
		r.detach(sub)
		p.endTurn(r)
		p.logger.Error("a2a task could not be recorded", "task_id", r.taskID, "error", err)
		return nil, nil, a2a.Task{}, a2a.Errorf(a2a.ErrorTypeInternal,
			"the task could not be recorded durably, so the turn was not started")
	}

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
	return r, sub, opening, nil
}

// endTurn releases the active-run slot if it still points at r.
//
// It is called from the run's terminal sequence, NOT from the request that
// started the turn. That is the whole of the detached-lifetime change: a task
// outlives its originating HTTP request, so a client may disconnect mid-turn
// and reattach later, a question may be parked for as long as a human takes to
// answer it, and configuration.returnImmediately is answerable. The cost is
// that a wedged turn holds the slot until something ends it — which is why
// CancelTask landed in the same story, and why an unanswered question has a
// deadline.
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

// liveRun returns the active run if it is the named task, and nil otherwise —
// including when some OTHER task is in flight.
//
// It performs no authorization of its own and must not be reached without one:
// every caller resolves the task through the principal-scoped store first, so
// the task id it passes here is already known to belong to the caller. Matching
// on the id alone here is therefore not a hole, it is the second half of a check
// whose first half is the only way to learn the id.
func (p *Plugin) liveRun(taskID string) *run {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active != nil && p.active.taskID == taskID {
		return p.active
	}
	return nil
}

// resumeTurn routes a message naming a task onto the question that task is
// parked on, and returns the SAME run with a fresh stream attached.
//
// This is A2A's own resume mechanism (specification section 3.4): an
// interrupted task is continued by sending a new message carrying the same
// taskId and contextId. It is emphatically NOT a new turn — no io.input is
// emitted, no second task is created, and the agent loop that asked the
// question simply stops blocking. A client that got INPUT_REQUIRED and answered
// it sees its answer land in the turn that asked.
//
// Every refusal below is resolved through the principal-scoped store first, so
// a task belonging to somebody else is refused exactly as an unknown id is,
// before anything about its state can leak.
func (p *Plugin) resumeTurn(in turnInput, caller nexusauth.Principal, opts streamOptions) (*run, *stream, a2a.Task, *a2a.Error) {
	rec, found, err := p.tasks.For(caller).Get(in.taskID)
	if err != nil {
		p.logger.Error("a2a task lookup failed", "task_id", in.taskID, "error", err)
		return nil, nil, a2a.Task{}, a2a.Errorf(a2a.ErrorTypeInternal, "the task could not be read")
	}
	if !found {
		return nil, nil, a2a.Task{}, a2a.ErrTaskNotFound(in.taskID)
	}
	if rec.Status.State.IsTerminal() {
		return nil, nil, a2a.Task{}, a2a.Errorf(a2a.ErrorTypeUnsupportedOperation,
			"task %q is in terminal state %s and accepts no further messages; send a message with no taskId to start a new task",
			in.taskID, rec.Status.State.String()).
			WithMetadata("taskId", in.taskID).
			WithMetadata("detail", reasonTaskTerminal)
	}
	// Section 3.4 requires the SAME contextId alongside the taskId. A client
	// that omits it is taken to mean the task's own context; a client that names
	// a different one is refused rather than quietly corrected, because the two
	// readings of that request — "continue this task" and "start a conversation
	// elsewhere" — have nothing in common.
	if in.contextID != "" && in.contextID != rec.ContextID {
		return nil, nil, a2a.Task{}, a2a.Errorf(a2a.ErrorTypeUnsupportedOperation,
			"message names task %q but context %q; a task is continued by naming the context it belongs to, %q",
			in.taskID, in.contextID, rec.ContextID).
			WithMetadata("taskId", in.taskID).
			WithMetadata("contextId", rec.ContextID).
			WithMetadata("detail", reasonContextMismatch)
	}

	r := p.liveRun(in.taskID)
	if r == nil {
		// Non-terminal in the store but not running here. The store's open-time
		// sweep settles tasks a previous process left behind, so reaching this
		// is a task that ended between the read above and now.
		return nil, nil, a2a.Task{}, a2a.Errorf(a2a.ErrorTypeUnsupportedOperation,
			"task %q is no longer running on this agent and cannot be continued", in.taskID).
			WithMetadata("taskId", in.taskID).
			WithMetadata("detail", reasonTaskNotLive)
	}
	parked, ok := r.pending()
	if !ok {
		return nil, nil, a2a.Task{}, a2a.Errorf(a2a.ErrorTypeUnsupportedOperation,
			"task %q is not waiting for input; a message may only continue a task that reported %s",
			in.taskID, a2a.TaskStateInputRequired.String()).
			WithMetadata("taskId", in.taskID).
			WithMetadata("detail", reasonTaskNotAwaiting)
	}

	// Attached BEFORE anything moves, so the transitions this call causes are
	// delivered to this request rather than raced past it.
	sub, opening := r.attach(opts)

	// The task returns to WORKING BEFORE the answer reaches the bus, and the
	// ordering is load-bearing rather than tidy: the moment hitl.responded is
	// emitted the agent loop unblocks and may run to completion inside that same
	// dispatch, so a WORKING transition emitted afterwards would race a terminal
	// one and be dropped. Moving first makes the sequence the client sees —
	// INPUT_REQUIRED, WORKING, then whatever the turn does next — deterministic.
	if !r.resume(parked.requestID) {
		r.detach(sub)
		return nil, nil, a2a.Task{}, a2a.Errorf(a2a.ErrorTypeUnsupportedOperation,
			"task %q stopped waiting for input before this answer arrived", in.taskID).
			WithMetadata("taskId", in.taskID).
			WithMetadata("detail", reasonTaskNotAwaiting)
	}
	r.recordMessage(messageRef{MessageID: in.messageID, Role: a2a.RoleUser, Text: in.text})

	resp := events.HITLResponse{
		SchemaVersion: events.HITLResponseVersion,
		RequestID:     parked.requestID,
	}
	// A choices question is answered by echoing an option id. Anything else is
	// free text, which is what free_text and both accept — and what a
	// choices-only question will be told is invalid by the plugin that asked.
	if id, matched := matchChoice(parked.choices, in.text); matched {
		resp.ChoiceID = id
	} else {
		resp.FreeText = in.text
	}
	if err := p.bus.Emit("hitl.responded", resp); err != nil {
		p.logger.Warn("a2a could not route an answer to the bus", "task_id", in.taskID, "error", err)
	}

	p.logger.Debug("a2a task resumed",
		"task_id", r.taskID, "context_id", r.contextID, "hitl_request_id", parked.requestID)
	return r, sub, opening, nil
}

// matchChoice resolves an answer's text against the parked question's option
// ids, case-insensitively and ignoring surrounding whitespace. It reports the
// canonical id, so the requesting plugin sees the id it published rather than
// the client's spelling of it.
func matchChoice(choices []string, text string) (string, bool) {
	answer := strings.TrimSpace(text)
	for _, id := range choices {
		if strings.EqualFold(id, answer) {
			return id, true
		}
	}
	return "", false
}

// cancelTask settles one of the caller's tasks at TASK_STATE_CANCELED and
// returns the stored record as it now stands.
//
// Cancelling an ALREADY-TERMINAL task is refused with TaskNotCancelableError —
// the error the specification reserves for exactly this (section 3.3.2) — and
// nothing is written. A terminal state is final, so "cancel" on one is a
// well-defined client mistake rather than an instruction to rewrite history.
//
// The A2A task is canceled synchronously and the bus is told afterwards, in
// this order: the stream must not be able to carry a frame produced by the
// teardown AFTER the terminal frame that closes it. Concretely the run is
// settled first, which makes every later frame a no-op, and only then does the
// cancellation reach the agent loop:
//
//  1. hitl.cancel, if the task was parked on a question — otherwise the agent
//     would stay blocked on an answer that is never coming, and the process's
//     one agent loop would be wedged by the very operation meant to free it.
//  2. cancel.request, which is the control.cancel capability's own entry point.
//     That plugin owns turn cancellation for every transport; this one asks
//     rather than reaching into the agent, exactly as the TUI and browser do.
func (p *Plugin) cancelTask(caller nexusauth.Principal, taskID string) (a2a.Task, *a2a.Error) {
	owner := p.tasks.For(caller)
	rec, found, err := owner.Get(taskID)
	if err != nil {
		p.logger.Error("a2a task lookup failed", "task_id", taskID, "error", err)
		return a2a.Task{}, a2a.Errorf(a2a.ErrorTypeInternal, "the task could not be read")
	}
	if !found {
		return a2a.Task{}, a2a.ErrTaskNotFound(taskID)
	}
	if rec.Status.State.IsTerminal() {
		return a2a.Task{}, a2a.ErrTaskNotCancelable(taskID, rec.Status.State)
	}

	const reason = "the task was canceled by the client"
	if r := p.liveRun(taskID); r != nil {
		parked, wasParked := r.unpark("")
		// The bus is told only if THIS call settled the task. A cancel racing
		// the turn's own ending loses, and the loser must not retract a question
		// that was answered or interrupt a turn that already finished.
		if settled := r.cancel(reason); settled {
			if wasParked {
				_ = p.bus.Emit("hitl.cancel", events.HITLCancel{
					SchemaVersion: events.HITLCancelVersion,
					RequestID:     parked.requestID,
					Reason:        reason,
				})
			}
			_ = p.bus.Emit("cancel.request", events.CancelRequest{
				SchemaVersion: events.CancelRequestVersion,
				TurnID:        r.boundTurn(),
				Source:        cancelSource,
			})
			p.logger.Info("a2a task canceled", "task_id", taskID, "was_parked", wasParked)
		} else {
			p.logger.Debug("a2a cancel raced the task's own ending", "task_id", taskID)
		}
	} else {
		// Non-terminal with no run behind it. Settling it in the store is still
		// the right answer: the client asked for a terminal state and there is
		// nothing left to interrupt.
		if err := owner.RecordStatus(taskID, canceledStatus(taskID, rec.ContextID, reason)); err != nil {
			p.logger.Error("a2a task could not be canceled", "task_id", taskID, "error", err)
			return a2a.Task{}, a2a.Errorf(a2a.ErrorTypeInternal, "the task could not be canceled")
		}
	}

	// Re-read rather than render the run's snapshot: the store is the source of
	// truth for every other read of a task, and answering CancelTask from a
	// second place is how the two come to disagree.
	settled, found, err := owner.Get(taskID)
	if err != nil || !found {
		return a2a.Task{}, a2a.Errorf(a2a.ErrorTypeInternal, "the canceled task could not be read back")
	}
	return settled.Task(renderOptions{}), nil
}

// canceledStatus builds the CANCELED status a task is settled at, carrying the
// reason as its message.
func canceledStatus(taskID, contextID, reason string) a2a.TaskStatus {
	return a2a.NewTaskStatus(a2a.TaskStateCanceled).WithMessage(
		a2a.NewAgentMessage(newMessageID(), reason).InContext(contextID).ForTask(taskID))
}

// resolveContextLocked resolves an A2A contextId to this listener's Nexus
// session, WITHOUT binding it. The caller holds p.mu.
//
// It is a pure read of p.contextID: it reports the context the turn would run
// in, or the refusal that request has earned. startTurn commits the result by
// assigning p.contextID at the same instant it takes the active-task slot, so a
// request refused for any other reason — a task already in flight, most
// obviously — leaves no binding behind. A refused request that had claimed the
// context would have captured this process's only conversation for the life of
// its session while getting an error for its trouble.
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
func (p *Plugin) resolveContextLocked(requested string) (string, *a2a.Error) {
	requested = strings.TrimSpace(requested)
	if p.contextID == "" {
		// Nothing is bound yet, so whatever this request names is servable. A
		// client that named none is assigned one; the assignment only becomes
		// this process's context if the caller commits it.
		if requested == "" {
			return p.defaultContextID(), nil
		}
		return requested, nil
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

// handleHITLRequested parks the task at TASK_STATE_INPUT_REQUIRED when the
// agent asks a human something.
//
// This is the mapping the specification already defines and Nexus already has
// the machinery for: nexus.control.hitl owns ask_user, emits hitl.requested and
// routes hitl.responded, and this transport never calls it — it watches the bus
// and renders what it sees. The question travels on the status message, which
// is where section 3.1.1 puts it, and the task stays LIVE: any open stream stays
// open, and the client resumes by sending a message naming the same taskId.
func (p *Plugin) handleHITLRequested(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	req, ok := e.Payload.(events.HITLRequest)
	if !ok {
		return
	}
	in := parkedInput{requestID: req.ID}
	for _, c := range req.Choices {
		in.choices = append(in.choices, c.ID)
	}
	if !r.park(in, describeQuestion(req.Prompt, req.Choices), p.cfg.inputTimeout,
		func() { p.expireInput(r, req.ID) }) {
		return
	}
	p.logger.Debug("a2a task awaiting input",
		"task_id", r.taskID, "hitl_request_id", req.ID, "timeout", p.cfg.inputTimeout)
}

// handleHITLResponded returns a parked task to WORKING once the question it was
// waiting on is answered — by ANY route, not only by an A2A message. A TUI
// operator, the hitl registry's on-disk answer, or another IO transport all end
// up here, so the A2A view of the task tracks the agent rather than tracking
// only what A2A itself did.
//
// A cancelled response resumes too. The agent loop is unblocked either way, and
// the task is back at work — deciding what to do about a refused question is
// the agent's business, not the transport's. The one case that must NOT resume
// is a task already terminal, and that is handled structurally: a terminated run
// drops every frame.
func (p *Plugin) handleHITLResponded(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	resp, ok := e.Payload.(events.HITLResponse)
	if !ok {
		return
	}
	if r.resume(resp.RequestID) {
		p.logger.Debug("a2a task resumed after input",
			"task_id", r.taskID, "hitl_request_id", resp.RequestID, "cancelled", resp.Cancelled)
	}
}

// expireInput enforces the input deadline: an unanswered question must not pin
// this listener's single agent loop for ever.
//
// The task is driven to a REAL terminal state rather than the stream being
// hung up, which is what pkg/a2a's parked-stream contract asks of a serving
// layer: a state transition keeps the store, every attached subscriber and the
// client's own view in agreement, where a silent close is indistinguishable
// from a dropped connection. FAILED is the honest state — the work did not
// complete, and nobody canceled it.
//
// The task is settled BEFORE hitl.cancel reaches the bus, so the synthetic
// hitl.responded that follows finds a terminated run and cannot resurrect it.
func (p *Plugin) expireInput(r *run, requestID string) {
	if r.terminated() {
		return
	}
	if _, ok := r.unpark(requestID); !ok {
		// Answered, re-asked or ended in the moment the timer was firing.
		return
	}
	reason := fmt.Sprintf(
		"the agent asked for input and no answer arrived within %s, so the task was abandoned", p.cfg.inputTimeout)
	if !r.fail(reason) {
		return
	}
	p.logger.Warn("a2a task timed out awaiting input",
		"task_id", r.taskID, "hitl_request_id", requestID, "timeout", p.cfg.inputTimeout)
	_ = p.bus.Emit("hitl.cancel", events.HITLCancel{
		SchemaVersion: events.HITLCancelVersion,
		RequestID:     requestID,
		Reason:        reason,
	})
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

// handleThinking, handleToolInvoke, handleToolResult and the three subagent
// handlers below feed the parts of a turn A2A has no canonical field for.
//
// Two destinations, chosen by what the payload IS rather than by convenience:
// a tool RESULT is output and becomes an artifact (plus a live telemetry
// signal); everything else — reasoning, the decision to call a tool, delegated
// progress, token counts — is telemetry only, and rides the Nexus extension on a
// status update. See telemetry.go for why that split is the honest one.

func (p *Plugin) handleThinking(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	step, ok := e.Payload.(events.ThinkingStep)
	if !ok {
		return
	}
	r.onThinking(step)
}

func (p *Plugin) handleToolInvoke(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	call, ok := e.Payload.(events.ToolCall)
	if !ok {
		return
	}
	r.onToolCall(call)
}

func (p *Plugin) handleToolResult(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	res, ok := e.Payload.(events.ToolResult)
	if !ok {
		return
	}
	r.onToolResult(res)
}

func (p *Plugin) handleSubagentStarted(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	s, ok := e.Payload.(events.SubagentStarted)
	if !ok {
		return
	}
	r.onSubagent(func() a2a.NexusEvent { return subagentStartedEvent(r.taskID, r.contextID, s) })
}

func (p *Plugin) handleSubagentIteration(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	s, ok := e.Payload.(events.SubagentIteration)
	if !ok {
		return
	}
	r.onSubagent(func() a2a.NexusEvent { return subagentIterationEvent(r.taskID, r.contextID, s) })
}

func (p *Plugin) handleSubagentComplete(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	s, ok := e.Payload.(events.SubagentComplete)
	if !ok {
		return
	}
	r.onSubagent(func() a2a.NexusEvent { return subagentCompleteEvent(r.taskID, r.contextID, s) })
}

// handleLLMRequest records the output schema a turn was constrained to, so the
// response artifact can name it. It is a read of the request only; nothing about
// the request is changed or answered here.
func (p *Plugin) handleLLMRequest(e engine.Event[any]) {
	r := p.currentRun()
	if r == nil {
		return
	}
	req, ok := e.Payload.(events.LLMRequest)
	if !ok {
		return
	}
	r.onLLMRequest(req)
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

	textOnly := false
	if cfg := req.Configuration; cfg != nil {
		if cfg.TaskPushNotificationConfig != nil {
			return turnInput{}, a2a.Errorf(a2a.ErrorTypePushNotificationNotSupported,
				"this agent does not deliver push notifications; the agent card declares capabilities.pushNotifications as false")
		}
		// configuration.returnImmediately is now honoured rather than refused.
		// It was refused while a run's lifetime was bound to its request —
		// returning early would have released the active-task slot mid-turn and
		// frozen the task at SUBMITTED. A run now ends when its TASK ends, so an
		// early return is exactly what it claims to be: the task id, followed by
		// GetTask or SubscribeToTask to watch the rest.
		if protoErr := checkAcceptedOutputModes(cfg.AcceptedOutputModes); protoErr != nil {
			return turnInput{}, protoErr
		}
		textOnly = acceptsTextOnly(cfg.AcceptedOutputModes)
	}

	// A message naming a task is a CONTINUATION of that task (specification
	// section 3.4), not a new one. Whether it is a legal continuation depends on
	// state the store and the live run own, so it is decided by resumeTurn; all
	// that happens here is that the id is carried through rather than refused.
	return turnInput{
		contextID: msg.ContextID,
		text:      text,
		messageID: msg.MessageID,
		taskID:    strings.TrimSpace(msg.TaskID),
		textOnly:  textOnly,
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

// acceptsTextOnly reports that the client named output modes and none of them
// admits anything but text.
//
// This is the read side of acceptedOutputModes (specification section 3.2.2),
// and it is honoured rather than merely validated: a task whose client said
// "text/plain" publishes no application/json part and inlines no file contents,
// because posting base64 to a client that told us it cannot render it is
// answering a question it did not ask. Files are still REPORTED — as the same
// metadata note an oversized file gets — so the client learns they exist.
//
// An empty list means no restriction, which is the common case and the one that
// gets the full artifact set.
func acceptsTextOnly(modes []string) bool {
	if len(modes) == 0 {
		return false
	}
	for _, mode := range modes {
		switch normalized := strings.ToLower(strings.TrimSpace(mode)); {
		case normalized == "*/*", normalized == "*":
			return false
		case strings.HasPrefix(normalized, "text/"), normalized == "text":
		default:
			return false
		}
	}
	return true
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
// See resolveContextLocked for the reasoning.
func errForeignContext(requested, bound string) *a2a.Error {
	return a2a.Errorf(a2a.ErrorTypeUnsupportedOperation,
		"context %q is not served by this agent: it is bound to context %q for the life of its Nexus session, so run one instance per context",
		requested, bound).
		WithMetadata("detail", reasonForeignContext).
		WithMetadata("contextId", bound)
}
