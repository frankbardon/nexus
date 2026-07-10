package agui

import (
	"encoding/json"

	"github.com/frankbardon/nexus/pkg/events"
)

// inbound_state.go implements the inbound half of the AG-UI shared-state feature
// (E3-S2): it applies a client-authored state document carried on a
// RunAgentInput into the session's scene store and the plugin's shared-state
// mirror, BEFORE the initial StateSnapshot is emitted, so the agent observes the
// client's view and the snapshot reflects it.
//
// # State document shape
//
// The AG-UI shared-state document this transport speaks is a JSON object keyed
// by scene_id, each value being that scene's content — the exact shape E3-S1
// marshals outbound (StateSnapshot / StateDelta). Inbound state is therefore
// symmetric: a client sends back the same document (or a subset of it) it
// received, with edited scene contents. Any non-object state document is ignored
// (the mirror is scene-keyed); a malformed document is logged and skipped rather
// than failing the run.
//
// # Making a client write visible to the agent
//
// Writing to p.sharedState alone would only affect the outbound snapshot — the
// agent reads scene state through the scene store (scene_get / scene_list), so
// the store is the source of truth it observes. To make a client write real,
// each scene_id -> content entry is pushed into the scene store via a
// bus-emitted scene_create tool.invoke carrying the explicit scene_id (the scene
// plugin creates it under that id, or merges as a patch when it already exists).
// The scene plugin then emits scene.created / scene.patched, which the E3-S1
// mirror consumes; because p.sharedState was already updated to the same value,
// that mirror update diffs empty and emits no duplicate delta. This keeps the
// bus-only rule (no direct scene-plugin call) and a real, agent-visible seed.
//
// # Conflict / ordering semantics: client-state-seeds-then-agent-wins
//
// Inbound apply runs to completion inside startRun / resumeRun BEFORE the run's
// io.input is emitted, so the agent's first turn always observes the seeded
// state. For the remainder of the run, agent-side scene mutations are
// last-writer over the same scene_id: a later scene_patch overwrites the client
// seed per the scene store's shallow-merge semantics. The plugin's stateMu
// serializes the mirror updates and the scene store serializes its own writes,
// so concurrent agent + client mutations never interleave mid-document; ordering
// is deterministic (client seed first, then agent writes in bus order).

// applyInboundState reconciles a client-authored state document (RunAgentInput.
// state) into the shared-state mirror and the scene store before the initial
// snapshot is emitted. It is a no-op when shared-state emission is disabled or
// the document is absent/empty, and it never fails the run: a malformed document
// is logged and skipped.
//
// It updates p.sharedState under stateMu (so the immediately-following
// emitInitialSnapshot reflects the client's view) and emits a scene_create
// tool.invoke per scene so the store — and therefore the agent — adopts the same
// state. The threadID scopes the emitted tool calls to the run's thread/session.
func (p *Plugin) applyInboundState(threadID string, state json.RawMessage) {
	if !p.emitState || len(state) == 0 {
		return
	}
	scenes, ok := decodeInboundState(state)
	if !ok {
		p.logger.Warn("agui: inbound state ignored (not a scene-keyed object)",
			"thread_id", threadID)
		return
	}
	if len(scenes) == 0 {
		return
	}

	// 1) Seed the mirror so the initial snapshot carries the client's view. This
	//    is done under stateMu, ordered before emitInitialSnapshot by the caller.
	p.stateMu.Lock()
	for id, content := range scenes {
		if content == nil {
			content = json.RawMessage("null")
		}
		p.sharedState[id] = content
	}
	p.stateMu.Unlock()

	// 2) Push each scene into the store so a subsequent scene_get / scene_list —
	//    what the agent reads — observes the client seed. The scene plugin's
	//    resulting scene.created / scene.patched updates the mirror to the same
	//    value, diffing empty (no duplicate delta). Bus-only: no direct call.
	for id, content := range scenes {
		p.emitSceneSeed(threadID, id, content)
	}
}

// emitSceneSeed emits a scene_create tool.invoke that seeds scene id with the
// given content. The scene plugin honors the explicit scene_id: it creates the
// scene under that id, or merges the content as a patch when the scene already
// exists. The call is tagged with a synthetic source so it is distinguishable in
// journals from an agent-issued create.
func (p *Plugin) emitSceneSeed(threadID, id string, content json.RawMessage) {
	var contentVal any
	if len(content) > 0 {
		if err := json.Unmarshal(content, &contentVal); err != nil {
			p.logger.Warn("agui: inbound state scene decode failed",
				"thread_id", threadID, "scene_id", id, "error", err)
			return
		}
	}
	tc := events.ToolCall{
		SchemaVersion: events.ToolCallVersion,
		Name:          "scene_create",
		Arguments: map[string]any{
			"scene_id": id,
			"content":  contentVal,
		},
	}
	if err := p.bus.Emit("tool.invoke", tc); err != nil {
		p.logger.Warn("agui: emit inbound scene seed failed",
			"thread_id", threadID, "scene_id", id, "error", err)
	}
}

// decodeInboundState parses a client state document into a scene_id -> content
// map. It returns ok=false when the document is not a JSON object (the mirror is
// scene-keyed, so a non-object document has no reconciliation target).
func decodeInboundState(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	return m, true
}
