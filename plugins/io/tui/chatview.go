package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type chatMessage struct {
	ID       string
	Role     string
	Content  string
	TurnID   string
	IsStream bool
}

type chatView struct {
	messages []chatMessage
	viewport viewport.Model
	input    textarea.Model
	spinner  spinner.Model
	styles   *Styles

	width, height int
	focused       bool
	status        statusMsg
	isWorking     bool
	streamTurnID  string
}

func newChatView(styles *Styles) chatView {
	ta := textarea.New()
	ta.Placeholder = "Type a message... (Enter to send)"
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.SetHeight(2)
	ta.KeyMap.InsertNewline.SetEnabled(false) // Enter submits, not newline
	ta.Blur()                                // Start blurred; Focus() called after first WindowSizeMsg

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = styles.Spinner

	vp := viewport.New(80, 20)

	return chatView{
		messages: []chatMessage{},
		viewport: vp,
		input:    ta,
		spinner:  sp,
		styles:   styles,
	}
}

func (c *chatView) SetSize(w, h int) {
	c.width = w
	c.height = h

	inputHeight := 4 // textarea + border
	statusHeight := 0
	if c.isWorking {
		statusHeight = 1
	}
	vpHeight := h - inputHeight - statusHeight
	if vpHeight < 1 {
		vpHeight = 1
	}

	c.viewport.Width = w
	c.viewport.Height = vpHeight
	c.input.SetWidth(w - 4)
	c.rebuildViewport()
}

func (c *chatView) Focus() {
	c.focused = true
	c.input.Focus()
}

func (c *chatView) Blur() {
	c.focused = false
	c.input.Blur()
}

func (c *chatView) AddMessage(msg chatMessage) {
	c.messages = append(c.messages, msg)
	c.rebuildViewport()
}

func (c *chatView) AppendToStream(turnID, content string) {
	if c.streamTurnID != turnID {
		c.streamTurnID = turnID
		c.messages = append(c.messages, chatMessage{
			ID:       "stream-" + turnID,
			Role:     "assistant",
			TurnID:   turnID,
			IsStream: true,
		})
	}
	for i := len(c.messages) - 1; i >= 0; i-- {
		if c.messages[i].TurnID == turnID && c.messages[i].IsStream {
			c.messages[i].Content += content
			break
		}
	}
	c.rebuildViewport()
}

func (c *chatView) EndStream() {
	c.streamTurnID = ""
	c.rebuildViewport()
}

// ClearStream removes the current partial stream message and any fully
// rendered content from the active streaming turn. Used by provider
// fallback to wipe incomplete output before retrying with another model.
func (c *chatView) ClearStream() {
	if c.streamTurnID == "" {
		return
	}
	turnID := c.streamTurnID
	c.streamTurnID = ""

	// Remove the partial stream message for this turn.
	filtered := c.messages[:0]
	for _, msg := range c.messages {
		if msg.TurnID == turnID && msg.IsStream {
			continue
		}
		filtered = append(filtered, msg)
	}
	c.messages = filtered
	c.rebuildViewport()
}

func (c *chatView) SetStatus(msg statusMsg) {
	wasWorking := c.isWorking
	c.status = msg
	c.isWorking = msg.State != "" && msg.State != "idle"
	if wasWorking != c.isWorking {
		c.SetSize(c.width, c.height)
	}
}

func (c *chatView) Update(msg tea.Msg) (tea.Cmd, string) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		if !c.focused {
			break
		}
		switch msg.String() {
		case "enter":
			val := strings.TrimSpace(c.input.Value())
			if val != "" {
				c.input.Reset()
				return nil, val
			}
			return nil, ""
		case "pgup", "pgdown":
			var cmd tea.Cmd
			c.viewport, cmd = c.viewport.Update(msg)
			cmds = append(cmds, cmd)
		default:
			var cmd tea.Cmd
			c.input, cmd = c.input.Update(msg)
			cmds = append(cmds, cmd)
		}

	case spinner.TickMsg:
		if c.isWorking {
			var cmd tea.Cmd
			c.spinner, cmd = c.spinner.Update(msg)
			cmds = append(cmds, cmd)
		}

	default:
		// Pass cursor blink and other internal messages to textarea
		if c.focused {
			var cmd tea.Cmd
			c.input, cmd = c.input.Update(msg)
			cmds = append(cmds, cmd)
		}
	}

	return tea.Batch(cmds...), ""
}

func (c *chatView) rebuildViewport() {
	content := c.renderMessages()
	c.viewport.SetContent(content)
	c.viewport.GotoBottom()
}

func (c *chatView) renderMessages() string {
	if len(c.messages) == 0 {
		return c.renderEmptyState()
	}

	var b strings.Builder
	maxWidth := c.width - 6
	if maxWidth < 20 {
		maxWidth = 20
	}

	for _, msg := range c.messages {
		label, bubble := c.styleForRole(msg.Role)
		labelStr := label.Render(c.roleName(msg.Role))

		content := msg.Content
		if content == "" {
			content = "..."
		}

		wrapped := wordWrap(content, maxWidth)

		if msg.Role == "user" {
			rendered := bubble.Width(maxWidth).Render(wrapped)
			lines := strings.Split(rendered, "\n")
			for i, line := range lines {
				pad := c.width - lipgloss.Width(line) - 1
				if pad < 0 {
					pad = 0
				}
				lines[i] = strings.Repeat(" ", pad) + line
			}
			labelPad := c.width - lipgloss.Width(labelStr) - 1
			if labelPad < 0 {
				labelPad = 0
			}
			b.WriteString(strings.Repeat(" ", labelPad) + labelStr + "\n")
			b.WriteString(strings.Join(lines, "\n") + "\n\n")
		} else {
			rendered := bubble.Width(maxWidth).Render(wrapped)
			b.WriteString(labelStr + "\n")
			b.WriteString(rendered + "\n\n")
		}
	}

	if c.isWorking && c.streamTurnID == "" {
		b.WriteString(c.styles.Dim.Render("  ···") + "\n")
	}

	return b.String()
}

func (c *chatView) renderEmptyState() string {
	title := c.styles.Logo.Render("⚡ Nexus")
	subtitle := c.styles.Dim.Render("Type a message to begin.")
	box := lipgloss.JoinVertical(lipgloss.Center, "", "", "", title, subtitle, "")
	return lipgloss.Place(c.width, c.viewport.Height,
		lipgloss.Center, lipgloss.Center, box)
}

func (c *chatView) styleForRole(role string) (lipgloss.Style, lipgloss.Style) {
	switch role {
	case "user":
		return c.styles.UserLabel, c.styles.UserBubble
	case "assistant":
		return c.styles.AssistantLabel, c.styles.AssistantBubble
	case "system":
		return c.styles.SystemLabel, c.styles.SystemBubble
	case "tool":
		return c.styles.ToolLabel, c.styles.ToolBubble
	case "thinking":
		return c.styles.Dim, c.styles.ThinkStep
	case "error":
		return c.styles.ErrorLabel, c.styles.ErrorBubble
	default:
		return c.styles.AssistantLabel, c.styles.AssistantBubble
	}
}

func (c *chatView) roleName(role string) string {
	switch role {
	case "user":
		return "You"
	case "assistant":
		return "Assistant"
	case "system":
		return "System"
	case "tool":
		return "Tool"
	case "thinking":
		return "Thinking"
	case "error":
		return "Error"
	default:
		return role
	}
}

func (c *chatView) View() string {
	var sections []string

	sections = append(sections, c.viewport.View())

	if c.isWorking {
		detail := c.status.Detail
		if detail == "" {
			detail = c.status.State
		}
		bar := fmt.Sprintf(" %s %s", c.spinner.View(), detail)
		sections = append(sections, c.styles.StatusBar.Width(c.width).Render(bar))
	}

	inputBox := c.styles.InputArea.Width(c.width).Render(c.input.View())
	sections = append(sections, inputBox)

	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}

func wordWrap(text string, width int) string {
	if width <= 0 {
		return text
	}
	var result strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if lipgloss.Width(line) <= width {
			if result.Len() > 0 {
				result.WriteString("\n")
			}
			result.WriteString(line)
			continue
		}
		words := strings.Fields(line)
		currentLine := ""
		for _, word := range words {
			if currentLine == "" {
				currentLine = word
			} else if lipgloss.Width(currentLine+" "+word) <= width {
				currentLine += " " + word
			} else {
				if result.Len() > 0 {
					result.WriteString("\n")
				}
				result.WriteString(currentLine)
				currentLine = word
			}
		}
		if currentLine != "" {
			if result.Len() > 0 {
				result.WriteString("\n")
			}
			result.WriteString(currentLine)
		}
	}
	return result.String()
}
