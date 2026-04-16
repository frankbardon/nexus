package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type sidebarTab int

const (
	tabChat sidebarTab = iota
	tabFiles
)

type sidebar struct {
	styles    *Styles
	tab       sidebarTab
	files     []fileEntry
	features  map[string]bool
	sessionID string
	cursor    int // selected item index in file list

	width, height int
	focused       bool
	fileViewport  viewport.Model
}

func newSidebar(styles *Styles) sidebar {
	vp := viewport.New(28, 10)
	return sidebar{
		styles:       styles,
		tab:          tabChat,
		features:     make(map[string]bool),
		fileViewport: vp,
	}
}

func (s *sidebar) SetSize(w, h int) {
	s.width = w
	s.height = h
	headerHeight := 7 // logo + session + tabs
	footerHeight := 3 // theme
	contentHeight := h - headerHeight - footerHeight
	if contentHeight < 1 {
		contentHeight = 1
	}
	s.fileViewport.Width = w - 3 // account for border
	s.fileViewport.Height = contentHeight
}

func (s *sidebar) Focus()        { s.focused = true }
func (s *sidebar) Blur()         { s.focused = false }
func (s *sidebar) SetTab(t sidebarTab) { s.tab = t }

func (s *sidebar) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if !s.focused {
			return nil
		}
		switch msg.String() {
		case "1":
			s.tab = tabChat
			s.cursor = 0
		case "2":
			s.tab = tabFiles
			s.cursor = 0
		case "up", "k":
			if s.tab == tabFiles && s.cursor > 0 {
				s.cursor--
			}
		case "down", "j":
			if s.tab == tabFiles {
				fileCount := s.fileCount()
				if s.cursor < fileCount-1 {
					s.cursor++
				}
			}
		}
	}
	return nil
}

func (s *sidebar) fileCount() int {
	count := 0
	for _, f := range s.files {
		if !f.IsDir {
			count++
		}
	}
	return count
}

func (s *sidebar) SelectedFile() *fileEntry {
	if s.tab != tabFiles {
		return nil
	}
	idx := 0
	for i := range s.files {
		if s.files[i].IsDir {
			continue
		}
		if idx == s.cursor {
			return &s.files[i]
		}
		idx++
	}
	return nil
}

func (s *sidebar) View() string {
	var sections []string

	// Logo
	logo := s.styles.SidebarHeader.Render("⚡ Nexus")
	sections = append(sections, logo)

	// Session ID
	sessionLine := s.styles.Dim.Render("  Session")
	if s.sessionID != "" {
		id := s.sessionID
		if len(id) > s.width-4 {
			id = id[:s.width-4]
		}
		sessionLine += "\n" + s.styles.Dim.Render("  "+id)
	}
	sections = append(sections, sessionLine)

	// Tabs
	chatTab := s.styles.TabInactive.Render("Chat")
	filesTab := s.styles.TabInactive.Render("Files")
	if s.tab == tabChat {
		chatTab = s.styles.TabActive.Render("Chat")
	} else {
		filesTab = s.styles.TabActive.Render("Files")
	}
	tabBar := lipgloss.JoinHorizontal(lipgloss.Top, chatTab, filesTab)
	sections = append(sections, tabBar)

	// Content
	contentHeight := s.height - 7 - 3
	if contentHeight < 1 {
		contentHeight = 1
	}

	var content string
	if s.tab == tabChat {
		content = s.renderChatNav(contentHeight)
	} else {
		content = s.renderFiles(contentHeight)
	}
	sections = append(sections, content)

	// Theme indicator
	themeLine := s.styles.Dim.Render(fmt.Sprintf("  Theme: %s", s.styles.Theme.Name))
	sections = append(sections, themeLine)

	body := lipgloss.JoinVertical(lipgloss.Left, sections...)

	return s.styles.Sidebar.Height(s.height).Render(body)
}

func (s *sidebar) renderChatNav(height int) string {
	lines := make([]string, height)
	return strings.Join(lines, "\n")
}

func (s *sidebar) renderFiles(height int) string {
	var b strings.Builder

	if len(s.files) == 0 {
		b.WriteString(s.styles.Dim.Render("  No files in session"))
	} else {
		idx := 0
		for _, f := range s.files {
			if f.IsDir {
				continue
			}
			name := f.Name
			maxName := s.width - 12
			if maxName < 8 {
				maxName = 8
			}
			if len(name) > maxName {
				name = name[:maxName-1] + "…"
			}

			size := formatFileSize(f.Size)
			line := fmt.Sprintf("  %s %s", name, s.styles.Dim.Render(size))

			if s.focused && idx == s.cursor {
				line = s.styles.Selected.Render(fmt.Sprintf("▸ %s %s", name, size))
			}

			b.WriteString(line + "\n")
			idx++
		}
	}

	result := b.String()
	lines := strings.Split(result, "\n")
	for len(lines) < height {
		lines = append(lines, "")
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

func formatFileSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
}
