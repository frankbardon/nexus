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
	"sync/atomic"
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
	replay  *engine.ReplayState

	pdftotext     string        // resolved path to pdftotext binary
	pdfinfo       string        // resolved path to pdfinfo binary (optional)
	timeout       time.Duration // per-command timeout
	saveToSession bool          // persist extracted text to session files
	saveFileName  string        // custom filename for session save (default: derived from PDF name)

	// defaultMode chooses between text extraction and native-document
	// pass-through when the LLM doesn't supply a `mode` argument.
	// Values: "text" (default — current behaviour, runs pdftotext) or
	// "document" (returns the raw PDF bytes as a file MessagePart).
	defaultMode string

	unsubs []func()

	liveCalls atomic.Uint64
}

// LiveCalls returns the count of read_pdf invocations that survived the
// replay short-circuit. Tests assert zero during replay.
func (p *Plugin) LiveCalls() uint64 { return p.liveCalls.Load() }

func New() engine.Plugin {
	return &Plugin{
		timeout:     30 * time.Second,
		defaultMode: modeText,
	}
}

// Modes for read_pdf. modeText runs pdftotext like before; modeDocument
// returns the raw PDF bytes as a file MessagePart so multimodal-capable
// providers (Anthropic, Gemini) can read the document natively.
const (
	modeText     = "text"
	modeDocument = "document"
)

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

	if ts, ok := ctx.Config["timeout"].(string); ok {
		d, err := time.ParseDuration(ts)
		if err != nil {
			return fmt.Errorf("pdf: invalid timeout %q: %w", ts, err)
		}
		p.timeout = d
	}

	if dm, ok := ctx.Config["default_mode"].(string); ok && dm != "" {
		if dm != modeText && dm != modeDocument {
			return fmt.Errorf("pdf: invalid default_mode %q (want %q or %q)", dm, modeText, modeDocument)
		}
		p.defaultMode = dm
	}

	// Allow overriding the binary path via config. pdftotext is only
	// required for text mode; if the operator wired default_mode:
	// document and never opts back into text per-call, the binary
	// stays absent and the document path still works.
	if bin, ok := ctx.Config["pdftotext_bin"].(string); ok {
		p.pdftotext = bin
	} else {
		if path, err := exec.LookPath("pdftotext"); err == nil {
			p.pdftotext = path
		} else if p.defaultMode == modeText {
			return fmt.Errorf("pdf: pdftotext not found in PATH — install poppler (brew install poppler / apt install poppler-utils) or set default_mode: document: %w", err)
		}
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
		Description: "Read a PDF file. Default mode 'text' extracts text via poppler/pdftotext. Mode 'document' returns the raw PDF bytes as a file MessagePart so multimodal providers (Anthropic, Gemini) can read the document natively (preserves images, layout, native rendering).",
		Class:       "data",
		Subclass:    "extract",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the PDF file",
				},
				"mode": map[string]any{
					"type":        "string",
					"description": "Output mode. 'text' (default) extracts text via pdftotext. 'document' returns the raw PDF bytes as a file MessagePart for native multimodal providers; pagination/layout args are ignored in document mode.",
					"enum":        []string{modeText, modeDocument},
				},
				"first_page": map[string]any{
					"type":        "integer",
					"description": "First page to extract (1-based). Omit to start from the beginning. Ignored in document mode.",
				},
				"last_page": map[string]any{
					"type":        "integer",
					"description": "Last page to extract (1-based). Omit to read through the end. Ignored in document mode.",
				},
				"layout": map[string]any{
					"type":        "boolean",
					"description": "Preserve original physical layout. Useful for tables and forms. Default false. Ignored in document mode.",
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
	if engine.ReplayToolShortCircuit(p.replay, p.bus, tc, p.logger) {
		return
	}
	p.liveCalls.Add(1)
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

	// Per-call mode override; falls back to plugin default. Document mode
	// short-circuits the pdftotext path entirely so a session that only
	// uses native-document hand-off doesn't need poppler installed.
	mode := p.defaultMode
	if m, ok := tc.Arguments["mode"].(string); ok && m != "" {
		if m != modeText && m != modeDocument {
			p.emitResult(tc, "", fmt.Sprintf("invalid mode %q (want %q or %q)", m, modeText, modeDocument))
			return
		}
		mode = m
	}
	if mode == modeDocument {
		p.handleReadPDFDocument(tc, path, absPath)
		return
	}

	if p.pdftotext == "" {
		p.emitResult(tc, "", "pdftotext binary not configured: set pdftotext_bin or install poppler-utils to use mode 'text'")
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

// handleReadPDFDocument is the native-document hand-off path. Reads the
// raw PDF bytes and emits them as a file MessagePart on
// ToolResult.OutputParts so multimodal-capable providers (Anthropic,
// Gemini) consume the document directly. No poppler call is made.
//
// Bytes ride inline on the part — the PDF plugin doesn't own a blob
// store. Operators with very large PDFs should pair this with
// nexus.tool.file's read_document, which routes through the per-session
// blob store. Keeping read_pdf inline avoids a duplicate blob-store
// wiring path for the common (sub-MB) case.
func (p *Plugin) handleReadPDFDocument(tc events.ToolCall, path, absPath string) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		p.emitResult(tc, "", fmt.Sprintf("read pdf bytes: %s", err))
		return
	}

	const mime = "application/pdf"
	part := events.MessagePart{
		Type:     "file",
		MimeType: mime,
		Data:     data,
	}
	summary := fmt.Sprintf("Loaded PDF %q (%d bytes) as native document", path, len(data))
	result := events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            tc.ID,
		Name:          tc.Name,
		Output:        summary,
		OutputStructured: map[string]any{
			"path":       path,
			"media_type": mime,
			"size":       int64(len(data)),
			"mode":       modeDocument,
		},
		OutputParts: []events.MessagePart{part},
		TurnID:      tc.TurnID,
	}
	if veto, vErr := p.bus.EmitVetoable("before:tool.result", &result); vErr == nil && veto.Vetoed {
		p.logger.Info("tool.result vetoed", "tool", tc.Name, "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("tool.result", result)
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
