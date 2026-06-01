package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/frankbardon/nexus/pkg/ui"
)

type rightRail struct {
	styles *Styles

	// Plan
	plan *planState

	// Thinking
	thinkingSteps []thinkingStep

	// Workflow status
	workflow *workflowState

	planViewport     viewport.Model
	thinkingViewport viewport.Model

	width, height int
	focused       bool
	features      map[string]bool
}

// workflowState captures the latest workflow.progress payload for the
// dedicated workflow panel. Updated in place: each new event replaces
// the prior snapshot rather than scrolling, so the panel always shows
// "where are we right now".
type workflowState struct {
	Name          string
	Stage         string
	StageLabel    string
	StageIndex    int
	StageTotal    int
	Iteration     int
	MaxIterations int
	Turn          int
	MaxTurns      int
	ItemsDone     int
	ItemsTotal    int
	CurrentItem   string
	Status        string
	Detail        string
	Failures      []string
}

type planState struct {
	PlanID  string
	Summary string
	Steps   []planStep
	Source  string
}

type planStep struct {
	ID          string
	Description string
	Status      string
	Order       int
}

type thinkingStep struct {
	Content string
	Phase   string
}

func newRightRail(styles *Styles) rightRail {
	return rightRail{
		styles:           styles,
		features:         make(map[string]bool),
		planViewport:     viewport.New(38, 10),
		thinkingViewport: viewport.New(38, 10),
	}
}

func (r *rightRail) Visible() bool {
	return r.plan != nil || r.workflow != nil
}

// SetWorkflowStatus stores the latest workflow progress snapshot. The
// panel updates in place (no scrollback) so users see the current state
// at a glance.
func (r *rightRail) SetWorkflowStatus(msg workflowStatusMsg) {
	r.workflow = &workflowState{
		Name:          msg.WorkflowName,
		Stage:         msg.Stage,
		StageLabel:    msg.StageLabel,
		StageIndex:    msg.StageIndex,
		StageTotal:    msg.StageTotal,
		Iteration:     msg.Iteration,
		MaxIterations: msg.MaxIterations,
		Turn:          msg.Turn,
		MaxTurns:      msg.MaxTurns,
		ItemsDone:     msg.ItemsDone,
		ItemsTotal:    msg.ItemsTotal,
		CurrentItem:   msg.CurrentItem,
		Status:        msg.Status,
		Detail:        msg.Detail,
		Failures:      msg.Failures,
	}
}

// workflowStatusLine builds a one-line summary suitable for the chat
// header / status bar. Used by the model when it sets the chat status
// alongside the right-rail update.
func workflowStatusLine(msg ui.WorkflowStatusMessage) string {
	var parts []string
	if msg.WorkflowName != "" {
		parts = append(parts, msg.WorkflowName)
	}
	if msg.Stage != "" {
		stage := msg.Stage
		if msg.StageIndex > 0 && msg.StageTotal > 0 {
			stage = fmt.Sprintf("%s (%d/%d)", stage, msg.StageIndex, msg.StageTotal)
		}
		parts = append(parts, stage)
	}
	switch {
	case msg.Iteration > 0 && msg.MaxIterations > 0:
		parts = append(parts, fmt.Sprintf("iter %d/%d", msg.Iteration, msg.MaxIterations))
	case msg.ItemsTotal > 0:
		parts = append(parts, fmt.Sprintf("items %d/%d", msg.ItemsDone, msg.ItemsTotal))
	}
	if msg.Detail != "" {
		parts = append(parts, msg.Detail)
	}
	return strings.Join(parts, " · ")
}

func (r *rightRail) SetSize(w, h int) {
	r.width = w
	r.height = h

	innerW := w - 3 // border
	if innerW < 1 {
		innerW = 1
	}

	if r.plan != nil && len(r.thinkingSteps) > 0 {
		half := h / 2
		r.planViewport.Width = innerW
		r.planViewport.Height = half - 3
		r.thinkingViewport.Width = innerW
		r.thinkingViewport.Height = h - half - 3
	} else if r.plan != nil {
		r.planViewport.Width = innerW
		r.planViewport.Height = h - 3
	} else {
		r.thinkingViewport.Width = innerW
		r.thinkingViewport.Height = h - 3
	}
}

func (r *rightRail) Focus() { r.focused = true }
func (r *rightRail) Blur()  { r.focused = false }

func (r *rightRail) SetPlan(msg planDisplayMsg) {
	steps := make([]planStep, len(msg.Steps))
	for i, s := range msg.Steps {
		steps[i] = planStep{
			ID:          s.ID,
			Description: s.Description,
			Status:      s.Status,
			Order:       s.Order,
		}
	}
	r.plan = &planState{
		PlanID:  msg.PlanID,
		Summary: msg.Summary,
		Steps:   steps,
		Source:  msg.Source,
	}
	r.rebuildPlanViewport()
}

func (r *rightRail) UpdateSteps(msg planUpdateMsg) {
	if r.plan == nil {
		// No plan from the planner — ignore tool-level step updates.
		return
	}

	// Update existing steps by matching order/index.
	for i, s := range msg.Steps {
		if i < len(r.plan.Steps) {
			r.plan.Steps[i].Status = s.Status
			if s.Description != "" {
				r.plan.Steps[i].Description = s.Description
			}
		}
	}
	r.rebuildPlanViewport()
}

// UpdateStepStatus mutates one step's status by matching StepID. Used
// by plan.progress events whose payload is keyed by step ID, not index.
// Plans that arrived from a different PlanID are ignored — a stale
// progress event must not stomp the current plan.
func (r *rightRail) UpdateStepStatus(msg planStepStatusMsg) {
	if r.plan == nil {
		return
	}
	if msg.PlanID != "" && r.plan.PlanID != "" && msg.PlanID != r.plan.PlanID {
		return
	}
	for i := range r.plan.Steps {
		if r.plan.Steps[i].ID == msg.StepID {
			r.plan.Steps[i].Status = msg.Status
			r.rebuildPlanViewport()
			return
		}
	}
}

func (r *rightRail) AddThinking(msg thinkingMsg) {
	phase := msg.Phase
	if phase == "" {
		phase = "thinking"
	}
	r.thinkingSteps = append(r.thinkingSteps, thinkingStep{
		Content: msg.Content,
		Phase:   phase,
	})
	r.rebuildThinkingViewport()
}

func (r *rightRail) ClearThinking() {
	r.thinkingSteps = nil
	r.rebuildThinkingViewport()
}

func (r *rightRail) Update(msg tea.Msg) tea.Cmd {
	if !r.focused {
		return nil
	}
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "pgup", "pgdown", "up", "down":
			if r.plan != nil {
				var cmd tea.Cmd
				r.planViewport, cmd = r.planViewport.Update(msg)
				return cmd
			}
			var cmd tea.Cmd
			r.thinkingViewport, cmd = r.thinkingViewport.Update(msg)
			return cmd
		}
	}
	return nil
}

func (r *rightRail) rebuildPlanViewport() {
	if r.plan == nil {
		r.planViewport.SetContent("")
		return
	}
	r.planViewport.SetContent(r.renderPlanContent())
	r.planViewport.GotoBottom()
}

func (r *rightRail) rebuildThinkingViewport() {
	r.thinkingViewport.SetContent(r.renderThinkingContent())
	r.thinkingViewport.GotoBottom()
}

func (r *rightRail) renderPlanContent() string {
	if r.plan == nil {
		return ""
	}
	var b strings.Builder
	if r.plan.Summary != "" {
		b.WriteString(r.styles.Dim.Render(wordWrap(r.plan.Summary, r.width-4)))
		b.WriteString("\n\n")
	}
	for _, step := range r.plan.Steps {
		icon, style := r.stepStyle(step.Status)
		line := fmt.Sprintf(" %s %s", icon, step.Description)
		b.WriteString(style.Render(wordWrap(line, r.width-4)))
		b.WriteString("\n")
	}
	return b.String()
}

func (r *rightRail) renderThinkingContent() string {
	var b strings.Builder
	for _, step := range r.thinkingSteps {
		phase := strings.ToUpper(step.Phase)
		b.WriteString(r.styles.Dim.Render(" "+phase) + "\n")
		b.WriteString(r.styles.ThinkStep.Render(" "+wordWrap(step.Content, r.width-6)) + "\n\n")
	}
	return b.String()
}

func (r *rightRail) stepStyle(status string) (string, lipgloss.Style) {
	switch status {
	case "active":
		return "◉", lipgloss.NewStyle().Foreground(r.styles.Theme.Primary)
	case "completed":
		return "✓", lipgloss.NewStyle().Foreground(r.styles.Theme.Success)
	case "failed":
		return "✗", lipgloss.NewStyle().Foreground(r.styles.Theme.Danger)
	default: // pending
		return "○", r.styles.Dim
	}
}

func (r *rightRail) View() string {
	var sections []string

	// Workflow status section — sticky panel at the top showing the
	// current workflow state at a glance. Updated in place on each
	// workflow.progress event; no scrollback.
	if r.workflow != nil {
		header := r.styles.Bold.Render(" Workflow")
		if r.workflow.Status != "" {
			header += " " + r.styles.Dim.Render("("+r.workflow.Status+")")
		}
		sections = append(sections, header)
		sections = append(sections, r.renderWorkflowContent())
	}

	// Plan section
	if r.plan != nil {
		header := r.styles.Bold.Render(" Plan")
		if r.plan.Source != "" {
			header += " " + r.styles.Dim.Render("("+r.plan.Source+")")
		}
		sections = append(sections, header)
		sections = append(sections, r.planViewport.View())
	}

	// Thinking section
	if len(r.thinkingSteps) > 0 {
		header := r.styles.Bold.Render(" Thinking")
		sections = append(sections, header)
		sections = append(sections, r.thinkingViewport.View())
	}

	body := lipgloss.JoinVertical(lipgloss.Left, sections...)
	return r.styles.RightRail.Height(r.height).Render(body)
}

func (r *rightRail) renderWorkflowContent() string {
	if r.workflow == nil {
		return ""
	}
	var b strings.Builder
	w := r.workflow
	if w.Name != "" {
		b.WriteString(r.styles.Dim.Render(" "+w.Name) + "\n")
	}
	if w.Stage != "" {
		stage := w.Stage
		if w.StageLabel != "" && w.StageLabel != w.Stage {
			stage = w.StageLabel
		}
		if w.StageIndex > 0 && w.StageTotal > 0 {
			stage = fmt.Sprintf("%s  %d/%d", stage, w.StageIndex, w.StageTotal)
		}
		b.WriteString(" " + stage + "\n")
	}
	if w.MaxIterations > 0 {
		b.WriteString(r.styles.Dim.Render(fmt.Sprintf("  iter %d/%d", w.Iteration, w.MaxIterations)) + "\n")
	}
	if w.MaxTurns > 0 {
		b.WriteString(r.styles.Dim.Render(fmt.Sprintf("  turn %d/%d", w.Turn, w.MaxTurns)) + "\n")
	}
	if w.ItemsTotal > 0 {
		line := fmt.Sprintf("  items %d/%d", w.ItemsDone, w.ItemsTotal)
		if w.CurrentItem != "" {
			line += "  " + w.CurrentItem
		}
		b.WriteString(r.styles.Dim.Render(line) + "\n")
	}
	if w.Detail != "" {
		b.WriteString(" " + wordWrap(w.Detail, r.width-3) + "\n")
	}
	if len(w.Failures) > 0 {
		b.WriteString(r.styles.Dim.Render(" last fail: "+strings.Join(w.Failures, ", ")) + "\n")
	}
	return b.String()
}
