package tui

import "github.com/charmbracelet/bubbles/key"

type keyMap struct {
	Quit        key.Binding
	Submit      key.Binding
	CyclePanel  key.Binding
	SwitchTab   key.Binding
	CycleTheme  key.Binding
	ScrollUp    key.Binding
	ScrollDown  key.Binding
	Back        key.Binding
	SelectItem  key.Binding
	NavUp       key.Binding
	NavDown     key.Binding
	ApproveYes  key.Binding
	ApproveDeny key.Binding
	AlwaysAllow key.Binding
}

var defaultKeys = keyMap{
	Quit: key.NewBinding(
		key.WithKeys("ctrl+c"),
		key.WithHelp("ctrl+c", "quit"),
	),
	Submit: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "send"),
	),
	CyclePanel: key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", "cycle panel"),
	),
	SwitchTab: key.NewBinding(
		key.WithKeys("1", "2"),
		key.WithHelp("1/2", "chat/files"),
	),
	CycleTheme: key.NewBinding(
		key.WithKeys("ctrl+t"),
		key.WithHelp("ctrl+t", "cycle theme"),
	),
	ScrollUp: key.NewBinding(
		key.WithKeys("pgup"),
		key.WithHelp("pgup", "scroll up"),
	),
	ScrollDown: key.NewBinding(
		key.WithKeys("pgdown"),
		key.WithHelp("pgdn", "scroll down"),
	),
	Back: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "back"),
	),
	SelectItem: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "select"),
	),
	NavUp: key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("up/k", "navigate up"),
	),
	NavDown: key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("down/j", "navigate down"),
	),
	ApproveYes: key.NewBinding(
		key.WithKeys("y"),
		key.WithHelp("y", "approve"),
	),
	ApproveDeny: key.NewBinding(
		key.WithKeys("d", "n"),
		key.WithHelp("d/n", "deny"),
	),
	AlwaysAllow: key.NewBinding(
		key.WithKeys("a"),
		key.WithHelp("a", "always allow"),
	),
}
