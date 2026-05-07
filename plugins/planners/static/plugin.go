package static

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.planner.static"
	pluginName = "Static Planner"
	version    = "0.1.0"
)

type approvalMode string

const (
	approvalAlways approvalMode = "always"
	approvalNever  approvalMode = "never"
	approvalAuto   approvalMode = "auto"
)

// Plugin implements a static planner that always uses the same configured plan.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace

	approval approvalMode
	summary  string
	steps    []configStep

	pendingPlanID string
	pendingTurnID string
	unsubs        []func()
}

type configStep struct {
	Description  string `yaml:"description"`
	Instructions string `yaml:"instructions"`
}

// New creates a new static planner plugin.
func New() engine.Plugin {
	return &Plugin{
		approval: approvalNever,
	}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return pluginName }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session

	// Parse approval mode.
	if mode, ok := ctx.Config["approval"].(string); ok {
		switch approvalMode(mode) {
		case approvalAlways:
			p.approval = approvalAlways
		case approvalAuto:
			// Auto defaults to never for static planner.
			p.approval = approvalNever
		default:
			p.approval = approvalNever
		}
	}

	// Parse summary.
	if s, ok := ctx.Config["summary"].(string); ok {
		p.summary = s
	}
	if p.summary == "" {
		p.summary = "Static execution plan"
	}

	// Parse steps from config.
	if rawSteps, ok := ctx.Config["steps"].([]any); ok {
		for _, raw := range rawSteps {
			if m, ok := raw.(map[string]any); ok {
				desc, _ := m["description"].(string)
				if desc != "" {
					inst, _ := m["instructions"].(string)
					p.steps = append(p.steps, configStep{Description: desc, Instructions: inst})
				}
			}
		}
	}
	if len(p.steps) == 0 {
		return fmt.Errorf("static planner: no steps configured")
	}

	// Register event handlers.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("plan.request", p.handlePlanRequestEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("plan.approval.response", p.handleApprovalResponseEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	p.logger.Info("static planner initialized", "steps", len(p.steps), "approval", p.approval)
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
		{EventType: "plan.request", Priority: 50},
		{EventType: "plan.approval.response", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"plan.result",
		"plan.approval.request",
		"plan.created",
		"thinking.step",
		"io.status",
	}
}

func (p *Plugin) handlePlanRequestEvent(event engine.Event[any]) {
	req, ok := event.Payload.(events.PlanRequest)
	if !ok {
		return
	}
	p.handlePlanRequest(req)
}

func (p *Plugin) handleApprovalResponseEvent(event engine.Event[any]) {
	resp, ok := event.Payload.(events.ApprovalResponse)
	if !ok {
		return
	}
	p.handleApprovalResponse(resp)
}

func (p *Plugin) handlePlanRequest(req events.PlanRequest) {
	planID := generatePlanID()

	_ = p.bus.Emit("io.status", events.StatusUpdate{SchemaVersion: events.StatusUpdateVersion, State: "thinking",
		Detail: "Planning: loading static plan",
	})

	// Build plan result from configured steps.
	steps := make([]events.PlanResultStep, len(p.steps))
	for i, s := range p.steps {
		steps[i] = events.PlanResultStep{
			ID:           fmt.Sprintf("step_%d", i+1),
			Description:  s.Description,
			Instructions: s.Instructions,
			Status:       "pending",
			Order:        i + 1,
		}
	}

	result := events.PlanResult{SchemaVersion: events.PlanResultVersion, TurnID: req.TurnID,
		PlanID:  planID,
		Steps:   steps,
		Summary: p.summary,
		Source:  "static",
	}

	// Persist request to session.
	p.persistRequest(planID, req)

	// Persist plan to session.
	p.persistPlan(planID, result)

	// Emit thinking steps.
	_ = p.bus.Emit("thinking.step", events.ThinkingStep{SchemaVersion: events.ThinkingStepVersion, TurnID: req.TurnID,
		Source:    pluginID,
		Content:   fmt.Sprintf("Using predefined plan: %s (%d steps)", p.summary, len(p.steps)),
		Phase:     "planning",
		Timestamp: time.Now(),
	})

	_ = p.bus.Emit("io.status", events.StatusUpdate{SchemaVersion: events.StatusUpdateVersion, State: "thinking",
		Detail: fmt.Sprintf("Planning: %d steps loaded", len(p.steps)),
	})

	// Emit plan.created so UIs can display it.
	_ = p.bus.Emit("plan.created", result)

	if p.approval == approvalAlways {
		// Store pending state for approval callback.
		p.pendingPlanID = planID
		p.pendingTurnID = req.TurnID

		// Build description showing plan steps.
		desc := fmt.Sprintf("Plan: %s\n", p.summary)
		for _, step := range steps {
			desc += fmt.Sprintf("  %d. %s\n", step.Order, step.Description)
		}

		_ = p.bus.Emit("plan.approval.request", events.ApprovalRequest{SchemaVersion: events.ApprovalRequestVersion, PromptID: planID,
			Description: desc,
			Risk:        "low",
		})
		return
	}

	// No approval needed — emit result immediately.
	result.Approved = true
	p.persistApproval(planID, true)
	_ = p.bus.Emit("plan.result", result)
}

func (p *Plugin) handleApprovalResponse(resp events.ApprovalResponse) {
	if resp.PromptID != p.pendingPlanID {
		return
	}

	planID := p.pendingPlanID
	turnID := p.pendingTurnID
	p.pendingPlanID = ""
	p.pendingTurnID = ""

	p.persistApproval(planID, resp.Approved)

	if !resp.Approved {
		_ = p.bus.Emit("thinking.step", events.ThinkingStep{SchemaVersion: events.ThinkingStepVersion, TurnID: turnID,
			Source:    pluginID,
			Content:   "Plan rejected by user",
			Phase:     "planning",
			Timestamp: time.Now(),
		})
		// Emit result with empty steps to signal rejection.
		_ = p.bus.Emit("plan.result", events.PlanResult{SchemaVersion: events.PlanResultVersion, TurnID: turnID,
			PlanID:   planID,
			Approved: false,
			Source:   "static",
		})
		return
	}

	// Rebuild steps and emit approved result.
	steps := make([]events.PlanResultStep, len(p.steps))
	for i, s := range p.steps {
		steps[i] = events.PlanResultStep{
			ID:           fmt.Sprintf("step_%d", i+1),
			Description:  s.Description,
			Instructions: s.Instructions,
			Status:       "pending",
			Order:        i + 1,
		}
	}

	_ = p.bus.Emit("plan.result", events.PlanResult{SchemaVersion: events.PlanResultVersion, TurnID: turnID,
		PlanID:   planID,
		Steps:    steps,
		Summary:  p.summary,
		Approved: true,
		Source:   "static",
	})
}

// Session persistence helpers.

func (p *Plugin) persistRequest(planID string, req events.PlanRequest) {
	data, err := json.MarshalIndent(map[string]any{
		"turn_id":    req.TurnID,
		"session_id": req.SessionID,
		"input":      req.Input,
		"created_at": time.Now().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		p.logger.Error("failed to marshal plan request", "error", err)
		return
	}
	path := fmt.Sprintf("plugins/%s/%s/request.json", pluginID, planID)
	if err := p.session.WriteFile(path, data); err != nil {
		p.logger.Error("failed to persist plan request", "error", err)
	}
}

func (p *Plugin) persistPlan(planID string, result events.PlanResult) {
	type stepJSON struct {
		ID          string `json:"id"`
		Description string `json:"description"`
		Status      string `json:"status"`
		Order       int    `json:"order"`
	}

	steps := make([]stepJSON, len(result.Steps))
	for i, s := range result.Steps {
		steps[i] = stepJSON{
			ID:          s.ID,
			Description: s.Description,
			Status:      s.Status,
			Order:       s.Order,
		}
	}

	data, err := json.MarshalIndent(map[string]any{
		"plan_id":    result.PlanID,
		"turn_id":    result.TurnID,
		"source":     result.Source,
		"summary":    result.Summary,
		"steps":      steps,
		"approved":   result.Approved,
		"created_at": time.Now().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		p.logger.Error("failed to marshal plan", "error", err)
		return
	}
	path := fmt.Sprintf("plugins/%s/%s/plan.json", pluginID, planID)
	if err := p.session.WriteFile(path, data); err != nil {
		p.logger.Error("failed to persist plan", "error", err)
	}
}

func (p *Plugin) persistApproval(planID string, approved bool) {
	data, err := json.MarshalIndent(map[string]any{
		"plan_id":    planID,
		"approved":   approved,
		"decided_at": time.Now().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		p.logger.Error("failed to marshal approval", "error", err)
		return
	}
	path := fmt.Sprintf("plugins/%s/%s/approval.json", pluginID, planID)
	if err := p.session.WriteFile(path, data); err != nil {
		p.logger.Error("failed to persist approval", "error", err)
	}
}

func generatePlanID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("plan_%x", b)
}
