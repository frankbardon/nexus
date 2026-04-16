package dynvars

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
)

const (
	pluginID   = "nexus.system.dynvars"
	pluginName = "Dynamic Variables"
	version    = "0.1.0"
)

// Plugin injects dynamic runtime variables into system prompts
// via the engine's PromptRegistry.
type Plugin struct {
	logger  *slog.Logger
	prompts *engine.PromptRegistry
	session *engine.SessionWorkspace
	system  *engine.SystemInfo
	config  varConfig
}

type varConfig struct {
	Date       bool
	Time       bool
	Timezone   bool
	CWD        bool
	SessionDir bool
	OS         bool
}

// New creates a new dynamic variables plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return pluginName }
func (p *Plugin) Version() string        { return version }
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription { return nil }
func (p *Plugin) Emissions() []string                       { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.logger = ctx.Logger
	p.prompts = ctx.Prompts
	p.session = ctx.Session
	p.system = ctx.System

	if v, ok := ctx.Config["date"].(bool); ok {
		p.config.Date = v
	}
	if v, ok := ctx.Config["time"].(bool); ok {
		p.config.Time = v
	}
	if v, ok := ctx.Config["timezone"].(bool); ok {
		p.config.Timezone = v
	}
	if v, ok := ctx.Config["cwd"].(bool); ok {
		p.config.CWD = v
	}
	if v, ok := ctx.Config["session_dir"].(bool); ok {
		p.config.SessionDir = v
	}
	if v, ok := ctx.Config["os"].(bool); ok {
		p.config.OS = v
	}

	if p.prompts != nil {
		p.prompts.Register(pluginID, 100, p.buildSection)
	}

	p.logger.Info("dynamic variables plugin initialized")
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	if p.prompts != nil {
		p.prompts.Unregister(pluginID)
	}
	return nil
}

func (p *Plugin) buildSection() string {
	var lines []string

	now := time.Now()

	if p.config.Date {
		lines = append(lines, fmt.Sprintf("Current date: %s", now.Format("2006-01-02")))
	}
	if p.config.Time {
		lines = append(lines, fmt.Sprintf("Current time: %s", now.Format("15:04:05")))
	}
	if p.config.Timezone {
		zone, _ := now.Zone()
		lines = append(lines, fmt.Sprintf("Timezone: %s", zone))
	}
	if p.config.CWD {
		if cwd, err := os.Getwd(); err == nil {
			lines = append(lines, fmt.Sprintf("Working directory: %s", cwd))
		}
	}
	if p.config.SessionDir && p.session != nil {
		lines = append(lines, fmt.Sprintf("Session directory: %s", p.session.RootDir))
	}
	if p.config.OS && p.system != nil {
		lines = append(lines, fmt.Sprintf("OS: %s/%s", p.system.OS, p.system.Arch))
	}

	if len(lines) == 0 {
		return ""
	}

	return "<system-context>\n" + strings.Join(lines, "\n") + "\n</system-context>"
}
