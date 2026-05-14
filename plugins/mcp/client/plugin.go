// Package client implements the nexus.mcp.client plugin: a developer-
// configured bridge between Nexus and one or more MCP (Model Context
// Protocol) servers. Each configured server contributes its tools to the
// Nexus tool catalog under the namespace mcp__<server>__<tool>, surfaces
// its resources as catalog tools (generic browse/read plus auto-registered
// statics and templates), and exposes its prompts as slash commands of
// the form /<command_prefix>.<server>.<prompt>.
//
// MCP capabilities deferred for Phase 1:
//   - sampling (server-initiated LLM calls) — tracked in issue #98
//   - roots beyond the session files dir
//   - server-initiated OAuth / dynamic client registration
//
// See docs/src/plugins/mcp-client.md for the user-facing reference.
package client

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/blobs"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.mcp.client"
	pluginName = "MCP Client"
	version    = "0.1.0"

	// Default cap on inline binary parts. Above this we route through the
	// session blob store and reference the payload by URI to keep the
	// journal compact. Mirrors plugins/tools/screenshot.
	defaultBlobInlineCutoff int64 = 64 * 1024
	defaultBlobByteBudget   int64 = 64 * 1024 * 1024
)

// Plugin is the MCP client plugin.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	session *engine.SessionWorkspace
	blobs   *blobs.Store

	cfg     Config
	servers map[string]*server

	mu             sync.RWMutex
	toolToServer   map[string]string // catalog name -> server name
	promptCommands map[string]promptRoute
	aliasReverse   map[string]string // alias slash command -> canonical
	unsubs         []func()
}

// promptRoute carries the resolution metadata for one slash command.
type promptRoute struct {
	server string
	prompt string
}

// New constructs the plugin.
func New() engine.Plugin {
	return &Plugin{
		servers:        map[string]*server{},
		toolToServer:   map[string]string{},
		promptCommands: map[string]promptRoute{},
		aliasReverse:   map[string]string{},
	}
}

func (p *Plugin) ID() string                     { return pluginID }
func (p *Plugin) Name() string                   { return pluginName }
func (p *Plugin) Version() string                { return version }
func (p *Plugin) Dependencies() []string         { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

// Capabilities advertises this plugin as the "mcp.client" provider so other
// plugins (e.g. a future UI or eval harness) can detect that an MCP bridge
// is active without string-matching the concrete plugin ID.
func (p *Plugin) Capabilities() []engine.Capability {
	return []engine.Capability{
		{
			Name:        "mcp.client",
			Description: "Bridges configured MCP servers into the Nexus tool catalog, resource surface, and slash-command set.",
		},
	}
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "tool.invoke", Priority: 50},
		{EventType: "before:io.input", Priority: 5},
		{EventType: "io.session.start", Priority: 10},
		{EventType: "io.session.end", Priority: 10},
		{EventType: "mcp.prompts.list", Priority: 10},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"tool.register",
		"tool.result",
		"before:tool.result",
		"io.input",
		"io.output",
		"mcp.resource.updated",
		"mcp.tools.refreshed",
		"mcp.prompts.refreshed",
	}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session

	cfg, err := parseConfig(ctx.Config)
	if err != nil {
		return fmt.Errorf("mcp.client: %w", err)
	}
	p.cfg = cfg

	for alias, target := range cfg.Aliases {
		canonical := strings.ToLower(target)
		p.aliasReverse["/"+strings.ToLower(alias)] = canonical
	}

	if p.session != nil {
		store, err := blobs.New(p.session.BlobsDir(), defaultBlobByteBudget)
		if err != nil {
			p.logger.Warn("mcp.client: blob store init failed, large binary payloads will be inlined", "error", err)
		} else {
			p.blobs = store
		}
	}

	for i := range cfg.Servers {
		sc := cfg.Servers[i]
		s := newServer(sc, p)
		p.servers[sc.Name] = s
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.handleToolInvoke, engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("before:io.input", p.handleInputIntercept, engine.WithPriority(5), engine.WithSource(pluginID)),
		p.bus.Subscribe("io.session.start", p.handleSessionStart, engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("io.session.end", p.handleSessionEnd, engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("mcp.prompts.list", p.handlePromptsListQuery, engine.WithPriority(10), engine.WithSource(pluginID)),
	)

	p.logger.Info("mcp.client initialized", "servers", len(p.servers))
	return nil
}

// Ready connects every server whose lifecycle is "engine". Servers with
// lifecycle "session" wait for io.session.start. Engine-lifecycle servers
// are required to come up at boot — a missing one is logged but not fatal
// so a single broken server doesn't prevent the rest of the engine from
// running.
func (p *Plugin) Ready() error {
	for _, s := range p.servers {
		if s.cfg.Lifecycle != "engine" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
		err := s.connect(ctx)
		cancel()
		if err != nil {
			p.logger.Error("mcp server failed to start", "server", s.cfg.Name, "error", err)
			continue
		}
	}
	return nil
}

// Shutdown closes every active MCP connection. A grace window per server
// keeps stdio subprocesses from leaking even when the server is unresponsive.
func (p *Plugin) Shutdown(ctx context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	var wg sync.WaitGroup
	for _, s := range p.servers {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.disconnect()
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		p.logger.Warn("mcp.client: shutdown grace exceeded; abandoning remaining servers")
	}
	return nil
}

// DrainTimeout asks the engine for a longer drain window so any in-flight
// MCP tool calls can return before the bus shuts down.
func (p *Plugin) DrainTimeout() time.Duration { return 5 * time.Second }

// handleSessionStart connects every session-lifecycle server. Each server's
// tool/resource/prompt set is namespaced by server name so multiple session
// starts during the engine's lifetime are safe.
func (p *Plugin) handleSessionStart(_ engine.Event[any]) {
	for _, s := range p.servers {
		if s.cfg.Lifecycle != "session" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
		err := s.connect(ctx)
		cancel()
		if err != nil {
			p.logger.Error("mcp server failed to start (session lifecycle)", "server", s.cfg.Name, "error", err)
		}
	}
}

func (p *Plugin) handleSessionEnd(_ engine.Event[any]) {
	for _, s := range p.servers {
		if s.cfg.Lifecycle != "session" {
			continue
		}
		s.disconnect()
	}
}

// registerToolRoute records the server that owns a freshly-registered
// catalog tool so dispatch knows where to route invocations.
func (p *Plugin) registerToolRoute(toolCatalogName, serverName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.toolToServer[toolCatalogName] = serverName
}

// unregisterToolRoute removes a tool from the dispatch table. Called when a
// list_changed notification removes a tool.
func (p *Plugin) unregisterToolRoute(toolCatalogName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.toolToServer, toolCatalogName)
}

func (p *Plugin) toolRoute(name string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	srv, ok := p.toolToServer[name]
	return srv, ok
}

// registerPrompt records a slash command and its (server, prompt) target.
func (p *Plugin) registerPrompt(command, serverName, promptName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.promptCommands[command] = promptRoute{server: serverName, prompt: promptName}
}

func (p *Plugin) unregisterPrompt(command string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.promptCommands, command)
}

func (p *Plugin) listPrompts() []events.MCPPromptDescriptor {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]events.MCPPromptDescriptor, 0, len(p.promptCommands))
	for cmd, route := range p.promptCommands {
		s, ok := p.servers[route.server]
		if !ok {
			continue
		}
		raw, ok := s.getPrompt(route.prompt)
		if !ok {
			continue
		}
		desc := events.MCPPromptDescriptor{
			Command:     cmd,
			Server:      route.server,
			Prompt:      route.prompt,
			Title:       raw.Title,
			Description: raw.Description,
		}
		for _, a := range raw.Arguments {
			desc.Arguments = append(desc.Arguments, events.MCPPromptArgument{
				Name:        a.Name,
				Description: a.Description,
				Required:    a.Required,
			})
		}
		out = append(out, desc)
	}
	return out
}
