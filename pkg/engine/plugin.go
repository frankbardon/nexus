package engine

import (
	"context"
	"log/slog"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/engine/sandbox"
	"github.com/frankbardon/nexus/pkg/engine/storage"
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
	Config map[string]any
	Bus    EventBus
	Logger *slog.Logger
	// PluginID is the configured plugin ID, including any instance suffix
	// (e.g. "nexus.agent.subagent/researcher"). Set by the lifecycle
	// manager when the context is built. Plugins that need to identify
	// themselves to engine services (storage, journal projections) read
	// this rather than hardcoding their base ID.
	PluginID string
	// DataDir is the per-plugin session-scoped directory:
	// <session.RootDir>/plugins/<PluginID>/. Created lazily on first
	// access. Empty when the engine is constructed without a session
	// (rare; mostly tests). For storage that should outlive the session,
	// use AppDataDir or AgentDataDir, or call Storage(ScopeApp/ScopeAgent).
	DataDir string
	// AppDataDir is the per-plugin machine-wide directory:
	// ~/.nexus/plugins/<PluginID>/. Survives across sessions and agents.
	AppDataDir string
	// AgentDataDir is the per-plugin per-agent directory:
	// ~/.nexus/agents/<AgentID>/plugins/<PluginID>/. Collapses to
	// AppDataDir when the engine has no AgentID configured (CLI / single-
	// agent embedders).
	AgentDataDir string
	// Storage opens scoped SQLite-backed storage for this plugin. The
	// returned handle exposes both KV sugar (Get/Put/Delete/List) and
	// raw *sql.DB access for plugins that need joins, transactions, or
	// virtual tables (FTS5). Handles are pooled — repeated calls with
	// the same scope return the same underlying database.
	Storage func(scope storage.Scope) (storage.Storage, error)
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

	// Logging lets a plugin register itself as a sink for the engine's
	// structured logger. Intended for the logger plugin (or equivalents);
	// most plugins should ignore this field. Registration replays buffered
	// pre-init records, then dispatches live records. See LoggingHost.
	Logging LoggingHost

	// InstanceID is set when a plugin is activated with an instance suffix
	// (e.g. "nexus.agent.subagent/researcher"). It contains the full ID
	// including the suffix. Plugins that support multiple instances should
	// use this as their identity instead of their hardcoded ID.
	InstanceID string

	// Replay is the engine-wide replay coordination point. Always non-nil.
	// Side-effecting plugins (LLM providers, tools) check Replay.Active()
	// in their event handlers and pop a journaled response from the queue
	// instead of calling out. Idle outside of a replay run.
	Replay *ReplayState

	// Journal is the per-session durable event log. Non-nil after
	// startJournal — i.e. always populated when plugin Init runs. Plugins
	// that observe events post-journal (rather than via the live bus)
	// register via Journal.SubscribeProjection so their derived files are
	// driven by durable envelopes instead of live dispatch.
	Journal *journal.Writer

	// Sandbox is the per-plugin execution sandbox. Resolved at boot from
	// the plugin's `sandbox:` config block; falls back to a default host
	// backend when no block is supplied. Tools that shell out or evaluate
	// code call Sandbox.Exec rather than reaching for os/exec or an
	// in-process interpreter directly. Always non-nil during Init.
	Sandbox sandbox.Sandbox
}

// LateShutdown is an optional marker interface. A plugin that implements it
// and returns true is shut down after every non-late plugin, regardless of
// its position in dependency order. Late shutdown is intended for observers
// (loggers, tracers) that must stay active through peer plugins' Shutdown
// calls so the records emitted during teardown still reach their sink.
//
// A late plugin is still initialized in dependency order; only shutdown
// ordering shifts. Late plugins are shut down among themselves in reverse
// init order, just like the normal phase.
type LateShutdown interface {
	LateShutdown() bool
}

// DrainOverride is an optional interface a plugin implements when it needs
// a longer drain window than the engine default for in-flight work to
// complete (batch pollers flushing pending submissions, MCP servers waiting
// on remote acks). The engine takes the maximum of the configured
// engine.shutdown.drain_timeout and every plugin override before draining
// the bus, so a single slow plugin can extend the window without forcing
// every operator to bump the global default. Returning a non-positive
// duration is equivalent to not implementing the interface.
type DrainOverride interface {
	DrainTimeout() time.Duration
}
