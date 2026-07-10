package agui

import (
	"encoding/json"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/engine"
)

// state.go implements the outbound AG-UI shared-state feature (E3-S1): it
// mirrors the session's scene store as an AG-UI shared-state document and emits
// a StateSnapshot at run start plus ordered StateDeltas (RFC 6902 JSON Patch) as
// scenes mutate during a run.
//
// State source: the nexus.scene plugin emits scene.created / scene.patched /
// scene.deleted on the bus, each carrying the scene's post-mutation content.
// This transport tracks those events into a full state document keyed by
// scene_id and diffs successive documents into JSON Patches. It never calls the
// scene plugin directly (bus-only rule); the content on the bus events is the
// sole input.
//
// The shared-state model lives on the Plugin, not the run: scenes are
// session-scoped and persist across runs on the same listener, so a second run
// on the same session must snapshot the accumulated state. Scene mutations that
// arrive between runs still update the model (so the next snapshot is correct)
// but emit no delta because there is no SSE stream to carry it.
//
// Inbound state application (RunAgentInput.state) lives in inbound_state.go
// (E3-S2): startRun/resumeRun call applyInboundState before emitInitialSnapshot,
// reconciling the client-authored state into both p.sharedState (under stateMu,
// so the snapshot reflects the client's view) and the scene store (so the agent
// observes it via scene_get/scene_list).

// sceneCreatedType, scenePatchedType, sceneDeletedType are the scene store bus
// event types the shared-state mirror consumes. Declared here so both Init's
// Subscribe calls and Subscriptions() reference one source.
const (
	sceneCreatedType = "scene.created"
	scenePatchedType = "scene.patched"
	sceneDeletedType = "scene.deleted"
)

// stateEventTypes lists the scene bus events wired only when state emission is
// enabled.
var stateEventTypes = []string{sceneCreatedType, scenePatchedType, sceneDeletedType}

// currentStateJSON marshals the current shared-state document (scene_id ->
// content). It always returns a valid JSON object, never nil, so a snapshot is
// well-formed even with no scenes yet. The caller must hold no lock; this method
// takes stateMu internally.
func (p *Plugin) currentStateJSON() json.RawMessage {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.marshalStateLocked()
}

// marshalStateLocked marshals the state model. Caller holds stateMu.
func (p *Plugin) marshalStateLocked() json.RawMessage {
	if len(p.sharedState) == 0 {
		return json.RawMessage("{}")
	}
	raw, err := json.Marshal(p.sharedState)
	if err != nil {
		return json.RawMessage("{}")
	}
	return raw
}

// emitInitialSnapshot queues a StateSnapshot of the current shared state onto
// the run so a client can render the agent's state before any delta. It is a
// no-op when state emission is disabled. Called from startRun after RunStarted
// so the snapshot rides inside the run envelope.
func (p *Plugin) emitInitialSnapshot(r *run) {
	if !p.emitState || r == nil {
		return
	}
	r.queue(agui.NewStateSnapshot(p.currentStateJSON()))
}

// applySceneMutation records a scene create/patch into the shared-state model
// and, when a run is active, queues a StateDelta whose delta is an RFC 6902 JSON
// Patch from the prior state document to the new one. It is the single write
// path for scene mutations so the snapshot the model marshals and the deltas the
// client applies stay consistent and ordered (stateMu serializes concurrent
// scene events).
func (p *Plugin) applySceneMutation(sceneID string, content json.RawMessage) {
	if sceneID == "" {
		return
	}
	p.stateMu.Lock()
	prev := p.marshalStateLocked()
	if content == nil {
		content = json.RawMessage("null")
	}
	p.sharedState[sceneID] = content
	next := p.marshalStateLocked()
	p.stateMu.Unlock()

	p.queueStateDelta(prev, next)
}

// removeScene drops a scene from the shared-state model and, when a run is
// active, queues the corresponding StateDelta.
func (p *Plugin) removeScene(sceneID string) {
	if sceneID == "" {
		return
	}
	p.stateMu.Lock()
	if _, ok := p.sharedState[sceneID]; !ok {
		p.stateMu.Unlock()
		return
	}
	prev := p.marshalStateLocked()
	delete(p.sharedState, sceneID)
	next := p.marshalStateLocked()
	p.stateMu.Unlock()

	p.queueStateDelta(prev, next)
}

// queueStateDelta diffs prev->next and queues a StateDelta on the active run.
// An empty diff (no effective change) or an absent run yields no event; the
// model has already been updated so the next snapshot stays correct.
func (p *Plugin) queueStateDelta(prev, next json.RawMessage) {
	if !p.emitState {
		return
	}
	r := p.currentRun()
	if r == nil {
		return
	}
	patch, err := agui.DiffState(prev, next)
	if err != nil {
		p.logger.Warn("agui: state diff failed", "error", err)
		return
	}
	if len(patch) == 0 {
		return
	}
	r.queue(agui.NewStateDelta(patch))
}

// handleSceneCreated / handleScenePatched / handleSceneDeleted translate scene
// store bus events into shared-state mutations. They read the scene_id and
// post-mutation content off the event payload (a map[string]any emitted by the
// scene plugin) and never touch the scene plugin directly.

func (p *Plugin) handleSceneCreated(e engine.Event[any]) {
	id, content := sceneMutationFields(e.Payload)
	p.applySceneMutation(id, content)
}

func (p *Plugin) handleScenePatched(e engine.Event[any]) {
	id, content := sceneMutationFields(e.Payload)
	p.applySceneMutation(id, content)
}

func (p *Plugin) handleSceneDeleted(e engine.Event[any]) {
	id, _ := sceneMutationFields(e.Payload)
	p.removeScene(id)
}

// sceneMutationFields extracts the scene_id and (JSON-encoded) content from a
// scene.* bus payload. The scene plugin emits a map[string]any with a
// "scene_id" string and, for create/patch, a "content" value. A missing or
// malformed payload yields an empty id, which the callers treat as a no-op.
func sceneMutationFields(payload any) (string, json.RawMessage) {
	m, ok := payload.(map[string]any)
	if !ok {
		return "", nil
	}
	id, _ := m["scene_id"].(string)
	content, hasContent := m["content"]
	if !hasContent {
		return id, nil
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return id, nil
	}
	return id, raw
}
