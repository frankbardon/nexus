package tui

import (
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type fileView struct {
	styles  *Styles
	path    string
	content string
	loading bool

	viewport      viewport.Model
	width, height int
}

func newFileView(styles *Styles, path string) fileView {
	vp := viewport.New(80, 20)
	return fileView{
		styles:   styles,
		path:     path,
		loading:  true,
		viewport: vp,
	}
}

func (f *fileView) SetSize(w, h int) {
	f.width = w
	f.height = h
	headerHeight := 2
	vpHeight := h - headerHeight
	if vpHeight < 1 {
		vpHeight = 1
	}
	f.viewport.Width = w
	f.viewport.Height = vpHeight
}

func (f *fileView) SetContent(content string) {
	f.content = content
	f.loading = false
	f.viewport.SetContent(content)
	f.viewport.GotoTop()
}

func (f *fileView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "pgup", "pgdown", "up", "down":
			var cmd tea.Cmd
			f.viewport, cmd = f.viewport.Update(msg)
			return cmd
		}
	}
	return nil
}

func (f *fileView) View() string {
	// Header
	backBtn := f.styles.Dim.Render("← Esc")
	pathLabel := lipgloss.NewStyle().
		Foreground(f.styles.Theme.Text).
		Bold(true).
		Render(f.path)
	header := lipgloss.JoinHorizontal(lipgloss.Top, backBtn, "  ", pathLabel)
	headerBar := lipgloss.NewStyle().
		BorderBottom(true).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(f.styles.Theme.Border).
		Width(f.width).
		Padding(0, 1).
		Render(header)

	if f.loading {
		body := lipgloss.Place(f.width, f.viewport.Height,
			lipgloss.Center, lipgloss.Center,
			f.styles.Dim.Render("Loading..."))
		return lipgloss.JoinVertical(lipgloss.Left, headerBar, body)
	}

	return lipgloss.JoinVertical(lipgloss.Left, headerBar, f.viewport.View())
}
