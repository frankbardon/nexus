package engine

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
)

// reloadMu serializes ReloadConfig calls across the engine. Two operators
// hitting SIGHUP simultaneously, or fsnotify firing during an admin POST,
// must not race the diff/apply phase. The mutex is strict: the second caller
// blocks until the first returns rather than failing fast, so a debounced
// watcher and an interactive operator stay deterministic.
var reloadMu sync.Mutex

// ReloadConfig applies a config change to a running engine in two phases:
//
//  1. Validate phase (atomic): the new config is run through the same schema
//     validation pass that Boot performs. Capability-provider identity is
//     pinned — a Requirement.Capability that resolved to plugin X at boot
//     cannot resolve to plugin Y on reload because the session would lose
//     its bound state (memory.history, vector.store handles, etc.). Any
//     failure here returns the error and leaves the engine untouched.
//
//  2. Apply phase (best-effort): the diff between current and new active
//     sets is walked. For each delta:
//
//     - Plugin added: Init -> Ready (subscriptions registered by Init).
//     - Plugin removed: Shutdown (subscriptions unregistered by Shutdown).
//     - Config-only change, plugin implements ConfigReloader: ReloadConfig
//     is called with old/new maps; subscriptions preserved in place.
//     - Config-only change, no ConfigReloader: Shutdown -> Init -> Ready.
//
// Atomicity caveat: the apply phase is best-effort. If a per-plugin
// ReloadConfig or Init/Ready fails partway through, prior changes have
// already taken effect (a restarted plugin has already side-effected onto
// the bus, journals, storage). The engine logs the failure and surfaces it
// to the caller; rollback to the pre-reload state is not attempted because
// "undoing" a Shutdown is not generally possible. Operators can recover by
// re-issuing ReloadConfig with the previous config.
//
// Engine-level fields (Engine.Shutdown.DrainTimeout, Engine.ConfigWatch)
// are swapped in atomically as part of the apply phase. They take effect on
// the next operation that reads them — DrainTimeout on the next Stop,
// ConfigWatch when the watcher restarts.
func (e *Engine) ReloadConfig(newConfig *Config) error {
	reloadMu.Lock()
	defer reloadMu.Unlock()

	if newConfig == nil {
		return fmt.Errorf("reload: new config is nil")
	}
	if e.Lifecycle == nil || e.Lifecycle.plugins == nil {
		return fmt.Errorf("reload: engine not booted")
	}

	old := e.Config

	// ---- Phase 1: validate ----
	plan, err := e.planReload(old, newConfig)
	if err != nil {
		return fmt.Errorf("reload validate: %w", err)
	}

	// ---- Phase 2: apply ----
	if err := e.applyReload(context.Background(), newConfig, plan); err != nil {
		return fmt.Errorf("reload apply: %w", err)
	}

	e.Config = newConfig
	e.Lifecycle.config = newConfig
	e.Logger.Info("config reloaded",
		"added", plan.added,
		"removed", plan.removed,
		"reloaded", plan.reloaded,
		"restarted", plan.restarted)
	return nil
}

// reloadPlan captures the per-plugin work decided during the validate phase.
// Field names mirror the operator-facing log line emitted on success.
type reloadPlan struct {
	added     []string // plugin IDs newly active
	removed   []string // plugin IDs going away
	reloaded  []string // plugin IDs whose config changed AND implement ConfigReloader
	restarted []string // plugin IDs whose config changed but lack ConfigReloader
}

func (p reloadPlan) empty() bool {
	return len(p.added) == 0 && len(p.removed) == 0 && len(p.reloaded) == 0 && len(p.restarted) == 0
}

// planReload validates the new config and produces a deterministic delta
// against the running engine. Capability provider swaps are rejected here
// so an operator gets a single clear error before any plugin is touched.
func (e *Engine) planReload(old, new *Config) (reloadPlan, error) {
	plan := reloadPlan{}

	// Schema validation against the new active set. Mirrors validateConfigSchemas
	// usage in lifecycle.Boot: instantiate factories so each plugin's
	// ConfigSchemaProvider can be inspected without committing to lifecycle.
	plugins := make(map[string]Plugin, len(new.Plugins.Active))
	for _, id := range new.Plugins.Active {
		factory, ok := e.Registry.Get(PluginBaseID(id))
		if !ok {
			return plan, fmt.Errorf("plugin %q not registered", id)
		}
		plugins[id] = factory()
	}
	if err := validateConfigSchemas(new, plugins, new.Plugins.Active, e.Logger); err != nil {
		return plan, err
	}

	// Capability provider identity pinning. memory.history, vector.store, and
	// other session-coupled providers have in-flight state — swapping the
	// concrete plugin behind the capability would orphan that state. Reject
	// any reload that changes the resolved provider for an active capability.
	if err := e.checkCapabilityPinning(old, new, plugins); err != nil {
		return plan, err
	}

	// Build delta sets.
	oldSet := make(map[string]bool, len(old.Plugins.Active))
	for _, id := range old.Plugins.Active {
		oldSet[id] = true
	}
	newSet := make(map[string]bool, len(new.Plugins.Active))
	for _, id := range new.Plugins.Active {
		newSet[id] = true
	}
	for _, id := range new.Plugins.Active {
		if !oldSet[id] {
			plan.added = append(plan.added, id)
		}
	}
	for _, id := range old.Plugins.Active {
		if !newSet[id] {
			plan.removed = append(plan.removed, id)
		}
	}

	// Config-change detection. Plugins present in both old and new active
	// sets get a full deepEqual against their config maps; equal maps are
	// no-ops, unequal maps go to ConfigReloader if available else restart.
	for _, id := range new.Plugins.Active {
		if !oldSet[id] {
			continue
		}
		oldCfg := old.Plugins.Configs[id]
		newCfg := new.Plugins.Configs[id]
		if reflect.DeepEqual(oldCfg, newCfg) {
			continue
		}
		var live Plugin
		for _, p := range e.Lifecycle.plugins {
			if pluginConfiguredID(p, e.Lifecycle, id) == id {
				live = p
				break
			}
		}
		if live == nil {
			// Active list said it was running but lifecycle disagrees; treat as a restart so Init runs fresh.
			plan.restarted = append(plan.restarted, id)
			continue
		}
		if _, ok := live.(ConfigReloader); ok {
			plan.reloaded = append(plan.reloaded, id)
		} else {
			plan.restarted = append(plan.restarted, id)
		}
	}

	sort.Strings(plan.added)
	sort.Strings(plan.removed)
	sort.Strings(plan.reloaded)
	sort.Strings(plan.restarted)
	return plan, nil
}

// pluginConfiguredID returns the configured ID under which a live plugin was
// initialized. lm.plugins stores pointers in init order matching activeIDs;
// we look up the same index in lm.config.Plugins.Active to recover the
// configured ID (necessary because instanced plugins share a base ID but
// run as distinct configured entries).
func pluginConfiguredID(p Plugin, lm *LifecycleManager, want string) string {
	for i, configured := range lm.config.Plugins.Active {
		if i < len(lm.plugins) && lm.plugins[i] == p {
			return configured
		}
	}
	if p.ID() == want {
		return want
	}
	return ""
}

// checkCapabilityPinning rejects a reload that would change the resolved
// provider for any capability that is currently bound. We compare the
// running engine's capability map (built at boot from advertised
// capabilities) against the capabilities advertised by the new active set
// and refuse a reload where a capability the engine resolved to plugin X
// would now resolve to plugin Y.
//
// Rationale: a capability like memory.history binds to in-flight session
// state (token-counted history buffer, persistence path under
// session_dir/plugins/<id>/). Hot-swapping the concrete provider would
// silently drop that state — the new provider would start from empty and
// the operator would notice mid-session. Surface as a hard reject so
// operators know to restart the session for provider changes.
func (e *Engine) checkCapabilityPinning(old, new *Config, newPlugins map[string]Plugin) error {
	current := e.Lifecycle.Capabilities()
	if len(current) == 0 {
		return nil
	}

	// Build the new active set's capability map (provider IDs in active-list
	// order). Mirrors the construction in lifecycle.Boot but operates on the
	// freshly-instantiated newPlugins probe set.
	newCaps := make(map[string][]string)
	for _, id := range new.Plugins.Active {
		p, ok := newPlugins[id]
		if !ok {
			continue
		}
		for _, c := range p.Capabilities() {
			newCaps[c.Name] = append(newCaps[c.Name], id)
		}
	}

	// resolvedProvider mirrors the explicit-pin → active-list precedence
	// resolveCapability uses at boot. Registry-fallback (auto-activate)
	// would mean expanding the active set, which would diverge from the
	// user's declared YAML — out of scope for hot-reload.
	resolvedProvider := func(cfg *Config, capName string, providers []string) string {
		if pinned, ok := cfg.Capabilities[capName]; ok && pinned != "" {
			return pinned
		}
		if len(providers) > 0 {
			return providers[0]
		}
		return ""
	}

	for capName, currentProviders := range current {
		if len(currentProviders) == 0 {
			continue
		}
		// resolved-at-boot is the first provider in the boot-time map (capability resolution order).
		oldProvider := resolvedProvider(old, capName, currentProviders)
		newProvider := resolvedProvider(new, capName, newCaps[capName])
		if newProvider == "" {
			return fmt.Errorf("capability provider %q cannot disappear at runtime; the new active set has no provider for it — restart the engine to retire this capability", capName)
		}
		if oldProvider != "" && newProvider != oldProvider {
			return fmt.Errorf("capability provider %q cannot change at runtime (%s -> %s); restart required to rebind session state", capName, oldProvider, newProvider)
		}
	}
	return nil
}

// applyReload walks the reloadPlan and mutates the engine in place. Order is
// fixed: removes first (so a removed-then-readded ID still reaches Init
// fresh), then config reloads (cheap, in-place), then restarts (Shutdown +
// Init + Ready), then adds. Engine-level fields are swapped before the
// per-plugin work so a restart that takes the new drain budget already sees
// the new value.
func (e *Engine) applyReload(ctx context.Context, new *Config, plan reloadPlan) error {
	if plan.empty() {
		// Engine-level fields can still differ even when the plugin set is identical.
		e.applyEngineFields(new)
		return nil
	}
	e.applyEngineFields(new)

	// Removed plugins: Shutdown and detach from lifecycle.plugins.
	for _, id := range plan.removed {
		if err := e.shutdownAndDetachPlugin(ctx, id); err != nil {
			return fmt.Errorf("removing %q: %w", id, err)
		}
	}

	// Config-only reloads via ConfigReloader. Errors here are transactional:
	// the plugin is expected to leave its prior state intact on error so the
	// reload as a whole can surface but not corrupt the running engine.
	for _, id := range plan.reloaded {
		if err := e.reloadPluginConfig(id, new); err != nil {
			return fmt.Errorf("reloading %q: %w", id, err)
		}
	}

	// Restart path: Shutdown + Init + Ready under the new config map.
	for _, id := range plan.restarted {
		if err := e.restartPlugin(ctx, id, new); err != nil {
			return fmt.Errorf("restarting %q: %w", id, err)
		}
	}

	// Add new plugins: Init + Ready, append to lifecycle.plugins.
	for _, id := range plan.added {
		if err := e.activatePlugin(id, new); err != nil {
			return fmt.Errorf("activating %q: %w", id, err)
		}
	}
	return nil
}

// applyEngineFields swaps engine-level config knobs that can take effect
// without per-plugin coordination. DrainTimeout is read on the next Stop,
// ConfigWatch is read by the CLI's watcher when it restarts.
func (e *Engine) applyEngineFields(new *Config) {
	e.Config.Engine = new.Engine
	if e.Lifecycle != nil && e.Lifecycle.config != nil {
		e.Lifecycle.config.Engine = new.Engine
	}
}

// shutdownAndDetachPlugin runs Shutdown and removes the plugin from the
// lifecycle plugins slice. Subscriptions are the plugin's responsibility to
// release in Shutdown — they always have been; the lifecycle does not track
// per-plugin subscription handles.
func (e *Engine) shutdownAndDetachPlugin(ctx context.Context, id string) error {
	lm := e.Lifecycle
	idx := lm.indexOf(id)
	if idx < 0 {
		return fmt.Errorf("plugin not running")
	}
	p := lm.plugins[idx]
	e.Logger.Info("reload: shutting down plugin", "plugin", id)
	if err := p.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	lm.plugins = append(lm.plugins[:idx], lm.plugins[idx+1:]...)
	delete(lm.config.Plugins.Configs, id)
	for i, active := range lm.config.Plugins.Active {
		if active == id {
			lm.config.Plugins.Active = append(lm.config.Plugins.Active[:i], lm.config.Plugins.Active[i+1:]...)
			break
		}
	}
	return nil
}

// reloadPluginConfig invokes the plugin's ConfigReloader hook with the old
// and new maps. The plugin owns the transactional guarantee — on error,
// the engine just logs and bails; we make no attempt to restart on top of
// a failed in-place reload.
func (e *Engine) reloadPluginConfig(id string, new *Config) error {
	lm := e.Lifecycle
	idx := lm.indexOf(id)
	if idx < 0 {
		return fmt.Errorf("plugin not running")
	}
	reloader, ok := lm.plugins[idx].(ConfigReloader)
	if !ok {
		return fmt.Errorf("plugin lacks ConfigReloader")
	}
	oldCfg := lm.config.Plugins.Configs[id]
	newCfg := new.Plugins.Configs[id]
	e.Logger.Info("reload: in-place config update", "plugin", id)
	if err := reloader.ReloadConfig(oldCfg, newCfg); err != nil {
		return err
	}
	if lm.config.Plugins.Configs == nil {
		lm.config.Plugins.Configs = make(map[string]map[string]any)
	}
	lm.config.Plugins.Configs[id] = newCfg
	return nil
}

// restartPlugin tears the plugin down and brings up a fresh instance under
// the new config. We use a fresh factory result so any in-memory state in
// the prior instance is discarded — the prior plugin's Shutdown is the
// last call it ever sees.
func (e *Engine) restartPlugin(ctx context.Context, id string, new *Config) error {
	lm := e.Lifecycle
	idx := lm.indexOf(id)
	if idx < 0 {
		return fmt.Errorf("plugin not running")
	}
	old := lm.plugins[idx]
	e.Logger.Info("reload: restarting plugin", "plugin", id)
	if err := old.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	factory, ok := e.Registry.Get(PluginBaseID(id))
	if !ok {
		return fmt.Errorf("factory missing for %q", id)
	}
	fresh := factory()
	lm.plugins[idx] = fresh

	if lm.config.Plugins.Configs == nil {
		lm.config.Plugins.Configs = make(map[string]map[string]any)
	}
	lm.config.Plugins.Configs[id] = new.Plugins.Configs[id]

	pctx, err := lm.buildPluginContext(id)
	if err != nil {
		return fmt.Errorf("context: %w", err)
	}
	if err := fresh.Init(pctx); err != nil {
		return fmt.Errorf("init: %w", err)
	}
	if err := fresh.Ready(); err != nil {
		return fmt.Errorf("ready: %w", err)
	}
	return nil
}

// activatePlugin runs the full lifecycle (Init + Ready) for a plugin newly
// added to the active set. Append to lifecycle.plugins so subsequent reloads
// recognize it.
func (e *Engine) activatePlugin(id string, new *Config) error {
	lm := e.Lifecycle
	factory, ok := e.Registry.Get(PluginBaseID(id))
	if !ok {
		return fmt.Errorf("factory missing for %q", id)
	}
	fresh := factory()
	lm.plugins = append(lm.plugins, fresh)
	lm.config.Plugins.Active = append(lm.config.Plugins.Active, id)
	if lm.config.Plugins.Configs == nil {
		lm.config.Plugins.Configs = make(map[string]map[string]any)
	}
	lm.config.Plugins.Configs[id] = new.Plugins.Configs[id]

	pctx, err := lm.buildPluginContext(id)
	if err != nil {
		return fmt.Errorf("context: %w", err)
	}
	e.Logger.Info("reload: activating plugin", "plugin", id)
	if err := fresh.Init(pctx); err != nil {
		return fmt.Errorf("init: %w", err)
	}
	if err := fresh.Ready(); err != nil {
		return fmt.Errorf("ready: %w", err)
	}
	return nil
}

// indexOf returns the slice index of the plugin with the given configured ID,
// or -1 if not found. The configured-ID mapping uses lm.config.Plugins.Active
// position to recover the right entry for instanced IDs.
func (lm *LifecycleManager) indexOf(configuredID string) int {
	for i, active := range lm.config.Plugins.Active {
		if active != configuredID {
			continue
		}
		if i < len(lm.plugins) {
			return i
		}
	}
	for i, p := range lm.plugins {
		if p.ID() == configuredID {
			return i
		}
	}
	return -1
}

// buildPluginContext reproduces the PluginContext lifecycle.Boot constructs
// for a single plugin. Used by reload paths so a freshly-Init'd plugin sees
// the same engine services its peers got at boot. The capability map is
// preserved as the boot-time snapshot — capability provider identity is
// pinned in checkCapabilityPinning so the snapshot remains accurate after
// reload.
func (lm *LifecycleManager) buildPluginContext(configuredID string) (PluginContext, error) {
	pluginCfg := lm.config.Plugins.Configs[configuredID]
	if pluginCfg == nil {
		pluginCfg = make(map[string]any)
	}
	sb, err := lm.resolveSandbox(configuredID, pluginCfg)
	if err != nil {
		return PluginContext{}, fmt.Errorf("sandbox: %w", err)
	}
	instanceID := ""
	if base := PluginBaseID(configuredID); base != configuredID {
		instanceID = configuredID
	}
	return PluginContext{
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
		InstanceID:   instanceID,
		Replay:       lm.replay,
		Journal:      lm.journal,
		Sandbox:      sb,
	}, nil
}
