package dynvars

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/nexusheaders"
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
	replay  *engine.ReplayState
	config  varConfig
}

type varConfig struct {
	Date       bool
	Time       bool
	Timezone   bool
	CWD        bool
	SessionDir bool
	OS         bool

	// RequestHeaders names the X-Nexus-* request headers of the current turn
	// to surface in the prompt, by normalized header name (`X-Nexus-Timezone`
	// -> `timezone`). Empty by default.
	//
	// # Why an allowlist and not a boolean
	//
	// These values are client-controlled and unauthenticated — a caller writes
	// whatever it likes into them (see pkg/nexusheaders). The engine therefore
	// binds them into the RESERVED session-label namespace, which every
	// <session_context> builder filters out, so nothing reaches a prompt just
	// because somebody sent it. This setting is the deliberate exception, and
	// naming each header is what keeps it deliberate: an operator who wants the
	// caller's timezone in the prompt gets exactly that, and does not silently
	// also get whatever new header a client starts sending next month.
	RequestHeaders []string
}

// New creates a new dynamic variables plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return pluginName }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription { return nil }
func (p *Plugin) Emissions() []string                       { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.logger = ctx.Logger
	p.prompts = ctx.Prompts
	p.session = ctx.Session
	p.system = ctx.System
	p.replay = ctx.Replay

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
	if raw, ok := ctx.Config["request_headers"].([]any); ok {
		for _, item := range raw {
			name, ok := item.(string)
			if !ok {
				return fmt.Errorf("nexus.system.dynvars: request_headers entries must be strings, got %T", item)
			}
			name = strings.ToLower(strings.TrimSpace(name))
			name = strings.TrimPrefix(name, strings.ToLower(nexusheaders.Prefix))
			if name == "" {
				continue
			}
			p.config.RequestHeaders = append(p.config.RequestHeaders, name)
		}
		sort.Strings(p.config.RequestHeaders)
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

	// Replay caveat: time.Now() and os.Getwd() are non-deterministic. The
	// llm.response is served from the journaled stash so the output is
	// stable, but the system prompt diverges from the original run. Warn
	// once so it shows up in replay diagnostics.
	if p.replay != nil && p.replay.Active() {
		engine.NondeterministicWarn(p.logger, "dynvars: time.Now / os.Getwd during replay")
	}

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
	// Read per render, not cached at Init: the headers belong to the turn in
	// flight, and a session serving several callers rebinds them on every one.
	// A header the caller did not send contributes no line at all, rather than
	// an empty one the model would have to interpret.
	for _, name := range p.config.RequestHeaders {
		if p.session == nil {
			break
		}
		if value, ok := p.session.RequestHeader(name); ok && value != "" {
			lines = append(lines, fmt.Sprintf("Request header %s%s: %s", nexusheaders.Prefix, name, value))
		}
	}

	if len(lines) == 0 {
		return ""
	}

	return "<system-context>\n" + strings.Join(lines, "\n") + "\n</system-context>"
}
