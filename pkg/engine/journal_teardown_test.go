package engine

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/events"
)

const teardownProbeEvent = "test.teardown.probe"

// teardownEmitterPlugin emits teardownProbeEvent from its Shutdown, standing in
// for any plugin that reports something on the way out.
type teardownEmitterPlugin struct {
	bus EventBus
}

func (p *teardownEmitterPlugin) ID() string                         { return "nexus.test.teardown_emitter" }
func (p *teardownEmitterPlugin) Name() string                       { return p.ID() }
func (p *teardownEmitterPlugin) Version() string                    { return "test" }
func (p *teardownEmitterPlugin) Dependencies() []string             { return nil }
func (p *teardownEmitterPlugin) Requires() []Requirement            { return nil }
func (p *teardownEmitterPlugin) Capabilities() []Capability         { return nil }
func (p *teardownEmitterPlugin) Subscriptions() []EventSubscription { return nil }
func (p *teardownEmitterPlugin) Emissions() []string                { return []string{teardownProbeEvent} }
func (p *teardownEmitterPlugin) Ready() error                       { return nil }
func (p *teardownEmitterPlugin) Init(ctx PluginContext) error {
	p.bus = ctx.Bus
	return nil
}
func (p *teardownEmitterPlugin) Shutdown(context.Context) error {
	return p.bus.Emit(teardownProbeEvent, map[string]any{"bye": true})
}

func readJournalEnvelopes(t *testing.T, dir string) []journal.Envelope {
	t.Helper()
	r, err := journal.Open(dir)
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	var out []journal.Envelope
	if err := r.Iter(func(e journal.Envelope) bool {
		out = append(out, e)
		return true
	}); err != nil {
		t.Fatalf("journal Iter: %v", err)
	}
	return out
}

func envelopeTypes(envs []journal.Envelope) []string {
	out := make([]string, len(envs))
	for i, e := range envs {
		out[i] = e.Type
	}
	return out
}

// runOneTurnAndStop boots eng, records one complete turn on the bus, stops it,
// and returns the session's journal directory.
func runOneTurnAndStop(t *testing.T, eng *Engine) string {
	t.Helper()
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	_ = eng.Bus.Emit("io.input", events.UserInput{Content: "hello"})
	_ = eng.Bus.Emit("agent.turn.start", events.TurnInfo{TurnID: "t1"})
	_ = eng.Bus.Emit("agent.turn.end", events.TurnInfo{TurnID: "t1"})
	dir := filepath.Join(eng.Session.RootDir, "journal")
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	return dir
}

// Stop journals its own teardown: core.shutdown and whatever plugins emit from
// Shutdown reach disk, in that order, after the last turn; and the journal's
// wildcard is gone once Stop returns.
func TestStop_JournalRecordsTeardownEvents(t *testing.T) {
	eng := newBootRetryEngine(t, &teardownEmitterPlugin{})
	before := busSubscriptionCounts(t, eng.Bus)

	dir := runOneTurnAndStop(t, eng)

	types := envelopeTypes(readJournalEnvelopes(t, dir))
	shutdownAt := slices.Index(types, "core.shutdown")
	probeAt := slices.Index(types, teardownProbeEvent)
	turnEndAt := slices.Index(types, "agent.turn.end")
	if shutdownAt < 0 {
		t.Fatalf("journal has no core.shutdown: %v", types)
	}
	if probeAt < 0 {
		t.Fatalf("journal has no event emitted during plugin Shutdown: %v", types)
	}
	if !(turnEndAt < shutdownAt && shutdownAt < probeAt) {
		t.Fatalf("want agent.turn.end < core.shutdown < %s, got indices %d, %d, %d in %v",
			teardownProbeEvent, turnEndAt, shutdownAt, probeAt, types)
	}

	if got := busSubscriptionCounts(t, eng.Bus); !equalCounts(got, before) {
		t.Fatalf("subscriptions after Stop = %v, want pre-Boot %v (journal wildcard leaked?)", got, before)
	}
}

// Replay over a journal that ends in teardown events re-emits only the
// recorded input: core.shutdown and the plugin's teardown event are not
// re-run, and the trailing envelopes do not read as a partial turn.
func TestReplay_ToleratesTrailingTeardownEvents(t *testing.T) {
	dir := runOneTurnAndStop(t, newBootRetryEngine(t, &teardownEmitterPlugin{}))

	if !slices.Contains(envelopeTypes(readJournalEnvelopes(t, dir)), "core.shutdown") {
		t.Fatal("precondition: journal has no core.shutdown")
	}

	bus := NewEventBus()
	var mu sync.Mutex
	var emitted []string
	unsub := bus.SubscribeAll(func(ev Event[any]) {
		mu.Lock()
		emitted = append(emitted, ev.Type)
		mu.Unlock()
	})
	defer unsub()

	coord, err := journal.NewCoordinator(dir, bus, NewReplayState(), journal.CoordinatorOptions{
		PayloadConverter: replayPayloadConverter,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	if coord.IsPartialTurn() {
		t.Fatal("journal ending in teardown events reads as a partial turn")
	}
	if err := coord.Run(context.Background()); err != nil {
		t.Fatalf("replay Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(emitted, []string{"io.input"}) {
		t.Fatalf("replay emitted %v, want only [io.input]", emitted)
	}
}

// Recalling a session whose journal ends in teardown events boots cleanly,
// does not re-fire the completed turn's input, and continues the journal's
// seq without gaps or repeats.
func TestRecall_ToleratesTrailingTeardownEvents(t *testing.T) {
	first := newBootRetryEngine(t, &teardownEmitterPlugin{})
	dir := runOneTurnAndStop(t, first)
	envs := readJournalEnvelopes(t, dir)
	lastSeq := envs[len(envs)-1].Seq

	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = first.Config.Core.Sessions.Root
	cfg.Core.Storage.Root = first.Config.Core.Storage.Root
	p := &teardownEmitterPlugin{}
	cfg.Plugins.Active = []string{p.ID()}
	second := newFromConfig(cfg)
	second.Registry.Register(p.ID(), func() Plugin { return p })
	second.RecallSessionID = first.Session.ID

	var mu sync.Mutex
	inputs := 0
	unsub := second.Bus.Subscribe("io.input", func(Event[any]) {
		mu.Lock()
		inputs++
		mu.Unlock()
	})
	defer unsub()

	if err := second.Boot(context.Background()); err != nil {
		t.Fatalf("recall Boot: %v", err)
	}
	if err := second.Stop(context.Background()); err != nil {
		t.Fatalf("recall Stop: %v", err)
	}

	mu.Lock()
	if inputs != 0 {
		t.Fatalf("recall re-fired %d io.input, want 0 (turn had completed)", inputs)
	}
	mu.Unlock()

	all := readJournalEnvelopes(t, dir)
	if len(all) <= len(envs) {
		t.Fatalf("recalled run journaled nothing: %d envelopes before, %d after", len(envs), len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].Seq != all[i-1].Seq+1 {
			t.Fatalf("journal seq not contiguous at %d: %d then %d", i, all[i-1].Seq, all[i].Seq)
		}
	}
	if all[len(envs)].Seq != lastSeq+1 {
		t.Fatalf("recalled run started at seq %d, want %d", all[len(envs)].Seq, lastSeq+1)
	}
}

// drainFinisherPlugin finishes an in-flight turn from a core.shutdown handler,
// standing in for an agent whose turn completes during the shutdown drain, and
// then emits a tool.result from its Shutdown, after that turn has ended.
type drainFinisherPlugin struct {
	bus EventBus
}

func (p *drainFinisherPlugin) ID() string                 { return "nexus.test.drain_finisher" }
func (p *drainFinisherPlugin) Name() string               { return p.ID() }
func (p *drainFinisherPlugin) Version() string            { return "test" }
func (p *drainFinisherPlugin) Dependencies() []string     { return nil }
func (p *drainFinisherPlugin) Requires() []Requirement    { return nil }
func (p *drainFinisherPlugin) Capabilities() []Capability { return nil }
func (p *drainFinisherPlugin) Subscriptions() []EventSubscription {
	return []EventSubscription{{EventType: "core.shutdown"}}
}
func (p *drainFinisherPlugin) Emissions() []string {
	return []string{"llm.response", "agent.turn.end", "tool.result"}
}
func (p *drainFinisherPlugin) Ready() error { return nil }
func (p *drainFinisherPlugin) Init(ctx PluginContext) error {
	p.bus = ctx.Bus
	ctx.Bus.Subscribe("core.shutdown", func(Event[any]) {
		_, _ = PublishLLMResponse(p.bus, events.LLMResponse{Content: "finished in drain"})
		_ = p.bus.Emit("agent.turn.end", events.TurnInfo{TurnID: "t1"})
	})
	return nil
}
func (p *drainFinisherPlugin) Shutdown(context.Context) error {
	return p.bus.Emit("tool.result", events.ToolResult{ID: "late", Output: "stale"})
}

// Stop lands mid-turn and the turn finishes during the core.shutdown drain:
// the journal records its agent.turn.end after core.shutdown, so -recall sees
// a completed turn and does not re-fire its input. Replay stashes the turn's
// own drain-time answer but not the tool.result emitted once it had ended.
func TestRecall_TurnFinishingInShutdownDrainIsComplete(t *testing.T) {
	first := newBootRetryEngine(t, &drainFinisherPlugin{})
	if err := first.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	_ = first.Bus.Emit("io.input", events.UserInput{Content: "hello"})
	_ = first.Bus.Emit("agent.turn.start", events.TurnInfo{TurnID: "t1"})
	dir := filepath.Join(first.Session.RootDir, "journal")
	if err := first.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	types := envelopeTypes(readJournalEnvelopes(t, dir))
	shutdownAt := slices.Index(types, "core.shutdown")
	turnEndAt := slices.Index(types, "agent.turn.end")
	if shutdownAt < 0 || turnEndAt < shutdownAt {
		t.Fatalf("precondition: want agent.turn.end after core.shutdown, got %v", types)
	}

	rep := NewReplayState()
	coord, err := journal.NewCoordinator(dir, NewEventBus(), rep, journal.CoordinatorOptions{
		PayloadConverter: replayPayloadConverter,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	if coord.IsPartialTurn() {
		t.Fatal("turn that ended in the shutdown drain reads as partial")
	}
	if n := rep.Remaining("llm.response"); n != 1 {
		t.Fatalf("llm.response stash = %d, want 1 (the drain-time answer)", n)
	}
	if n := rep.Remaining("tool.result"); n != 0 {
		t.Fatalf("tool.result stash = %d, want 0 (emitted after the turn ended)", n)
	}

	cfg := DefaultConfig()
	cfg.Core.Sessions.Root = first.Config.Core.Sessions.Root
	cfg.Core.Storage.Root = first.Config.Core.Storage.Root
	p := &drainFinisherPlugin{}
	cfg.Plugins.Active = []string{p.ID()}
	second := newFromConfig(cfg)
	second.Registry.Register(p.ID(), func() Plugin { return p })
	second.RecallSessionID = first.Session.ID

	var mu sync.Mutex
	inputs := 0
	unsub := second.Bus.Subscribe("io.input", func(Event[any]) {
		mu.Lock()
		inputs++
		mu.Unlock()
	})
	defer unsub()

	if err := second.Boot(context.Background()); err != nil {
		t.Fatalf("recall Boot: %v", err)
	}
	mu.Lock()
	got := inputs
	mu.Unlock()
	if err := second.Stop(context.Background()); err != nil {
		t.Fatalf("recall Stop: %v", err)
	}
	if got != 0 {
		t.Fatalf("recall re-fired %d io.input, want 0 (turn finished in the drain)", got)
	}
}
