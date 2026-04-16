package engine

import (
	"context"
	"log/slog"
)

// Plugin is the interface that all Nexus plugins must implement.
type Plugin interface {
	// ID returns the unique identifier for this plugin (e.g. "provider.anthropic").
	ID() string
	// Name returns a human-readable name.
	Name() string
	// Version returns the plugin version string.
	Version() string
	// Dependencies returns a list of plugin IDs this plugin depends on.
	Dependencies() []string
	// Init initializes the plugin with its context. Called during boot.
	Init(ctx PluginContext) error
	// Ready is called after all plugins have been initialized.
	Ready() error
	// Shutdown gracefully stops the plugin.
	Shutdown(ctx context.Context) error
	// Subscriptions declares the events this plugin listens to.
	Subscriptions() []EventSubscription
	// Emissions declares the event types this plugin may emit.
	Emissions() []string
}

// PluginContext provides plugins with access to engine services.
type PluginContext struct {
	Config  map[string]any
	Bus     EventBus
	Logger  *slog.Logger
	DataDir string
	Session *SessionWorkspace
	Models  *ModelRegistry
	Prompts *PromptRegistry
	System  *SystemInfo

	// InstanceID is set when a plugin is activated with an instance suffix
	// (e.g. "nexus.agent.subagent/researcher"). It contains the full ID
	// including the suffix. Plugins that support multiple instances should
	// use this as their identity instead of their hardcoded ID.
	InstanceID string
}
