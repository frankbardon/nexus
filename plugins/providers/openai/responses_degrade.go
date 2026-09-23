package openai

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// This file holds the two ways the Responses path is known to degrade, and it
// gives them deliberately opposite treatments because they cost deliberately
// opposite things.
//
//   - A replayed reasoning Item the API refuses to verify is a **correctness**
//     cliff. The next round would be answered with none of the model's own
//     reasoning behind it, and the answer changes — quietly, plausibly, and in
//     a way nothing downstream can see. So the request fails, naming the cause.
//     There is deliberately no retry-without-reasoning: it would turn a visible
//     failure into an invisible quality drop, which is the one outcome nobody
//     could act on.
//   - A `prediction` on this surface is a **latency** optimisation with no
//     counterpart here at all. Dropping it costs time and nothing else, so the
//     turn proceeds and the operator is told once.
//
// The asymmetry is the point: loud where the answer changes, quiet-but-stated
// where only the clock does.

// --- 1. the correctness cliff: a rejected reasoning replay -------------------

// reasoningReplayMarkers are the substrings that identify an API failure as the
// rejection of a *replayed reasoning Item*, rather than as any other bad
// request.
//
// Each marker names an Item — its id, its type, or the encrypted blob itself —
// and none of them is the bare word "reasoning", which also appears in the
// unrelated failures of the request's `reasoning` object (an effort value a
// particular model does not accept, say). Misattributing one of those to replay
// would send an operator hunting the wrong thing entirely.
//
// OpenAI publishes no stable error code for this, so the match is on text and
// is best-effort by construction. That is survivable precisely because of what
// the fallback is: a rejection this list does not recognise still **fails the
// request**, just with the raw API message instead of the explanation. Nothing
// is swallowed either way; the list only decides how good the message is.
var reasoningReplayMarkers = []string{
	"encrypted_content",
	"encrypted content",
	"of type 'reasoning'",
	`of type "reasoning"`,
	"reasoning item",
	"reasoning_item",
	"item 'rs_",
	`item "rs_`,
}

// isReasoningReplayRejection reports whether an API error names a replayed
// reasoning Item.
//
// All three fields are searched because the identifying detail lands in
// different ones on different failures: a validation error names the blob in
// `param` (`input[0].encrypted_content`), while a verification error names the
// Item in the prose.
func isReasoningReplayRejection(code, message, param string) bool {
	hay := strings.ToLower(code + "\x00" + message + "\x00" + param)
	for _, marker := range reasoningReplayMarkers {
		if strings.Contains(hay, marker) {
			return true
		}
	}
	return false
}

// reasoningReplayError builds the error a rejected replay fails the request
// with.
//
// It is long on purpose. This failure has two quite different causes with the
// same wire symptom, and the operator cannot tell them apart from the API's own
// message:
//
//   - the blob simply stopped verifying — reported in the field after three or
//     four tool rounds under `store: false`, which is the mode Nexus always
//     sends;
//   - the conversation moved between model families. Encrypted reasoning is
//     reusable only within one family, so a `fallback` or `fanout` chain naming
//     models from two of them replays Items the second model cannot verify.
//     Nexus does not detect that, because detecting it would mean shipping a
//     model-family table — so the message has to be good enough to stand in for
//     the detection.
func reasoningReplayError(code, message string) error {
	if code == "" {
		code = "invalid_request"
	}
	return fmt.Errorf("openai: the Responses API rejected a replayed reasoning item (%s): %s — "+
		"the turn is failed rather than retried without it, because continuing without the model's "+
		"own reasoning changes the answer rather than merely slowing it down. Two causes produce "+
		"this: an encrypted_content blob that stopped verifying after several tool rounds under "+
		"store: false, or a conversation that moved between model families (a fallback or fanout "+
		"chain naming models from more than one) — encrypted reasoning is reusable only within one "+
		"family, and this provider does not detect the swap. Start a fresh turn, or keep the role's "+
		"chain inside a single model family",
		code, message)
}

// responsesAPIErrorEnvelope is the body a non-200 carries on this surface:
// the failure object nested under `error`, the same shape the in-200
// `reply.Error` uses.
type responsesAPIErrorEnvelope struct {
	Error *responsesError `json:"error"`
}

// replayRejectionFromStatus classifies a non-200 Responses reply, returning the
// explanatory error when the status names a rejected reasoning replay and nil
// when it is any other failure.
//
// It is scoped to HTTP 400 because that is the status a request the server
// refuses to *accept* comes back as, which is what a bad replayed Item is. A
// 401, a 429 or a 5xx is about the deployment or the service and has nothing to
// do with the conversation's reasoning; classifying one of those as a replay
// rejection would be actively misleading.
//
// An undecodable body still gets scanned as raw text: a proxy that rewrites the
// envelope but keeps the message would otherwise lose the explanation, and
// scanning a string that happens to contain none of the markers simply returns
// nil.
func replayRejectionFromStatus(status int, body []byte) error {
	if status != http.StatusBadRequest {
		return nil
	}

	var env responsesAPIErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Error == nil {
		text := strings.TrimSpace(string(body))
		if isReasoningReplayRejection("", text, "") {
			return reasoningReplayError("", text)
		}
		return nil
	}

	if !isReasoningReplayRejection(env.Error.Code, env.Error.Message, env.Error.Param) {
		return nil
	}
	return reasoningReplayError(env.Error.Code, env.Error.Message)
}

// --- 2. the latency optimisation: a dropped prediction ----------------------

// warnPredictionDropped states, once per role, that Predicted Outputs does not
// exist on this surface.
//
// Predicted Outputs is a Chat Completions feature: the caller hands over the
// text it expects back (rewrite this paragraph, fix this line) and the model
// returns the unchanged portions near-instantly. The Responses API has no
// counterpart field, so the value is dropped rather than sent and rejected, and
// the turn proceeds — the caller loses the speed-up and nothing else.
//
// It warns rather than logging at DEBUG because a silently ignored request
// field is exactly the kind of thing an operator discovers as "why is this
// deployment suddenly slower" months later. It warns **once per role** rather
// than per request because it is a configuration fact, not a per-turn event:
// the agent that sets a prediction sets one on every turn, and a per-request
// warning would bury the log of a busy role. The dedupe is keyed by role for
// the same reason warnEffortClamped's is — two roles degrading is two facts.
func (p *Plugin) warnPredictionDropped(role string) {
	if _, seen := p.predictionDropWarned.LoadOrStore(role, struct{}{}); seen {
		return
	}
	logger := p.logger
	if logger == nil {
		logger = slog.Default()
	}
	attrs := []any{"api", string(apiResponses)}
	if role != "" {
		attrs = append(attrs, "role", role)
	}
	logger.Warn("openai: dropping prediction — Predicted Outputs is a Chat Completions feature with no counterpart on the Responses API; the turn proceeds without it, losing latency rather than correctness", attrs...)
}
