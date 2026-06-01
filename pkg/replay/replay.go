// Package replay is the read-only walker over a session's durable journal.
// Given a session ID and the engine's session root directory, it reconstructs
// every event the session dispatched and rebuilds scene state at any seq.
// Replay does not re-run agents or tools — reconstruction is deterministic
// from the journals.
//
// Use cases:
//
//   - Debugging: a user reports an issue with a session; replay reconstructs
//     the event sequence so an operator can read what happened.
//   - Audit: walk the causation DAG to verify what a session did.
//   - Time-travel: reconstruct scene state at an earlier point.
//   - Branch and fork: replay to a point, then continue from there with a
//     new agent message (the engine's existing rewind primitive consumes
//     the DAG produced here).
package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/scene"
)

// Event is one node in the reconstructed causation DAG. Mirrors the journal
// Envelope with the fields callers walk explicitly.
type Event struct {
	Seq       uint64 `json:"seq"`
	ParentSeq uint64 `json:"parent_seq,omitempty"`
	ParentID  string `json:"parent_id,omitempty"`
	EventID   string `json:"event_id,omitempty"`
	Type      string `json:"type"`
	AgentID   string `json:"agent_id,omitempty"`
	Depth     int    `json:"depth,omitempty"`
	Vetoed    bool   `json:"vetoed,omitempty"`
	Payload   any    `json:"payload,omitempty"`
}

// SceneSnap is the rebuilt content of a scene at a specific point in the
// session's history.
type SceneSnap struct {
	Handle    scene.SceneHandle `json:"handle"`
	AtSeq     uint64            `json:"at_seq"`
	Content   any               `json:"content"`
	UpdatedAt string            `json:"updated_at,omitempty"`
}

// Replay is the materialized reconstruction returned to callers.
type Replay struct {
	SessionID string      `json:"session_id"`
	Events    []Event     `json:"events"`
	Scenes    []SceneSnap `json:"scenes"`
	LastSeq   uint64      `json:"last_seq"`
}

// Options configure a replay walk.
type Options struct {
	// SessionsRoot is the engine's session root (typically ~/.nexus/sessions).
	SessionsRoot string
	// AtSeq limits reconstruction to events with Seq <= AtSeq. Zero means
	// "to the end of the journal".
	AtSeq uint64
	// IncludeVetoed controls whether vetoed before:* envelopes appear in
	// the returned Events slice. Defaults to true; set false for clean
	// audit reports.
	IncludeVetoed bool
}

// ScenePluginID is the plugin ID whose data dir holds the scene patch
// journal. Hard-coded because the replay primitive does not have access
// to the live plugin registry.
const ScenePluginID = "nexus.scene"

// Session reads sessionID's journal and produces a full Replay. Returns an
// error when the session directory or journal cannot be opened.
func Session(ctx context.Context, sessionID string, opts Options) (Replay, error) {
	if opts.SessionsRoot == "" {
		return Replay{}, errors.New("replay: SessionsRoot is required")
	}
	if sessionID == "" {
		return Replay{}, errors.New("replay: sessionID is required")
	}

	root := filepath.Join(opts.SessionsRoot, sessionID)
	journalDir := filepath.Join(root, "journal")

	r, err := journal.Open(journalDir)
	if err != nil {
		return Replay{}, fmt.Errorf("replay: open journal: %w", err)
	}

	out := Replay{SessionID: sessionID}
	includeVetoed := true
	if !opts.IncludeVetoed {
		includeVetoed = false
	}

	walkErr := r.Iter(func(env journal.Envelope) bool {
		if ctx.Err() != nil {
			return false
		}
		if opts.AtSeq > 0 && env.Seq > opts.AtSeq {
			return false
		}
		if env.Vetoed && !includeVetoed {
			out.LastSeq = env.Seq
			return true
		}
		out.Events = append(out.Events, Event{
			Seq:       env.Seq,
			ParentSeq: env.ParentSeq,
			ParentID:  env.ParentID,
			EventID:   env.EventID,
			Type:      env.Type,
			AgentID:   env.AgentID,
			Depth:     env.Depth,
			Vetoed:    env.Vetoed,
			Payload:   env.Payload,
		})
		out.LastSeq = env.Seq
		return true
	})
	if walkErr != nil {
		return out, fmt.Errorf("replay: iter: %w", walkErr)
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}

	// Reconstruct scene snapshots from the scene plugin's patch journal.
	scenes, sErr := reconstructScenes(root, opts.AtSeq)
	if sErr != nil {
		// Scene reconstruction is best-effort — a session without scenes
		// still returns its events.
		return out, nil
	}
	out.Scenes = scenes
	return out, nil
}

// reconstructScenes reads the scene plugin's JSONL patch journal and folds
// each entry into a deterministic snapshot. atSeq currently filters scene
// events by their position in the scene journal, not the session journal —
// the scene journal does not carry the bus seq today, so this is a coarse
// filter that returns scenes mutated before atSeq lines have been read.
// Improving this requires teaching the scene plugin to record bus seq in
// each line; that is a follow-up.
func reconstructScenes(sessionRoot string, atSeq uint64) ([]SceneSnap, error) {
	scenesPath := filepath.Join(sessionRoot, "plugins", ScenePluginID, "scenes.jsonl")
	f, err := os.Open(scenesPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	type entry struct {
		Kind    string `json:"kind"`
		SceneID string `json:"scene_id"`
		Schema  string `json:"schema"`
		Version int    `json:"version"`
		AgentID string `json:"agent_id"`
		Patch   any    `json:"patch"`
	}

	merger := scene.ShallowMerge{}
	state := make(map[string]*scene.Scene)
	dec := json.NewDecoder(f)
	read := uint64(0)
	for {
		var e entry
		if err := dec.Decode(&e); err != nil {
			break
		}
		read++
		if atSeq > 0 && read > atSeq {
			break
		}
		switch e.Kind {
		case "created":
			state[e.SceneID] = &scene.Scene{
				Handle: scene.SceneHandle{
					ID:      e.SceneID,
					Schema:  e.Schema,
					Version: e.Version,
				},
				Content: e.Patch,
			}
		case "patched":
			sc, ok := state[e.SceneID]
			if !ok {
				continue
			}
			sc.Content = merger.Apply(sc.Content, e.Patch)
			sc.Handle.Version = e.Version
		case "deleted":
			delete(state, e.SceneID)
		}
	}

	out := make([]SceneSnap, 0, len(state))
	for _, sc := range state {
		out = append(out, SceneSnap{
			Handle:  sc.Handle,
			AtSeq:   atSeq,
			Content: sc.Content,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle.ID < out[j].Handle.ID })
	return out, nil
}

// SessionAt is a convenience that wraps Session with a non-zero AtSeq.
func SessionAt(ctx context.Context, sessionID string, atSeq uint64, opts Options) (Replay, error) {
	opts.AtSeq = atSeq
	return Session(ctx, sessionID, opts)
}

// Children returns the events whose ParentSeq matches the given seq. Useful
// when consumers want to walk the DAG node-by-node rather than scanning the
// flat Events slice.
func (r Replay) Children(parentSeq uint64) []Event {
	out := make([]Event, 0)
	for _, e := range r.Events {
		if e.ParentSeq == parentSeq {
			out = append(out, e)
		}
	}
	return out
}

// Roots returns events with no parent — typically io.session.start,
// io.input arrivals, and other operator-driven entry points.
func (r Replay) Roots() []Event {
	out := make([]Event, 0)
	for _, e := range r.Events {
		if e.ParentSeq == 0 {
			out = append(out, e)
		}
	}
	return out
}

// ByAgent returns events whose AgentID matches. Sub-agent invocations carry
// their own AgentID per the causation contract, so this is the easiest way
// to scope a replay to a single specialist's work.
func (r Replay) ByAgent(agentID string) []Event {
	out := make([]Event, 0)
	for _, e := range r.Events {
		if e.AgentID == agentID {
			out = append(out, e)
		}
	}
	return out
}
