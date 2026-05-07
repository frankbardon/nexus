package dynamic

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.planner.dynamic"
	pluginName = "Dynamic Planner"
	version    = "0.1.0"
)

type approvalMode string

const (
	approvalAlways approvalMode = "always"
	approvalNever  approvalMode = "never"
	approvalAuto   approvalMode = "auto"
)

const defaultPlanPrompt = `You are a planning assistant. Given the user's request, create a step-by-step execution plan.

Output ONLY valid JSON in this format:
{
  "summary": "Brief description of the overall plan",
  "needs_approval": true or false based on whether this plan involves risky or destructive operations,
  "steps": [
    {"description": "Step description"}
  ]
}

Keep the plan concise (typically 3-7 steps). Do not include any text outside the JSON.`

// Plugin implements a dynamic planner that uses the LLM to generate plans.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace

	approval   approvalMode
	planPrompt string
	model      string // Explicit model ID override (backward compat)
	modelRole  string // Model role for role-based selection
	maxSteps   int

	mu             sync.Mutex
	pendingRequest *events.PlanRequest
	pendingPlanID  string
	pendingResult  *events.PlanResult
	unsubs         []func()
}

// New creates a new dynamic planner plugin.
func New() engine.Plugin {
	return &Plugin{
		approval:   approvalAuto,
		planPrompt: defaultPlanPrompt,
		maxSteps:   10,
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
		case approvalNever:
			p.approval = approvalNever
		default:
			p.approval = approvalAuto
		}
	}

	// Parse plan prompt (file takes precedence over inline).
	if promptFile, ok := ctx.Config["plan_prompt_file"].(string); ok && promptFile != "" {
		data, err := os.ReadFile(promptFile)
		if err != nil {
			return fmt.Errorf("dynamic planner: failed to read plan prompt file %s: %w", promptFile, err)
		}
		p.planPrompt = string(data)
	} else if prompt, ok := ctx.Config["plan_prompt"].(string); ok && prompt != "" {
		p.planPrompt = prompt
	}

	// Parse model role (preferred) or explicit model override (backward compat).
	if mr, ok := ctx.Config["model_role"].(string); ok {
		p.modelRole = mr
	}
	if m, ok := ctx.Config["model"].(string); ok {
		p.model = m
	}

	// Parse max steps.
	if ms, ok := ctx.Config["max_steps"].(int); ok {
		p.maxSteps = ms
	} else if ms, ok := ctx.Config["max_steps"].(float64); ok {
		p.maxSteps = int(ms)
	}

	// Register event handlers.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("plan.request", p.handlePlanRequestEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("llm.response", p.handleLLMResponseEvent,
			engine.WithPriority(40), engine.WithSource(pluginID)),
		p.bus.Subscribe("plan.approval.response", p.handleApprovalResponseEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	p.logger.Info("dynamic planner initialized", "approval", p.approval, "model_role", p.modelRole, "model", p.model)
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
		{EventType: "llm.response", Priority: 40},
		{EventType: "plan.approval.response", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"llm.request",
		"before:llm.request",
		"plan.result",
		"plan.approval.request",
		"plan.created",
		"thinking.step",
		"io.status",
	}
}

// Event handler wrappers.

func (p *Plugin) handlePlanRequestEvent(event engine.Event[any]) {
	req, ok := event.Payload.(events.PlanRequest)
	if !ok {
		return
	}
	p.handlePlanRequest(req)
}

func (p *Plugin) handleLLMResponseEvent(event engine.Event[any]) {
	resp, ok := event.Payload.(events.LLMResponse)
	if !ok {
		return
	}
	// Only handle responses tagged for this plugin.
	source, _ := resp.Metadata["_source"].(string)
	if source != pluginID {
		return
	}
	p.handleLLMResponse(resp)
}

func (p *Plugin) handleApprovalResponseEvent(event engine.Event[any]) {
	resp, ok := event.Payload.(events.ApprovalResponse)
	if !ok {
		return
	}
	p.handleApprovalResponse(resp)
}

// Core logic.

func (p *Plugin) handlePlanRequest(req events.PlanRequest) {
	planID := generatePlanID()

	p.mu.Lock()
	p.pendingRequest = &req
	p.pendingPlanID = planID
	p.mu.Unlock()

	// Persist request to session.
	p.persistRequest(planID, req)

	// Emit thinking steps so the user sees planning progress.
	_ = p.bus.Emit("thinking.step", events.ThinkingStep{SchemaVersion: events.ThinkingStepVersion, TurnID: req.TurnID,
		Source:    pluginID,
		Content:   "Analyzing request to create execution plan...",
		Phase:     "planning",
		Timestamp: time.Now(),
	})

	_ = p.bus.Emit("io.status", events.StatusUpdate{SchemaVersion: events.StatusUpdateVersion, State: "thinking",
		Detail: "Planning: analyzing request",
	})

	_ = p.bus.Emit("thinking.step", events.ThinkingStep{SchemaVersion: events.ThinkingStepVersion, TurnID: req.TurnID,
		Source:    pluginID,
		Content:   "Sending request to LLM for plan generation...",
		Phase:     "planning",
		Timestamp: time.Now(),
	})

	// Build messages for plan generation LLM call.
	messages := []events.Message{
		{Role: "system", Content: p.planPrompt},
		{Role: "user", Content: req.Input},
	}

	// Emit LLM request tagged for this plugin.
	llmReq := events.LLMRequest{SchemaVersion: events.LLMRequestVersion, Role: p.modelRole,
		Model:    p.model,
		Messages: messages,
		Stream:   false,
		Metadata: map[string]any{
			"_source":   pluginID,
			"task_kind": "plan",
		},
		Tags: map[string]string{"source_plugin": pluginID},
	}
	if veto, err := p.bus.EmitVetoable("before:llm.request", &llmReq); err == nil && veto.Vetoed {
		p.logger.Info("llm.request vetoed", "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("llm.request", llmReq)
}

func (p *Plugin) handleLLMResponse(resp events.LLMResponse) {
	p.mu.Lock()
	req := p.pendingRequest
	planID := p.pendingPlanID
	p.mu.Unlock()

	if req == nil {
		return
	}

	_ = p.bus.Emit("io.status", events.StatusUpdate{SchemaVersion: events.StatusUpdateVersion, State: "thinking",
		Detail: "Planning: processing LLM response",
	})

	_ = p.bus.Emit("thinking.step", events.ThinkingStep{SchemaVersion: events.ThinkingStepVersion, TurnID: req.TurnID,
		Source:    pluginID,
		Content:   "Received plan from LLM, parsing structure...",
		Phase:     "planning",
		Timestamp: time.Now(),
	})

	// Parse the LLM response as JSON plan.
	result, needsApproval := p.parsePlanResponse(resp.Content, req.TurnID, planID)

	// Persist the plan.
	p.persistPlan(planID, result)

	// Emit thinking step with plan summary.
	_ = p.bus.Emit("thinking.step", events.ThinkingStep{SchemaVersion: events.ThinkingStepVersion, TurnID: req.TurnID,
		Source:    pluginID,
		Content:   fmt.Sprintf("Plan generated: %s (%d steps)", result.Summary, len(result.Steps)),
		Phase:     "planning",
		Timestamp: time.Now(),
	})

	_ = p.bus.Emit("io.status", events.StatusUpdate{SchemaVersion: events.StatusUpdateVersion, State: "thinking",
		Detail: fmt.Sprintf("Planning: %d steps generated", len(result.Steps)),
	})

	// Emit plan.created for UI display.
	_ = p.bus.Emit("plan.created", result)

	// Determine whether approval is needed.
	requireApproval := false
	switch p.approval {
	case approvalAlways:
		requireApproval = true
	case approvalAuto:
		requireApproval = needsApproval
	case approvalNever:
		requireApproval = false
	}

	if requireApproval {
		p.mu.Lock()
		p.pendingResult = &result
		p.mu.Unlock()

		_ = p.bus.Emit("io.status", events.StatusUpdate{SchemaVersion: events.StatusUpdateVersion, State: "waiting",
			Detail: "Planning: awaiting approval",
		})

		_ = p.bus.Emit("thinking.step", events.ThinkingStep{SchemaVersion: events.ThinkingStepVersion, TurnID: req.TurnID,
			Source:    pluginID,
			Content:   "Plan requires approval before execution",
			Phase:     "planning",
			Timestamp: time.Now(),
		})

		// Build description showing plan steps.
		desc := fmt.Sprintf("Plan: %s\n", result.Summary)
		for _, step := range result.Steps {
			desc += fmt.Sprintf("  %d. %s\n", step.Order, step.Description)
		}

		_ = p.bus.Emit("plan.approval.request", events.ApprovalRequest{SchemaVersion: events.ApprovalRequestVersion, PromptID: planID,
			Description: desc,
			Risk:        "medium",
		})
		return
	}

	// No approval needed — emit result.
	result.Approved = true
	p.persistApproval(planID, true)

	p.mu.Lock()
	p.pendingRequest = nil
	p.pendingPlanID = ""
	p.mu.Unlock()

	_ = p.bus.Emit("plan.result", result)
}

func (p *Plugin) handleApprovalResponse(resp events.ApprovalResponse) {
	p.mu.Lock()
	if resp.PromptID != p.pendingPlanID {
		p.mu.Unlock()
		return
	}
	result := p.pendingResult
	planID := p.pendingPlanID
	p.pendingRequest = nil
	p.pendingPlanID = ""
	p.pendingResult = nil
	p.mu.Unlock()

	if result == nil {
		return
	}

	p.persistApproval(planID, resp.Approved)

	if !resp.Approved {
		_ = p.bus.Emit("thinking.step", events.ThinkingStep{SchemaVersion: events.ThinkingStepVersion, TurnID: result.TurnID,
			Source:    pluginID,
			Content:   "Plan rejected by user",
			Phase:     "planning",
			Timestamp: time.Now(),
		})
		_ = p.bus.Emit("plan.result", events.PlanResult{SchemaVersion: events.PlanResultVersion, TurnID: result.TurnID,
			PlanID:   result.PlanID,
			Approved: false,
			Source:   "dynamic",
		})
		return
	}

	result.Approved = true
	_ = p.bus.Emit("plan.result", *result)
}

// parsePlanResponse extracts a structured plan from the LLM's JSON output.
// Returns the plan result and whether the LLM indicated approval is needed.
func (p *Plugin) parsePlanResponse(content, turnID, planID string) (events.PlanResult, bool) {
	var parsed struct {
		Summary       string `json:"summary"`
		NeedsApproval bool   `json:"needs_approval"`
		Steps         []struct {
			Description string `json:"description"`
		} `json:"steps"`
	}

	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		p.logger.Warn("failed to parse plan JSON, using fallback", "error", err)
		return events.PlanResult{SchemaVersion: events.PlanResultVersion, TurnID: turnID,
			PlanID:  planID,
			Summary: "Execute the user's request directly",
			Steps: []events.PlanResultStep{
				{ID: "step_1", Description: "Execute the user's request directly", Status: "pending", Order: 1},
			},
			Source: "dynamic",
		}, false
	}

	// Enforce max steps.
	if len(parsed.Steps) > p.maxSteps {
		parsed.Steps = parsed.Steps[:p.maxSteps]
	}

	steps := make([]events.PlanResultStep, len(parsed.Steps))
	for i, s := range parsed.Steps {
		steps[i] = events.PlanResultStep{
			ID:          fmt.Sprintf("step_%d", i+1),
			Description: s.Description,
			Status:      "pending",
			Order:       i + 1,
		}
	}

	summary := parsed.Summary
	if summary == "" {
		summary = "Dynamic execution plan"
	}

	return events.PlanResult{SchemaVersion: events.PlanResultVersion, TurnID: turnID,
		PlanID:  planID,
		Steps:   steps,
		Summary: summary,
		Source:  "dynamic",
	}, parsed.NeedsApproval
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
