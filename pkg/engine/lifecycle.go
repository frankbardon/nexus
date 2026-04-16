package engine

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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
	session  *SessionWorkspace
	models   *ModelRegistry
	prompts  *PromptRegistry
	system   *SystemInfo
}

// NewLifecycleManager creates a new lifecycle manager.
func NewLifecycleManager(registry *PluginRegistry, bus EventBus, config *Config, logger *slog.Logger, models *ModelRegistry, prompts *PromptRegistry, system *SystemInfo) *LifecycleManager {
	return &LifecycleManager{
		registry: registry,
		bus:      bus,
		config:   config,
		logger:   logger,
		models:   models,
		prompts:  prompts,
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

		pctx := PluginContext{
			Config:     pluginCfg,
			Bus:        lm.bus,
			Logger:     lm.logger.With("plugin", configuredID),
			DataDir:    "",
			Session:    lm.session,
			Models:     lm.models,
			Prompts:    lm.prompts,
			System:     lm.system,
			InstanceID: instanceIDs[configuredID],
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

// Shutdown tears down plugins in reverse order, drains the bus, and signals completion.
func (lm *LifecycleManager) Shutdown(ctx context.Context) error {
	lm.logger.Info("shutting down engine")

	_ = lm.bus.Emit("core.shutdown", nil)

	if err := lm.bus.Drain(ctx); err != nil {
		lm.logger.Warn("drain timed out", "error", err)
	}

	// Shutdown in reverse dependency order.
	var firstErr error
	for i := len(lm.plugins) - 1; i >= 0; i-- {
		p := lm.plugins[i]
		lm.logger.Info("shutting down plugin", "plugin", p.ID())
		if err := p.Shutdown(ctx); err != nil {
			lm.logger.Error("plugin shutdown error", "plugin", p.ID(), "error", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("plugin %q shutdown failed: %w", p.ID(), err)
			}
		}
	}

	lm.logger.Info("engine shutdown complete")
	return firstErr
}

// Plugins returns the ordered list of initialized plugins.
func (lm *LifecycleManager) Plugins() []Plugin {
	return lm.plugins
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
