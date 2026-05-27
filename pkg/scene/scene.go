// Package scene defines the Scene primitive — a named, structured, mutable
// session-scoped entity an agent uses to build durable visual output.
// Scenes are content-agnostic from the runtime's perspective: the engine
// stores blobs, journals patches, and emits scene.* events; downstream
// renderers (UIs, exporters) interpret the schema-specific content.
//
// The Store interface is what the nexus.scene plugin implements; it backs
// the SceneHandle methods the scene tools surface to agents and exposes
// the patch history that the replay primitive consumes for time-travel.
package scene

import (
	"errors"
	"sync"
	"time"
)

// SceneHandle is the stable, session-scoped identity of a Scene plus a few
// pieces of metadata the agent's prompt may want to reference (the schema
// it conforms to, the current version). The Content lives in the Store.
type SceneHandle struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Schema    string `json:"schema"`
	Version   int    `json:"version"`
}

// Scene is the full materialized record: handle, current content, and an
// optional patch history. Returned from Store.Get; the caller owns the
// content map.
type Scene struct {
	Handle    SceneHandle  `json:"handle"`
	Content   any          `json:"content"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
	History   []SceneEvent `json:"history,omitempty"`
}

// SceneEvent records a single mutation on the scene's history. The patch is
// the value passed to PatchScene at the time; the first event records the
// initial content.
type SceneEvent struct {
	Sequence  int       `json:"sequence"`
	Timestamp time.Time `json:"timestamp"`
	AgentID   string    `json:"agent_id,omitempty"`
	Patch     any       `json:"patch"`
	Initial   bool      `json:"initial,omitempty"`
}

// Store is the contract the nexus.scene plugin implements and tools / the
// replay primitive consume. Implementations must serialize concurrent
// patches against the same Scene (parent + sub-agent contention) and emit
// scene.created / scene.patched / scene.deleted on the bus.
type Store interface {
	Create(sessionID, schema string, initial any, agentID string) (SceneHandle, error)
	Get(sessionID, id string) (Scene, error)
	Patch(sessionID, id string, patch any, agentID string) (SceneHandle, error)
	Delete(sessionID, id string) error
	List(sessionID string) []SceneHandle
}

// Patcher merges a patch into existing content. The runtime is schema-
// agnostic, so the default Patcher does shallow JSON merge for maps and
// otherwise replaces. Schema-specific renderers can provide their own
// Patcher implementation if they need a richer semantics, but the runtime
// only ships shallow merge.
type Patcher interface {
	Apply(existing, patch any) any
}

// ShallowMerge is the default Patcher: maps merge key-by-key (the patch
// wins on collision), everything else replaces.
type ShallowMerge struct{}

// Apply merges patch into existing per the package doc.
func (ShallowMerge) Apply(existing, patch any) any {
	exMap, exOk := existing.(map[string]any)
	pMap, pOk := patch.(map[string]any)
	if !exOk || !pOk {
		return patch
	}
	out := make(map[string]any, len(exMap)+len(pMap))
	for k, v := range exMap {
		out[k] = v
	}
	for k, v := range pMap {
		out[k] = v
	}
	return out
}

// ErrNotFound is returned by Get/Patch/Delete when the scene does not exist.
var ErrNotFound = errors.New("scene: not found")

// MemoryStore is a goroutine-safe in-memory implementation suitable for the
// engine's default. Persistence is the plugin's job — MemoryStore is the
// authoritative source between persistence flushes.
type MemoryStore struct {
	mu      sync.RWMutex
	patcher Patcher
	scenes  map[string]map[string]*Scene
}

// NewMemoryStore returns an empty store using ShallowMerge as the patcher.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		patcher: ShallowMerge{},
		scenes:  make(map[string]map[string]*Scene),
	}
}

// WithPatcher swaps the default ShallowMerge for a schema-specific merger.
func (s *MemoryStore) WithPatcher(p Patcher) *MemoryStore {
	s.patcher = p
	return s
}

func (s *MemoryStore) Create(sessionID, schema string, initial any, agentID string) (SceneHandle, error) {
	if sessionID == "" {
		return SceneHandle{}, errors.New("scene: sessionID required")
	}
	id := newSceneID()
	now := time.Now()
	scene := &Scene{
		Handle: SceneHandle{
			ID:        id,
			SessionID: sessionID,
			Schema:    schema,
			Version:   1,
		},
		Content:   initial,
		CreatedAt: now,
		UpdatedAt: now,
		History: []SceneEvent{{
			Sequence:  1,
			Timestamp: now,
			AgentID:   agentID,
			Patch:     initial,
			Initial:   true,
		}},
	}
	s.mu.Lock()
	bySession, ok := s.scenes[sessionID]
	if !ok {
		bySession = make(map[string]*Scene)
		s.scenes[sessionID] = bySession
	}
	bySession[id] = scene
	s.mu.Unlock()
	return scene.Handle, nil
}

func (s *MemoryStore) Get(sessionID, id string) (Scene, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	bySession, ok := s.scenes[sessionID]
	if !ok {
		return Scene{}, ErrNotFound
	}
	scene, ok := bySession[id]
	if !ok {
		return Scene{}, ErrNotFound
	}
	out := *scene
	out.History = append([]SceneEvent(nil), scene.History...)
	return out, nil
}

func (s *MemoryStore) Patch(sessionID, id string, patch any, agentID string) (SceneHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bySession, ok := s.scenes[sessionID]
	if !ok {
		return SceneHandle{}, ErrNotFound
	}
	scene, ok := bySession[id]
	if !ok {
		return SceneHandle{}, ErrNotFound
	}
	scene.Content = s.patcher.Apply(scene.Content, patch)
	now := time.Now()
	scene.UpdatedAt = now
	scene.Handle.Version++
	scene.History = append(scene.History, SceneEvent{
		Sequence:  scene.Handle.Version,
		Timestamp: now,
		AgentID:   agentID,
		Patch:     patch,
	})
	return scene.Handle, nil
}

func (s *MemoryStore) Delete(sessionID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	bySession, ok := s.scenes[sessionID]
	if !ok {
		return ErrNotFound
	}
	if _, ok := bySession[id]; !ok {
		return ErrNotFound
	}
	delete(bySession, id)
	return nil
}

func (s *MemoryStore) List(sessionID string) []SceneHandle {
	s.mu.RLock()
	defer s.mu.RUnlock()
	bySession, ok := s.scenes[sessionID]
	if !ok {
		return nil
	}
	out := make([]SceneHandle, 0, len(bySession))
	for _, sc := range bySession {
		out = append(out, sc.Handle)
	}
	return out
}

// newSceneID is centralized so the format ("scene_<hex>") stays stable for
// downstream consumers (UI templates, replay correlation) that may pattern
// match the prefix.
func newSceneID() string {
	return "scene_" + shortID()
}
