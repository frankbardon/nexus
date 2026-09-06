package engine

import (
	"fmt"
	"strings"

	"github.com/frankbardon/nexus/pkg/events"
)

// reservedLabelPrefix marks a SessionMeta.Labels key as host-only. Any key
// beginning with it can only ever be written through
// SessionWorkspace.SetReservedLabel / DeleteReservedLabel — never through the
// general before:session.tag.set / before:session.tag.delete bus path. See
// IsReservedLabelKey.
const reservedLabelPrefix = "_"

// IsReservedLabelKey reports whether key belongs to the reserved (host-only)
// session-label namespace: any key starting with "_". Exported so every
// other consumer that needs the same rule — ICM's OperatorTemplateCtx.Context
// projection, react/orchestrator/subagent's <session_context> prompt
// builders, and the opt-in nexus.tool.session_tags plugin — shares this one
// definition rather than each re-implementing the prefix check and risking
// the two drifting apart.
func IsReservedLabelKey(key string) bool {
	return strings.HasPrefix(key, reservedLabelPrefix)
}

// SetReservedLabel writes a reserved-namespace ("_"-prefixed) session label
// directly, bypassing the general before:session.tag.set bus path entirely.
//
// # This is the only sanctioned write path for a reserved key
//
// installSessionTagHandlers's before:session.tag.set handler rejects every
// reserved key unconditionally, with no veto exception and no caller-trust
// distinction — that is the one enforcement point protecting the reserved
// namespace from an ordinary plugin writing (or overwriting) something like
// `_principal_id` through the general request path. This method is the other
// side of that wall: the identity layer (nexus.io.agui today; a future
// transport tomorrow) calls it directly as a Go method, never through the
// bus, precisely because the bus path is barred to it. Calling this with a
// non-reserved key is rejected too — defensively, since this method bypasses
// the general validation completely and a caller mistake here would
// otherwise write an uncontrolled key with none of the vetoable path's
// checks.
//
// Ends by calling the same internal apply-and-announce step the general path
// uses, so session.tag.set fires for a reserved write exactly as it does for
// a general one — one announce channel for everything watching session
// labels, regardless of which path produced the write.
func (s *SessionWorkspace) SetReservedLabel(key, value string) error {
	if !IsReservedLabelKey(key) {
		return fmt.Errorf("session labels: SetReservedLabel called with non-reserved key %q (must start with %q)", key, reservedLabelPrefix)
	}
	return s.applyLabel(key, value, false)
}

// DeleteReservedLabel removes a reserved-namespace ("_"-prefixed) session
// label directly. See SetReservedLabel's doc comment — same sanctioned-path
// reasoning, same defensive rejection of a non-reserved key, same shared
// apply-and-announce step.
func (s *SessionWorkspace) DeleteReservedLabel(key string) error {
	if !IsReservedLabelKey(key) {
		return fmt.Errorf("session labels: DeleteReservedLabel called with non-reserved key %q (must start with %q)", key, reservedLabelPrefix)
	}
	return s.applyLabel(key, "", true)
}

// applyLabel is the single internal apply-and-announce step for every
// session-label write, general or reserved. Both installSessionTagHandlers
// (after it has already rejected a reserved key on the general path) and
// SetReservedLabel/DeleteReservedLabel (after validating the key actually is
// reserved) call this — never SaveMeta directly — so there is exactly one
// place that mutates Labels and exactly one announce event for anything
// downstream watching session labels to subscribe to.
func (s *SessionWorkspace) applyLabel(key, value string, deleted bool) error {
	meta, err := s.SessionMetadata()
	if err != nil {
		return fmt.Errorf("reading session metadata: %w", err)
	}
	if meta.Labels == nil {
		meta.Labels = make(map[string]string)
	}
	if deleted {
		delete(meta.Labels, key)
	} else {
		meta.Labels[key] = value
	}
	if err := s.SaveMeta(meta); err != nil {
		return fmt.Errorf("saving session metadata: %w", err)
	}

	if s.bus == nil {
		return nil
	}
	if deleted {
		_ = s.bus.Emit("session.tag.deleted", events.SessionTagDeleted{
			SchemaVersion: events.SessionTagDeletedVersion,
			SessionID:     s.ID,
			Key:           key,
		})
		return nil
	}
	_ = s.bus.Emit("session.tag.set", events.SessionTagSet{
		SchemaVersion: events.SessionTagSetVersion,
		SessionID:     s.ID,
		Key:           key,
		Value:         value,
	})
	return nil
}

// installSessionTagHandlers wires the general-namespace session-tag write
// path: before:session.tag.set and before:session.tag.delete. Called once
// from Boot, unsubscribed by Stop along with every other run-scoped
// subscription in e.runUnsubs.
//
// # The one enforcement point
//
// Every reserved ("_"-prefixed) Key is rejected here unconditionally — no
// veto exception, no caller-trust distinction between one emitting plugin and
// another. This is deliberate and is the ONLY place that rule is enforced:
// nothing downstream re-checks it, and nothing upstream is trusted to have
// already checked it. A reserved key reaching this handler is vetoed and
// Labels is never touched, regardless of who emitted the request or why. The
// reserved namespace's only legitimate writer is SetReservedLabel /
// DeleteReservedLabel, a direct Go method that never touches the bus at all —
// see its doc comment for why that seam exists instead of a second,
// trusted-caller bus path.
//
// A non-reserved Key is applied immediately by this same handler: there is no
// second "session.tag.set" consumer that performs the write after the veto
// check passes (unlike before:io.input, where the emitting plugin itself
// calls Emit("io.input", ...) once EmitVetoable reports no veto). The
// request event doubles as the trigger because the core engine is the only
// thing that knows how to mutate SessionMeta.Labels safely; there is nothing
// for a second step to do.
func (e *Engine) installSessionTagHandlers() {
	e.runUnsubs = append(e.runUnsubs, e.Bus.Subscribe("before:session.tag.set", func(event Event[any]) {
		vp, ok := event.Payload.(*VetoablePayload)
		if !ok || e.Session == nil {
			return
		}
		req, ok := vp.Original.(*events.SessionTagSetRequest)
		if !ok {
			return
		}
		if IsReservedLabelKey(req.Key) {
			vp.Veto = VetoResult{
				Vetoed: true,
				Reason: fmt.Sprintf("session tag key %q is reserved (starts with %q) and can only be written via SessionWorkspace.SetReservedLabel", req.Key, reservedLabelPrefix),
			}
			return
		}
		if err := e.Session.applyLabel(req.Key, req.Value, false); err != nil {
			vp.Veto = VetoResult{Vetoed: true, Reason: fmt.Sprintf("session tag set failed: %v", err)}
		}
	}, WithSource("nexus.engine.session_tags")))

	e.runUnsubs = append(e.runUnsubs, e.Bus.Subscribe("before:session.tag.delete", func(event Event[any]) {
		vp, ok := event.Payload.(*VetoablePayload)
		if !ok || e.Session == nil {
			return
		}
		req, ok := vp.Original.(*events.SessionTagDeleteRequest)
		if !ok {
			return
		}
		if IsReservedLabelKey(req.Key) {
			vp.Veto = VetoResult{
				Vetoed: true,
				Reason: fmt.Sprintf("session tag key %q is reserved (starts with %q) and can only be deleted via SessionWorkspace.DeleteReservedLabel", req.Key, reservedLabelPrefix),
			}
			return
		}
		if err := e.Session.applyLabel(req.Key, "", true); err != nil {
			vp.Veto = VetoResult{Vetoed: true, Reason: fmt.Sprintf("session tag delete failed: %v", err)}
		}
	}, WithSource("nexus.engine.session_tags")))
}
