package pdf

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.tool.pdf"
	pluginName = "PDF Reader Tool"
	version    = "0.1.0"
)

// Plugin implements PDF text extraction using pdftotext (poppler-utils).
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace

	pdftotext     string        // resolved path to pdftotext binary
	pdfinfo       string        // resolved path to pdfinfo binary (optional)
	timeout       time.Duration // per-command timeout
	saveToSession bool          // persist extracted text to session files
	saveFileName  string        // custom filename for session save (default: derived from PDF name)
	unsubs        []func()
}

func New() engine.Plugin {
	return &Plugin{
		timeout: 30 * time.Second,
	}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return pluginName }
func (p *Plugin) Version() string        { return version }
func (p *Plugin) Dependencies() []string { return nil }
func (p *Plugin) Requires() []engine.Requirement { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session

	if ts, ok := ctx.Config["timeout"].(string); ok {
		d, err := time.ParseDuration(ts)
		if err != nil {
			return fmt.Errorf("pdf: invalid timeout %q: %w", ts, err)
		}
		p.timeout = d
	}

	// Allow overriding the binary path via config.
	if bin, ok := ctx.Config["pdftotext_bin"].(string); ok {
		p.pdftotext = bin
	} else {
		path, err := exec.LookPath("pdftotext")
		if err != nil {
			return fmt.Errorf("pdf: pdftotext not found in PATH — install poppler (brew install poppler / apt install poppler-utils): %w", err)
		}
		p.pdftotext = path
	}

	if save, ok := ctx.Config["save_to_session"].(bool); ok {
		p.saveToSession = save
	}
	if name, ok := ctx.Config["save_file_name"].(string); ok {
		p.saveFileName = name
	}

	// pdfinfo is optional, used for page count metadata.
	if bin, ok := ctx.Config["pdfinfo_bin"].(string); ok {
		p.pdfinfo = bin
	} else {
		if path, err := exec.LookPath("pdfinfo"); err == nil {
			p.pdfinfo = path
		}
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.handleEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	return nil
}

func (p *Plugin) Ready() error {
	_ = p.bus.Emit("tool.register", events.ToolDef{
		Name:        "read_pdf",
		Description: "Extract text content from a PDF file. Returns the full text by default, or a specific page range.",
		Class:       "data",
		Subclass:    "extract",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the PDF file",
				},
				"first_page": map[string]any{
					"type":        "integer",
					"description": "First page to extract (1-based). Omit to start from the beginning.",
				},
				"last_page": map[string]any{
					"type":        "integer",
					"description": "Last page to extract (1-based). Omit to read through the end.",
				},
				"layout": map[string]any{
					"type":        "boolean",
					"description": "Preserve original physical layout. Useful for tables and forms. Default false.",
				},
			},
			"required": []string{"path"},
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
	}
}

func (p *Plugin) handleEvent(event engine.Event[any]) {
	tc, ok := event.Payload.(events.ToolCall)
	if !ok || tc.Name != "read_pdf" {
		return
	}
	p.handleReadPDF(tc)
}

func (p *Plugin) handleReadPDF(tc events.ToolCall) {
	path, _ := tc.Arguments["path"].(string)
	if path == "" {
		p.emitResult(tc, "", "path argument is required")
		return
	}

	// Resolve to absolute path.
	absPath, err := filepath.Abs(path)
	if err != nil {
		p.emitResult(tc, "", fmt.Sprintf("invalid path: %s", err))
		return
	}

	// Verify the file exists and is a PDF.
	info, err := os.Stat(absPath)
	if err != nil {
		p.emitResult(tc, "", fmt.Sprintf("file not found: %s", err))
		return
	}
	if info.IsDir() {
		p.emitResult(tc, "", "path is a directory, not a file")
		return
	}

	// Build pdftotext arguments.
	args := []string{}

	if first, ok := toInt(tc.Arguments["first_page"]); ok && first > 0 {
		args = append(args, "-f", strconv.Itoa(first))
	}
	if last, ok := toInt(tc.Arguments["last_page"]); ok && last > 0 {
		args = append(args, "-l", strconv.Itoa(last))
	}
	if layout, ok := tc.Arguments["layout"].(bool); ok && layout {
		args = append(args, "-layout")
	}

	// Input file and "-" for stdout output.
	args = append(args, absPath, "-")

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, p.pdftotext, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			p.emitResult(tc, "", fmt.Sprintf("pdftotext timed out after %s", p.timeout))
			return
		}
		errMsg := err.Error()
		if stderr.Len() > 0 {
			errMsg = strings.TrimSpace(stderr.String())
		}
		p.emitResult(tc, "", fmt.Sprintf("pdftotext failed: %s", errMsg))
		return
	}

	text := stdout.String()

	// Prepend page count metadata if pdfinfo is available.
	if p.pdfinfo != "" {
		if pageCount := p.getPageCount(absPath); pageCount > 0 {
			text = fmt.Sprintf("[PDF: %s | %d pages]\n\n%s", filepath.Base(absPath), pageCount, text)
		}
	}

	if strings.TrimSpace(text) == "" {
		p.emitResult(tc, "", "PDF produced no text output — it may be image-based (scanned) and require OCR")
		return
	}

	// Save extracted text to session files when configured.
	if p.saveToSession && p.session != nil {
		baseName := p.saveFileName
		if baseName == "" {
			baseName = strings.TrimSuffix(filepath.Base(absPath), filepath.Ext(absPath)) + ".txt"
		}
		sessionPath := "files/" + baseName
		if err := p.session.WriteFile(sessionPath, []byte(text)); err != nil {
			p.logger.Warn("failed to save PDF text to session", "path", sessionPath, "error", err)
		} else {
			_ = p.bus.Emit("session.file.created", map[string]any{
				"session_id": p.session.ID,
				"path":       baseName,
				"size":       len(text),
				"source":     absPath,
			})
			p.logger.Info("saved PDF text to session", "path", sessionPath)
		}
	}

	p.emitResult(tc, text, "")
}

// getPageCount uses pdfinfo to extract the page count.
func (p *Plugin) getPageCount(path string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, p.pdfinfo, path).Output()
	if err != nil {
		return 0
	}

	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Pages:") {
			field := strings.TrimSpace(strings.TrimPrefix(line, "Pages:"))
			if n, err := strconv.Atoi(field); err == nil {
				return n
			}
		}
	}
	return 0
}

// toInt extracts an integer from a value that may be int or float64 (YAML unmarshalling).
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		return int(n), true
	}
	return 0, false
}

func (p *Plugin) emitResult(tc events.ToolCall, output, errMsg string) {
	result := events.ToolResult{
		ID:     tc.ID,
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
