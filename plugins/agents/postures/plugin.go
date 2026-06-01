// Package postures wires the posture.Registry into the engine as a plugin
// (nexus.agent.postures). It loads postures from one or more on-disk
// directories at startup and watches those directories with fsnotify so
// operators can edit posture YAML in production without restarting.
//
// Other plugins consume the registry by requiring the "posture.registry"
// capability; the registry is exposed via a typed accessor on the plugin
// instance, looked up through the engine's plugin lookup helpers.
package postures

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/posture"
)

const (
	pluginID = "nexus.agent.postures"
	name     = "Posture Registry"
	version  = "0.1.0"

	capRegistry = "posture.registry"

	defaultDebounce = 250 * time.Millisecond
)

// Plugin loads AgentPosture YAML and exposes a posture.Registry to other
// plugins via the "posture.registry" capability. The registry itself is the
// goroutine-safe in-memory implementation from pkg/posture.
type Plugin struct {
	logger *slog.Logger
	bus    engine.EventBus

	registry posture.Registry

	scanDirs []string
	debounce time.Duration

	mu     sync.Mutex
	closeC chan struct{}
	wg     sync.WaitGroup
}

// New returns a default-configured Plugin.
func New() engine.Plugin {
	return &Plugin{
		registry: posture.NewRegistry(),
		debounce: defaultDebounce,
	}
}

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return name }
func (p *Plugin) Version() string                { return version }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }
func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{
		{Name: capRegistry, Description: "Resolves AgentPosture configurations by name."},
	}
}

// Registry exposes the loaded posture registry to other plugins that
// resolve this provider through the engine's plugin lookup. Returns the
// underlying registry — implementations are goroutine-safe.
func (p *Plugin) Registry() posture.Registry { return p.registry }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.logger = ctx.Logger
	p.bus = ctx.Bus
	p.closeC = make(chan struct{})

	if v, ok := ctx.Config["scan_dirs"].([]any); ok {
		for _, raw := range v {
			if s, ok := raw.(string); ok && s != "" {
				p.scanDirs = append(p.scanDirs, engine.ExpandPath(s))
			}
		}
	}
	if d, ok := ctx.Config["debounce_ms"].(int); ok && d > 0 {
		p.debounce = time.Duration(d) * time.Millisecond
	}
	if d, ok := ctx.Config["debounce_ms"].(float64); ok && d > 0 {
		p.debounce = time.Duration(d) * time.Millisecond
	}

	for _, dir := range p.scanDirs {
		p.loadDir(dir)
	}
	return nil
}

func (p *Plugin) Ready() error {
	for _, dir := range p.scanDirs {
		if err := p.watch(dir); err != nil {
			p.logger.Warn("posture watch failed; continuing without hot reload", "dir", dir, "error", err)
		}
	}
	return nil
}

func (p *Plugin) Shutdown(_ context.Context) error {
	p.mu.Lock()
	if p.closeC != nil {
		select {
		case <-p.closeC:
		default:
			close(p.closeC)
		}
	}
	p.mu.Unlock()
	p.wg.Wait()
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription { return nil }
func (p *Plugin) Emissions() []string                       { return []string{"posture.registered", "posture.removed"} }

// loadDir reads all *.yaml/*.yml in dir and registers each posture. Parse
// errors land as WARN; a single malformed file does not block others.
func (p *Plugin) loadDir(dir string) {
	postures, errs := posture.LoadDir(dir)
	for _, err := range errs {
		p.logger.Warn("posture load error", "dir", dir, "error", err)
	}
	for _, post := range postures {
		if err := p.registry.Register(post); err != nil {
			p.logger.Warn("posture register failed", "name", post.Name, "error", err)
			continue
		}
		_ = p.bus.Emit("posture.registered", map[string]any{
			"name":    post.Name,
			"version": post.Version,
			"source":  dir,
		})
	}
}

// watch starts an fsnotify watcher on dir that re-loads changed posture files
// after a small debounce. Editors that swap inodes on save (Vim's :w) still
// produce reliable notifications because we watch the directory.
func (p *Plugin) watch(dir string) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return err
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer w.Close()

		var (
			timer   *time.Timer
			pending = make(map[string]struct{})
			pendMu  sync.Mutex
		)
		fire := func() {
			pendMu.Lock()
			paths := make([]string, 0, len(pending))
			for k := range pending {
				paths = append(paths, k)
			}
			pending = make(map[string]struct{})
			pendMu.Unlock()
			for _, path := range paths {
				p.handleChange(path)
			}
		}

		for {
			select {
			case <-p.closeC:
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				lower := filepath.Ext(ev.Name)
				if lower != ".yaml" && lower != ".yml" {
					continue
				}
				pendMu.Lock()
				pending[ev.Name] = struct{}{}
				pendMu.Unlock()
				if timer != nil {
					timer.Stop()
				}
				timer = time.AfterFunc(p.debounce, fire)
			case werr, ok := <-w.Errors:
				if !ok {
					return
				}
				p.logger.Warn("posture watch error", "error", werr)
			}
		}
	}()
	return nil
}

// handleChange re-reads a single posture file. On read failure (file was
// removed), the posture is dropped from the registry.
func (p *Plugin) handleChange(path string) {
	post, err := posture.LoadFile(path)
	if err != nil {
		name := filepath.Base(path)
		// Strip extension to derive the name used at register time.
		ext := filepath.Ext(name)
		base := name[:len(name)-len(ext)]
		if rerr := p.registry.Remove(base); rerr == nil {
			_ = p.bus.Emit("posture.removed", map[string]any{"name": base})
		}
		p.logger.Debug("posture removed (load failed)", "path", path, "error", err)
		return
	}
	if err := p.registry.Register(post); err != nil {
		p.logger.Warn("posture re-register failed", "name", post.Name, "error", err)
		return
	}
	_ = p.bus.Emit("posture.registered", map[string]any{
		"name":    post.Name,
		"version": post.Version,
		"source":  path,
	})
}
