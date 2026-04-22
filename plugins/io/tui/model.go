package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/frankbardon/nexus/pkg/ui"
)

type activeView int

const (
	viewChat activeView = iota
	viewFile
)

type focusPanel int

const (
	focusSidebar focusPanel = iota
	focusChat
	focusRightRail
)

const (
	sidebarWidth   = 30
	rightRailWidth = 40
)

// model is the root BubbleTea model.
type model struct {
	adapter *Adapter

	styles    Styles
	themes    []Theme
	themeIdx  int
	sidebar   sidebar
	chat      chatView
	rightRail rightRail
	approval  *approvalState
	askModal  *askState
	fileView  *fileView
	markdown  *markdownRenderer

	activeView activeView
	focus      focusPanel

	width, height int
	ready         bool

	agentBusy       bool // true when agent is processing
	cancelResumable bool // true when a cancelled turn can be resumed
}

func newModel(adapter *Adapter) model {
	themes := []Theme{darkTheme, lightTheme}
	styles := newStyles(themes[0])

	m := model{
		adapter:   adapter,
		styles:    styles,
		themes:    themes,
		themeIdx:  0,
		sidebar:   newSidebar(&styles),
		chat:      newChatView(&styles),
		rightRail: newRightRail(&styles),
		focus:     focusChat,
	}
	// Don't focus chat yet — wait for first WindowSizeMsg so terminal
	// query responses don't leak into the textarea.
	return m
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		m.chat.spinner.Tick,
		m.loadSessionInfo,
		m.loadPlugins,
	)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		firstReady := !m.ready
		m.ready = true
		m.recalcLayout()
		if firstReady {
			// Terminal init is done — safe to focus the textarea now.
			m.chat.Focus()
			// Reload plugins (may not have been written when Init ran).
			return m, tea.Batch(m.loadPlugins, m.loadFileList)
		}
		return m, nil

	case tea.KeyMsg:
		// Approval modal captures all input
		if m.approval != nil {
			resp := m.approval.Update(msg)
			if resp != nil {
				m.approval = nil
				// Send response to adapter's approval channel
				if m.adapter.approvalCh != nil {
					select {
					case m.adapter.approvalCh <- ui.ApprovalResponseMessage{
						PromptID: resp.PromptID,
						Approved: resp.Approved,
						Always:   resp.Always,
					}:
					default:
					}
				}
				return m, nil
			}
			return m, nil
		}

		// Ask modal captures all input
		if m.askModal != nil {
			resp, cmd := m.askModal.Update(msg)
			if resp != nil {
				m.askModal = nil
				if m.adapter.askCh != nil {
					select {
					case m.adapter.askCh <- *resp:
					default:
					}
				}
				return m, nil
			}
			return m, cmd
		}

		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "tab":
			m.cyclePanel()
			return m, nil
		case "ctrl+t":
			m.cycleTheme()
			return m, nil
		case "esc":
			if m.activeView == viewFile {
				m.activeView = viewChat
				m.fileView = nil
				m.recalcLayout()
				return m, nil
			}
			if m.agentBusy {
				m.agentBusy = false
				handler := m.adapter.cancelHandler
				if handler != nil {
					return m, func() tea.Msg {
						handler()
						return nil
					}
				}
				return m, nil
			}
		case "enter":
			// Check if sidebar is focused and on files tab — select file
			if m.focus == focusSidebar && m.sidebar.tab == tabFiles {
				if f := m.sidebar.SelectedFile(); f != nil {
					return m, m.openFile(f.Path)
				}
				return m, nil
			}
		}

		// Route keys to focused panel
		switch m.focus {
		case focusSidebar:
			cmd := m.sidebar.Update(msg)
			cmds = append(cmds, cmd)
		case focusChat:
			if m.activeView == viewFile && m.fileView != nil {
				cmd := m.fileView.Update(msg)
				cmds = append(cmds, cmd)
			} else {
				cmd, input := m.chat.Update(msg)
				cmds = append(cmds, cmd)
				if input != "" {
					m.chat.AddMessage(chatMessage{
						ID:      "user-input",
						Role:    "user",
						Content: input,
					})
					// Run input handler async to avoid blocking BubbleTea
					handler := m.adapter.inputHandler
					cmds = append(cmds, func() tea.Msg {
						if handler != nil {
							handler(ui.InputMessage{Content: input})
						}
						return nil
					})
				}
			}
		case focusRightRail:
			cmd := m.rightRail.Update(msg)
			cmds = append(cmds, cmd)
		}

	// Event bus messages
	case outputMsg:
		content := msg.Content
		if m.markdown != nil && (msg.Role == "assistant" || msg.Role == "tool") {
			content = m.markdown.Render(content)
		}
		m.chat.AddMessage(chatMessage{
			ID:      msg.TurnID,
			Role:    msg.Role,
			Content: content,
			TurnID:  msg.TurnID,
		})

	case streamChunkMsg:
		m.chat.AppendToStream(msg.TurnID, msg.Content)

	case outputClearMsg:
		m.chat.ClearStream()

	case streamEndMsg:
		// Apply markdown to the completed stream message
		if m.markdown != nil {
			for i := len(m.chat.messages) - 1; i >= 0; i-- {
				if m.chat.messages[i].IsStream && m.chat.messages[i].TurnID == msg.TurnID {
					m.chat.messages[i].Content = m.markdown.Render(m.chat.messages[i].Content)
					m.chat.messages[i].IsStream = false
					break
				}
			}
		}
		m.chat.EndStream()

	case statusMsg:
		m.chat.SetStatus(msg)
		m.agentBusy = msg.State != "idle"

	case thinkingMsg:
		phase := msg.Phase
		if phase == "" {
			phase = "thinking"
		}
		m.chat.AddMessage(chatMessage{
			ID:      "thinking-" + msg.TurnID,
			Role:    "thinking",
			Content: strings.ToUpper(phase) + ": " + msg.Content,
			TurnID:  msg.TurnID,
		})

	case codeExecStdoutMsg:
		m.chat.AppendCodeExecStdout(msg.CallID, msg.TurnID, msg.Chunk, msg.Final, msg.Truncated)

	case planDisplayMsg:
		m.rightRail.SetPlan(msg)
		m.recalcLayout()

	case planUpdateMsg:
		m.rightRail.UpdateSteps(msg)
		m.recalcLayout()

	case approvalRequestMsg:
		m.approval = &approvalState{
			request: msg,
			styles:  &m.styles,
		}

	case askUserMsg:
		state := newAskState(msg, &m.styles)
		m.askModal = &state

	case cancelCompleteMsg:
		m.cancelResumable = msg.Resumable

	case fileChangedMsg:
		cmds = append(cmds, m.loadFileList)

	case fileListMsg:
		m.sidebar.files = msg.Files

	case fileContentMsg:
		if m.fileView != nil && m.fileView.path == msg.Path {
			if msg.Err != nil {
				m.fileView.SetContent("Error loading file: " + msg.Err.Error())
			} else {
				m.fileView.SetContent(msg.Content)
			}
		}

	case sessionInfoMsg:
		m.sidebar.sessionID = msg.ID

	case pluginListMsg:
		m.sidebar.features = msg.Features
		m.rightRail.features = msg.Features
		m.recalcLayout()

	case spinner.TickMsg:
		cmd, _ := m.chat.Update(msg)
		cmds = append(cmds, cmd)

	case quitMsg:
		return m, tea.Quit

	default:
		// Forward unhandled messages (cursor blink, etc.) to focused chat
		if m.focus == focusChat {
			cmd, _ := m.chat.Update(msg)
			cmds = append(cmds, cmd)
		}
	}

	return m, tea.Batch(cmds...)
}

func (m model) View() string {
	if !m.ready {
		return ""
	}

	// Build main content
	var mainContent string
	if m.activeView == viewFile && m.fileView != nil {
		mainContent = m.fileView.View()
	} else {
		mainContent = m.chat.View()
	}

	// Compose columns
	columns := lipgloss.JoinHorizontal(lipgloss.Top,
		m.sidebar.View(),
		mainContent,
	)

	if m.rightRail.Visible() {
		columns = lipgloss.JoinHorizontal(lipgloss.Top,
			m.sidebar.View(),
			mainContent,
			m.rightRail.View(),
		)
	}

	// Overlay modals if active
	if m.approval != nil {
		return m.approval.View(m.width, m.height)
	}
	if m.askModal != nil {
		return m.askModal.View(m.width, m.height)
	}

	return columns
}

func (m *model) recalcLayout() {
	if m.width == 0 || m.height == 0 {
		return
	}

	railW := 0
	if m.rightRail.Visible() {
		railW = rightRailWidth
	}

	mainW := m.width - sidebarWidth - railW - 2 // borders
	if mainW < 20 {
		mainW = 20
	}

	m.sidebar.SetSize(sidebarWidth-1, m.height) // -1 for border
	m.chat.SetSize(mainW, m.height)
	m.rightRail.SetSize(railW, m.height)

	if m.fileView != nil {
		m.fileView.SetSize(mainW, m.height)
	}

	mdWidth := mainW - 10
	themeName := m.styles.Theme.Name
	if m.markdown == nil || m.markdown.width != mdWidth || m.markdown.themeName != themeName {
		m.markdown = newMarkdownRenderer(mdWidth, themeName)
	}
}

func (m *model) cyclePanel() {
	panels := []focusPanel{focusSidebar, focusChat}
	if m.rightRail.Visible() {
		panels = append(panels, focusRightRail)
	}

	// Find current index
	for i, p := range panels {
		if p == m.focus {
			next := panels[(i+1)%len(panels)]
			m.setFocus(next)
			return
		}
	}
	m.setFocus(focusChat)
}

func (m *model) setFocus(panel focusPanel) {
	m.sidebar.Blur()
	m.chat.Blur()
	m.rightRail.Blur()

	m.focus = panel
	switch panel {
	case focusSidebar:
		m.sidebar.Focus()
	case focusChat:
		m.chat.Focus()
	case focusRightRail:
		m.rightRail.Focus()
	}
}

func (m *model) cycleTheme() {
	m.themeIdx = (m.themeIdx + 1) % len(m.themes)
	m.styles = newStyles(m.themes[m.themeIdx])

	// Update all component style pointers
	m.sidebar.styles = &m.styles
	m.chat.styles = &m.styles
	m.rightRail.styles = &m.styles
	if m.approval != nil {
		m.approval.styles = &m.styles
	}
	if m.fileView != nil {
		m.fileView.styles = &m.styles
	}
	m.chat.spinner.Style = m.styles.Spinner
}

func (m model) loadSessionInfo() tea.Msg {
	if m.adapter.session == nil {
		return nil
	}
	return sessionInfoMsg{ID: m.adapter.session.ID}
}

func (m model) loadPlugins() tea.Msg {
	if m.adapter.session == nil {
		return pluginListMsg{}
	}
	data, err := m.adapter.session.ReadFile("metadata/plugins.json")
	if err != nil {
		return pluginListMsg{}
	}
	var manifest struct {
		Active []string `json:"active"`
	}
	if json.Unmarshal(data, &manifest) != nil {
		return pluginListMsg{}
	}

	features := make(map[string]bool)
	for _, id := range manifest.Active {
		if strings.HasPrefix(id, "nexus.planner.") {
			features["planner"] = true
		}
		if id == "nexus.observe.thinking" {
			features["thinking"] = true
		}
		if id == "nexus.skills" {
			features["skills"] = true
		}
		if strings.HasPrefix(id, "nexus.memory.") {
			features["memory"] = true
		}
		if id == "nexus.control.cancel" {
			features["cancel"] = true
		}
	}
	return pluginListMsg{Features: features}
}

func (m model) loadFileList() tea.Msg {
	if m.adapter.session == nil {
		return fileListMsg{}
	}
	var files []fileEntry
	filesDir := m.adapter.session.FilesDir()
	_ = filepath.WalkDir(filesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(filesDir, path)
		if err != nil || rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		files = append(files, fileEntry{
			Name:  d.Name(),
			Path:  rel,
			IsDir: d.IsDir(),
			Size:  info.Size(),
		})
		return nil
	})
	return fileListMsg{Files: files}
}

func (m *model) openFile(path string) tea.Cmd {
	fv := newFileView(&m.styles, path)
	m.fileView = &fv
	m.activeView = viewFile
	m.recalcLayout()

	return func() tea.Msg {
		if m.adapter.session == nil {
			return fileContentMsg{Path: path, Err: os.ErrNotExist}
		}
		data, err := m.adapter.session.ReadFile(filepath.Join("files", path))
		if err != nil {
			return fileContentMsg{Path: path, Err: err}
		}
		return fileContentMsg{Path: path, Content: string(data)}
	}
}
