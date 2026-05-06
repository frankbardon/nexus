package opener

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.tool.opener"
	pluginName = "File/Folder Opener Tool"
	version    = "0.1.0"
)

// Plugin implements a tool that opens files or folders using the OS-native handler.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger

	openCmd  string   // resolved OS command (open, xdg-open, etc.)
	openArgs []string // extra args prepended before the path
	timeout  time.Duration
	unsubs   []func()
}

func New() engine.Plugin {
	return &Plugin{
		timeout: 10 * time.Second,
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

	if ts, ok := ctx.Config["timeout"].(string); ok {
		d, err := time.ParseDuration(ts)
		if err != nil {
			return fmt.Errorf("opener: invalid timeout %q: %w", ts, err)
		}
		p.timeout = d
	}

	// Allow explicit override of the open command.
	if bin, ok := ctx.Config["open_cmd"].(string); ok {
		p.openCmd = bin
	} else if ctx.System != nil && ctx.System.HasOpen() {
		p.openCmd = ctx.System.OpenCmd
		p.openArgs = ctx.System.OpenArgs
	} else {
		return fmt.Errorf("opener: no open command available for this platform (%s)", ctx.System.OS)
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.handleEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	return nil
}

func (p *Plugin) Ready() error {
	return p.bus.Emit("tool.register", events.ToolDef{
		Name:        "open_path",
		Description: "Open a file or folder using the operating system's default handler. Files open in their associated application; folders open in the file manager.",
		Class:       "filesystem",
		Subclass:    "navigate",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the file or folder to open",
				},
			},
			"required": []string{"path"},
		},
	})
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
	}
}

func (p *Plugin) handleEvent(event engine.Event[any]) {
	tc, ok := event.Payload.(events.ToolCall)
	if !ok || tc.Name != "open_path" {
		return
	}
	p.handleOpen(tc)
}

func (p *Plugin) handleOpen(tc events.ToolCall) {
	path, _ := tc.Arguments["path"].(string)
	if path == "" {
		p.emitResult(tc, "", "path argument is required")
		return
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		p.emitResult(tc, "", fmt.Sprintf("invalid path: %s", err))
		return
	}

	if _, err := os.Stat(absPath); err != nil {
		p.emitResult(tc, "", fmt.Sprintf("path not found: %s", err))
		return
	}

	// Build the command: openCmd [openArgs...] <path>
	args := make([]string, len(p.openArgs), len(p.openArgs)+1)
	copy(args, p.openArgs)
	args = append(args, absPath)

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.openCmd, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			p.emitResult(tc, "", fmt.Sprintf("open command timed out after %s", p.timeout))
			return
		}
		errMsg := err.Error()
		if len(output) > 0 {
			errMsg = string(output)
		}
		p.emitResult(tc, "", fmt.Sprintf("open failed: %s", errMsg))
		return
	}

	p.emitResult(tc, fmt.Sprintf("Opened %s", absPath), "")
}

func (p *Plugin) emitResult(tc events.ToolCall, output, errMsg string) {
	result := events.ToolResult{SchemaVersion: events.ToolResultVersion, ID: tc.ID,
		Name:   tc.Name,
		Output: output,
		Error:  errMsg,
		TurnID: tc.TurnID,
	}
	if veto, err := p.bus.EmitVetoable("before:tool.result", &result); err == nil && veto.Vetoed {
		p.logger.Info("tool.result vetoed", "tool", tc.Name, "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("tool.result", result)
}
