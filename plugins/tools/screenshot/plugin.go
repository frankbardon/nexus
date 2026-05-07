// Package screenshot registers the take_screenshot tool. The captured PNG
// is returned as an image MessagePart on ToolResult.OutputParts so memory
// plugins forward it to the next LLM turn alongside any text.
//
// Capture path is platform-specific and shells out to a tool that ships
// with the OS or is commonly preinstalled:
//
//   - darwin: screencapture -t png -x <tmpfile>
//   - linux:  gnome-screenshot -f <tmpfile>, then grim, then ImageMagick's
//     `import -window root <tmpfile>` as fallbacks.
//
// All other platforms surface a "screenshot not supported on this platform"
// error from the tool result, leaving the agent free to recover.
package screenshot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/blobs"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.tool.screenshot"
	pluginName = "Screenshot Tool"
	version    = "0.1.0"

	toolName = "take_screenshot"
)

// Default knobs for the per-session blob store backing take_screenshot.
// Values mirror nexus.tool.file so screenshots and document reads share
// budgets when both plugins are active in a session.
const (
	defaultBlobByteBudget      int64 = 2 * 1024 * 1024 * 1024
	defaultBlobInlineThreshold int64 = 256 * 1024
	defaultCaptureTimeout            = 15 * time.Second
)

// runFunc is the seam tests use to swap out exec.Run. The default
// implementation runs the binary via os/exec; tests substitute a
// stub that writes a fixture PNG to outPath.
type runFunc func(ctx context.Context, bin string, args []string, outPath string) error

// Plugin implements the take_screenshot tool.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace
	replay  *engine.ReplayState

	timeout          time.Duration
	blobStore        *blobs.Store // nil when session workspace is unavailable
	blobInlineCutoff int64

	// run is the exec seam. Production uses execRun; tests inject a stub
	// to avoid actually shelling out for screenshot binaries that may not
	// be installed on the CI host.
	run runFunc

	unsubs []func()

	liveCalls atomic.Uint64
}

// LiveCalls returns the count of take_screenshot invocations that survived
// the replay short-circuit. Tests assert zero during replay.
func (p *Plugin) LiveCalls() uint64 { return p.liveCalls.Load() }

// New returns a fresh screenshot plugin.
func New() engine.Plugin {
	return &Plugin{
		timeout: defaultCaptureTimeout,
		run:     execRun,
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
	if p.run == nil {
		p.run = execRun
	}

	if ts, ok := ctx.Config["timeout"].(string); ok && ts != "" {
		d, err := time.ParseDuration(ts)
		if err != nil {
			return fmt.Errorf("screenshot: invalid timeout %q: %w", ts, err)
		}
		p.timeout = d
	}

	budget := defaultBlobByteBudget
	p.blobInlineCutoff = defaultBlobInlineThreshold
	if bs, ok := ctx.Config["blob_store"].(map[string]any); ok {
		if v, ok := intLike(bs["byte_budget"]); ok {
			budget = v
		}
		if v, ok := intLike(bs["inline_threshold"]); ok {
			p.blobInlineCutoff = v
		}
	}
	if p.session != nil {
		store, err := blobs.New(p.session.BlobsDir(), budget)
		if err != nil {
			return fmt.Errorf("screenshot: blob store init: %w", err)
		}
		p.blobStore = store
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.handleEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)
	return nil
}

func (p *Plugin) Ready() error {
	_ = p.bus.Emit("tool.register", events.ToolDef{
		Name:        toolName,
		Description: "Capture the user's screen as a PNG and attach it to the next LLM turn as multimodal content. Use when the user asks 'what's on my screen?', references something they're looking at, or wants you to inspect a UI without sharing a file path.",
		Class:       "system",
		Subclass:    "capture",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"region": map[string]any{
					"type":        "string",
					"description": "Optional region selector. Currently ignored — the tool always captures the full screen. Reserved for future per-display / per-rectangle support.",
				},
			},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"media_type": map[string]any{"type": "string"},
				"size":       map[string]any{"type": "integer"},
				"blob_uri":   map[string]any{"type": "string", "description": "nexus-blob:<sha256> reference when stored as a blob; empty when inlined."},
			},
			"required": []string{"media_type", "size"},
		},
	})
	return nil
}

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, u := range p.unsubs {
		u()
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
	if !ok || tc.Name != toolName {
		return
	}
	if engine.ReplayToolShortCircuit(p.replay, p.bus, tc, p.logger) {
		return
	}
	p.liveCalls.Add(1)
	p.handleScreenshot(tc)
}

func (p *Plugin) handleScreenshot(tc events.ToolCall) {
	if p.blobStore == nil {
		p.emitResult(tc, "", "screenshot capture is unavailable: blob store not initialised (no session workspace)", nil, nil)
		return
	}

	tmpDir, err := os.MkdirTemp("", "nexus-screenshot-*")
	if err != nil {
		p.emitResult(tc, "", fmt.Sprintf("create temp dir: %s", err), nil, nil)
		return
	}
	defer os.RemoveAll(tmpDir)
	outPath := filepath.Join(tmpDir, "screen.png")

	bin, args, perr := p.platformCommand(outPath)
	if perr != nil {
		p.emitResult(tc, "", perr.Error(), nil, nil)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	if err := p.run(ctx, bin, args, outPath); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			p.emitResult(tc, "", fmt.Sprintf("screenshot timed out after %s", p.timeout), nil, nil)
			return
		}
		p.emitResult(tc, "", fmt.Sprintf("screenshot capture failed: %s", err), nil, nil)
		return
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		p.emitResult(tc, "", fmt.Sprintf("read screenshot bytes: %s", err), nil, nil)
		return
	}
	if len(data) == 0 {
		p.emitResult(tc, "", "screenshot capture produced an empty file", nil, nil)
		return
	}

	const mime = "image/png"
	part := events.MessagePart{Type: "image", MimeType: mime}
	structured := map[string]any{
		"media_type": mime,
		"size":       int64(len(data)),
	}
	if int64(len(data)) <= p.blobInlineCutoff {
		part.Data = data
	} else {
		h, err := p.blobStore.Put(data, mime)
		if err != nil {
			p.emitResult(tc, "", fmt.Sprintf("store blob: %s", err), nil, nil)
			return
		}
		if _, _, err := p.blobStore.Sweep(); err != nil {
			p.logger.Warn("screenshot: blob store sweep failed", "error", err)
		}
		part.URI = h.URI()
		structured["blob_uri"] = h.URI()
	}

	summary := fmt.Sprintf("Captured screen (%s, %d bytes)", mime, len(data))
	p.emitResult(tc, summary, "", structured, []events.MessagePart{part})
}

// platformCommand returns the binary + args to capture the full screen to
// outPath as PNG, picking the first installed tool per platform. Returns
// an unsupported-platform error when nothing is appropriate or no candidate
// is on PATH.
func (p *Plugin) platformCommand(outPath string) (string, []string, error) {
	switch runtime.GOOS {
	case "darwin":
		bin, err := exec.LookPath("screencapture")
		if err != nil {
			return "", nil, fmt.Errorf("screencapture not found on PATH: %w", err)
		}
		// -t png: PNG output. -x: silent (no shutter sound). Region capture
		// (-R) is intentionally not wired in v1; arg validation lands when
		// we add a region selector to the tool schema.
		return bin, []string{"-t", "png", "-x", outPath}, nil
	case "linux":
		if bin, err := exec.LookPath("gnome-screenshot"); err == nil {
			return bin, []string{"-f", outPath}, nil
		}
		if bin, err := exec.LookPath("grim"); err == nil {
			return bin, []string{outPath}, nil
		}
		if bin, err := exec.LookPath("import"); err == nil {
			return bin, []string{"-window", "root", outPath}, nil
		}
		return "", nil, errors.New("no screenshot tool found on PATH (tried gnome-screenshot, grim, ImageMagick import)")
	default:
		return "", nil, fmt.Errorf("screenshot not supported on this platform (%s)", runtime.GOOS)
	}
}

// execRun is the production runFunc — runs bin with args under ctx. The
// outPath is irrelevant here (the screenshot binary writes to it directly)
// but we accept it so test stubs share the same signature.
func execRun(ctx context.Context, bin string, args []string, _ string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w (output: %s)", bin, err, string(out))
	}
	return nil
}

func (p *Plugin) emitResult(tc events.ToolCall, output, errMsg string, structured map[string]any, parts []events.MessagePart) {
	result := events.ToolResult{
		SchemaVersion:    events.ToolResultVersion,
		ID:               tc.ID,
		Name:             tc.Name,
		Output:           output,
		Error:            errMsg,
		OutputStructured: structured,
		OutputParts:      parts,
		TurnID:           tc.TurnID,
	}
	if veto, err := p.bus.EmitVetoable("before:tool.result", &result); err == nil && veto.Vetoed {
		p.logger.Info("tool.result vetoed", "tool", tc.Name, "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("tool.result", result)
}

// intLike accepts the JSON-ish numeric kinds yaml.v3 unmarshals into.
func intLike(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	}
	return 0, false
}
