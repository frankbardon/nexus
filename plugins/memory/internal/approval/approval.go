// Package approval is the shared human-in-the-loop wrapper used by the
// memory plugins (longterm, vector, compaction) that gain an opt-in
// require_approval config.
//
// Each plugin owns its own match logic + payload construction. This package
// only handles the bus dance: subscribe to hitl.responded, emit
// hitl.requested, block until the matching response (or timeout) arrives,
// then unsubscribe and return the typed response.
//
// Per the project rule "extract on the second real caller" — three real
// callers exist (one per memory plugin), so the helper is justified. It is
// deliberately small: no glob matching, no payload trimming, no choice
// list construction. Those concerns live with the caller because they
// vary per plugin.
package approval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// DefaultChoices is the canonical allow/reject pair memory approvals use.
// Callers may pass a custom slice if they want a third option (e.g. an
// "edit" choice), but in this PR every caller uses these two.
var DefaultChoices = []events.HITLChoice{
	{ID: "allow", Label: "Approve", Kind: events.ChoiceAllow},
	{ID: "reject", Label: "Reject", Kind: events.ChoiceReject},
}

// Request is the input to RequestApproval.
type Request struct {
	// Bus is the event bus used to emit hitl.requested and observe
	// hitl.responded. Required.
	Bus engine.EventBus
	// Logger is used for warn-level diagnostic messages (rejections,
	// edited-payload warnings, deadline expiry). Required.
	Logger *slog.Logger
	// PluginID is the dotted ID of the requesting plugin (e.g.
	// "nexus.memory.longterm"). Set as RequesterPlugin on the request.
	PluginID string
	// ActionKind is the dotted action discriminator (e.g.
	// "memory.longterm.write"). Set verbatim on the request.
	ActionKind string
	// ActionRef is the opaque payload describing the pending action;
	// surfaced verbatim to operators. Callers should truncate large
	// content fields before passing them in.
	ActionRef map[string]any
	// Prompt is the literal text shown to the operator. Required.
	Prompt string
	// Choices is the list of options. When nil, DefaultChoices is used.
	Choices []events.HITLChoice
	// DefaultChoiceID is the choice picked on deadline expiry. When
	// empty, no default is set; expiry resolves with Cancelled=true.
	DefaultChoiceID string
	// Timeout, when > 0, sets a Deadline on the request and aborts the
	// in-process wait if no response arrives in time.
	Timeout time.Duration
	// SessionID, when set, is forwarded as HITLRequest.SessionID for
	// out-of-band CLI tooling. Optional.
	SessionID string
}

// RequestApproval emits hitl.requested and blocks until a matching
// hitl.responded arrives, the timeout elapses, or ctx is cancelled.
//
// The boolean return is true when the response indicates "allow" (the
// choice's Kind == ChoiceAllow, or — when Kind is unset on a custom
// choice — the choice ID is "allow"). Rejected, cancelled, and timed-out
// requests return false. Callers decide what to do with the result.
//
// The error is non-nil only on internal failure (nil bus, emit failure
// returned by the bus). A timeout is not an error — it returns a
// Cancelled response with allowed=false.
func RequestApproval(ctx context.Context, req Request) (events.HITLResponse, bool, error) {
	if req.Bus == nil {
		return events.HITLResponse{}, false, errors.New("approval: bus is required")
	}
	logger := req.Logger
	if logger == nil {
		logger = slog.Default()
	}

	choices := req.Choices
	if len(choices) == 0 {
		choices = DefaultChoices
	}

	id, err := newRequestID()
	if err != nil {
		return events.HITLResponse{}, false, fmt.Errorf("approval: id generation: %w", err)
	}

	respCh := make(chan events.HITLResponse, 1)
	unsub := req.Bus.Subscribe("hitl.responded", func(ev engine.Event[any]) {
		resp, ok := toHITLResponse(ev.Payload)
		if !ok {
			return
		}
		if resp.RequestID != id {
			return
		}
		select {
		case respCh <- resp:
		default:
			// channel already has a response — drop the duplicate.
		}
	}, engine.WithPriority(50), engine.WithSource(req.PluginID))
	defer unsub()

	hitlReq := events.HITLRequest{
		ID:              id,
		SessionID:       req.SessionID,
		RequesterPlugin: req.PluginID,
		ActionKind:      req.ActionKind,
		ActionRef:       req.ActionRef,
		Mode:            events.HITLModeChoices,
		Choices:         choices,
		DefaultChoiceID: req.DefaultChoiceID,
		Prompt:          req.Prompt,
	}
	if req.Timeout > 0 {
		hitlReq.Deadline = time.Now().Add(req.Timeout)
	}

	if err := req.Bus.Emit("hitl.requested", hitlReq); err != nil {
		return events.HITLResponse{}, false, fmt.Errorf("approval: emit hitl.requested: %w", err)
	}

	var timer <-chan time.Time
	if req.Timeout > 0 {
		t := time.NewTimer(req.Timeout)
		defer t.Stop()
		timer = t.C
	}

	select {
	case resp := <-respCh:
		allowed := isAllow(resp, choices)
		if !allowed && resp.EditedPayload != nil {
			// Full edit semantics are out of scope for this PR. Surface a
			// warning so the behavior is observable and the operator
			// understands their edit was ignored.
			logger.Warn("approval: edited_payload ignored (edit flow not implemented)",
				"plugin", req.PluginID,
				"action_kind", req.ActionKind,
				"request_id", id,
			)
		}
		return resp, allowed, nil

	case <-timer:
		logger.Warn("approval: deadline expired",
			"plugin", req.PluginID,
			"action_kind", req.ActionKind,
			"request_id", id,
			"default_choice_id", req.DefaultChoiceID,
		)
		// Deadline expired without a response. Resolve to default choice
		// when one is set; otherwise treat as cancelled.
		resp := events.HITLResponse{
			RequestID:    id,
			ChoiceID:     req.DefaultChoiceID,
			Cancelled:    req.DefaultChoiceID == "",
			CancelReason: "deadline expired",
		}
		allowed := req.DefaultChoiceID != "" && choiceIDIsAllow(req.DefaultChoiceID, choices)
		return resp, allowed, nil

	case <-ctx.Done():
		logger.Warn("approval: context cancelled",
			"plugin", req.PluginID,
			"action_kind", req.ActionKind,
			"request_id", id,
			"err", ctx.Err(),
		)
		resp := events.HITLResponse{
			RequestID:    id,
			Cancelled:    true,
			CancelReason: "context cancelled",
		}
		return resp, false, nil
	}
}

// isAllow returns true when the response's chosen ID maps to a Kind ==
// ChoiceAllow (or, when Kind is unset, when the ID literally equals
// "allow"). Cancelled responses are never allowed.
func isAllow(resp events.HITLResponse, choices []events.HITLChoice) bool {
	if resp.Cancelled {
		return false
	}
	if resp.ChoiceID == "" {
		return false
	}
	return choiceIDIsAllow(resp.ChoiceID, choices)
}

func choiceIDIsAllow(id string, choices []events.HITLChoice) bool {
	for _, c := range choices {
		if c.ID != id {
			continue
		}
		if c.Kind == events.ChoiceAllow {
			return true
		}
		if c.Kind == "" && c.ID == "allow" {
			return true
		}
		return false
	}
	// ID does not reference any known choice — treat as not allowed so a
	// stray response cannot escalate to an approval.
	return false
}

// toHITLResponse mirrors the conversion in plugins/control/hitl. The bus
// may carry typed values (live emitter) or map[string]any (journal
// replay), and we want both paths to drop into the same select.
func toHITLResponse(payload any) (events.HITLResponse, bool) {
	switch v := payload.(type) {
	case events.HITLResponse:
		return v, true
	case *events.HITLResponse:
		if v == nil {
			return events.HITLResponse{}, false
		}
		return *v, true
	case map[string]any:
		resp := events.HITLResponse{}
		resp.RequestID, _ = v["request_id"].(string)
		resp.ChoiceID, _ = v["choice_id"].(string)
		resp.FreeText, _ = v["free_text"].(string)
		resp.Cancelled, _ = v["cancelled"].(bool)
		resp.CancelReason, _ = v["cancel_reason"].(string)
		if ep, ok := v["edited_payload"].(map[string]any); ok {
			resp.EditedPayload = ep
		}
		if resp.RequestID == "" {
			return resp, false
		}
		return resp, true
	default:
		return events.HITLResponse{}, false
	}
}

// newRequestID returns a stable, journal-friendly request ID.
func newRequestID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "memhitl-" + hex.EncodeToString(b[:]), nil
}
