package protocol

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
)

// fakeEmbedderPlugin stands in for an embedder-authored plugin handed to
// protocol.Run via Request.ExtraPlugins.
type fakeEmbedderPlugin struct {
	id         string
	initCalled bool
}

func (p *fakeEmbedderPlugin) ID() string                                { return p.id }
func (p *fakeEmbedderPlugin) Name() string                              { return p.id }
func (p *fakeEmbedderPlugin) Version() string                           { return "test" }
func (p *fakeEmbedderPlugin) Dependencies() []string                    { return nil }
func (p *fakeEmbedderPlugin) Requires() []engine.Requirement            { return nil }
func (p *fakeEmbedderPlugin) Capabilities() []engine.Capability         { return nil }
func (p *fakeEmbedderPlugin) Init(engine.PluginContext) error           { p.initCalled = true; return nil }
func (p *fakeEmbedderPlugin) Ready() error                              { return nil }
func (p *fakeEmbedderPlugin) Shutdown(context.Context) error            { return nil }
func (p *fakeEmbedderPlugin) Subscriptions() []engine.EventSubscription { return nil }
func (p *fakeEmbedderPlugin) Emissions() []string                       { return nil }

const embedderPluginID = "acme.tool.echo"

func configWithEmbedderPlugin() string {
	return strings.Replace(minimalMockConfig(),
		"    - nexus.memory.capped\n",
		"    - nexus.memory.capped\n    - "+embedderPluginID+"\n", 1)
}

func TestRun_ExtraPluginsBootsEmbedderPlugin(t *testing.T) {
	fake := &fakeEmbedderPlugin{id: embedderPluginID}
	req := &Request{
		Schema:       SchemaVersion,
		ConfigInline: configWithEmbedderPlugin(),
		UserInput:    "hi there",
		ExtraPlugins: map[string]engine.PluginFactory{
			embedderPluginID: func() engine.Plugin { return fake },
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := Run(ctx, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.FinalAssistantMessage != "Hello from mock" {
		t.Errorf("final=%q", resp.FinalAssistantMessage)
	}
	if !fake.initCalled {
		t.Error("expected ExtraPlugins-registered plugin to be Init'd")
	}
}

func TestRun_UnregisteredEmbedderPluginFailsBoot(t *testing.T) {
	req := &Request{
		Schema:       SchemaVersion,
		ConfigInline: configWithEmbedderPlugin(),
		UserInput:    "hi there",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := Run(ctx, req)
	if err == nil {
		t.Fatal("expected boot failure for unregistered plugin id")
	}
	if !strings.Contains(err.Error(), `plugin "`+embedderPluginID+`" not found in registry`) {
		t.Errorf("unexpected error: %v", err)
	}
}
