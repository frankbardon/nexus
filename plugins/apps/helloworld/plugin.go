// Package helloworld is a minimal Nexus plugin that responds to
// hello.request events with a configurable greeting. It serves as a
// proof-of-concept for the bus bridge pattern and as a built-in
// placeholder agent for desktop shell installations.
package helloworld

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/frankbardon/nexus/pkg/engine"
)

const PluginID = "nexus.app.helloworld"

var _ engine.Plugin = (*Plugin)(nil)

type Plugin struct {
	bus      engine.EventBus
	logger   *slog.Logger
	greeting string
	unsubs   []func()
}

func New() engine.Plugin { return &Plugin{} }

func (p *Plugin) ID() string             { return PluginID }
func (p *Plugin) Name() string           { return "Hello World" }
func (p *Plugin) Version() string        { return "0.1.0" }
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "hello.request", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{"hello.response", "session.meta.title"}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger

	p.greeting = "Hello"
	if g, ok := ctx.Config["greeting"].(string); ok && g != "" {
		p.greeting = g
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("hello.request", p.handleRequest, engine.WithSource(PluginID)),
	)

	p.logger.Info("hello-world plugin initialized", "greeting", p.greeting)
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	p.unsubs = nil
	return nil
}

func (p *Plugin) handleRequest(e engine.Event[any]) {
	var name string
	switch payload := e.Payload.(type) {
	case map[string]any:
		name, _ = payload["name"].(string)
	}
	if name == "" {
		name = "World"
	}

	message := fmt.Sprintf("%s, %s!", p.greeting, name)
	_ = p.bus.Emit("hello.response", map[string]any{
		"message": message,
	})

	// Emit session title so the desktop shell can label this run.
	_ = p.bus.Emit("session.meta.title", map[string]any{
		"title": message,
	})
}
