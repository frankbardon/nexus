package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type rightRail struct {
	styles *Styles

	// Plan
	plan *planState

	// Thinking
	thinkingSteps []thinkingStep

	planViewport     viewport.Model
	thinkingViewport viewport.Model

	width, height int
	focused       bool
	features      map[string]bool
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
	return r.plan != nil
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
