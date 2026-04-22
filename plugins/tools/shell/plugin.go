package shell

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.tool.shell"
	pluginName = "Shell Tool"
	version    = "0.1.0"
)

// Plugin implements the shell command execution tool.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace

	allowedCommands []string
	timeout         time.Duration
	sandbox         bool
	unsubs          []func()
}

// New creates a new shell tool plugin.
func New() engine.Plugin {
	return &Plugin{
		timeout: 30 * time.Second,
	}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return pluginName }
func (p *Plugin) Version() string                   { return version }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session

	// Parse allowed commands.
	if ac, ok := ctx.Config["allowed_commands"].([]any); ok {
		for _, cmd := range ac {
			if s, ok := cmd.(string); ok {
				p.allowedCommands = append(p.allowedCommands, s)
			}
		}
	}

	// Parse timeout.
	if ts, ok := ctx.Config["timeout"].(string); ok {
		d, err := time.ParseDuration(ts)
		if err != nil {
			return fmt.Errorf("shell: invalid timeout %q: %w", ts, err)
		}
		p.timeout = d
	}

	// Parse sandbox mode.
	if sb, ok := ctx.Config["sandbox"].(bool); ok {
		p.sandbox = sb
	}

	// Register event handler.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.handleEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	return nil
}

func (p *Plugin) Ready() error {
	// Register the shell tool with the agent.
	_ = p.bus.Emit("tool.register", events.ToolDef{
		Name:        "shell",
		Description: "Execute shell commands and return their output. Use this to run CLI tools, scripts, and system commands.",
		Class:       "code",
		Subclass:    "execute",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The shell command to execute",
				},
			},
			"required": []string{"command"},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"stdout":    map[string]any{"type": "string", "description": "Standard output"},
				"stderr":    map[string]any{"type": "string", "description": "Standard error"},
				"exit_code": map[string]any{"type": "integer", "description": "Process exit code (0 = success)"},
				"timed_out": map[string]any{"type": "boolean", "description": "True when killed by the configured timeout"},
			},
			"required": []string{"stdout", "stderr", "exit_code"},
		},
	})
	return nil
}

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "tool.invoke", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"before:tool.result",
		"tool.result",
		"tool.register",
		"core.error",
	}
}

// handleEvent dispatches incoming events, filtering for shell tool invocations.
func (p *Plugin) handleEvent(event engine.Event[any]) {
	if event.Type != "tool.invoke" {
		return
	}

	tc, ok := event.Payload.(events.ToolCall)
	if !ok || tc.Name != "shell" {
		return
	}

	p.handleInvoke(tc)
}

func (p *Plugin) handleInvoke(tc events.ToolCall) {
	command, _ := tc.Arguments["command"].(string)
	if command == "" {
		p.emitResult(tc, "", "command argument is required", nil)
		return
	}

	// Log command to session history.
	if p.session != nil {
		entry := fmt.Sprintf("%s\n", command)
		_ = p.session.AppendFile("plugins/"+pluginID+"/history.txt", []byte(entry))
	}

	// Validate command against allowed list.
	if !p.isCommandAllowed(command) {
		p.emitResult(tc, "", fmt.Sprintf("command not allowed: %s", command), nil)
		return
	}

	// Execute the command with timeout.
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)

	// If sandbox mode, restrict the environment.
	if p.sandbox {
		cmd.Env = []string{
			"PATH=/usr/bin:/bin",
			"HOME=/tmp",
			"LANG=en_US.UTF-8",
		}
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	stdoutStr := stdout.String()
	stderrStr := stderr.String()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	timedOut := ctx.Err() == context.DeadlineExceeded

	structured := map[string]any{
		"stdout":    stdoutStr,
		"stderr":    stderrStr,
		"exit_code": exitCode,
		"timed_out": timedOut,
	}

	// Keep LLM-facing Output human-readable — concatenate stdout and stderr
	// the same way we always have.
	output := stdoutStr
	if stderrStr != "" {
		if output != "" {
			output += "\n"
		}
		output += "STDERR:\n" + stderrStr
	}

	if err != nil {
		if timedOut {
			p.emitResult(tc, output, fmt.Sprintf("command timed out after %s", p.timeout), structured)
			return
		}
		errMsg := err.Error()
		if output != "" {
			errMsg = output + "\n" + errMsg
		}
		p.emitResult(tc, "", errMsg, structured)
		return
	}

	p.emitResult(tc, output, "", structured)
}

// isCommandAllowed checks if a command is permitted.
func (p *Plugin) isCommandAllowed(command string) bool {
	if len(p.allowedCommands) == 0 {
		return true // no restrictions configured
	}

	// Extract the base command (first word).
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return false
	}
	baseCmd := parts[0]

	for _, allowed := range p.allowedCommands {
		if baseCmd == allowed {
			return true
		}
	}
	return false
}

func (p *Plugin) emitResult(tc events.ToolCall, output, errMsg string, structured map[string]any) {
	result := events.ToolResult{
		ID:               tc.ID,
		Name:             tc.Name,
		Output:           output,
		Error:            errMsg,
		OutputStructured: structured,
		TurnID:           tc.TurnID,
	}
	if veto, err := p.bus.EmitVetoable("before:tool.result", &result); err == nil && veto.Vetoed {
		p.logger.Info("tool.result vetoed", "tool", tc.Name, "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("tool.result", result)
}
