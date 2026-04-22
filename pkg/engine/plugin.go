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
	// Capabilities returns the abstract capabilities this plugin advertises
	// to the engine (e.g. "memory.history", "control.cancel"). Other plugins
	// can require a capability name via Requirement.Capability rather than a
	// concrete plugin ID, letting the engine resolve a provider at boot.
	// Return nil when the plugin advertises nothing.
	Capabilities() []Capability
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

// Capability is an abstract service a plugin advertises. Names are dotted
// namespaces mirroring event types ("memory.history", "memory.compaction",
// "control.cancel", "tool.catalog"). Description is a short, human-readable
// blurb for introspection (e.g. self-description prompts, debug logs).
type Capability struct {
	// Name is the dotted capability identifier (e.g. "memory.history").
	Name string
	// Description is a one-line human-readable summary of what the capability
	// provides. Shown in logs and surfaced through eng.Capabilities() for
	// agents that describe themselves.
	Description string
}

// Requirement declares another plugin that a plugin needs to be active.
// Exactly one of ID or Capability must be set.
//
// ID-based lifecycle expansion:
//   - If the required ID is already in the user's active list, nothing changes;
//     the user's config wins entirely (no field-level merge).
//   - If the required ID is not active and the factory is registered, it is
//     appended to the active set and Default is installed as its config only
//     when the user has not configured that ID at all.
//   - If the factory is not registered and Optional is true, the requirement
//     is skipped with a WARN log; boot continues.
//   - If the factory is not registered and Optional is false, boot fails.
//
// Capability-based lifecycle expansion:
//   - If an explicit pin exists in Config.Capabilities, that provider is used.
//   - Otherwise the first active plugin advertising the capability (in
//     plugins.active order) satisfies the requirement.
//   - Otherwise the engine auto-activates a registered provider advertising
//     the capability, sorted alphabetically with a WARN log when more than
//     one candidate exists without an explicit pin.
//   - Default is installed on the resolved provider ID using the same
//     whole-object replace rule as ID-based requirements.
//
// The merge rule for Default is whole-object replace: when the user has
// supplied any config for the resolved ID, Default is discarded entirely.
// This keeps precedence predictable and avoids surprise field merges.
type Requirement struct {
	// ID is the plugin ID to auto-activate (e.g. "nexus.memory.capped").
	// Mutually exclusive with Capability.
	ID string
	// Capability is an abstract capability name (e.g. "memory.history") the
	// engine resolves to a concrete provider at boot. Mutually exclusive
	// with ID.
	Capability string
	// Default is the config to install when the user has not configured the
	// resolved ID. Ignored entirely when the user supplies any config.
	Default map[string]any
	// Optional controls behavior when the requirement cannot be satisfied:
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

	// Capabilities is a snapshot of capability → provider-IDs resolved at
	// boot. Plugins that want to conditionally enable behavior when a given
	// capability is active can inspect it here rather than string-matching
	// specific plugin IDs.
	Capabilities map[string][]string

	// InstanceID is set when a plugin is activated with an instance suffix
	// (e.g. "nexus.agent.subagent/researcher"). It contains the full ID
	// including the suffix. Plugins that support multiple instances should
	// use this as their identity instead of their hardcoded ID.
	InstanceID string
}
