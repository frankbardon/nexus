package tui

import (
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/frankbardon/nexus/pkg/ui"
)

type askState struct {
	request askUserMsg
	input   textarea.Model
	styles  *Styles
}

func newAskState(request askUserMsg, styles *Styles) askState {
	ta := textarea.New()
	ta.Placeholder = "Type your answer..."
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.SetHeight(3)
	ta.Focus()

	return askState{
		request: request,
		input:   ta,
		styles:  styles,
	}
}

func (a *askState) Update(msg tea.Msg) (*ui.AskUserResponseMessage, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			val := a.input.Value()
			resp := ui.AskUserResponseMessage{
				PromptID: a.request.PromptID,
				Answer:   val,
			}
			return &resp, nil
		default:
			var cmd tea.Cmd
			a.input, cmd = a.input.Update(msg)
			return nil, cmd
		}
	default:
		var cmd tea.Cmd
		a.input, cmd = a.input.Update(msg)
		return nil, cmd
	}
}

func (a *askState) View(width, height int) string {
	var content string

	header := a.styles.Bold.Render("? Question")
	question := "  " + wordWrap(a.request.Question, 54)
	inputBox := a.input.View()
	hint := a.styles.Dim.Render("  Enter to submit")

	content = header + "\n\n" + question + "\n\n" + inputBox + "\n" + hint

	modal := a.styles.ApprovalBox.Render(content)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, modal)
}
