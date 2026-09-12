package engine

import (
	"fmt"
	"sort"
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

// SetLabel writes a general-namespace (non-reserved) session label directly,
// bypassing the before:session.tag.set vetoable bus path entirely.
//
// # A second sanctioned direct-write seam, for trusted decode-time input
//
// Every other general-namespace write goes through installSessionTagHandlers,
// which lets a subscriber veto. This method exists for a caller that already
// sits on trusted, already-authenticated, already-decoded input and gains
// nothing from a veto hop — the same reasoning nexus.io.agui's buildUserInput
// already applies to RunAgentInput.messages when folding them into
// events.UserInput with no veto hop of its own. RunAgentInput.Context items
// are the motivating case: they ride an authenticated transport already, so
// re-litigating them through a gate built for untrusted general writes (e.g.
// a tool call) would add a hop with no caller able to usefully veto it.
//
// A reserved ("_"-prefixed) key is rejected here, defensively, for the exact
// reason SetReservedLabel/DeleteReservedLabel reject a non-reserved one: this
// method bypasses the general validation completely, and without this check
// a caller mistake (or a client smuggling a "_"-prefixed key through
// RunAgentInput.Context) could write into the reserved namespace with none of
// the vetoable path's protection. See installSessionTagHandlers's doc comment
// for the risk this guards against on the bus path; this is the same guard on
// this second, non-bus path.
//
// Ends by calling the same internal apply-and-announce step every other
// label write uses, so session.tag.set fires here exactly as it does for the
// vetoable general path or a reserved direct write.
func (s *SessionWorkspace) SetLabel(key, value string) error {
	if IsReservedLabelKey(key) {
		return fmt.Errorf("session labels: SetLabel called with reserved key %q (must not start with %q); use SetReservedLabel", key, reservedLabelPrefix)
	}
	return s.applyLabel(key, value, false)
}

// applyLabel is the single-key form of applyLabels, and the shape every
// caller that writes one label at a time uses. Both installSessionTagHandlers
// (after it has already rejected a reserved key on the general path) and
// SetReservedLabel/DeleteReservedLabel (after validating the key actually is
// reserved) call this — never SaveMeta directly.
func (s *SessionWorkspace) applyLabel(key, value string, deleted bool) error {
	if deleted {
		return s.applyLabels(nil, []string{key})
	}
	return s.applyLabels(map[string]string{key: value}, nil)
}

// applyLabels is the single internal apply-and-announce step for every
// session-label write, general or reserved, one key or many — so there is
// exactly one place that mutates Labels and exactly one announce event per
// key for anything downstream watching session labels to subscribe to.
//
// # Why a batch form exists at all
//
// A whole-set replacement (SetRequestHeaders is the motivating caller: it
// rebinds every X-Nexus-* header of a request on every turn) would otherwise
// be N separate read-modify-write round trips through SessionMetadata and
// SaveMeta, each one re-reading and re-serializing the whole session file to
// change one key. Worse, a crash midway through would leave the labels
// half-replaced. One read, one write, then the announcements keeps a
// replacement atomic on disk and cheap per turn.
//
// Deletes are applied before sets so a caller replacing a set can pass the
// stale keys and the fresh ones together without ordering them itself. Both
// the application and the announcements run in sorted key order, so two runs
// of the same replacement produce the same sequence of events rather than
// whichever order Go's map iteration happened to take.
func (s *SessionWorkspace) applyLabels(set map[string]string, del []string) error {
	if len(set) == 0 && len(del) == 0 {
		return nil
	}

	meta, err := s.SessionMetadata()
	if err != nil {
		return fmt.Errorf("reading session metadata: %w", err)
	}
	if meta.Labels == nil {
		meta.Labels = make(map[string]string)
	}

	delKeys := append([]string(nil), del...)
	sort.Strings(delKeys)
	setKeys := make([]string, 0, len(set))
	for k := range set {
		setKeys = append(setKeys, k)
	}
	sort.Strings(setKeys)

	for _, key := range delKeys {
		delete(meta.Labels, key)
	}
	for _, key := range setKeys {
		meta.Labels[key] = set[key]
	}

	if err := s.SaveMeta(meta); err != nil {
		return fmt.Errorf("saving session metadata: %w", err)
	}

	if s.bus == nil {
		return nil
	}
	for _, key := range delKeys {
		_ = s.bus.Emit("session.tag.deleted", events.SessionTagDeleted{
			SchemaVersion: events.SessionTagDeletedVersion,
			SessionID:     s.ID,
			Key:           key,
		})
	}
	for _, key := range setKeys {
		_ = s.bus.Emit("session.tag.set", events.SessionTagSet{
			SchemaVersion: events.SessionTagSetVersion,
			SessionID:     s.ID,
			Key:           key,
			Value:         set[key],
		})
	}
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
