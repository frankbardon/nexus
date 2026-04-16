package thinking

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.observe.thinking"
	pluginName = "Thinking Observer"
	version    = "0.1.0"
)

// Plugin persists thinking steps and plan progress events to session JSONL files.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace
	unsubs  []func()
}

// New creates a new thinking observer plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return pluginName }
func (p *Plugin) Version() string        { return version }
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session

	// Low priority (90) — observer role, runs after primary handlers.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("thinking.step", p.handleThinkingStepEvent,
			engine.WithPriority(90), engine.WithSource(pluginID)),
		p.bus.Subscribe("plan.progress", p.handlePlanProgressEvent,
			engine.WithPriority(90), engine.WithSource(pluginID)),
	)

	p.logger.Info("thinking observer initialized")
	return nil
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
