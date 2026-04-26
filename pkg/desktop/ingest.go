package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// ingestRecentLimit caps how many completed ingestions we keep per agent.
// Memory-only; nothing persists across shell restarts.
const ingestRecentLimit = 50

// IngestEntry is the serializable view of a single ingestion sent to the
// frontend. The same shape covers both in-flight and finished entries —
// CompletedAt / Chunks / Error are zero values while Status == "active".
type IngestEntry struct {
	ID            string     `json:"id"`
	Path          string     `json:"path"`
	Namespace     string     `json:"namespace"`
	Status        string     `json:"status"` // active | completed | failed
	StartedAt     time.Time  `json:"started_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	Chunks        int        `json:"chunks"`
	SkippedCached int        `json:"skipped_cached"`
	Error         string     `json:"error,omitempty"`
}

// IngestState is the per-agent projection returned by GetIngestState and
// pushed on every change via the {agentID}:ingest.updated Wails event.
type IngestState struct {
	Active []IngestEntry `json:"active"`
	Recent []IngestEntry `json:"recent"`
}

// ingestTracker tracks RAG ingestion activity across all agent engines
// owned by the shell. The key inside the active map is the *RAGIngest
// pointer the bus carries — the same pointer flows through "rag.ingest"
// and "rag.ingest.result", so it's a stable correlation id without any
// synthetic key scheme.
type ingestTracker struct {
	mu     sync.Mutex
	agents map[string]*agentIngest
}

type agentIngest struct {
	active map[*events.RAGIngest]*IngestEntry
	recent []IngestEntry
	seq    uint64
}

func newIngestTracker() *ingestTracker {
	return &ingestTracker{agents: make(map[string]*agentIngest)}
}

// state returns a deep-enough copy of the agent's projection that the
// caller can hand it to JSON marshalling without holding the mutex.
func (t *ingestTracker) state(agentID string) IngestState {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := IngestState{Active: []IngestEntry{}, Recent: []IngestEntry{}}
	a := t.agents[agentID]
	if a == nil {
		return out
	}
	for _, e := range a.active {
		out.Active = append(out.Active, *e)
	}
	out.Recent = append(out.Recent, a.recent...)
	return out
}

// install subscribes to rag.ingest (high priority so the "active" entry
// is visible to the frontend before the priority-50 ingest plugin runs
// the synchronous read+chunk+embed+upsert work) and rag.ingest.result
// (default priority — fires after the work completes).
func (t *ingestTracker) install(ctx context.Context, eng *engine.Engine, agentID string) []func() {
	if eng == nil || eng.Bus == nil {
		return nil
	}
	var unsubs []func()

	unsubs = append(unsubs, eng.Bus.Subscribe("rag.ingest", func(ev engine.Event[any]) {
		req, ok := ev.Payload.(*events.RAGIngest)
		if !ok {
			return
		}
		t.start(agentID, req)
		emitIngestUpdated(ctx, agentID, t.state(agentID))
	}, engine.WithPriority(10)))

	unsubs = append(unsubs, eng.Bus.Subscribe("rag.ingest.result", func(ev engine.Event[any]) {
		req, ok := ev.Payload.(*events.RAGIngest)
		if !ok {
			return
		}
		t.complete(agentID, req)
		emitIngestUpdated(ctx, agentID, t.state(agentID))
	}))

	return unsubs
}

func (t *ingestTracker) start(agentID string, req *events.RAGIngest) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.agents[agentID]
	if a == nil {
		a = &agentIngest{active: make(map[*events.RAGIngest]*IngestEntry)}
		t.agents[agentID] = a
	}
	a.seq++
	a.active[req] = &IngestEntry{
		ID:        fmt.Sprintf("ing-%d-%d", time.Now().UnixNano(), a.seq),
		Path:      req.Path,
		Namespace: req.Namespace,
		Status:    "active",
		StartedAt: time.Now(),
	}
}

func (t *ingestTracker) complete(agentID string, req *events.RAGIngest) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.agents[agentID]
	if a == nil {
		a = &agentIngest{active: make(map[*events.RAGIngest]*IngestEntry)}
		t.agents[agentID] = a
	}
	e := a.active[req]
	if e != nil {
		delete(a.active, req)
	} else {
		// Result without a matching start — happens when subs were installed
		// after the start event fired (Boot returns just before we install).
		// Synthesize an entry so the user still sees the result.
		a.seq++
		e = &IngestEntry{
			ID:        fmt.Sprintf("ing-%d-%d", time.Now().UnixNano(), a.seq),
			Path:      req.Path,
			Namespace: req.Namespace,
			StartedAt: time.Now(),
		}
	}
	now := time.Now()
	e.CompletedAt = &now
	e.Chunks = req.Chunks
	e.SkippedCached = req.SkippedCached
	if req.Error != "" {
		e.Status = "failed"
		e.Error = req.Error
	} else {
		e.Status = "completed"
	}
	a.recent = append([]IngestEntry{*e}, a.recent...)
	if len(a.recent) > ingestRecentLimit {
		a.recent = a.recent[:ingestRecentLimit]
	}
}

// clearActive drops any in-flight entries for an agent. Called on
// StopAgent — when the engine shuts down, in-flight ingestions are
// killed mid-flight and the result event will never arrive.
func (t *ingestTracker) clearActive(agentID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.agents[agentID]
	if a == nil {
		return
	}
	a.active = make(map[*events.RAGIngest]*IngestEntry)
}

func emitIngestUpdated(ctx context.Context, agentID string, state IngestState) {
	if ctx == nil {
		return
	}
	data, err := json.Marshal(state)
	if err != nil {
		log.Printf("ingest tracker: marshal failed: %v", err)
		return
	}
	wailsruntime.EventsEmit(ctx, agentID+":ingest.updated", string(data))
}
