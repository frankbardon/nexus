package tui

import "github.com/charmbracelet/lipgloss"

// Theme holds all lipgloss styles for the TUI.
type Theme struct {
	Name string

	// Base colors
	Primary    lipgloss.Color
	Background lipgloss.Color
	Surface    lipgloss.Color
	Border     lipgloss.Color
	Text       lipgloss.Color
	TextDim    lipgloss.Color

	// Role colors
	Assistant lipgloss.Color
	User      lipgloss.Color
	System    lipgloss.Color
	Tool      lipgloss.Color
	Error     lipgloss.Color

	// Semantic
	Success lipgloss.Color
	Warning lipgloss.Color
	Danger  lipgloss.Color
}

var darkTheme = Theme{
	Name:       "dark",
	Primary:    lipgloss.Color("#7C3AED"),
	Background: lipgloss.Color("#1a1b26"),
	Surface:    lipgloss.Color("#24283b"),
	Border:     lipgloss.Color("#3b4261"),
	Text:       lipgloss.Color("#c0caf5"),
	TextDim:    lipgloss.Color("#565f89"),
	Assistant:  lipgloss.Color("#9ece6a"),
	User:       lipgloss.Color("#7aa2f7"),
	System:     lipgloss.Color("#e0af68"),
	Tool:       lipgloss.Color("#7dcfff"),
	Error:      lipgloss.Color("#f7768e"),
	Success:    lipgloss.Color("#9ece6a"),
	Warning:    lipgloss.Color("#e0af68"),
	Danger:     lipgloss.Color("#f7768e"),
}

var lightTheme = Theme{
	Name:       "light",
	Primary:    lipgloss.Color("#7C3AED"),
	Background: lipgloss.Color("#f5f5f5"),
	Surface:    lipgloss.Color("#e8e8e8"),
	Border:     lipgloss.Color("#d0d0d0"),
	Text:       lipgloss.Color("#1a1a1a"),
	TextDim:    lipgloss.Color("#888888"),
	Assistant:  lipgloss.Color("#4d7c0f"),
	User:       lipgloss.Color("#1d4ed8"),
	System:     lipgloss.Color("#b45309"),
	Tool:       lipgloss.Color("#0284c7"),
	Error:      lipgloss.Color("#dc2626"),
	Success:    lipgloss.Color("#4d7c0f"),
	Warning:    lipgloss.Color("#b45309"),
	Danger:     lipgloss.Color("#dc2626"),
}

// Styles holds computed lipgloss styles derived from a theme.
type Styles struct {
	Theme Theme

	// Layout
	Sidebar       lipgloss.Style
	MainPanel     lipgloss.Style
	RightRail     lipgloss.Style
	StatusBar     lipgloss.Style
	InputArea     lipgloss.Style
	SidebarHeader lipgloss.Style

	// Chat bubbles
	UserBubble      lipgloss.Style
	AssistantBubble lipgloss.Style
	SystemBubble    lipgloss.Style
	ToolBubble      lipgloss.Style
	ErrorBubble     lipgloss.Style

	// Labels
	UserLabel      lipgloss.Style
	AssistantLabel lipgloss.Style
	SystemLabel    lipgloss.Style
	ToolLabel      lipgloss.Style
	ErrorLabel     lipgloss.Style

	// Components
	TabActive   lipgloss.Style
	TabInactive lipgloss.Style
	FileItem    lipgloss.Style
	PluginItem  lipgloss.Style
	PlanStep    lipgloss.Style
	ThinkStep   lipgloss.Style
	ApprovalBox lipgloss.Style
	RiskHigh    lipgloss.Style
	RiskNormal  lipgloss.Style

	// Misc
	Logo     lipgloss.Style
	Dim      lipgloss.Style
	Bold     lipgloss.Style
	Spinner  lipgloss.Style
	Accent   lipgloss.Style
	Selected lipgloss.Style
}

func newStyles(t Theme) Styles {
	s := Styles{Theme: t}

	// Layout
	s.Sidebar = lipgloss.NewStyle().
		Width(30).
		BorderRight(true).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(t.Border)

	s.MainPanel = lipgloss.NewStyle()

	s.RightRail = lipgloss.NewStyle().
		Width(40).
		BorderLeft(true).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(t.Border)

	s.StatusBar = lipgloss.NewStyle().
		Foreground(t.TextDim).
		Padding(0, 1)

	s.InputArea = lipgloss.NewStyle().
		BorderTop(true).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(t.Border).
		Padding(0, 1)

	s.SidebarHeader = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.Primary).
		Padding(0, 1)

	// Chat bubbles
	s.UserBubble = lipgloss.NewStyle().
		Foreground(t.User).
		Padding(0, 1).
		MarginLeft(4)

	s.AssistantBubble = lipgloss.NewStyle().
		Foreground(t.Text).
		Padding(0, 1).
		MarginRight(4)

	s.SystemBubble = lipgloss.NewStyle().
		Foreground(t.System).
		Padding(0, 1).
		MarginRight(4)

	s.ToolBubble = lipgloss.NewStyle().
		Foreground(t.Tool).
		Padding(0, 1).
		MarginRight(4)

	s.ErrorBubble = lipgloss.NewStyle().
		Foreground(t.Error).
		Padding(0, 1).
		MarginRight(4)

	// Labels
	s.UserLabel = lipgloss.NewStyle().
		Foreground(t.User).
		Bold(true)

	s.AssistantLabel = lipgloss.NewStyle().
		Foreground(t.Assistant).
		Bold(true)

	s.SystemLabel = lipgloss.NewStyle().
		Foreground(t.System).
		Bold(true)

	s.ToolLabel = lipgloss.NewStyle().
		Foreground(t.Tool).
		Bold(true)

	s.ErrorLabel = lipgloss.NewStyle().
		Foreground(t.Error).
		Bold(true)

	// Tabs
	s.TabActive = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.Primary).
		BorderBottom(true).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(t.Primary).
		Padding(0, 2)

	s.TabInactive = lipgloss.NewStyle().
		Foreground(t.TextDim).
		Padding(0, 2)

	// Components
	s.FileItem = lipgloss.NewStyle().
		Foreground(t.Text).
		Padding(0, 1)

	s.PluginItem = lipgloss.NewStyle().
		Foreground(t.TextDim).
		Padding(0, 1)

	s.PlanStep = lipgloss.NewStyle().
		Padding(0, 1)

	s.ThinkStep = lipgloss.NewStyle().
		Foreground(t.TextDim).
		Padding(0, 1)

	s.ApprovalBox = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Warning).
		Padding(1, 2).
		Width(60)

	s.RiskHigh = lipgloss.NewStyle().
		Foreground(t.Danger).
		Bold(true)

	s.RiskNormal = lipgloss.NewStyle().
		Foreground(t.Warning).
		Bold(true)

	// Misc
	s.Logo = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.Primary)

	s.Dim = lipgloss.NewStyle().
		Foreground(t.TextDim)

	s.Bold = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.Text)

	s.Spinner = lipgloss.NewStyle().
		Foreground(t.Primary)

	s.Accent = lipgloss.NewStyle().
		Foreground(t.Primary)

	s.Selected = lipgloss.NewStyle().
		Foreground(t.Primary).
		Bold(true)

	return s
}
