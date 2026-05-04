package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/frankbardon/nexus/pkg/ui"
)

type askState struct {
	request     askUserMsg
	input       textarea.Model
	styles      *Styles
	hasFreeText bool
}

func newAskState(request askUserMsg, styles *Styles) askState {
	hasFreeText := request.Mode == "" || request.Mode == "free_text" || request.Mode == "both"

	ta := textarea.New()
	ta.Placeholder = "Type your answer..."
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.SetHeight(3)
	if hasFreeText {
		ta.Focus()
	}

	return askState{
		request:     request,
		input:       ta,
		styles:      styles,
		hasFreeText: hasFreeText,
	}
}

func (a *askState) Update(msg tea.Msg) (*ui.HITLResponseMessage, tea.Cmd) {
	keyMsg, isKey := msg.(tea.KeyMsg)
	if !isKey {
		if a.hasFreeText {
			var cmd tea.Cmd
			a.input, cmd = a.input.Update(msg)
			return nil, cmd
		}
		return nil, nil
	}

	keyStr := keyMsg.String()

	// Multi-choice modes: numeric keys 1..9 pick a choice. In "both" mode,
	// the user can also type freeform and press enter.
	if len(a.request.Choices) > 0 {
		if idx, err := strconv.Atoi(keyStr); err == nil && idx >= 1 && idx <= len(a.request.Choices) {
			resp := ui.HITLResponseMessage{
				RequestID: a.request.RequestID,
				ChoiceID:  a.request.Choices[idx-1].ID,
			}
			return &resp, nil
		}
	}

	if keyStr == "enter" && a.hasFreeText {
		val := a.input.Value()
		if val == "" && len(a.request.Choices) > 0 {
			return nil, nil
		}
		resp := ui.HITLResponseMessage{
			RequestID: a.request.RequestID,
			FreeText:  val,
		}
		return &resp, nil
	}

	if a.hasFreeText {
		var cmd tea.Cmd
		a.input, cmd = a.input.Update(keyMsg)
		return nil, cmd
	}
	return nil, nil
}

func (a *askState) View(width, height int) string {
	header := a.styles.Bold.Render("? " + a.headerLabel())
	prompt := "  " + wordWrap(a.request.Prompt, 54)

	parts := []string{header, "", prompt}

	if len(a.request.Choices) > 0 {
		parts = append(parts, "")
		for i, c := range a.request.Choices {
			parts = append(parts, fmt.Sprintf("  %d. %s", i+1, c.Label))
		}
	}

	if a.hasFreeText {
		parts = append(parts, "", a.input.View())
	}

	hint := a.styles.Dim.Render("  " + a.hintLine())
	parts = append(parts, hint)

	modal := a.styles.ApprovalBox.Render(strings.Join(parts, "\n"))
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, modal)
}

func (a *askState) headerLabel() string {
	if len(a.request.Choices) > 0 && !a.hasFreeText {
		return "Choose"
	}
	if len(a.request.Choices) > 0 {
		return "Choose or answer"
	}
	return "Question"
}

func (a *askState) hintLine() string {
	switch {
	case len(a.request.Choices) > 0 && a.hasFreeText:
		return "1-9 to pick · Enter to submit text"
	case len(a.request.Choices) > 0:
		return "Press 1-9 to pick"
	default:
		return "Enter to submit"
	}
}
