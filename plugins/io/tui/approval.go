package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/frankbardon/nexus/pkg/ui"
)

type approvalState struct {
	request approvalRequestMsg
	styles  *Styles
}

func (a *approvalState) Update(msg tea.Msg) *ui.ApprovalResponseMessage {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch km.String() {
	case "y":
		resp := ui.ApprovalResponseMessage{PromptID: a.request.PromptID, Approved: true}
		return &resp
	case "d", "n":
		resp := ui.ApprovalResponseMessage{PromptID: a.request.PromptID}
		return &resp
	case "a":
		resp := ui.ApprovalResponseMessage{PromptID: a.request.PromptID, Approved: true, Always: true}
		return &resp
	}
	return nil
}

func (a *approvalState) View(width, height int) string {
	var b strings.Builder

	header := a.styles.Bold.Render("⚠ Approval Required")
	b.WriteString(header + "\n\n")

	if a.request.Risk != "" {
		riskStyle := a.styles.RiskNormal
		if a.request.Risk == "high" {
			riskStyle = a.styles.RiskHigh
		}
		b.WriteString(riskStyle.Render("  Risk: "+a.request.Risk) + "\n\n")
	}

	if a.request.Description != "" {
		wrapped := wordWrap(a.request.Description, 54)
		b.WriteString("  " + wrapped + "\n\n")
	}

	if a.request.ToolCall != "" {
		toolBox := lipgloss.NewStyle().
			Foreground(a.styles.Theme.Tool).
			Padding(0, 1).
			Render("Tool: " + a.request.ToolCall)
		b.WriteString(toolBox + "\n\n")
	}

	actions := fmt.Sprintf("  [%s]eny  [%s]lways allow  [%s]pprove",
		a.styles.Bold.Render("d"),
		a.styles.Bold.Render("a"),
		a.styles.Bold.Render("y"),
	)
	b.WriteString(actions)

	modal := a.styles.ApprovalBox.Render(b.String())
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, modal)
}
