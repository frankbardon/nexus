package tui

import (
	"strings"

	"github.com/charmbracelet/glamour"
)

type markdownRenderer struct {
	renderer  *glamour.TermRenderer
	width     int
	themeName string
}

func newMarkdownRenderer(width int, themeName string) *markdownRenderer {
	style := "dark"
	if themeName == "light" {
		style = "light"
	}
	r, _ := glamour.NewTermRenderer(
		glamour.WithStylePath(style),
		glamour.WithWordWrap(width),
	)
	return &markdownRenderer{
		renderer:  r,
		width:     width,
		themeName: themeName,
	}
}

func (m *markdownRenderer) Render(content string) string {
	if m.renderer == nil {
		return content
	}
	result, err := m.renderer.Render(content)
	if err != nil {
		return content
	}
	return strings.TrimSpace(result)
}

func (m *markdownRenderer) SetWidth(width int, themeName string) {
	if width == m.width && themeName == m.themeName {
		return
	}
	m.width = width
	m.themeName = themeName
	style := "dark"
	if themeName == "light" {
		style = "light"
	}
	m.renderer, _ = glamour.NewTermRenderer(
		glamour.WithStylePath(style),
		glamour.WithWordWrap(width),
	)
}
