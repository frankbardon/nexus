// Package agui implements the nexus.io.agui serve plugin: an HTTP listener that
// exposes the AG-UI ("Agent-User Interaction") endpoint. Clients POST a
// RunAgentInput and receive a text/event-stream SSE response, one stream per
// run. The wire format is defined by pkg/agui (not the pkg/ui Envelope used by
// the browser/wails transports).
//
// This file is the transport shell and config surface. The actual bus <-> SSE
// event mapping (io.input/io.output/llm.stream.chunk/... <-> AG-UI events) is
// wired in a later story; the POST handler currently parses and validates the
// RunAgentInput and returns a not-yet-wired RunError stream, leaving a clear
// seam for that work.
package agui

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
)

const pluginID = "nexus.io.agui"

// defaultBindAddr binds loopback by default so the endpoint is not exposed on
// the network without explicit operator opt-in.
const defaultBindAddr = "127.0.0.1:8090"

// Plugin is the AG-UI serve plugin. It stands up an embedded *http.Server that
// exposes the AG-UI POST endpoint with safe-by-default exposure: loopback bind,
// optional bearer-token auth, and configurable CORS.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	server *Server

	bindAddr    string
	bearerToken string
	corsOrigins []string
}

// New creates a new AG-UI serve plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "AG-UI IO" }
func (p *Plugin) Version() string                   { return "0.1.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

// Subscriptions is empty in the skeleton: the bus -> SSE mapping lands in a
// later story. Declaring it accurately keeps the contract harness honest.
func (p *Plugin) Subscriptions() []engine.EventSubscription { return nil }

// Emissions is empty in the skeleton: the SSE -> bus mapping (io.input etc.)
// lands in a later story.
func (p *Plugin) Emissions() []string { return nil }

// Init reads config and constructs the server. Nothing binds a socket here; the
// listener starts in Ready so all plugins have finished Init first.
func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	p.bindAddr = defaultBindAddr
	if v, ok := ctx.Config["bind"].(string); ok && strings.TrimSpace(v) != "" {
		p.bindAddr = strings.TrimSpace(v)
	}

	// Bearer token: an inline `bearer_token` takes precedence; otherwise
	// `bearer_token_env` names an environment variable to read it from. Auth
	// is enforced only when a non-empty token is resolved.
	if v, ok := ctx.Config["bearer_token"].(string); ok {
		p.bearerToken = strings.TrimSpace(v)
	}
	if p.bearerToken == "" {
		if envVar, ok := ctx.Config["bearer_token_env"].(string); ok && strings.TrimSpace(envVar) != "" {
			p.bearerToken = strings.TrimSpace(os.Getenv(strings.TrimSpace(envVar)))
		}
	}

	p.corsOrigins = parseCORSOrigins(ctx.Config["cors_origins"])

	p.server = NewServer(serverConfig{
		addr:        p.bindAddr,
		bearerToken: p.bearerToken,
		corsOrigins: p.corsOrigins,
		logger:      p.logger,
	})

	p.logger.Info("agui serve plugin initialized",
		"bind", p.bindAddr,
		"auth", p.bearerToken != "",
		"cors_origins", len(p.corsOrigins),
	)
	return nil
}

// Ready starts the HTTP listener.
func (p *Plugin) Ready() error {
	if err := p.server.Start(); err != nil {
		return fmt.Errorf("starting agui server: %w", err)
	}
	return nil
}

// Shutdown gracefully stops the HTTP server.
func (p *Plugin) Shutdown(_ context.Context) error {
	if p.server == nil {
		return nil
	}
	// Use a fresh context with a deadline: the incoming context may already be
	// cancelled during engine teardown.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down agui server: %w", err)
	}
	return nil
}

// parseCORSOrigins normalizes the cors_origins config value into a slice of
// trimmed, non-empty origins. It accepts a YAML list ([]any of strings) or a
// single comma-separated string for convenience.
func parseCORSOrigins(raw any) []string {
	var out []string
	switch v := raw.(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
	case []string:
		for _, s := range v {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	case string:
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}
