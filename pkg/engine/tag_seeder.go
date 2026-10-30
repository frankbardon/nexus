package engine

import (
	"sync/atomic"

	"github.com/frankbardon/nexus/pkg/events"
)

// tagSeederLabelKeys are the SessionMeta.Labels keys the seeder copies onto
// every llm.request as cost-attribution tags.
var tagSeederLabelKeys = []string{"tenant", "project", "user"}

// installTagSeeder subscribes the baseline cost-attribution seeder on
// before:llm.request. The engine owns the session dimensions (session_id,
// plus tenant/project/user from SessionMeta.Labels); plugins layer their own
// (source_plugin, task_kind, parent_call_id) and win on a collision, because
// every seed is set-if-absent.
//
// Boot calls this before Lifecycle.Boot and before the recall branch, so no
// request a plugin can emit — from Ready, from history replay, or from the
// turn crashResumeIfPartial re-fires — predates it. Labels are re-read from
// session metadata on every request rather than captured at install time, so
// labels an embedder writes after Boot returns apply to the next request.
//
// A request the seeder cannot fully tag (no session, unreadable metadata)
// is logged at WARN once per cause for the life of the subscription, so an
// accounting hole shows up in the log without a line per request.
func (e *Engine) installTagSeeder() {
	var warnedNoSession, warnedMeta atomic.Bool
	e.runUnsubs = append(e.runUnsubs, e.Bus.Subscribe("before:llm.request", func(event Event[any]) {
		vp, ok := event.Payload.(*VetoablePayload)
		if !ok {
			return
		}
		req, ok := vp.Original.(*events.LLMRequest)
		if !ok {
			return
		}
		session := e.Session
		if session == nil {
			if warnedNoSession.CompareAndSwap(false, true) {
				e.Logger.Warn("tag seeder: llm.request emitted with no session; cost-attribution tags not seeded",
					"source_plugin", req.Tags["source_plugin"])
			}
			return
		}
		if req.Tags == nil {
			req.Tags = make(map[string]string)
		}
		// session_id first and unconditionally: it does not depend on the
		// metadata read below, so a metadata failure must not drop it.
		setTagIfAbsent(req.Tags, "session_id", session.ID)

		meta, err := session.SessionMetadata()
		if err != nil {
			if warnedMeta.CompareAndSwap(false, true) {
				e.Logger.Warn("tag seeder: session metadata unreadable; tenant/project/user tags not seeded",
					"session_id", session.ID, "error", err)
			}
			return
		}
		for _, k := range tagSeederLabelKeys {
			if v, ok := meta.Labels[k]; ok && v != "" {
				setTagIfAbsent(req.Tags, k, v)
			}
		}
	}, WithPriority(100), WithSource("nexus.engine.tag_seeder")))
}
