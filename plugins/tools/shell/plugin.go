package shell

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/sandbox"
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
	replay  *engine.ReplayState
	sandbox sandbox.Sandbox

	workingDir string
	timeout    time.Duration
	unsubs     []func()

	liveCalls atomic.Uint64
}

// LiveCalls returns the count of tool.invoke handlers that survived the
// replay short-circuit. Tests assert zero during replay.
func (p *Plugin) LiveCalls() uint64 { return p.liveCalls.Load() }

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
	p.replay = ctx.Replay
	p.sandbox = ctx.Sandbox

	// Working directory. Default to session files dir so shell commands and
	// the file_io plugin see the same filesystem location.
	if wd, ok := ctx.Config["working_dir"].(string); ok && wd != "" {
		p.workingDir = engine.ExpandPath(wd)
	} else if p.session != nil {
		p.workingDir = p.session.FilesDir()
	}

	// Per-call default timeout.
	if ts, ok := ctx.Config["timeout"].(string); ok {
		d, err := time.ParseDuration(ts)
		if err != nil {
			return fmt.Errorf("shell: invalid timeout %q: %w", ts, err)
		}
		p.timeout = d
	}

	// Backwards-compatible legacy config: when the plugin has no `sandbox:`
	// block but does have any of `allowed_commands`, `path_dirs`, or
	// `sandbox: <bool>`, synthesise an equivalent host-backend config and
	// replace the engine-supplied default sandbox. Documented as deprecated;
	// removal targeted for the milestone after gVisor lands.
	if needsLegacyShim(ctx.Config) {
		shim, err := buildLegacyShim(ctx.Config)
		if err != nil {
			return err
		}
		p.sandbox = shim
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.handleEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	return nil
}

// needsLegacyShim reports whether the plugin config carries pre-Sandbox keys
// (`allowed_commands`, `path_dirs`, top-level `sandbox: <bool>`). A new
// `sandbox:` map block opts back into the structured path even if the legacy
// keys are also present, since callers that bothered to write the new block
// are unambiguously asking for it.
func needsLegacyShim(cfg map[string]any) bool {
	if cfg == nil {
		return false
	}
	if _, ok := cfg["sandbox"].(map[string]any); ok {
		return false
	}
	if _, ok := cfg["allowed_commands"]; ok {
		return true
	}
	if _, ok := cfg["path_dirs"]; ok {
		return true
	}
	if _, ok := cfg["sandbox"].(bool); ok {
		return true
	}
	return false
}

// buildLegacyShim translates the deprecated top-level keys into a host
// backend so existing configs keep working without the new sandbox block.
func buildLegacyShim(cfg map[string]any) (sandbox.Sandbox, error) {
	block := map[string]any{}
	if v, ok := cfg["allowed_commands"]; ok {
		block["allowed_commands"] = v
	}
	if v, ok := cfg["path_dirs"]; ok {
		// path_dirs in the legacy shape are pre-expansion strings — pass
		// through; the host backend doesn't expand, the caller does.
		raw, ok := v.([]any)
		if ok {
			expanded := make([]any, 0, len(raw))
			for _, entry := range raw {
				if s, ok := entry.(string); ok {
					expanded = append(expanded, engine.ExpandPath(s))
				}
			}
			block["path_dirs"] = expanded
		}
	}
	if v, ok := cfg["sandbox"].(bool); ok {
		block["env_restrict"] = v
	}
	if ts, ok := cfg["timeout"].(string); ok {
		block["timeout"] = ts
	}
	return sandbox.New(sandbox.BackendHost, block)
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
	if engine.ReplayToolShortCircuit(p.replay, p.bus, tc, p.logger) {
		return
	}
	p.liveCalls.Add(1)

	command, _ := tc.Arguments["command"].(string)
	if command == "" {
		p.emitResult(tc, "", "command argument is required", nil)
		return
	}

	if p.session != nil {
		entry := fmt.Sprintf("%s\n", command)
		_ = p.session.AppendFile("plugins/"+pluginID+"/history.txt", []byte(entry))
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	res, err := p.sandbox.Exec(ctx, sandbox.ExecRequest{
		Kind:    sandbox.KindShell,
		Source:  []byte(command),
		WorkDir: p.workingDir,
		Timeout: p.timeout,
	})
	if err != nil {
		p.emitResult(tc, "", fmt.Sprintf("sandbox: %v", err), nil)
		return
	}

	stdoutStr := string(res.Stdout)
	stderrStr := string(res.Stderr)

	structured := map[string]any{
		"stdout":    stdoutStr,
		"stderr":    stderrStr,
		"exit_code": res.Exit,
		"timed_out": res.TimedOut,
	}

	output := stdoutStr
	if stderrStr != "" {
		if output != "" {
			output += "\n"
		}
		output += "STDERR:\n" + stderrStr
	}

	if res.TimedOut {
		p.emitResult(tc, output, fmt.Sprintf("command timed out after %s", p.timeout), structured)
		return
	}
	if res.Exit != 0 && stderrStr == "" && stdoutStr == "" {
		p.emitResult(tc, "", fmt.Sprintf("command exited %d", res.Exit), structured)
		return
	}
	p.emitResult(tc, output, "", structured)
}

func (p *Plugin) emitResult(tc events.ToolCall, output, errMsg string, structured map[string]any) {
	result := events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: tc.ID,
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
