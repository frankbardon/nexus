package thinking

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.observe.thinking"
	pluginName = "Thinking Observer"
	version    = "0.1.0"
)

// Plugin persists thinking steps and plan progress events to session JSONL
// files. Driven by journal projections rather than live bus subscriptions
// — projection handlers fire after the envelope lands on disk, so the
// thinking files always lag durable state by zero envelopes.
//
// On Init, if the session journal already contains thinking events but
// the derived files are missing (operator deleted them, fresh recall
// after a crash), the plugin walks the journal once via journal.ProjectFile
// to backfill. This is the post-mortem regeneration path the journal
// design was intended to enable.
type Plugin struct {
	logger  *slog.Logger
	session *engine.SessionWorkspace
	journal *journal.Writer
	unsubs  []func()
}

// New creates a new thinking observer plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return pluginName }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.logger = ctx.Logger
	p.session = ctx.Session
	p.journal = ctx.Journal

	if p.journal == nil {
		// Journal is always-on per Phase 1; this branch covers the legacy
		// embedder path that constructed the engine without the journal
		// wired through. Fall back to the bus path so the plugin still
		// produces output rather than silently going dark.
		p.unsubs = append(p.unsubs,
			ctx.Bus.Subscribe("thinking.step", p.handleThinkingStepEvent,
				engine.WithPriority(90), engine.WithSource(pluginID)),
			ctx.Bus.Subscribe("plan.progress", p.handlePlanProgressEvent,
				engine.WithPriority(90), engine.WithSource(pluginID)),
		)
		p.logger.Warn("thinking observer falling back to bus subscription — journal not wired",
			"reason", "PluginContext.Journal nil")
		return nil
	}

	// Backfill: regenerate derived files from the existing journal when
	// the live session is a recall or the operator deleted them between
	// runs. Idempotent — files are append-only, but we only backfill
	// when the file is missing so a normal recall does not duplicate
	// entries from prior runs.
	if err := p.backfillIfMissing(); err != nil {
		p.logger.Warn("thinking observer backfill failed; continuing with live projection only",
			"error", err)
	}

	// Live projection — fired by the writer's drain goroutine after each
	// envelope lands on disk.
	p.unsubs = append(p.unsubs,
		p.journal.SubscribeProjection(
			[]string{"thinking.step", "plan.progress"},
			p.handleProjection,
		),
	)

	p.logger.Info("thinking observer initialized via journal projection")
	return nil
}

// backfillIfMissing walks the journal and rebuilds thinking.jsonl /
// progress.jsonl from scratch if either file does not exist on disk. The
// plugin uses journal.ProjectFile so the same handler that drives live
// emits also drives backfill — single code path, single output format.
func (p *Plugin) backfillIfMissing() error {
	if p.session == nil {
		return nil
	}
	thinkingPath := filepath.Join(p.session.PluginDir(pluginID), "thinking.jsonl")
	progressPath := filepath.Join(p.session.PluginDir(pluginID), "progress.jsonl")

	thinkingMissing := !fileExists(thinkingPath)
	progressMissing := !fileExists(progressPath)
	if !thinkingMissing && !progressMissing {
		return nil
	}

	types := make([]string, 0, 2)
	if thinkingMissing {
		types = append(types, "thinking.step")
	}
	if progressMissing {
		types = append(types, "plan.progress")
	}

	dir := p.journal.JournalDir()
	return journal.ProjectFile(dir, types, p.handleProjection)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "thinking.step", Priority: 90},
		{EventType: "plan.progress", Priority: 90},
	}
}

func (p *Plugin) Emissions() []string {
	return nil
}

// handleProjection is the single sink for both live (writer drain) and
// post-mortem (ProjectFile walk) envelope deliveries. The journal package
// hands envelopes through with Payload typed as either the original
// in-process struct (live writes) or a map[string]any (post-mortem JSON
// round-trip), so both branches go via journal.PayloadAs.
func (p *Plugin) handleProjection(env journal.Envelope) {
	switch env.Type {
	case "thinking.step":
		step, err := journal.PayloadAs[events.ThinkingStep](env.Payload)
		if err != nil {
			return
		}
		p.persistThinkingStep(step)
	case "plan.progress":
		progress, err := journal.PayloadAs[events.PlanProgress](env.Payload)
		if err != nil {
			return
		}
		p.persistPlanProgress(progress)
	}
}

// handleThinkingStepEvent / handlePlanProgressEvent remain for the
// fallback bus-subscription path used when no journal is wired. The
// journal-driven code path goes through handleProjection instead.
func (p *Plugin) handleThinkingStepEvent(event engine.Event[any]) {
	step, ok := event.Payload.(events.ThinkingStep)
	if !ok {
		return
	}
	p.persistThinkingStep(step)
}

func (p *Plugin) handlePlanProgressEvent(event engine.Event[any]) {
	progress, ok := event.Payload.(events.PlanProgress)
	if !ok {
		return
	}
	p.persistPlanProgress(progress)
}

func (p *Plugin) persistThinkingStep(step events.ThinkingStep) {
	ts := step.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	entry := map[string]any{
		"turn_id":   step.TurnID,
		"source":    step.Source,
		"content":   step.Content,
		"phase":     step.Phase,
		"timestamp": ts.Format(time.RFC3339),
	}

	data, err := json.Marshal(entry)
	if err != nil {
		p.logger.Error("failed to marshal thinking step", "error", err)
		return
	}
	data = append(data, '\n')

	path := fmt.Sprintf("plugins/%s/thinking.jsonl", pluginID)
	if err := p.session.AppendFile(path, data); err != nil {
		p.logger.Error("failed to persist thinking step", "error", err)
	}
}

func (p *Plugin) persistPlanProgress(progress events.PlanProgress) {
	entry := map[string]any{
		"turn_id":   progress.TurnID,
		"plan_id":   progress.PlanID,
		"step_id":   progress.StepID,
		"status":    progress.Status,
		"detail":    progress.Detail,
		"timestamp": time.Now().Format(time.RFC3339),
	}

	data, err := json.Marshal(entry)
	if err != nil {
		p.logger.Error("failed to marshal plan progress", "error", err)
		return
	}
	data = append(data, '\n')

	path := fmt.Sprintf("plugins/%s/progress.jsonl", pluginID)
	if err := p.session.AppendFile(path, data); err != nil {
		p.logger.Error("failed to persist plan progress", "error", err)
	}
}
