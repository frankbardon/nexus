package engine

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/engine/sandbox"
	"github.com/frankbardon/nexus/pkg/engine/storage"

	// Side-effect import: registers the default host sandbox factory so
	// resolveSandbox can construct the baseline backend even when no plugin
	// explicitly opts into a stricter tier.
	_ "github.com/frankbardon/nexus/pkg/engine/sandbox/host"
)

// PluginBaseID extracts the base plugin ID from a potentially instanced ID.
// For example, "nexus.agent.subagent/researcher" returns "nexus.agent.subagent".
// A plain ID like "nexus.agent.react" returns itself unchanged.
func PluginBaseID(id string) string {
	if idx := strings.IndexByte(id, '/'); idx >= 0 {
		return id[:idx]
	}
	return id
}

// LifecycleManager orchestrates plugin boot and shutdown sequences.
type LifecycleManager struct {
	registry *PluginRegistry
	bus      EventBus
	config   *Config
	plugins  []Plugin
	logger   *slog.Logger
	logging  LoggingHost
	session  *SessionWorkspace
	models   *ModelRegistry
	prompts  *PromptRegistry
	schemas  *SchemaRegistry
	system   *SystemInfo
	replay   *ReplayState
	journal  *journal.Writer
	storage  *storage.Manager
	// provenance records why each configured ID is active: the zero value
	// (empty requiredBy) means the user listed it explicitly; otherwise it
	// was auto-activated to satisfy the named plugin's Requires().
	provenance map[string]activationOrigin
	// capabilities maps capability name to the list of active provider IDs
	// that advertise it. Populated at the end of Boot; exposed via
	// Engine.Capabilities() for introspection.
	capabilities map[string][]string
}

// activationOrigin records why a plugin appears in the expanded active set.
type activationOrigin struct {
	// requiredBy is the plugin ID that pulled this one in via Requires().
	// Empty when the user listed the plugin in config.Plugins.Active.
	requiredBy string
	// configFromDefault is true when the plugin's config was installed from
	// a Requirement.Default rather than supplied by the user.
	configFromDefault bool
}

// NewLifecycleManager creates a new lifecycle manager.
func NewLifecycleManager(registry *PluginRegistry, bus EventBus, config *Config, logger *slog.Logger, models *ModelRegistry, prompts *PromptRegistry, schemas *SchemaRegistry, system *SystemInfo) *LifecycleManager {
	return &LifecycleManager{
		registry: registry,
		bus:      bus,
		config:   config,
		logger:   logger,
		models:   models,
		prompts:  prompts,
		schemas:  schemas,
		system:   system,
	}
}

// Boot initializes all active plugins in dependency order, then signals readiness.
func (lm *LifecycleManager) Boot(ctx context.Context) error {
	lm.logger.Info("booting engine")

	activeIDs := lm.config.Plugins.Active
	if len(activeIDs) == 0 {
		lm.logger.Info("no active plugins configured")
		_ = lm.bus.Emit("core.boot", nil)
		_ = lm.bus.Emit("core.ready", nil)
		return nil
	}

	// Create plugin instances from the registry.
	// Supports instanced IDs: "nexus.agent.subagent/researcher" uses the
	// factory registered under "nexus.agent.subagent" but is treated as a
	// separate plugin with its own config.
	pluginMap := make(map[string]Plugin, len(activeIDs))
	instanceIDs := make(map[string]string, len(activeIDs)) // configuredID -> instanceID (only if instanced)
	for _, id := range activeIDs {
		baseID := PluginBaseID(id)
		factory, ok := lm.registry.Get(baseID)
		if !ok {
			return fmt.Errorf("plugin %q not found in registry", baseID)
		}
		pluginMap[id] = factory()
		if baseID != id {
			instanceIDs[id] = id
		}
	}

	// Expand the active set with Requires() transitively. Auto-activated
	// plugins inherit the user-supplied config if present; otherwise they get
	// the Default from the Requirement (whole-object replace, never merged).
	activeIDs, err := lm.expandRequirements(activeIDs, pluginMap, instanceIDs)
	if err != nil {
		return err
	}

	lm.logActivePlugins(activeIDs)

	// Build final capability → providers map from the fully expanded active
	// set. Emit one INFO per resolved capability for operator visibility.
	lm.capabilities = make(map[string][]string)
	for _, id := range activeIDs {
		for _, c := range pluginMap[id].Capabilities() {
			lm.capabilities[c.Name] = append(lm.capabilities[c.Name], id)
		}
	}
	capNames := make([]string, 0, len(lm.capabilities))
	for name := range lm.capabilities {
		capNames = append(capNames, name)
	}
	sort.Strings(capNames)
	for _, name := range capNames {
		providers := lm.capabilities[name]
		source := "active-list"
		if pinned, ok := lm.config.Capabilities[name]; ok && pinned != "" && stringsContain(providers, pinned) {
			source = "explicit-config"
		}
		lm.logger.Info("capability resolved",
			"capability", name,
			"providers", providers,
			"source", source)
	}

	// Topological sort based on dependencies.
	sortedIDs, err := lm.topoSort(activeIDs, pluginMap)
	if err != nil {
		return fmt.Errorf("dependency resolution failed: %w", err)
	}
	lm.plugins = make([]Plugin, len(sortedIDs))
	for i, id := range sortedIDs {
		lm.plugins[i] = pluginMap[id]
	}

	_ = lm.bus.Emit("core.boot", nil)

	// Init phase.
	for _, configuredID := range sortedIDs {
		p := pluginMap[configuredID]
		pluginCfg := lm.config.Plugins.Configs[configuredID]
		if pluginCfg == nil {
			pluginCfg = make(map[string]any)
		}

		sb, err := lm.resolveSandbox(configuredID, pluginCfg)
		if err != nil {
			return fmt.Errorf("plugin %q sandbox resolve failed: %w", configuredID, err)
		}

		pctx := PluginContext{
			Config:       pluginCfg,
			Bus:          lm.bus,
			Logger:       lm.logger.With("plugin", configuredID),
			PluginID:     configuredID,
			DataDir:      lm.sessionPluginDir(configuredID),
			AppDataDir:   lm.appPluginDir(configuredID),
			AgentDataDir: lm.agentPluginDir(configuredID),
			Storage:      lm.storageOpener(configuredID),
			Session:      lm.session,
			Models:       lm.models,
			Prompts:      lm.prompts,
			Schemas:      lm.schemas,
			System:       lm.system,
			Capabilities: lm.capabilitiesCopy(),
			Logging:      lm.logging,
			InstanceID:   instanceIDs[configuredID],
			Replay:       lm.replay,
			Journal:      lm.journal,
			Sandbox:      sb,
		}

		lm.logger.Info("initializing plugin", "plugin", configuredID, "version", p.Version())
		if err := p.Init(pctx); err != nil {
			return fmt.Errorf("plugin %q init failed: %w", configuredID, err)
		}
	}

	// Ready phase (parallel, per PRD 4.5).
	var readyWg sync.WaitGroup
	readyErrs := make(chan error, len(lm.plugins))
	for _, p := range lm.plugins {
		readyWg.Add(1)
		go func(pl Plugin) {
			defer readyWg.Done()
			lm.logger.Info("plugin ready", "plugin", pl.ID())
			if err := pl.Ready(); err != nil {
				readyErrs <- fmt.Errorf("plugin %q ready failed: %w", pl.ID(), err)
			}
		}(p)
	}
	readyWg.Wait()
	close(readyErrs)
	for err := range readyErrs {
		return err
	}

	_ = lm.bus.Emit("core.ready", nil)
	lm.logger.Info("engine ready", "plugins", len(lm.plugins))
	return nil
}

// Shutdown tears down plugins in reverse order, drains the bus, and signals
// completion. Plugins that implement LateShutdown and return true are shut
// down after every non-late plugin, so observers (logger, tracers) can still
// receive records emitted during peer teardown.
func (lm *LifecycleManager) Shutdown(ctx context.Context) error {
	lm.logger.Info("shutting down engine")

	_ = lm.bus.Emit("core.shutdown", nil)

	if err := lm.bus.Drain(ctx); err != nil {
		lm.logger.Warn("drain timed out", "error", err)
	}

	// Partition plugins into normal and late phases. Preserve init order so
	// that reverse iteration below mirrors the non-late topological order.
	normal := make([]Plugin, 0, len(lm.plugins))
	late := make([]Plugin, 0)
	for _, p := range lm.plugins {
		if ls, ok := p.(LateShutdown); ok && ls.LateShutdown() {
			late = append(late, p)
			continue
		}
		normal = append(normal, p)
	}

	var firstErr error
	shutdown := func(plugins []Plugin, phase string) {
		for i := len(plugins) - 1; i >= 0; i-- {
			p := plugins[i]
			lm.logger.Info("shutting down plugin", "plugin", p.ID(), "phase", phase)
			if err := p.Shutdown(ctx); err != nil {
				lm.logger.Error("plugin shutdown error", "plugin", p.ID(), "phase", phase, "error", err)
				if firstErr == nil {
					firstErr = fmt.Errorf("plugin %q shutdown failed: %w", p.ID(), err)
				}
			}
		}
	}

	shutdown(normal, "normal")
	shutdown(late, "late")

	lm.logger.Info("engine shutdown complete")
	return firstErr
}

// Plugins returns the ordered list of initialized plugins.
func (lm *LifecycleManager) Plugins() []Plugin {
	return lm.plugins
}

// Capabilities returns a defensive copy of the capability → provider-IDs map
// built during Boot. Callers can mutate the returned map safely.
func (lm *LifecycleManager) Capabilities() map[string][]string {
	return lm.capabilitiesCopy()
}

func (lm *LifecycleManager) capabilitiesCopy() map[string][]string {
	if lm.capabilities == nil {
		return nil
	}
	out := make(map[string][]string, len(lm.capabilities))
	for k, v := range lm.capabilities {
		dup := make([]string, len(v))
		copy(dup, v)
		out[k] = dup
	}
	return out
}

// sessionPluginDir returns the per-plugin session-scoped directory used for
// PluginContext.DataDir. Empty when no session is active (rare; mostly tests).
func (lm *LifecycleManager) sessionPluginDir(pluginID string) string {
	if lm.session == nil {
		return ""
	}
	return lm.session.PluginDir(pluginID)
}

// appPluginDir returns the per-plugin App-scope directory used for
// PluginContext.AppDataDir. Created lazily by the consumer; the lifecycle
// only resolves the path.
func (lm *LifecycleManager) appPluginDir(pluginID string) string {
	root := storageRoot(lm.config)
	return filepath.Join(root, "plugins", pluginID)
}

// agentPluginDir returns the per-plugin Agent-scope directory used for
// PluginContext.AgentDataDir. Collapses to the App-scope path when no
// AgentID is configured (CLI / single-agent embedders).
func (lm *LifecycleManager) agentPluginDir(pluginID string) string {
	root := storageRoot(lm.config)
	if lm.config.Core.AgentID == "" {
		return filepath.Join(root, "plugins", pluginID)
	}
	return filepath.Join(root, "agents", lm.config.Core.AgentID, "plugins", pluginID)
}

// storageOpener returns the closure that PluginContext.Storage exposes.
// Captures pluginID once so the plugin never has to pass its own ID. Returns
// a closure that always errors when no storage manager is wired (test
// harnesses can opt out by leaving lm.storage nil).
func (lm *LifecycleManager) storageOpener(pluginID string) func(storage.Scope) (storage.Storage, error) {
	if lm.storage == nil {
		return func(scope storage.Scope) (storage.Storage, error) {
			return nil, fmt.Errorf("storage: no manager attached (engine constructed without storage)")
		}
	}
	mgr := lm.storage
	return func(scope storage.Scope) (storage.Storage, error) {
		return mgr.Open(scope, pluginID)
	}
}

// resolveSandbox builds the per-plugin sandbox.Sandbox. Plugins with a
// `sandbox:` config block use the named backend; plugins without one fall
// back to a default host backend, which honors no command-allowlist and is
// effectively the legacy "exec freely" behavior. Strict-mode boot: an
// unknown backend name returns an error so the engine never silently
// downgrades to host.
func (lm *LifecycleManager) resolveSandbox(pluginID string, pluginCfg map[string]any) (sandbox.Sandbox, error) {
	name, block := sandbox.FromPluginConfig(pluginCfg, sandbox.BackendHost)
	if block == nil {
		// No `sandbox:` block — use a baseline host backend so callers can
		// still rely on ctx.Sandbox being non-nil. Empty config = no
		// allowed-commands restriction (preserves prior behavior).
		sb, err := sandbox.New(sandbox.BackendHost, nil)
		if err != nil {
			return nil, fmt.Errorf("default host sandbox for %q: %w", pluginID, err)
		}
		return sb, nil
	}
	if lm.session != nil {
		block = substituteSessionPlaceholders(block, lm.session.ID).(map[string]any)
	}
	sb, err := sandbox.New(name, block)
	if err != nil {
		return nil, fmt.Errorf("sandbox %q for %q: %w", name, pluginID, err)
	}
	return sb, nil
}

// substituteSessionPlaceholders walks a config tree and replaces ${session_id}
// in every string with sessionID. Returns a new tree — the input is not
// mutated. Used so configs can hard-code session-scoped paths like
// `~/.nexus/sessions/${session_id}/files` and have the engine resolve the
// real session ID at boot time. Strings without the placeholder pass through
// unchanged; non-string scalars are returned as-is.
func substituteSessionPlaceholders(v any, sessionID string) any {
	switch x := v.(type) {
	case string:
		if !strings.Contains(x, "${session_id}") {
			return x
		}
		return strings.ReplaceAll(x, "${session_id}", sessionID)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = substituteSessionPlaceholders(vv, sessionID)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = substituteSessionPlaceholders(vv, sessionID)
		}
		return out
	default:
		return v
	}
}

// expandRequirements walks Requires() transitively on every plugin already in
// activeIDs, adding missing plugins to the set. It mutates pluginMap and
// config.Plugins.Configs in place, records provenance, and logs each
// auto-activation. Returns the expanded active list in a stable order
// (user-declared IDs first, then auto-activations in discovery order).
//
// Requirements come in two flavors, ID- and Capability-based:
//
//   - ID: the Requirement names a concrete plugin ID. If already active (by
//     exact match or base-of-instance), satisfied; otherwise the registered
//     factory is used and Default is installed when the user has no config.
//
//   - Capability: the Requirement names an abstract capability. Resolution
//     order is (1) explicit pin in Config.Capabilities, (2) first currently
//     active provider advertising the capability, (3) auto-activate a
//     registered provider that advertises it. When more than one registered
//     candidate exists without an explicit pin, the engine picks
//     alphabetically and emits a WARN naming every candidate.
//
// Merge rule: when a Requirement resolves to an ID that is already in the
// active set or already has user-supplied config, the user's config wins
// entirely and the Requirement.Default is discarded. No field-level merge.
func (lm *LifecycleManager) expandRequirements(
	activeIDs []string,
	pluginMap map[string]Plugin,
	instanceIDs map[string]string,
) ([]string, error) {
	lm.provenance = make(map[string]activationOrigin, len(activeIDs))
	for _, id := range activeIDs {
		lm.provenance[id] = activationOrigin{} // user-declared
	}

	activeSet := make(map[string]bool, len(activeIDs))
	for _, id := range activeIDs {
		activeSet[id] = true
	}
	// baseActive tracks whether any configured ID shares this base ID so a
	// Requirement on the base can be considered satisfied when an instanced
	// form is already active (mirrors topoSort's resolveDep behavior).
	baseActive := make(map[string]bool, len(activeIDs))
	for _, id := range activeIDs {
		baseActive[PluginBaseID(id)] = true
	}

	// activeCapProviders maps capability name to active plugin IDs (in
	// active-list order) that advertise it. Seeded from the user's active
	// list, then extended as auto-activations resolve more capabilities.
	activeCapProviders := make(map[string][]string)
	for _, id := range activeIDs {
		for _, c := range pluginMap[id].Capabilities() {
			activeCapProviders[c.Name] = append(activeCapProviders[c.Name], id)
		}
	}

	// registryCapProviders lazily maps capability name to every registered
	// plugin ID that advertises it, alphabetically sorted. Built once on
	// first need; probing the registry instantiates factories so we only
	// pay when a capability-based Requirement cannot be satisfied by the
	// already-active set.
	var registryCapProviders map[string][]string
	scanRegistry := func() map[string][]string {
		if registryCapProviders != nil {
			return registryCapProviders
		}
		registryCapProviders = make(map[string][]string)
		ids := lm.registry.List()
		sort.Strings(ids)
		for _, id := range ids {
			f, ok := lm.registry.Get(id)
			if !ok {
				continue
			}
			probe := f()
			for _, c := range probe.Capabilities() {
				registryCapProviders[c.Name] = append(registryCapProviders[c.Name], id)
			}
		}
		return registryCapProviders
	}

	// BFS: process plugins in order; each may pull in more.
	queue := append([]string(nil), activeIDs...)
	for len(queue) > 0 {
		src := queue[0]
		queue = queue[1:]

		reqs := pluginMap[src].Requires()
		for _, req := range reqs {
			if req.ID != "" && req.Capability != "" {
				return nil, fmt.Errorf("plugin %q has a Requirement with both ID=%q and Capability=%q; they are mutually exclusive", src, req.ID, req.Capability)
			}
			if req.ID == "" && req.Capability == "" {
				continue
			}

			// Resolve Capability → concrete provider ID first.
			resolvedID := req.ID
			resolveSource := ""
			if req.Capability != "" {
				id, source, err := lm.resolveCapability(req, src, activeCapProviders, scanRegistry)
				if err != nil {
					if req.Optional {
						lm.logger.Warn("optional capability requirement skipped",
							"required_by", src,
							"capability", req.Capability,
							"error", err.Error())
						continue
					}
					return nil, err
				}
				resolvedID = id
				resolveSource = source
			}

			// Already satisfied? Either by exact ID or by some instance of
			// the base. User-supplied config wins; nothing else to do.
			if activeSet[resolvedID] || baseActive[PluginBaseID(resolvedID)] {
				if req.Capability != "" {
					lm.logger.Info("capability satisfied",
						"capability", req.Capability,
						"provider", resolvedID,
						"required_by", src,
						"source", resolveSource)
				}
				continue
			}

			baseID := PluginBaseID(resolvedID)
			factory, ok := lm.registry.Get(baseID)
			if !ok {
				if req.Optional {
					lm.logger.Warn("optional requirement skipped: factory not registered",
						"required_by", src,
						"required_id", resolvedID)
					continue
				}
				return nil, fmt.Errorf("plugin %q requires %q which has no registered factory", src, resolvedID)
			}

			// Activate. Use user config if present, else install Default.
			userCfg := lm.config.Plugins.Configs[resolvedID]
			fromDefault := false
			if userCfg == nil && len(req.Default) > 0 {
				cfgCopy := make(map[string]any, len(req.Default))
				for k, v := range req.Default {
					cfgCopy[k] = v
				}
				if lm.config.Plugins.Configs == nil {
					lm.config.Plugins.Configs = make(map[string]map[string]any)
				}
				lm.config.Plugins.Configs[resolvedID] = cfgCopy
				fromDefault = true
			}

			inst := factory()
			pluginMap[resolvedID] = inst
			if baseID != resolvedID {
				instanceIDs[resolvedID] = resolvedID
			}
			activeIDs = append(activeIDs, resolvedID)
			activeSet[resolvedID] = true
			baseActive[baseID] = true
			for _, c := range inst.Capabilities() {
				activeCapProviders[c.Name] = append(activeCapProviders[c.Name], resolvedID)
			}
			lm.provenance[resolvedID] = activationOrigin{
				requiredBy:        src,
				configFromDefault: fromDefault,
			}
			queue = append(queue, resolvedID)

			configSource := "user-override"
			if fromDefault {
				configSource = "default"
			} else if userCfg == nil {
				configSource = "empty"
			}
			if req.Capability != "" {
				lm.logger.Info("auto-activating plugin",
					"plugin", resolvedID,
					"required_by", src,
					"capability", req.Capability,
					"capability_source", resolveSource,
					"config_source", configSource)
			} else {
				lm.logger.Info("auto-activating plugin",
					"plugin", resolvedID,
					"required_by", src,
					"config_source", configSource)
			}
		}
	}

	return activeIDs, nil
}

// resolveCapability returns the provider ID that should satisfy a
// Capability-based Requirement and a short string describing where the
// resolution came from ("explicit-config" | "active-list" | "auto-activated").
// Precedence:
//  1. Config.Capabilities[cap] pins a provider; must either already be active
//     or have a registered factory that advertises the capability.
//  2. The first currently active plugin that advertises the capability wins.
//  3. Walk the registry; the alphabetically first provider wins. When more
//     than one provider exists, emit a WARN naming every candidate.
//
// Returns an error only when no candidate can be found at all.
func (lm *LifecycleManager) resolveCapability(
	req Requirement,
	src string,
	activeCapProviders map[string][]string,
	scanRegistry func() map[string][]string,
) (string, string, error) {
	capName := req.Capability

	// 1. Explicit config pin.
	if pinned, ok := lm.config.Capabilities[capName]; ok && pinned != "" {
		// Validate the pinned ID can actually satisfy the capability.
		if providers := activeCapProviders[capName]; stringsContain(providers, pinned) {
			return pinned, "explicit-config", nil
		}
		// Pin may point at a not-yet-active registered provider.
		candidates := scanRegistry()[capName]
		if stringsContain(candidates, pinned) {
			return pinned, "explicit-config", nil
		}
		return "", "", fmt.Errorf("plugin %q requires capability %q pinned to %q in config, but no registered provider advertises that capability with that ID", src, capName, pinned)
	}

	// 2. Active provider (first in active-list order).
	if providers := activeCapProviders[capName]; len(providers) > 0 {
		return providers[0], "active-list", nil
	}

	// 3. Registry scan.
	candidates := scanRegistry()[capName]
	if len(candidates) == 0 {
		return "", "", fmt.Errorf("plugin %q requires capability %q but no registered plugin advertises it", src, capName)
	}
	if len(candidates) > 1 {
		lm.logger.Warn("capability has multiple candidate providers; picking first alphabetically — pin one explicitly under capabilities: in config to silence",
			"capability", capName,
			"required_by", src,
			"candidates", candidates,
			"picked", candidates[0])
	}
	return candidates[0], "auto-activated", nil
}

// stringsContain reports whether needle is in haystack. Tiny helper kept
// local to avoid leaking a generic "contains" into the engine package.
func stringsContain(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// logActivePlugins emits a single INFO line summarizing the final active
// plugin set with provenance annotations. Makes auto-activation visible at a
// glance in logs.
func (lm *LifecycleManager) logActivePlugins(activeIDs []string) {
	annotated := make([]string, len(activeIDs))
	for i, id := range activeIDs {
		origin := lm.provenance[id]
		if origin.requiredBy == "" {
			annotated[i] = id + " [user]"
			continue
		}
		tag := "auto: required-by=" + origin.requiredBy
		if origin.configFromDefault {
			tag += ",config=default"
		} else {
			tag += ",config=user-override"
		}
		annotated[i] = id + " [" + tag + "]"
	}
	lm.logger.Info("active plugin set resolved",
		"count", len(activeIDs),
		"plugins", annotated)
}

// topoSort performs a topological sort on plugin dependencies.
// It fails fast on cycles or missing dependencies.
// It supports instanced plugin IDs: a dependency on "nexus.agent.subagent" is
// satisfied by any active ID with that base (e.g. "nexus.agent.subagent/researcher").
func (lm *LifecycleManager) topoSort(activeIDs []string, pluginMap map[string]Plugin) ([]string, error) {
	activeSet := make(map[string]bool, len(activeIDs))
	for _, id := range activeIDs {
		activeSet[id] = true
	}

	// Build a mapping from base ID to all configured IDs that share it.
	// This lets us resolve dependencies like "nexus.agent.subagent" when only
	// instanced versions (e.g. "nexus.agent.subagent/foo") are active.
	baseToActive := make(map[string][]string, len(activeIDs))
	for _, id := range activeIDs {
		base := PluginBaseID(id)
		baseToActive[base] = append(baseToActive[base], id)
	}

	// resolveDep finds the configured ID that satisfies a dependency.
	// It checks exact match first, then falls back to base ID matching.
	// For base-ID dependencies satisfied by multiple instances, the first
	// instance (in active list order) is used as the dependency edge target.
	resolveDep := func(dep string) (string, bool) {
		if activeSet[dep] {
			return dep, true
		}
		if instances := baseToActive[dep]; len(instances) > 0 {
			return instances[0], true
		}
		return "", false
	}

	// Validate all dependencies can be resolved.
	for _, id := range activeIDs {
		for _, dep := range pluginMap[id].Dependencies() {
			if _, ok := resolveDep(dep); !ok {
				return nil, fmt.Errorf("plugin %q depends on %q which is not active", id, dep)
			}
		}
	}

	// Kahn's algorithm.
	inDegree := make(map[string]int, len(activeIDs))
	dependents := make(map[string][]string, len(activeIDs))
	for _, id := range activeIDs {
		if _, ok := inDegree[id]; !ok {
			inDegree[id] = 0
		}
		for _, dep := range pluginMap[id].Dependencies() {
			resolved, _ := resolveDep(dep)
			inDegree[id]++
			dependents[resolved] = append(dependents[resolved], id)
		}
	}

	var queue []string
	for _, id := range activeIDs {
		if inDegree[id] == 0 {
			queue = append(queue, id)
		}
	}

	var sorted []string
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		sorted = append(sorted, id)

		for _, dep := range dependents[id] {
			inDegree[dep]--
			if inDegree[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}

	if len(sorted) != len(activeIDs) {
		return nil, fmt.Errorf("dependency cycle detected among plugins")
	}

	return sorted, nil
}
