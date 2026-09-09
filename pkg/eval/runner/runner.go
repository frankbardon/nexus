// Package runner executes a single eval case. It is a thin wrapper around
// engine.Replay() and the journal projection: a fresh engine is constructed
// from the case's config bytes, the journal is loaded, the deterministic
// stash is seeded, the io.input events are re-fired, and observed events
// are collected via a side-channel subscription. Side-effecting plugins
// (LLM providers, tools) detect engine.Replay.Active() and pop stashed
// responses instead of calling out, so no API key is required.
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/allplugins"
	"github.com/frankbardon/nexus/pkg/engine/journal"
	evalcase "github.com/frankbardon/nexus/pkg/eval/case"
	"github.com/frankbardon/nexus/pkg/eval/internal/livedrive"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/events/compat"
)

// Result is everything Run produces about a single case execution.
type Result struct {
	CaseID     string                     `json:"case_id"`
	StartedAt  time.Time                  `json:"started_at"`
	EndedAt    time.Time                  `json:"ended_at"`
	Pass       bool                       `json:"pass"`
	Assertions []evalcase.AssertionResult `json:"assertions"`
	// Counts is a per-event-type histogram for the observed (replayed)
	// stream. Useful for the Phase 2 baseline differ.
	Counts map[string]int `json:"counts,omitempty"`
	// JournalDir is the case's golden journal dir, echoed back for the
	// reporter's convenience.
	JournalDir string `json:"journal_dir,omitempty"`
	// Transcript is a rendered, legible plain-text rendering of the
	// session's full, unfiltered observed event stream (see
	// RenderTranscript). Populated for both Run and RunLive — they share
	// buildResult's tail — but it exists primarily for RunLive: a
	// downstream judge call feeds it as protocol.Request.UserInput
	// alongside a rubric (E2-S3).
	Transcript string `json:"transcript,omitempty"`
	// CustomScores is caller-populated — neither Run nor RunLive sets it.
	// An external caller (e.g. after computing a correctness check or a
	// judge verdict) sets it on the Result it already holds before
	// passing the Result to report.Aggregate, which copies it onto the
	// corresponding CaseEntry.
	CustomScores map[string]float64 `json:"custom_scores,omitempty"`
}

// Options tune Run.
type Options struct {
	// Logger, when non-nil, is used by the engine. Defaults to a discard
	// handler so test output stays clean.
	Logger *slog.Logger
	// BootTimeout caps engine boot.
	BootTimeout time.Duration
	// ReplayTimeout caps the replay run (per-turn timeout lives in the
	// journal coordinator; this is the overall ceiling).
	ReplayTimeout time.Duration
	// SessionsRoot, when non-empty, is the absolute path the engine should
	// use as core.sessions.root. Tests pass t.TempDir(); production passes
	// the empty string and accepts the case-config default.
	SessionsRoot string
	// ExtraPlugins registers additional plugin factories onto the engine's
	// registry, keyed by plugin ID, alongside the built-ins
	// allplugins.RegisterAll already provides. This is an embedder's seam
	// for booting cases that reference their own (non-nexus.*) plugins —
	// e.g. an eval harness embedded in another module, exercising its own
	// tools or agents.
	//
	// Registering an ID here only makes its factory available to the
	// engine's registry; it does not activate the plugin. The case's own
	// config (`plugins.active`) must still name the ID for the engine to
	// construct and Init it during Boot — exactly as for any built-in.
	//
	// An ID that collides with a built-in registered by
	// allplugins.RegisterAll is not guarded against: entries here are
	// registered after the built-ins, so PluginRegistry.Register's existing
	// silent-overwrite behavior means an ExtraPlugins entry always wins over
	// a same-ID built-in. This is deliberate, not an oversight — it lets an
	// embedder shadow a built-in for testing.
	//
	// A nil or empty map changes nothing about existing behavior.
	ExtraPlugins map[string]engine.PluginFactory
}

// Run executes one case end-to-end and returns its Result.
//
// The flow:
//
//  1. Decode the case config (raw YAML stored on the case so the engine
//     owns parsing).
//  2. Optionally override core.sessions.root (tests use t.TempDir()).
//  3. engine.NewFromBytes → allplugins.RegisterAll → Boot.
//  4. Subscribe a wildcard collector for the observed stream.
//  5. Drive replay via engine.Replay (a thin wrapper around
//     journal.NewCoordinator).
//  6. Stop the engine. Project the case's golden journal into a stream,
//     evaluate assertions.
func Run(ctx context.Context, c *evalcase.Case, opts Options) (*Result, error) {
	if c == nil {
		return nil, fmt.Errorf("nil case")
	}
	if opts.BootTimeout == 0 {
		opts.BootTimeout = 30 * time.Second
	}
	if opts.ReplayTimeout == 0 {
		opts.ReplayTimeout = 60 * time.Second
	}

	cfgBytes := c.ConfigYAML
	if opts.SessionsRoot != "" {
		// The engine accepts overrides via the YAML body; rather than mutate
		// a parsed Config (the embedder pattern explicitly forbids that),
		// rewrite the bytes with a small YAML overlay below the top-level
		// `core.sessions.root`. This is intentionally surgical: a full YAML
		// merge would require pulling in extra deps.
		var err error
		cfgBytes, err = overrideSessionsRoot(cfgBytes, opts.SessionsRoot)
		if err != nil {
			return nil, fmt.Errorf("override sessions root: %w", err)
		}
	}

	eng, err := bootEngine(ctx, cfgBytes, opts)
	if err != nil {
		return nil, err
	}

	// Belt-and-braces shutdown: the explicit Stop below runs first; this
	// defer covers the early-error paths between Boot and that explicit
	// Stop call. eng.Stop is idempotent for the journal-close path.
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_ = eng.Stop(stopCtx)
		cancel()
	}()

	// Side-channel wildcard collector — useful as a fallback when the live
	// session's journal is not yet flushed (Stop ordering races) and as a
	// debugging aid. The authoritative observed stream below is read from
	// the live session's journal in causally-correct seq order. Wildcard
	// dispatch is post-order (children emit + complete before the parent
	// emit's wildcard fires) which would skew event_sequence_strict and
	// event_sequence_distance.
	var (
		mu       sync.Mutex
		observed []evalcase.ObservedEvent
	)
	unsub := eng.Bus.SubscribeAll(func(ev engine.Event[any]) {
		mu.Lock()
		observed = append(observed, evalcase.ObservedEvent{
			Type:      ev.Type,
			Timestamp: ev.Timestamp,
			Payload:   ev.Payload,
		})
		mu.Unlock()
	})
	defer unsub()

	// Capture the live session dir before Stop tears it down.
	liveJournalDir := ""
	if eng.Session != nil {
		liveJournalDir = filepath.Join(eng.Session.RootDir, "journal")
	}

	// Drive replay. Coordinator handles seeding the FIFO stash with
	// llm.response/tool.result/io.ask.response payloads from the journal
	// and re-firing io.inputs in seq order.
	replayCtx, replayCancel := context.WithTimeout(ctx, opts.ReplayTimeout)
	defer replayCancel()

	if err := replay(replayCtx, eng, c.JournalDir); err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}

	// Stop the engine before reading the live journal so the writer flushes
	// in-flight envelopes. The deferred Stop above also runs (idempotent),
	// but pulling it forward here makes the journal observation atomic.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := eng.Stop(stopCtx); err != nil {
		stopCancel()
		// Continue anyway — we may still have a partially-written journal.
		eng.Logger.Warn("eval runner: engine stop failed", "error", err)
	} else {
		stopCancel()
	}

	// Authoritative observed stream: project the live session's journal in
	// seq order. Falls back to the wildcard collector if the live journal
	// could not be read.
	var finalObserved []evalcase.ObservedEvent
	if liveJournalDir != "" {
		if live, err := loadJournal(liveJournalDir); err == nil {
			finalObserved = live
		}
	}
	if finalObserved == nil {
		mu.Lock()
		finalObserved = append([]evalcase.ObservedEvent(nil), observed...)
		mu.Unlock()
	}

	return buildResult(c, finalObserved)
}

// RunLive executes one case's Inputs end-to-end against a real, live engine
// boot — no engine.Replay stash, real LLM calls (or, in tests, a case
// config whose own nexus.io.test plugin mocks the LLM response itself, the
// same pattern pkg/eval/protocol's own tests use — see
// plugins/io/test's mock_responses). It is a third execution path,
// structurally distinct from Run's replay: instead of replaying a journal
// stash, it overlays the case's config so a live nexus.io.test transport
// fires c.Inputs and signals natural completion, then waits for that
// completion via the same "wait to idle" mechanics protocol.Run uses
// (factored into pkg/eval/internal/livedrive.WaitForIdle so neither package
// reimplements it).
//
// Otherwise RunLive mirrors Run closely: identical Options/Result shapes,
// identical ExtraPlugins registration, and the same
// Assertions.Deterministic evaluation against (live-observed, golden) —
// so EventSequenceDistance/ToolInvocationParity still catch structural
// drift between a live run and the golden journal even though the LLM call
// itself isn't replayed from a stash.
func RunLive(ctx context.Context, c *evalcase.Case, opts Options) (*Result, error) {
	if c == nil {
		return nil, fmt.Errorf("nil case")
	}
	if opts.BootTimeout == 0 {
		opts.BootTimeout = 30 * time.Second
	}
	if opts.ReplayTimeout == 0 {
		opts.ReplayTimeout = 60 * time.Second
	}

	cfgBytes := c.ConfigYAML
	if opts.SessionsRoot != "" {
		var err error
		cfgBytes, err = overrideSessionsRoot(cfgBytes, opts.SessionsRoot)
		if err != nil {
			return nil, fmt.Errorf("override sessions root: %w", err)
		}
	}
	cfgBytes, err := overlayLiveInputs(cfgBytes, c.Inputs)
	if err != nil {
		return nil, fmt.Errorf("overlay live inputs: %w", err)
	}

	eng, err := bootEngine(ctx, cfgBytes, opts)
	if err != nil {
		return nil, err
	}

	// Belt-and-braces shutdown — same rationale as Run's.
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_ = eng.Stop(stopCtx)
		cancel()
	}()

	// Side-channel wildcard collector — same fallback rationale as Run's.
	var (
		mu       sync.Mutex
		observed []evalcase.ObservedEvent
	)
	unsub := eng.Bus.SubscribeAll(func(ev engine.Event[any]) {
		mu.Lock()
		observed = append(observed, evalcase.ObservedEvent{
			Type:      ev.Type,
			Timestamp: ev.Timestamp,
			Payload:   ev.Payload,
		})
		mu.Unlock()
	})
	defer unsub()

	liveJournalDir := ""
	if eng.Session != nil {
		liveJournalDir = filepath.Join(eng.Session.RootDir, "journal")
	}

	// Drive: nexus.io.test (ensured active by overlayLiveInputs above) fires
	// c.Inputs itself once booted; here we just wait for it to signal
	// natural completion (or the ctx deadline to run out — RunLive passes
	// maxTurns=0, disabling livedrive's turn-cap path entirely, since a
	// case's Inputs list — not a turn count — is what bounds a live run).
	driveCtx, driveCancel := context.WithTimeout(ctx, opts.ReplayTimeout)
	defer driveCancel()
	if _, err := livedrive.WaitForIdle(driveCtx, eng, 0); err != nil {
		return nil, fmt.Errorf("live drive: %w", err)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := eng.Stop(stopCtx); err != nil {
		stopCancel()
		eng.Logger.Warn("eval runner: engine stop failed", "error", err)
	} else {
		stopCancel()
	}

	var finalObserved []evalcase.ObservedEvent
	if liveJournalDir != "" {
		if live, err := loadJournal(liveJournalDir); err == nil {
			finalObserved = live
		}
	}
	if finalObserved == nil {
		mu.Lock()
		finalObserved = append([]evalcase.ObservedEvent(nil), observed...)
		mu.Unlock()
	}

	return buildResult(c, finalObserved)
}

// bootEngine constructs and boots a fresh engine from cfgBytes, applying
// opts.Logger and opts.ExtraPlugins identically for every eval-execution
// path — Run's replay and RunLive's live drive both call this, so
// allplugins.RegisterAll + the ExtraPlugins registration loop has exactly
// one call site.
func bootEngine(ctx context.Context, cfgBytes []byte, opts Options) (*engine.Engine, error) {
	eng, err := engine.NewFromBytes(cfgBytes)
	if err != nil {
		return nil, fmt.Errorf("engine.NewFromBytes: %w", err)
	}
	if opts.Logger != nil {
		eng.Logger = opts.Logger
	}
	allplugins.RegisterAll(eng.Registry)
	for id, factory := range opts.ExtraPlugins {
		eng.Registry.Register(id, factory)
	}

	bootCtx, bootCancel := context.WithTimeout(ctx, opts.BootTimeout)
	defer bootCancel()
	if err := eng.Boot(bootCtx); err != nil {
		return nil, fmt.Errorf("boot: %w", err)
	}
	return eng, nil
}

// buildResult loads the case's golden journal and evaluates its
// Assertions.Deterministic against finalObserved — the tail both Run
// (replayed observed stream) and RunLive (live-observed stream) share.
func buildResult(c *evalcase.Case, finalObserved []evalcase.ObservedEvent) (*Result, error) {
	golden, err := loadJournal(c.JournalDir)
	if err != nil {
		return nil, fmt.Errorf("load golden journal: %w", err)
	}

	res := &Result{
		CaseID:     c.ID,
		StartedAt:  time.Now(), // overwritten with first observed timestamp below
		Pass:       true,
		Counts:     make(map[string]int),
		JournalDir: c.JournalDir,
		Transcript: RenderTranscript(finalObserved),
	}
	if len(finalObserved) > 0 {
		res.StartedAt = finalObserved[0].Timestamp
		res.EndedAt = finalObserved[len(finalObserved)-1].Timestamp
	}
	for _, e := range finalObserved {
		res.Counts[e.Type]++
	}
	for _, a := range c.Assertions.Deterministic {
		ar := a.Evaluate(finalObserved, golden)
		res.Assertions = append(res.Assertions, ar)
		if !ar.Pass {
			res.Pass = false
		}
	}

	return res, nil
}

// replay opens the case journal via journal.NewCoordinator on the live
// engine's bus + replay state and runs it. Mirrors the production
// engine.ReplaySession path but reads the journal from the case dir
// instead of resolving via core.sessions.root, so cases can live anywhere.
func replay(ctx context.Context, eng *engine.Engine, journalDir string) error {
	coord, err := journal.NewCoordinator(journalDir, eng.Bus, eng.Replay, journal.CoordinatorOptions{
		TurnTimeout:      30 * time.Second,
		Logger:           eng.Logger.With("subsystem", "eval-replay"),
		PayloadConverter: replayPayloadConverter,
	})
	if err != nil {
		return fmt.Errorf("coordinator: %w", err)
	}
	coord.AttachTurnSync(func(handler func(seq uint64)) func() {
		return eng.Bus.Subscribe("agent.turn.end", func(_ engine.Event[any]) {
			handler(0)
		})
	})
	return coord.Run(ctx)
}

// replayPayloadConverter mirrors engine.replayPayloadConverter (which is
// unexported). The engine's coordinator path uses its own converter; the
// runner's case-driven path needs the same conversion table so io.input
// payloads land as typed events.UserInput. Routes recorded payloads
// through pkg/events/compat to lift older schema versions to the running
// version before re-typing.
func replayPayloadConverter(eventType string, payload any) (any, error) {
	switch eventType {
	case "io.input":
		return convertVersioned[events.UserInput](eventType, events.UserInputVersion, payload)
	case "llm.response":
		return convertVersioned[events.LLMResponse](eventType, events.LLMResponseVersion, payload)
	case "tool.result":
		return convertVersioned[events.ToolResult](eventType, events.ToolResultVersion, payload)
	default:
		return payload, nil
	}
}

func convertVersioned[T any](eventType string, currentVer int, payload any) (T, error) {
	if m, ok := payload.(map[string]any); ok {
		recorded := 0
		if v, ok := m["_schema_version"]; ok {
			switch n := v.(type) {
			case float64:
				recorded = int(n)
			case int:
				recorded = n
			}
		}
		if recorded == 0 {
			recorded = currentVer
		}
		migrated, err := compat.Apply(eventType, recorded, currentVer, m)
		if err != nil {
			var zero T
			return zero, fmt.Errorf("compat: %w", err)
		}
		payload = migrated
	}
	return journal.PayloadAs[T](payload)
}

// loadJournal reads every envelope out of a journal directory into an
// ObservedEvent slice. Used both for the case's golden journal (drift
// assertions) and for the live session's freshly-written journal (the
// authoritative observed stream).
func loadJournal(journalDir string) ([]evalcase.ObservedEvent, error) {
	r, err := journal.Open(journalDir)
	if err != nil {
		return nil, err
	}
	var out []evalcase.ObservedEvent
	err = r.Iter(func(e journal.Envelope) bool {
		out = append(out, evalcase.ObservedEvent{
			Type:      e.Type,
			Timestamp: e.Ts,
			Payload:   e.Payload,
		})
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// overrideSessionsRoot rewrites the YAML's core.sessions.root key to a new
// absolute path. The implementation parses to a generic node, surgical-
// edits, and re-marshals — preserves the rest of the config exactly.
func overrideSessionsRoot(in []byte, root string) ([]byte, error) {
	// Use yaml.v3 node-level API to keep formatting reasonably stable.
	// Defer the import here to avoid pulling extra surface into the file
	// header; gopkg.in/yaml.v3 is already a project-wide dep.
	return rewriteCoreSessionsRoot(in, root)
}

// rewriteCoreSessionsRoot is split out so it can be unit-tested without a
// full Run.
func rewriteCoreSessionsRoot(in []byte, root string) ([]byte, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// Defer-import the yaml package via a small helper. Done inline to keep
	// the surface flat.
	return yamlSetCoreSessionsRoot(in, abs)
}
