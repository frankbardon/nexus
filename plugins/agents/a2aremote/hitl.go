package a2aremote

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// Chained human-in-the-loop.
//
// A2A parks a task at TASK_STATE_INPUT_REQUIRED when the remote agent needs
// something only its caller can supply, and puts the question on the status
// message (specification section 3.1.1). The task stays LIVE: it is resumed by
// sending an ordinary message carrying the SAME taskId and contextId (section
// 3.4), which is what makes the continuation a continuation rather than a new
// conversation.
//
// # Who answers, and who must not
//
// The question goes to the HUMAN driving the local session, through the same
// hitl.requested / hitl.responded pair that backs ask_user. It does NOT go to
// the delegating model. That is the whole point: a question a remote agent
// cannot answer for itself is, nearly always, one only a person can settle —
// which deployment, which fiscal year, whose budget — and handing it to the
// model that asked for the delegation invites it to invent an answer and then
// act on it. Nothing in this file gives the model a path to supply one, and the
// contract test asserts that this plugin never emits hitl.responded.
//
// nexus.control.hitl is reached ONLY over the bus. This plugin emits
// before:hitl.requested (vetoable, so a gate or a prompt synthesizer can rewrite
// or refuse the question) and then hitl.requested, exactly as the approval gates
// and the memory plugins do; it subscribes to hitl.responded to hear the answer.
// There is no direct call in either direction, and control/hitl need not be
// active for the mechanism to work — any transport that renders hitl.requested
// and answers with hitl.responded serves.
//
// # Transitivity
//
// The mapping composes. A Nexus instance serving over nexus.io.a2a turns its own
// hitl.requested into an INPUT_REQUIRED status; this file turns an inbound
// INPUT_REQUIRED into a local hitl.requested. Chain two of them and a question
// raised two hops down arrives in front of the human at the top, each hop
// resuming its own task with its own taskId when the answer comes back.
//
// # Deadlines
//
// Two run concurrently and the earlier one wins:
//
//   - The whole-call budget. It keeps running while the task is parked, because
//     a remote waiting on a human is still work this session authorized. This is
//     what stops an unanswered question pinning the session.
//   - hitl.input_timeout, the dedicated bound on ONE question (default 15m, "0s"
//     removes it). It is the outbound twin of nexus.io.a2a's tasks.input_timeout.
//
// With the default 5m call timeout the CALL budget expires first; an operator
// who expects a remote to ask questions raises `timeout` too. On either expiry
// the pending question is retracted with hitl.cancel, the remote task is
// cancelled with CancelTask so nobody is left working for a caller that has gone
// away, and the delegation ends as a clean tool error naming which deadline
// fired.

// answerVerdict is how a question in front of a human ended.
type answerVerdict int

const (
	// answerGiven means a human supplied text to resume the remote task with.
	answerGiven answerVerdict = iota
	// answerDeclined means a human (or a gate) refused the question.
	answerDeclined
	// answerExpired means hitl.input_timeout elapsed with no answer.
	answerExpired
	// answerAborted means the call's own context ended first: the whole-call
	// budget ran out, or the local turn was cancelled.
	answerAborted
)

// humanAnswer is the outcome of putting one remote question to a human.
type humanAnswer struct {
	verdict answerVerdict
	// text is the answer to resume the remote task with, set only when the
	// verdict is answerGiven.
	text string
	// reason is the human's or the gate's stated reason for declining, empty
	// when none was given.
	reason string
}

// askHuman raises one remote question on the local bus and waits for the answer.
//
// It never returns a Go error: every way this can end is a verdict the caller
// turns into either a resumption or a tool error.
func (p *Plugin) askHuman(ctx context.Context, s *session, question string, round int) humanAnswer {
	policy := s.ra.cfg.transport.hitl
	wait := policy.wait()

	taskID, contextID := s.task()
	requestID := fmt.Sprintf("a2a-%s-%s-%d", s.ra.cfg.toolName, s.spawnID, round)

	req := events.HITLRequest{
		SchemaVersion:   events.HITLRequestVersion,
		ID:              requestID,
		TurnID:          s.parentTurn,
		RequesterPlugin: pluginID,
		ActionKind:      hitlActionKind,
		Mode:            events.HITLModeFreeText,
		Prompt:          hitlPrompt(s.ra.cfg.name, question),
		ActionRef: map[string]any{
			"agent":      s.ra.cfg.name,
			"tool":       s.ra.cfg.toolName,
			"task_id":    taskID,
			"context_id": contextID,
			"round":      round + 1,
			"question":   question,
		},
	}
	if deadline := effectiveDeadline(ctx, wait); !deadline.IsZero() {
		req.Deadline = deadline
	}

	ch := make(chan events.HITLResponse, 1)
	p.registerHITL(requestID, ch)
	defer p.releaseHITL(requestID)
	s.setHITL(requestID)
	defer s.setHITL("")

	// The vetoable pre-emit first, so a gate or the prompt synthesizer sees the
	// question and can rewrite or refuse it before any transport renders it.
	// This is the same Option B shape nexus.control.hitl uses for ask_user.
	if veto, err := p.bus.EmitVetoable("before:hitl.requested", &req); err == nil && veto.Vetoed {
		reason := veto.Reason
		if reason == "" {
			reason = "a before:hitl.requested handler refused the question"
		}
		return humanAnswer{verdict: answerDeclined, reason: reason}
	}
	_ = p.bus.Emit("hitl.requested", req)

	p.logger.Info("a2a_remote asked the human a remote agent's question",
		"agent", s.ra.cfg.name, "spawn_id", s.spawnID,
		"hitl_request_id", requestID, "task_id", taskID,
		"round", round+1, "input_timeout", wait)

	var expiry <-chan time.Time
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		expiry = timer.C
	}

	select {
	case resp := <-ch:
		if resp.Cancelled {
			return humanAnswer{verdict: answerDeclined, reason: resp.CancelReason}
		}
		text := strings.TrimSpace(resp.FreeText)
		if text == "" {
			text = strings.TrimSpace(resp.ChoiceID)
		}
		if text == "" {
			return humanAnswer{verdict: answerDeclined,
				reason: "the answer carried no text"}
		}
		return humanAnswer{verdict: answerGiven, text: text}

	case <-expiry:
		p.retract(requestID, fmt.Sprintf(
			"no answer arrived within %s, so the delegation to %q was abandoned", wait, s.ra.cfg.name))
		return humanAnswer{verdict: answerExpired}

	case <-ctx.Done():
		p.retract(requestID, fmt.Sprintf(
			"the delegation to %q ended before the question was answered", s.ra.cfg.name))
		return humanAnswer{verdict: answerAborted}
	}
}

// hitlActionKind discriminates this request for renderers and prompt
// synthesizers. It is a plugin-defined extension of the ActionKind vocabulary,
// which control/hitl passes through verbatim.
const hitlActionKind = "a2a.input_required"

// hitlPrompt renders the question for a person.
//
// It is prose rather than XML-tagged: this text is shown to a HUMAN by an IO
// transport, not injected into a model's prompt, and the house XML convention
// exists for the latter. The remote's own words are quoted and attributed, so a
// person can tell what the remote asked from what Nexus is telling them about it.
func hitlPrompt(agentName, question string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The remote agent %q needs an answer before it can finish the task it was delegated.\n\n", agentName)
	if q := strings.TrimSpace(question); q != "" {
		b.WriteString(q)
	} else {
		b.WriteString("It did not say what it needs, only that it is waiting for input.")
	}
	return b.String()
}

// effectiveDeadline is the wall-clock moment the question stops mattering:
// whichever of the call's own deadline and the input timeout comes first. It is
// published on the request so an out-of-band answerer — the hitl registry's
// on-disk queue, say — can see there is no point answering afterwards.
func effectiveDeadline(ctx context.Context, wait time.Duration) time.Time {
	var out time.Time
	if wait > 0 {
		out = time.Now().Add(wait)
	}
	if callDeadline, ok := ctx.Deadline(); ok {
		if out.IsZero() || callDeadline.Before(out) {
			out = callDeadline
		}
	}
	return out
}

// retract withdraws a question nobody is going to answer.
//
// hitl.cancel is the registry's own retraction signal: it removes any persisted
// request file and emits a synthetic hitl.responded so anything else waiting on
// the same id unblocks. Emitting it here rather than simply walking away is what
// stops a stale prompt sitting in a TUI or an on-disk queue after the delegation
// that raised it has ended.
func (p *Plugin) retract(requestID, reason string) {
	_ = p.bus.Emit("hitl.cancel", events.HITLCancel{
		SchemaVersion: events.HITLCancelVersion,
		RequestID:     requestID,
		Reason:        reason,
	})
}

// registerHITL records a channel awaiting one request id.
func (p *Plugin) registerHITL(requestID string, ch chan events.HITLResponse) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pendingHITL[requestID] = ch
}

// releaseHITL forgets a request id.
func (p *Plugin) releaseHITL(requestID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pendingHITL, requestID)
}

// onHITLResponded routes an answer to the call waiting on it.
//
// It accepts an answer from ANY source — a TUI operator, the browser, the hitl
// registry's on-disk queue, another transport entirely — because who rendered
// the question is not this plugin's business. An answer naming an unknown
// request is dropped silently: it is either a duplicate the first delivery
// already satisfied, or a response to somebody else's question.
func (p *Plugin) onHITLResponded(ev engine.Event[any]) {
	resp, ok := ev.Payload.(events.HITLResponse)
	if !ok {
		return
	}
	if resp.RequestID == "" {
		return
	}
	p.mu.Lock()
	ch, waiting := p.pendingHITL[resp.RequestID]
	p.mu.Unlock()
	if !waiting {
		return
	}
	select {
	case ch <- resp:
	default:
		// Already answered; first response wins, as it does in control/hitl.
	}
}

// ---- Interrupted-task outcomes ----

// parkedOutcome renders a delegation that ended while the remote was still
// waiting on a human.
//
// Each variant deliberately tells the calling model NOT to answer the question
// itself. A model handed "the human did not answer, here is the question" will
// cheerfully supply one, and an invented answer to a question that was routed to
// a person precisely because it needed a person is worse than no answer at all —
// it is a wrong answer wearing a human's authority.
func parkedOutcome(agentName, question string, answer humanAnswer, wait time.Duration, base outcome) outcome {
	out := base
	question = oneLine(strings.TrimSpace(question))

	switch answer.verdict {
	case answerDeclined:
		out.err = fmt.Sprintf("the remote A2A agent %q asked a question and it was declined", agentName)
		if answer.reason != "" {
			out.err += ": " + oneLine(answer.reason)
		}
	case answerExpired:
		out.err = fmt.Sprintf(
			"the remote A2A agent %q asked a question and no answer arrived within %s", agentName, wait)
	default:
		out.err = fmt.Sprintf(
			"the remote A2A agent %q asked a question and the call ended before it was answered", agentName)
	}
	if question != "" {
		out.err += ". The question was: " + question
	}
	out.err += ". It was put to a human and is unanswered — do NOT answer it on their behalf. " +
		"Report that the delegation could not complete, or ask the user directly."
	return out
}

// exhaustedRoundsOutcome renders a remote that kept asking.
func exhaustedRoundsOutcome(agentName string, rounds int, base outcome) outcome {
	out := base
	out.err = fmt.Sprintf(
		"the remote A2A agent %q asked for input %d times in one delegation, which is the configured limit. "+
			"The task was abandoned rather than continuing to interrogate the user. "+
			"Try a more specific task, or raise hitl.max_rounds for this agent.",
		agentName, rounds)
	return out
}
