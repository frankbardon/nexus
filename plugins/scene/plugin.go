// Package scene hosts the nexus.scene plugin — the runtime that owns the
// Scene store, persists scene state into the session workspace, and exposes
// scene_create / scene_patch / scene_get / scene_list / scene_delete tools
// the LLM uses to build durable visual output. Patches are journaled to
// the session's plugin data dir as JSONL so the replay primitive can
// reconstruct historical scene state.
package scene

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/scene"
)

const (
	pluginID = "nexus.scene"
	name     = "Scene Store"
	version  = "0.1.0"

	capScene = "scene.store"

	toolCreate = "scene_create"
	toolPatch  = "scene_patch"
	toolGet    = "scene_get"
	toolList   = "scene_list"
	toolDelete = "scene_delete"

	patchJournalName = "scenes.jsonl"
	stateName        = "scenes.json"
)

// Plugin wires a scene.Store to the bus and persists state into the session
// plugin dir. Exposes a Store() accessor so other in-process plugins (the
// replay primitive) can read scene state without going through tools.
type Plugin struct {
	logger *slog.Logger
	bus    engine.EventBus
	store  *scene.MemoryStore

	sessionID string
	dataDir   string
	// session is captured so the two raw writers below (scenes.jsonl and
	// scenes.json) can announce themselves on the bus. The plugin holds its
	// own *os.File for the patch journal and writes scenes.json in one shot,
	// so neither can go through the SessionWorkspace helpers; announcing
	// after the fact is how they stop being invisible to the sync layer.
	// Nil when the engine was constructed without a session (tests).
	session *engine.SessionWorkspace

	mu       sync.Mutex
	journalF *os.File
	unsubs   []func()
}

func New() engine.Plugin {
	return &Plugin{
		store: scene.NewMemoryStore(),
	}
}

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return name }
func (p *Plugin) Version() string                { return version }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }
func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{{Name: capScene, Description: "Session-scoped scene store with patch history."}}
}

// Store returns the underlying scene store. Consumers (replay primitive) use
// this to walk a session's scenes without going through tool calls.
func (p *Plugin) Store() scene.Store { return p.store }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.logger = ctx.Logger
	p.bus = ctx.Bus
	p.dataDir = ctx.DataDir
	if ctx.Session != nil {
		p.sessionID = ctx.Session.ID
		p.session = ctx.Session
	}

	if p.dataDir != "" {
		if err := os.MkdirAll(p.dataDir, 0o755); err != nil {
			return fmt.Errorf("scene: mkdir data dir: %w", err)
		}
		if err := p.loadState(); err != nil {
			p.logger.Warn("scene: load state failed", "error", err)
		}
		if err := p.openJournal(); err != nil {
			p.logger.Warn("scene: open patch journal failed", "error", err)
		}
	}

	return nil
}

func (p *Plugin) Ready() error {
	for _, tool := range builtinTools() {
		_ = p.bus.Emit("tool.register", tool)
	}
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.onToolInvoke, engine.WithPriority(50), engine.WithSource(pluginID)),
	)
	return nil
}

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, u := range p.unsubs {
		u()
	}
	if p.dataDir != "" {
		if err := p.persistState(); err != nil {
			p.logger.Warn("scene: persist state failed", "error", err)
		}
	}
	p.mu.Lock()
	if p.journalF != nil {
		_ = p.journalF.Close()
		p.journalF = nil
	}
	p.mu.Unlock()
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{{EventType: "tool.invoke", Priority: 50}}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"tool.register",
		"tool.result",
		"before:tool.result",
		"scene.created",
		"scene.patched",
		"scene.deleted",
		// This plugin emitted eight event types and not one of them was about
		// the two files it writes, so it looked instrumented while being
		// entirely invisible to anything syncing the session tree. These two
		// come from SessionWorkspace.AnnounceAppend / AnnounceWrite, which
		// emit exactly the payload WriteFile does.
		"session.file.created",
		"session.file.updated",
	}
}

func (p *Plugin) onToolInvoke(ev engine.Event[any]) {
	tc, ok := ev.Payload.(events.ToolCall)
	if !ok {
		return
	}
	switch tc.Name {
	case toolCreate:
		p.handleCreate(tc, ev)
	case toolPatch:
		p.handlePatch(tc, ev)
	case toolGet:
		p.handleGet(tc)
	case toolList:
		p.handleList(tc)
	case toolDelete:
		p.handleDelete(tc, ev)
	}
}

func (p *Plugin) handleCreate(tc events.ToolCall, ev engine.Event[any]) {
	schema, _ := tc.Arguments["schema"].(string)
	initial := tc.Arguments["content"]
	// An optional scene_id lets a caller (the AG-UI inbound-state reconciler,
	// E3-S2) seed a scene under a known id so a subsequent scene_get finds it.
	// When the id already exists this is a patch, not a create, so the existing
	// content is preserved and merged (matching scene_patch semantics).
	id, _ := tc.Arguments["scene_id"].(string)
	if id != "" {
		if _, gErr := p.store.Get(p.sessionID, id); gErr == nil {
			p.handlePatch(events.ToolCall{
				ID:        tc.ID,
				Name:      tc.Name,
				TurnID:    tc.TurnID,
				Arguments: map[string]any{"scene_id": id, "patch": initial},
			}, ev)
			return
		}
	}
	handle, err := p.store.CreateWithID(p.sessionID, id, schema, initial, ev.Causation.AgentID)
	if err != nil {
		p.respondError(tc, err.Error())
		return
	}
	// content carries the scene's full materialized content so bus consumers
	// (e.g. the AG-UI transport's shared-state sync) can track scene state
	// without going through a tool call. It is the post-create content, which
	// for a create is the initial value.
	_ = p.bus.Emit("scene.created", map[string]any{
		"session_id": handle.SessionID,
		"scene_id":   handle.ID,
		"schema":     handle.Schema,
		"version":    handle.Version,
		"agent_id":   ev.Causation.AgentID,
		"content":    p.currentContent(handle.ID),
	})
	p.journalEvent("created", handle, initial, ev.Causation.AgentID)
	body, _ := json.Marshal(handle)
	p.respondOK(tc, string(body))
}

func (p *Plugin) handlePatch(tc events.ToolCall, ev engine.Event[any]) {
	id, _ := tc.Arguments["scene_id"].(string)
	if id == "" {
		p.respondError(tc, "scene_id is required")
		return
	}
	patch := tc.Arguments["patch"]
	handle, err := p.store.Patch(p.sessionID, id, patch, ev.Causation.AgentID)
	if err != nil {
		p.respondError(tc, err.Error())
		return
	}
	// content carries the scene's full materialized content AFTER the patch is
	// applied (the scene store's patch semantics are shallow-merge, not RFC
	// 6902, so consumers that need an RFC 6902 delta diff the full content
	// themselves). See scene.created for rationale.
	_ = p.bus.Emit("scene.patched", map[string]any{
		"session_id": handle.SessionID,
		"scene_id":   handle.ID,
		"version":    handle.Version,
		"agent_id":   ev.Causation.AgentID,
		"content":    p.currentContent(handle.ID),
	})
	p.journalEvent("patched", handle, patch, ev.Causation.AgentID)
	body, _ := json.Marshal(handle)
	p.respondOK(tc, string(body))
}

func (p *Plugin) handleGet(tc events.ToolCall) {
	id, _ := tc.Arguments["scene_id"].(string)
	sc, err := p.store.Get(p.sessionID, id)
	if err != nil {
		p.respondError(tc, err.Error())
		return
	}
	body, _ := json.Marshal(sc)
	p.respondOK(tc, string(body))
}

func (p *Plugin) handleList(tc events.ToolCall) {
	handles := p.store.List(p.sessionID)
	body, _ := json.Marshal(handles)
	p.respondOK(tc, string(body))
}

func (p *Plugin) handleDelete(tc events.ToolCall, ev engine.Event[any]) {
	id, _ := tc.Arguments["scene_id"].(string)
	if err := p.store.Delete(p.sessionID, id); err != nil {
		p.respondError(tc, err.Error())
		return
	}
	_ = p.bus.Emit("scene.deleted", map[string]any{
		"session_id": p.sessionID,
		"scene_id":   id,
		"agent_id":   ev.Causation.AgentID,
	})
	p.journalEvent("deleted", scene.SceneHandle{ID: id, SessionID: p.sessionID}, nil, ev.Causation.AgentID)
	p.respondOK(tc, `{"deleted":true}`)
}

// currentContent returns the scene's current materialized content, or nil if
// the scene cannot be read. It is used to attach the post-mutation content to
// scene.created / scene.patched bus events.
func (p *Plugin) currentContent(id string) any {
	sc, err := p.store.Get(p.sessionID, id)
	if err != nil {
		return nil
	}
	return sc.Content
}

func (p *Plugin) respondOK(tc events.ToolCall, body string) {
	res := events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            tc.ID,
		Name:          tc.Name,
		Output:        body,
		TurnID:        tc.TurnID,
	}
	if veto, vErr := p.bus.EmitVetoable("before:tool.result", &res); vErr == nil && veto.Vetoed {
		return
	}
	_ = p.bus.Emit("tool.result", res)
}

func (p *Plugin) respondError(tc events.ToolCall, msg string) {
	res := events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            tc.ID,
		Name:          tc.Name,
		Error:         msg,
		TurnID:        tc.TurnID,
	}
	if veto, vErr := p.bus.EmitVetoable("before:tool.result", &res); vErr == nil && veto.Vetoed {
		return
	}
	_ = p.bus.Emit("tool.result", res)
}

// journalEvent appends a JSONL line per scene mutation under the plugin's
// session data dir. The replay primitive reads this back when a session
// resumes without a memory snapshot, and the file is the durable source of
// truth for time-travel scene reconstruction.
//
// Object-store disposition: emit. Every append announces itself on the bus via
// AnnounceAppend, so a sync backend sees scene mutations as they land instead
// of waiting for the turn to end. This is the highest-churn raw writer under a
// session tree and the one whose loss hurts most — a run killed mid-turn drops
// exactly the scenes it just built. See engine.SessionTreeWriters.
func (p *Plugin) journalEvent(kind string, handle scene.SceneHandle, patch any, agentID string) {
	line, err := json.Marshal(map[string]any{
		"kind":     kind,
		"scene_id": handle.ID,
		"schema":   handle.Schema,
		"version":  handle.Version,
		"agent_id": agentID,
		"patch":    patch,
	})
	if err != nil {
		return
	}
	line = append(line, '\n')

	p.mu.Lock()
	if p.journalF == nil {
		p.mu.Unlock()
		return
	}
	n, wErr := p.journalF.Write(line)
	p.mu.Unlock()

	// Announced outside p.mu on purpose. Emit dispatches synchronously on
	// this goroutine, so a subscriber that reached back into the scene store
	// — the replay primitive holds a Store() handle, and an IO transport
	// reacting to a file event is exactly the shape that would — would
	// deadlock against the lock this write was taken under. Nothing needs the
	// announcement to be inside the critical section: the bytes are already
	// durable and their offset is derived from the file, not from p.
	if wErr != nil || n == 0 || p.session == nil {
		return
	}
	p.session.AnnounceAppend(filepath.Join(p.dataDir, patchJournalName), n)
}

func (p *Plugin) openJournal() error {
	f, err := os.OpenFile(filepath.Join(p.dataDir, patchJournalName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.journalF = f
	p.mu.Unlock()
	return nil
}

// persistState writes a full snapshot of the in-memory store to scenes.json.
// Called from Shutdown so a clean restart picks up where we left off.
//
// Object-store disposition: emit. The announcement is safe here even though
// Shutdown runs late — a bus Emit after the journal writer has closed is
// dropped by the writer rather than stalling it, because Append short-circuits
// on the closed flag and no later envelope is waiting on this one's sequence
// number. (The dangerous direction is the other one: emitting *before* the
// journal subscribes, which strands a sequence number the drain waits on
// forever. Nothing in this plugin writes that early — Init only reads
// scenes.json and opens the journal descriptor.)
func (p *Plugin) persistState() error {
	handles := p.store.List(p.sessionID)
	out := make([]scene.Scene, 0, len(handles))
	for _, h := range handles {
		sc, err := p.store.Get(p.sessionID, h.ID)
		if err == nil {
			out = append(out, sc)
		}
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(p.dataDir, stateName)
	// Sampled before the write: afterwards every path exists, so checking then
	// would report every first snapshot as an update and no subscriber would
	// ever see scenes.json appear.
	_, statErr := os.Stat(path)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	if p.session != nil {
		p.session.AnnounceWrite(path, statErr == nil)
	}
	return nil
}

// loadState restores scenes.json on Init. Per-scene history is preserved.
func (p *Plugin) loadState() error {
	path := filepath.Join(p.dataDir, stateName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var scenes []scene.Scene
	if err := json.Unmarshal(data, &scenes); err != nil {
		return err
	}
	for _, sc := range scenes {
		// Replay by Create + each post-initial patch so version counters
		// stay consistent with what produced the journal.
		_, _ = p.store.Create(sc.Handle.SessionID, sc.Handle.Schema, sc.Content, "")
	}
	return nil
}

func builtinTools() []events.ToolDef {
	return []events.ToolDef{
		{
			Name:        toolCreate,
			Description: "Create a new Scene — a named, mutable, session-scoped structured visual output. Returns a scene_id you can reference in subsequent scene_patch and scene_get calls.",
			Class:       "scenes",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"schema": map[string]any{
						"type":        "string",
						"description": "Name of the schema this scene's content conforms to (e.g. 'chart.vega', 'doc.markdown'). The renderer interprets it.",
					},
					"content": map[string]any{
						"description": "Initial content. Free-form — the runtime is schema-agnostic.",
					},
					"scene_id": map[string]any{
						"type":        "string",
						"description": "Optional explicit scene id. When omitted the store assigns one. When supplied and a scene with that id already exists, the content is merged as a patch instead.",
					},
				},
				"required": []string{"schema"},
			},
		},
		{
			Name:        toolPatch,
			Description: "Apply a patch to an existing Scene. Map patches merge shallow (keys in the patch overwrite the existing keys); non-map patches replace the content entirely.",
			Class:       "scenes",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"scene_id": map[string]any{"type": "string"},
					"patch":    map[string]any{"description": "The patch to apply."},
				},
				"required": []string{"scene_id", "patch"},
			},
		},
		{
			Name:        toolGet,
			Description: "Return a Scene's current content and patch history.",
			Class:       "scenes",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"scene_id": map[string]any{"type": "string"}},
				"required":   []string{"scene_id"},
			},
		},
		{
			Name:        toolList,
			Description: "List all Scenes in the current session.",
			Class:       "scenes",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        toolDelete,
			Description: "Delete a Scene by id.",
			Class:       "scenes",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"scene_id": map[string]any{"type": "string"}},
				"required":   []string{"scene_id"},
			},
		},
	}
}
