package a2aremote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/a2a/a2aclient"
	"github.com/frankbardon/nexus/pkg/engine"
)

// invocation is one tool call, parsed.
type invocation struct {
	task        string
	contextMap  map[string]any
	parentTurn  string
	parentDepth int
	// timeout is the per-call override from the tool arguments, zero when the
	// model did not supply one.
	timeout time.Duration
}

// budget is the resolved resource envelope for one call. A2A gives a client no
// say over the remote's token or tool-call spend — that is the remote's own
// business and its own budget — so the only dimension this plugin can actually
// enforce is wall-clock time, plus the depth cap that stops a delegation cycle.
// The posture's other budget fields are deliberately not silently ignored: see
// resolveBudget.
type budget struct {
	timeout  time.Duration
	maxDepth int
	// postureName is the posture the budget came from, empty when none applied.
	postureName string
	// postureVer is the posture's content hash, folded into the cache key so a
	// posture edit invalidates cached results the way it does for delegate.
	postureVer string
}

// runRemote executes one delegated call end to end and returns the outcome the
// tool result carries. It never returns a Go error: every failure is folded
// into outcome.err as a sentence the calling model can act on, because a
// delegating agent's next move depends on knowing WHY the remote did not
// answer, and an engine-level failure would deny it that.
func (p *Plugin) runRemote(parent context.Context, ra *remote, in invocation) outcome {
	spawnID := engine.GenerateID()[:16]
	depth := in.parentDepth + 1

	bud, err := p.resolveBudget(ra)
	if err != nil {
		return outcome{err: err.Error()}
	}
	if bud.maxDepth > 0 && depth > bud.maxDepth {
		return outcome{err: fmt.Sprintf(
			"delegation depth limit reached: this call would be depth %d and the limit is %d. "+
				"Answer from what you already have, or delegate from a shallower point.",
			depth, bud.maxDepth)}
	}

	p.logger.Debug("a2a_remote budget resolved",
		"agent", ra.cfg.name, "posture", bud.postureName,
		"timeout", bud.timeout, "max_depth", bud.maxDepth, "depth", depth)

	key := ra.cacheKey(in.task, in.contextMap) + "\x00" + bud.postureVer
	if p.cache != nil {
		if cached, ok := p.cache.get(key); ok {
			p.logger.Debug("a2a_remote cache hit", "agent", ra.cfg.name, "spawn_id", spawnID)
			// Still surface the started/complete pair so an observer sees the
			// call happened, exactly as delegate does for a cache hit.
			p.emitStarted(spawnID, ra, in)
			p.emitComplete(spawnID, in.parentTurn, cached)
			return cached
		}
	}

	if cc, ok := p.bus.(engine.CausationController); ok {
		// SessionID is carried forward by hand: a pushed frame REPLACES the
		// active one whole (see PushCausationContext), so omitting it would
		// blank the session on every event this remote call emits rather than
		// inherit it. The read is on this goroutine, which is the one that
		// pushes, so it sees the caller's frame.
		pop := cc.PushCausationContext(engine.CausationContext{
			SessionID: cc.CurrentCausationContext().SessionID,
			AgentID:   "a2a_remote/" + ra.cfg.name + "/" + spawnID,
			Depth:     depth,
		})
		defer pop()
	}

	p.emitStarted(spawnID, ra, in)

	timeout := bud.timeout
	if in.timeout > 0 {
		timeout = in.timeout
	}

	// The call always gets a cancellable context, even with no timeout: it is
	// the handle the local cancellation path pulls, and a call that could not be
	// cancelled would keep a remote working for a turn the user has abandoned.
	//
	// The budget's deadline is set here rather than around the message alone
	// because it must keep running while the task sits at INPUT_REQUIRED. A
	// remote waiting on a human is still work this session authorized, and
	// pausing the budget for the wait is how a question nobody answers pins the
	// session for ever.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	sess := newSession(p, ra, spawnID, in.parentTurn, cancel)
	p.trackSession(sess)
	defer p.untrackSession(sess)

	out := p.call(ctx, ra, sess, in, timeout)

	// A task this call is walking away from — because the budget ran out, the
	// stream broke, the human never answered or the local turn was cancelled —
	// is cancelled on the remote. Leaving it running would burn somebody else's
	// tokens on an answer nobody is left to read. It self-noops for a task that
	// already reached a terminal state.
	sess.abandon("the delegating call ended before the task did")

	if p.cache != nil && !out.failed() && !out.humanInput {
		p.cache.put(key, out)
	}
	p.emitComplete(spawnID, in.parentTurn, out)
	return out
}

// call performs discovery and then drives the exchange to a terminal state,
// looping through as many human answers as the remote asks for.
//
// The loop is the substance. A2A has no resume operation: a task parked at
// INPUT_REQUIRED is continued by an ORDINARY message carrying the same taskId
// and contextId (specification section 3.4), so each round here is a fresh send
// whose identity is what makes it a continuation. The accumulated result carries
// artifacts across rounds, because a resumed stream is a new connection and the
// remote is under no obligation to resend what it already delivered.
func (p *Plugin) call(ctx context.Context, ra *remote, s *session, in invocation, timeout time.Duration) outcome {
	// Discovery, lazily, on first use. A card that will not resolve is fatal to
	// THIS call and to nothing else: the plugin booted without it and the next
	// call will try again, since a2aclient caches a card only on success.
	if _, err := ra.resolveCard(ctx); err != nil {
		return outcome{err: p.describeFailure(ra, err, timeout)}
	}
	// The card may have arrived just now, in which case the tool description
	// the model sees is the operator's placeholder and the remote's own account
	// of itself is available. Publish it once.
	p.refreshDescription(ra)

	streaming := ra.streamingSupported()
	policy := ra.cfg.transport.hitl
	req := buildRequest(in)

	var acc remoteResult
	humanInput := false

	for round := 0; ; round++ {
		r, err := p.exchange(ctx, ra, s, req, streaming)
		acc = acc.merge(r)

		if err != nil {
			// A failed stream keeps the frames that DID arrive, so a task that
			// broke after producing partial output still hands that output to
			// the calling model alongside the reason it stopped. A blocking
			// send has no partial state to keep.
			var out outcome
			if streaming {
				out = classify(ra, acc)
			}
			out.err = p.describeFailure(ra, err, timeout)
			out.humanInput = humanInput
			return out
		}

		// Anything that is not a live task parked on a question is this call's
		// answer, for better or worse.
		if !policy.on() || acc.state != a2a.TaskStateInputRequired || acc.taskID == "" {
			out := classify(ra, acc)
			out.humanInput = humanInput
			return out
		}

		if max := policy.rounds(); max > 0 && round >= max {
			return exhaustedRoundsOutcome(ra.cfg.name, max, humanBase(ra, acc, humanInput))
		}

		answer := p.askHuman(ctx, s, acc.statusText, round)
		humanInput = true
		if answer.verdict != answerGiven {
			return parkedOutcome(ra.cfg.name, acc.statusText, answer, policy.wait(),
				humanBase(ra, acc, humanInput))
		}

		p.logger.Info("a2a_remote resuming a remote task with the human's answer",
			"agent", ra.cfg.name, "spawn_id", s.spawnID,
			"task_id", acc.taskID, "context_id", acc.contextID, "round", round+1)

		req = a2aclient.ResumeText(acc.taskID, acc.contextID,
			"nexus-"+engine.GenerateID()[:16], answer.text)
	}
}

// humanBase renders the partial answer an abandoned delegation still carries,
// with the interruption's own error text left off — the caller supplies a more
// specific one.
func humanBase(ra *remote, r remoteResult, humanInput bool) outcome {
	out := rawOutcome(ra, r)
	out.humanInput = humanInput
	return out
}

// exchange performs one send and returns what the remote answered with.
//
// The streaming arm reads the frames itself rather than calling a2aclient.Run,
// which drains them internally: every frame is handed to the session on its way
// past so a long delegation reports progress on the local bus instead of going
// silent for minutes. The accumulated result is otherwise identical to Run's.
//
// # Why it stops on an interruption instead of draining
//
// A2A leaves it to the server whether an INPUT_REQUIRED park closes the SSE
// stream, and both readings are legal. A server that HOLDS the stream open —
// which is exactly what nexus.io.a2a does, deliberately, with keep-alive
// comments and no terminal frame — will send nothing more until the task is
// resumed, and a task is resumed by a NEW message carrying the same taskId
// (specification section 3.4), never on the connection the question arrived on.
// So a reader that waits for the stream to end is waiting for a remote that is
// waiting for it: the question never reaches a human and the delegation dies on
// the call budget or the idle timeout.
//
// The interruption frame IS the end of this exchange. Once it is seen the
// stream is abandoned — the caller-initiated close is a clean stop here, not a
// cancellation, so stream.Err() is deliberately not consulted on this path —
// and the accumulated result carries everything the remote sent before parking,
// which is what the human-in-the-loop round in call needs.
//
// The one frame that does NOT mean "just parked" is the OPENING frame of a
// resumption: a server answers a resuming message with a snapshot of the task
// as it stands, which is the very interruption being answered. Acting on that
// would re-ask the human the question they have just answered, so it is skipped
// and only a LATER interruption — a genuine second question — stops the read.
func (p *Plugin) exchange(ctx context.Context, ra *remote, s *session, req a2a.SendMessageRequest, streaming bool) (remoteResult, error) {
	if !streaming {
		resp, err := ra.client.SendMessage(ctx, req)
		if err != nil {
			return remoteResult{}, err
		}
		r := fromSendMessage(resp)
		s.noteTask(r.taskID, r.contextID)
		s.noteState(r.state)
		return r, nil
	}

	stream, err := ra.client.SendStreamingMessage(ctx, req)
	if err != nil {
		return remoteResult{}, err
	}
	defer stream.Close()

	// A message naming a task is a continuation, so its opening snapshot is the
	// interruption it continues rather than a new one.
	resuming := strings.TrimSpace(req.Message.TaskID) != ""
	opening := true

	for frame := range stream.Frames() {
		parked := s.observe(frame)
		first := opening
		opening = false

		if !parked || (first && resuming) {
			continue
		}
		return fromStream(stream.Result()), nil
	}
	return fromStream(stream.Result()), stream.Err()
}

// classify turns a successfully-transported answer into an outcome. A clean
// transport does not mean a successful task: FAILED, REJECTED, CANCELED and
// INPUT_REQUIRED are all answers a stream delivers without failing.
func classify(ra *remote, r remoteResult) outcome {
	out := rawOutcome(ra, r)

	switch {
	case r.state == a2a.TaskStateFailed, r.state == a2a.TaskStateRejected:
		out.err = fmt.Sprintf("the remote A2A agent %q ended its task in state %s",
			ra.cfg.name, string(r.state))
		if detail := strings.TrimSpace(r.statusText); detail != "" {
			out.err += ": " + oneLine(detail)
		}

	case r.state == a2a.TaskStateCanceled:
		out.err = fmt.Sprintf("the remote A2A agent %q cancelled its task", ra.cfg.name)

	case r.state == a2a.TaskStateAuthRequired:
		// AUTH_REQUIRED is NOT routed to a human, unlike INPUT_REQUIRED. The
		// remote is asking for a credential, and no answer a person types into a
		// chat is one: the fix is a credentials block in this instance's
		// configuration. Saying so beats prompting somebody for a token.
		out.err = fmt.Sprintf(
			"the remote A2A agent %q paused in state %s: it needs credentials this instance did not present",
			ra.cfg.name, string(r.state))
		if q := strings.TrimSpace(r.statusText); q != "" {
			out.err += " (" + oneLine(q) + ")"
		}
		out.err += ". An operator must configure this agent's credentials; retrying will not fix it."

	case r.state.IsInterrupted():
		// Reached only when chained human-in-the-loop is switched off for this
		// remote. With it on, an INPUT_REQUIRED task never gets here — the
		// question goes to a person and the task is resumed. See hitl.go.
		out.err = fmt.Sprintf(
			"the remote A2A agent %q paused in state %s and is waiting for input",
			ra.cfg.name, string(r.state))
		if q := strings.TrimSpace(r.statusText); q != "" {
			out.err += ": " + oneLine(q)
		}
		out.err += ". Re-delegate with the answer included in the task."
	}
	return out
}

// rawOutcome folds a remote result into the tool output without judging it. It
// is classify's first half, split out so the human-in-the-loop paths can supply
// their own verdict over the same folded document.
func rawOutcome(ra *remote, r remoteResult) outcome {
	return outcome{
		output:    fold(ra.cfg.name, r),
		taskID:    r.taskID,
		contextID: r.contextID,
		state:     r.state,
	}
}

// describeFailure renders a transport or protocol failure as a sentence the
// calling model can act on.
//
// The taxonomy is a2aclient's, and it is worth preserving rather than
// collapsing: "the remote's card is unreachable" and "the remote refused the
// request" call for different next moves from the delegating agent, and a
// single "remote a2a error: %v" would hide that distinction behind a Go error
// string it has to parse.
func (p *Plugin) describeFailure(ra *remote, err error, timeout time.Duration) string {
	name := ra.cfg.name
	prefix := fmt.Sprintf("delegation to the remote A2A agent %q failed", name)

	var cardErr *a2aclient.CardError
	if errors.As(err, &cardErr) {
		where := cardErr.URL
		if where == "" {
			where = ra.client.CardURL()
		}
		switch cardErr.Stage {
		case "fetch", "status":
			return fmt.Sprintf("%s: the agent is unreachable — its agent card at %s could not be fetched (%s). "+
				"The remote may be down; try again later or proceed without it.",
				prefix, where, oneLine(causeText(cardErr.Err)))
		default:
			return fmt.Sprintf("%s: the agent card at %s is not usable (%s: %s). "+
				"This is a misconfiguration on the remote, not something retrying will fix.",
				prefix, where, cardErr.Stage, oneLine(causeText(cardErr.Err)))
		}
	}

	var bindingErr *a2aclient.BindingError
	if errors.As(err, &bindingErr) {
		return fmt.Sprintf("%s: the agent does not expose the %s binding this instance is configured for (%s).",
			prefix, bindingErr.Binding, oneLine(bindingErr.Detail))
	}

	var streamErr *a2aclient.StreamError
	if errors.As(err, &streamErr) {
		switch streamErr.Reason {
		case a2aclient.StreamReasonIdle:
			return fmt.Sprintf("%s: the agent went silent mid-run and the stream timed out after %d frames. "+
				"Any output above is partial.", prefix, streamErr.Frames)
		case a2aclient.StreamReasonOpenTimeout:
			return fmt.Sprintf("%s: the agent did not accept the streaming request in time. "+
				"The remote may be overloaded; try again.", prefix)
		case a2aclient.StreamReasonTruncated:
			return fmt.Sprintf("%s: the agent closed the stream after %d frames without finishing its task. "+
				"Any output above is partial.", prefix, streamErr.Frames)
		case a2aclient.StreamReasonCanceled:
			if deadline := deadlineNote(err, timeout); deadline != "" {
				return prefix + ": " + deadline
			}
			return fmt.Sprintf("%s: the call was cancelled after %d frames.", prefix, streamErr.Frames)
		case a2aclient.StreamReasonMalformed, a2aclient.StreamReasonProtocol, a2aclient.StreamReasonNotSSE:
			return fmt.Sprintf("%s: the agent sent a response this client cannot read (%s: %s). "+
				"This is a defect in the remote, not something retrying will fix.",
				prefix, string(streamErr.Reason), oneLine(streamErr.Detail))
		}
		// Fall through to the protocol/transport cases below, which describe
		// the wrapped cause more precisely than the stream wrapper does.
		err = errors.Unwrap(streamErr)
		if err == nil {
			return fmt.Sprintf("%s: %s", prefix, oneLine(streamErr.Detail))
		}
	}

	var protoErr *a2a.Error
	if errors.As(err, &protoErr) {
		return fmt.Sprintf("%s: the agent refused the request (%s): %s",
			prefix, string(protoErr.Type), oneLine(protoErr.Message))
	}

	var httpErr *a2aclient.HTTPError
	if errors.As(err, &httpErr) {
		return fmt.Sprintf("%s: the agent answered HTTP %d. %s",
			prefix, httpErr.StatusCode, httpAdvice(httpErr.StatusCode))
	}

	if deadline := deadlineNote(err, timeout); deadline != "" {
		return prefix + ": " + deadline
	}
	return fmt.Sprintf("%s: %s", prefix, oneLine(causeText(err)))
}

// deadlineNote renders a deadline or cancellation, naming the budget that
// produced it so an operator reading the transcript can tell a too-tight
// timeout from a genuinely slow remote.
func deadlineNote(err error, timeout time.Duration) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		if timeout > 0 {
			return fmt.Sprintf("the agent did not finish within the %s budget for this call. "+
				"Any output above is partial.", timeout)
		}
		return "the agent did not finish before the call's deadline. Any output above is partial."
	case errors.Is(err, context.Canceled):
		return "the call was cancelled before the agent finished."
	}
	return ""
}

// httpAdvice turns a status code into the next move.
func httpAdvice(status int) string {
	switch {
	case status == 401 || status == 403:
		return "The credentials this instance presents are not accepted; an operator must fix the configuration."
	case status == 404:
		return "The configured endpoint does not exist; an operator must fix the configuration."
	case status == 429:
		return "The agent is rate limiting; try again later."
	case status >= 500:
		return "The agent is failing on its side; try again later."
	default:
		return "The agent rejected the request."
	}
}

// causeText renders an error, tolerating nil.
func causeText(err error) string {
	if err == nil {
		return "no further detail"
	}
	return err.Error()
}

// oneLine collapses whitespace so a multi-line remote detail does not break the
// single-sentence shape of a tool error.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ---- Budgets ----

// resolveBudget resolves the posture-driven envelope for a remote.
//
// A posture is the delegate family's budget authority, and naming one here
// means the same YAML that bounds a local sub-agent bounds a remote one. Only
// two of its dimensions can be honored across an A2A boundary — the timeout and
// the recursion depth — because the protocol gives a client no control over the
// remote's token or tool-call spend. Rather than accept a budget it cannot
// enforce, this refuses a posture whose budget sets one of those: silently
// ignoring `max_tokens` on a posture an operator wrote to cap cost would be a
// worse failure than saying so.
func (p *Plugin) resolveBudget(ra *remote) (budget, error) {
	bud := budget{
		timeout:  ra.cfg.transport.callTimeout(),
		maxDepth: p.cfg.maxDepth,
	}
	if bud.timeout == 0 {
		bud.timeout = defaultCallTimeout
	}

	name := ra.cfg.posture
	if name == "" {
		return bud, nil
	}
	if p.postures == nil {
		return bud, fmt.Errorf(
			"remote A2A agent %q names posture %q but no posture registry is active; "+
				"activate nexus.agent.postures or remove the posture from the agent's configuration",
			ra.cfg.name, name)
	}

	post, err := p.postures.Get(name)
	if err != nil {
		return bud, fmt.Errorf("remote A2A agent %q names posture %q, which is not registered: %w",
			ra.cfg.name, name, err)
	}
	if post.DefaultBudget.MaxTokens > 0 || post.DefaultBudget.MaxToolCalls > 0 {
		return bud, fmt.Errorf(
			"posture %q sets max_tokens or max_tool_calls, which cannot be enforced on a remote A2A agent "+
				"(the remote runs its own loop under its own budget); use a posture that bounds only timeout "+
				"for agent %q", name, ra.cfg.name)
	}

	bud.postureName = post.Name
	bud.postureVer = post.Version
	if post.DefaultBudget.Timeout > 0 {
		bud.timeout = post.DefaultBudget.Timeout
	}
	if post.MaxRecursionDepth > 0 && (bud.maxDepth == 0 || post.MaxRecursionDepth < bud.maxDepth) {
		bud.maxDepth = post.MaxRecursionDepth
	}
	return bud, nil
}

// ---- Request construction ----

// buildRequest assembles the A2A message carrying the delegated task.
//
// The task and the structured context are separated by XML tag boundaries
// rather than concatenated, per the house convention: the remote agent is an
// LLM too, and an undelimited blob of JSON followed by an instruction is
// exactly the ambiguity the convention exists to prevent.
func buildRequest(in invocation) a2a.SendMessageRequest {
	content := in.task
	if len(in.contextMap) > 0 {
		if encoded, err := json.MarshalIndent(in.contextMap, "", "  "); err == nil {
			var b strings.Builder
			engine.XMLTag(&b, "delegate_context")
			b.WriteString(engine.XMLCDATA(string(encoded)))
			b.WriteByte('\n')
			engine.XMLClose(&b, "delegate_context")
			b.WriteByte('\n')
			engine.XMLTag(&b, "task")
			b.WriteString(engine.XMLCDATA(in.task))
			b.WriteByte('\n')
			engine.XMLClose(&b, "task")
			content = b.String()
		}
	}
	return a2a.SendMessageRequest{
		Message: a2a.NewUserMessage("nexus-"+engine.GenerateID()[:16], content),
	}
}
