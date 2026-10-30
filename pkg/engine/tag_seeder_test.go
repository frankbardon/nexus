package engine

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

// readyEmitterPlugin emits a before:llm.request from Ready — the earliest
// point a plugin can start agent work, and inside Lifecycle.Boot. Before
// #148 the tag seeder was subscribed after Lifecycle.Boot and the recall
// branch, so a request emitted here carried no session dimensions at all.
type readyEmitterPlugin struct {
	bus  EventBus
	mu   sync.Mutex
	tags []map[string]string
}

func (p *readyEmitterPlugin) ID() string                         { return "nexus.test.ready_emitter" }
func (p *readyEmitterPlugin) Name() string                       { return p.ID() }
func (p *readyEmitterPlugin) Version() string                    { return "test" }
func (p *readyEmitterPlugin) Dependencies() []string             { return nil }
func (p *readyEmitterPlugin) Requires() []Requirement            { return nil }
func (p *readyEmitterPlugin) Capabilities() []Capability         { return nil }
func (p *readyEmitterPlugin) Subscriptions() []EventSubscription { return nil }
func (p *readyEmitterPlugin) Emissions() []string                { return []string{"before:llm.request"} }
func (p *readyEmitterPlugin) Shutdown(context.Context) error     { return nil }
func (p *readyEmitterPlugin) Init(ctx PluginContext) error       { p.bus = ctx.Bus; return nil }
func (p *readyEmitterPlugin) Ready() error                       { p.emit(); return nil }

func (p *readyEmitterPlugin) emit() {
	req := &events.LLMRequest{
		SchemaVersion: events.LLMRequestVersion,
		Tags:          map[string]string{"source_plugin": p.ID()},
	}
	_, _ = p.bus.EmitVetoable("before:llm.request", req)
	p.mu.Lock()
	p.tags = append(p.tags, req.Tags)
	p.mu.Unlock()
}

func (p *readyEmitterPlugin) last(t *testing.T) map[string]string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.tags) == 0 {
		t.Fatal("probe emitted no request")
	}
	return p.tags[len(p.tags)-1]
}

func newTagSeederEngine(t *testing.T, sessionsRoot string, probe *readyEmitterPlugin) *Engine {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = sessionsRoot
	cfg.Core.Storage.Root = filepath.Dir(sessionsRoot)
	cfg.Plugins.Active = []string{probe.ID()}
	eng := newFromConfig(cfg)
	eng.Registry.Register(probe.ID(), func() Plugin { return probe })
	return eng
}

// A request emitted during boot — from a plugin's Ready, on a fresh boot and
// on a recall — is seeded with session_id; labels written after Boot returns
// (the embedder pattern from #148) reach the next request, because the seeder
// reads them per request rather than once at install.
func TestTagSeeder_SeedsBootAndRecallRequestsAndLateLabels(t *testing.T) {
	sessionsRoot := filepath.Join(t.TempDir(), "sessions")

	probe := &readyEmitterPlugin{}
	eng := newTagSeederEngine(t, sessionsRoot, probe)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	sessionID := eng.Session.ID
	if got := probe.last(t)["session_id"]; got != sessionID {
		t.Fatalf("request from Ready: session_id = %q, want %q", got, sessionID)
	}

	// Embedder writes labels after Boot returns.
	meta, err := eng.Session.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	if meta.Labels == nil {
		meta.Labels = map[string]string{}
	}
	meta.Labels["tenant"] = "acme"
	meta.Labels["user"] = "user:42"
	if err := eng.Session.SaveMeta(meta); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}
	probe.emit()
	got := probe.last(t)
	if got["tenant"] != "acme" || got["user"] != "user:42" || got["session_id"] != sessionID {
		t.Fatalf("post-Boot labels not seeded: %v", got)
	}
	if got["source_plugin"] != probe.ID() {
		t.Fatalf("plugin-set tag clobbered: %v", got)
	}
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Recall: the request emitted during the resumed boot carries the full
	// set — session_id and the labels persisted by the previous run.
	probe2 := &readyEmitterPlugin{}
	eng2 := newTagSeederEngine(t, sessionsRoot, probe2)
	eng2.RecallSessionID = sessionID
	if err := eng2.Boot(context.Background()); err != nil {
		t.Fatalf("recall Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng2.Stop(context.Background()) })
	got = probe2.last(t)
	if got["session_id"] != sessionID || got["tenant"] != "acme" || got["user"] != "user:42" {
		t.Fatalf("request during recall boot not fully seeded: %v", got)
	}
}

// With no session the seeder cannot tag anything; it says so once, not once
// per request.
func TestTagSeeder_WarnsOnceWithoutSession(t *testing.T) {
	var buf bytes.Buffer
	eng := &Engine{
		Bus:    NewEventBus(),
		Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	eng.installTagSeeder()
	for range 3 {
		req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion}
		if _, err := eng.Bus.EmitVetoable("before:llm.request", req); err != nil {
			t.Fatalf("EmitVetoable: %v", err)
		}
	}
	if n := strings.Count(buf.String(), "tag seeder"); n != 1 {
		t.Fatalf("want exactly one warn, got %d:\n%s", n, buf.String())
	}
}

// An unreadable session metadata file must not cost the request its
// session_id — that dimension does not depend on the metadata read.
func TestTagSeeder_SessionIDSurvivesUnreadableMetadata(t *testing.T) {
	bus := NewEventBus()
	ws, err := NewSessionWorkspace(t.TempDir(), bus)
	if err != nil {
		t.Fatalf("NewSessionWorkspace: %v", err)
	}
	var buf bytes.Buffer
	eng := &Engine{
		Bus:     bus,
		Session: ws,
		Logger:  slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	eng.installTagSeeder()
	if err := ws.WriteFile("metadata/session.json", []byte("{not json")); err != nil {
		t.Fatalf("corrupt metadata: %v", err)
	}
	req := &events.LLMRequest{SchemaVersion: events.LLMRequestVersion}
	if _, err := bus.EmitVetoable("before:llm.request", req); err != nil {
		t.Fatalf("EmitVetoable: %v", err)
	}
	if req.Tags["session_id"] != ws.ID {
		t.Fatalf("session_id = %q, want %q", req.Tags["session_id"], ws.ID)
	}
	if !strings.Contains(buf.String(), "metadata unreadable") {
		t.Fatalf("expected metadata warn, got:\n%s", buf.String())
	}
}
