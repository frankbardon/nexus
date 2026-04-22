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
	// Dependencies() only validates boot order; it does NOT activate anything.
	// To auto-activate a required plugin, use Requires() instead.
	Dependencies() []string
	// Requires returns plugins this one needs active to function, and the
	// default config to use when the user has not configured them.
	// The lifecycle manager expands the active plugin set at boot to include
	// every Requirement reachable from a user-declared plugin. Requires()
	// differs from Dependencies() in that it activates; Dependencies() only
	// orders. See LifecycleManager.Boot for the expansion and merge rules.
	Requires() []Requirement
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

// Requirement declares another plugin that a plugin needs to be active.
// Lifecycle expansion rules:
//   - If the required ID is already in the user's active list, nothing changes;
//     the user's config wins entirely (no field-level merge).
//   - If the required ID is not active and the factory is registered, it is
//     appended to the active set and Default is installed as its config only
//     when the user has not configured that ID at all.
//   - If the factory is not registered and Optional is true, the requirement
//     is skipped with a WARN log; boot continues.
//   - If the factory is not registered and Optional is false, boot fails.
//
// The merge rule for Default is whole-object replace: when the user has
// supplied any config for the ID, Default is discarded entirely. This keeps
// precedence predictable and avoids surprise field merges.
type Requirement struct {
	// ID is the plugin ID to auto-activate (e.g. "nexus.memory.conversation").
	ID string
	// Default is the config to install when the user has not configured ID.
	// Ignored entirely when the user supplies any config for ID.
	Default map[string]any
	// Optional controls behavior when ID's factory is not registered.
	// Optional=true skips with a WARN; Optional=false fails boot.
	Optional bool
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
	Schemas *SchemaRegistry
	System  *SystemInfo

	// InstanceID is set when a plugin is activated with an instance suffix
	// (e.g. "nexus.agent.subagent/researcher"). It contains the full ID
	// including the suffix. Plugins that support multiple instances should
	// use this as their identity instead of their hardcoded ID.
	InstanceID string
}
