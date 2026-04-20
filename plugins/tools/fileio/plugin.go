package fileio

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.tool.file"
	pluginName = "File I/O Tools"
	version    = "0.1.0"
)

// Plugin implements file reading, writing, and listing tools.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	session *engine.SessionWorkspace

	baseDir             string
	allowExternalWrites bool
	enabled             map[string]bool
	unsubs              []func()
}

// New creates a new file I/O tool plugin.
func New() engine.Plugin {
	return &Plugin{}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return pluginName }
func (p *Plugin) Version() string        { return version }
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.session = ctx.Session

	// All tools enabled by default.
	p.enabled = map[string]bool{
		"read_file":       true,
		"read_file_chunk": true,
		"write_file":      true,
		"check_file_size": true,
		"list_files":      true,
	}

	// Allow per-tool enable/disable via config.
	if tools, ok := ctx.Config["tools"].(map[string]any); ok {
		for name, v := range tools {
			if _, known := p.enabled[name]; !known {
				p.logger.Warn("ignoring unknown tool in config", "tool", name)
				continue
			}
			if enabled, ok := v.(bool); ok {
				p.enabled[name] = enabled
			}
		}
	}

	if bd, ok := ctx.Config["base_dir"].(string); ok {
		p.baseDir = bd
	}

	if v, ok := ctx.Config["allow_external_writes"].(bool); ok {
		p.allowExternalWrites = v
	}

	// Default to session files directory so agent-generated files land in the
	// session workspace unless the user explicitly configures a base_dir.
	if p.baseDir == "" && p.session != nil {
		p.baseDir = p.session.FilesDir()
	}

	// Register event handler.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.handleEvent,
			engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	return nil
}

func (p *Plugin) registerTool(def events.ToolDef) {
	if !p.enabled[def.Name] {
		p.logger.Info("tool disabled by config", "tool", def.Name)
		return
	}
	_ = p.bus.Emit("tool.register", def)
}

func (p *Plugin) Ready() error {
	p.registerTool(events.ToolDef{
		Name:        "read_file",
		Description: "Read the contents of a file at the given path. Returns the file content as a string.",
		Class:       "filesystem",
		Subclass:    "read",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "The file path to read, relative to the base directory",
				},
			},
			"required": []string{"path"},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content":    map[string]any{"type": "string"},
				"bytes_read": map[string]any{"type": "integer"},
			},
			"required": []string{"content", "bytes_read"},
		},
	})

	p.registerTool(events.ToolDef{
		Name:        "read_file_chunk",
		Description: "Read a chunk of a file starting at a byte offset. Returns the chunk content, bytes read, and total file size so the caller can page through large files.",
		Class:       "filesystem",
		Subclass:    "read",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "The file path to read, relative to the base directory",
				},
				"offset": map[string]any{
					"type":        "number",
					"description": "Byte offset to start reading from (default 0)",
				},
				"length": map[string]any{
					"type":        "number",
					"description": "Maximum number of bytes to read (default 4096)",
				},
			},
			"required": []string{"path"},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content":    map[string]any{"type": "string"},
				"bytes_read": map[string]any{"type": "integer"},
				"offset":     map[string]any{"type": "integer"},
				"total_size": map[string]any{"type": "integer"},
			},
			"required": []string{"content", "bytes_read", "offset", "total_size"},
		},
	})

	p.registerTool(events.ToolDef{
		Name:        "write_file",
		Description: "Write content to a file at the given path. Creates the file if it doesn't exist, overwrites if it does.",
		Class:       "filesystem",
		Subclass:    "write",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "The file path to write to, relative to the base directory",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "The content to write to the file",
				},
			},
			"required": []string{"path", "content"},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":          map[string]any{"type": "string"},
				"bytes_written": map[string]any{"type": "integer"},
				"created":       map[string]any{"type": "boolean"},
			},
			"required": []string{"path", "bytes_written", "created"},
		},
	})

	p.registerTool(events.ToolDef{
		Name:        "check_file_size",
		Description: "Get the size of a file in bytes. Works on Windows, macOS, and Linux.",
		Class:       "filesystem",
		Subclass:    "read",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "The file path to check, relative to the base directory",
				},
			},
			"required": []string{"path"},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
				"size": map[string]any{"type": "integer"},
			},
			"required": []string{"path", "size"},
		},
	})

	p.registerTool(events.ToolDef{
		Name:        "list_files",
		Description: "List files in a directory, optionally matching a glob pattern.",
		Class:       "filesystem",
		Subclass:    "navigate",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "The directory path to list, relative to the base directory",
				},
				"pattern": map[string]any{
					"type":        "string",
					"description": "Optional glob pattern to filter files (e.g. '*.go', '**/*.json')",
				},
			},
			"required": []string{"path"},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
				"entries": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name":   map[string]any{"type": "string"},
							"is_dir": map[string]any{"type": "boolean"},
							"size":   map[string]any{"type": "integer"},
						},
						"required": []string{"name", "is_dir"},
					},
				},
			},
			"required": []string{"path", "entries"},
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
		"session.file.created",
		"session.file.updated",
	}
}

// handleEvent dispatches incoming events, filtering for file tool invocations.
func (p *Plugin) handleEvent(event engine.Event[any]) {
	if event.Type != "tool.invoke" {
		return
	}

	tc, ok := event.Payload.(events.ToolCall)
	if !ok {
		return
	}

	if !p.enabled[tc.Name] {
		return
	}

	switch tc.Name {
	case "read_file":
		p.handleReadFile(tc)
	case "read_file_chunk":
		p.handleReadFileChunk(tc)
	case "write_file":
		p.handleWriteFile(tc)
	case "check_file_size":
		p.handleCheckFileSize(tc)
	case "list_files":
		p.handleListFiles(tc)
	}
}

// resolvePath resolves a path relative to the base directory and ensures it
// does not escape the base directory via traversal.
func (p *Plugin) resolvePath(path string) (string, error) {
	if p.baseDir == "" {
		return filepath.Abs(path)
	}

	absBase, err := filepath.Abs(p.baseDir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve base directory: %w", err)
	}

	resolved := filepath.Join(absBase, filepath.Clean(path))

	// Prevent path traversal outside base directory.
	if !strings.HasPrefix(resolved, absBase+string(filepath.Separator)) && resolved != absBase {
		return "", fmt.Errorf("path %q escapes base directory", path)
	}

	return resolved, nil
}

// resolveWritePath resolves a write path. When allow_external_writes is false
// (the default), writes are always constrained to the session files directory
// regardless of base_dir. When true, writes use the normal base_dir resolution.
func (p *Plugin) resolveWritePath(path string) (string, error) {
	if !p.allowExternalWrites && p.session != nil {
		filesDir := p.session.FilesDir()
		absBase, err := filepath.Abs(filesDir)
		if err != nil {
			return "", fmt.Errorf("failed to resolve session files directory: %w", err)
		}

		resolved := filepath.Join(absBase, filepath.Clean(path))

		if !strings.HasPrefix(resolved, absBase+string(filepath.Separator)) && resolved != absBase {
			return "", fmt.Errorf("path %q escapes session files directory", path)
		}

		return resolved, nil
	}

	return p.resolvePath(path)
}

func (p *Plugin) handleReadFile(tc events.ToolCall) {
	path, _ := tc.Arguments["path"].(string)
	if path == "" {
		p.emitResult(tc, "", "path argument is required", nil)
		return
	}

	resolved, err := p.resolvePath(path)
	if err != nil {
		p.emitResult(tc, "", err.Error(), nil)
		return
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		p.emitResult(tc, "", fmt.Sprintf("failed to read file: %s", err), nil)
		return
	}

	content := string(data)
	p.emitResult(tc, content, "", map[string]any{
		"content":    content,
		"bytes_read": len(data),
	})
}

func (p *Plugin) handleReadFileChunk(tc events.ToolCall) {
	path, _ := tc.Arguments["path"].(string)
	if path == "" {
		p.emitResult(tc, "", "path argument is required", nil)
		return
	}

	offset := int64(0)
	if v, ok := tc.Arguments["offset"].(float64); ok {
		offset = int64(v)
	}

	length := int64(4096)
	if v, ok := tc.Arguments["length"].(float64); ok {
		length = int64(v)
	}

	if offset < 0 {
		p.emitResult(tc, "", "offset must not be negative", nil)
		return
	}
	if length <= 0 {
		p.emitResult(tc, "", "length must be greater than zero", nil)
		return
	}

	resolved, err := p.resolvePath(path)
	if err != nil {
		p.emitResult(tc, "", err.Error(), nil)
		return
	}

	f, err := os.Open(resolved)
	if err != nil {
		p.emitResult(tc, "", fmt.Sprintf("failed to open file: %s", err), nil)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		p.emitResult(tc, "", fmt.Sprintf("failed to stat file: %s", err), nil)
		return
	}
	if info.IsDir() {
		p.emitResult(tc, "", fmt.Sprintf("%s is a directory, not a file", path), nil)
		return
	}

	totalSize := info.Size()

	makeResult := func(chunk string, bytesRead int) map[string]any {
		return map[string]any{
			"content":    chunk,
			"bytes_read": bytesRead,
			"offset":     offset,
			"total_size": totalSize,
		}
	}

	if offset >= totalSize {
		p.emitResult(tc,
			fmt.Sprintf("{\"content\":\"\",\"bytes_read\":0,\"offset\":%d,\"total_size\":%d}", offset, totalSize),
			"",
			makeResult("", 0))
		return
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		p.emitResult(tc, "", fmt.Sprintf("failed to seek: %s", err), nil)
		return
	}

	buf := make([]byte, length)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		p.emitResult(tc, "", fmt.Sprintf("failed to read file: %s", err), nil)
		return
	}

	chunk := string(buf[:n])
	p.emitResult(tc,
		fmt.Sprintf("{\"content\":%q,\"bytes_read\":%d,\"offset\":%d,\"total_size\":%d}", chunk, n, offset, totalSize),
		"",
		makeResult(chunk, n))
}

func (p *Plugin) handleCheckFileSize(tc events.ToolCall) {
	path, _ := tc.Arguments["path"].(string)
	if path == "" {
		p.emitResult(tc, "", "path argument is required", nil)
		return
	}

	resolved, err := p.resolvePath(path)
	if err != nil {
		p.emitResult(tc, "", err.Error(), nil)
		return
	}

	info, err := os.Stat(resolved)
	if err != nil {
		p.emitResult(tc, "", fmt.Sprintf("failed to stat file: %s", err), nil)
		return
	}

	if info.IsDir() {
		p.emitResult(tc, "", fmt.Sprintf("%s is a directory, not a file", path), nil)
		return
	}

	p.emitResult(tc, fmt.Sprintf("%d", info.Size()), "", map[string]any{
		"path": path,
		"size": info.Size(),
	})
}

func (p *Plugin) handleWriteFile(tc events.ToolCall) {
	path, _ := tc.Arguments["path"].(string)
	content, _ := tc.Arguments["content"].(string)

	if path == "" {
		p.emitResult(tc, "", "path argument is required", nil)
		return
	}

	resolved, err := p.resolveWritePath(path)
	if err != nil {
		p.emitResult(tc, "", err.Error(), nil)
		return
	}

	// Check if the file already exists (for session event type).
	_, existErr := os.Stat(resolved)
	existed := existErr == nil

	// Ensure parent directory exists.
	dir := filepath.Dir(resolved)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		p.emitResult(tc, "", fmt.Sprintf("failed to create directory: %s", err), nil)
		return
	}

	if err := os.WriteFile(resolved, []byte(content), 0o644); err != nil {
		p.emitResult(tc, "", fmt.Sprintf("failed to write file: %s", err), nil)
		return
	}

	// Emit session file event if writing within the session files directory.
	if p.session != nil {
		filesDir := p.session.FilesDir()
		absFiles, _ := filepath.Abs(filesDir)
		if absFiles != "" && strings.HasPrefix(resolved, absFiles+string(filepath.Separator)) {
			rel, _ := filepath.Rel(absFiles, resolved)
			eventType := "session.file.created"
			if existed {
				eventType = "session.file.updated"
			}
			_ = p.bus.Emit(eventType, map[string]any{
				"session_id": p.session.ID,
				"path":       rel,
				"size":       len(content),
			})
		}
	}

	p.emitResult(tc,
		fmt.Sprintf("Successfully wrote %d bytes to %s", len(content), path),
		"",
		map[string]any{
			"path":          path,
			"bytes_written": len(content),
			"created":       !existed,
		})
}

func (p *Plugin) handleListFiles(tc events.ToolCall) {
	path, _ := tc.Arguments["path"].(string)
	pattern, _ := tc.Arguments["pattern"].(string)

	if path == "" {
		path = "."
	}

	resolved, err := p.resolvePath(path)
	if err != nil {
		p.emitResult(tc, "", err.Error(), nil)
		return
	}

	if pattern != "" {
		// Use glob matching within the directory.
		globPattern := filepath.Join(resolved, pattern)
		matches, err := filepath.Glob(globPattern)
		if err != nil {
			p.emitResult(tc, "", fmt.Sprintf("invalid glob pattern: %s", err), nil)
			return
		}

		var names []string
		structuredEntries := make([]map[string]any, 0, len(matches))
		for _, m := range matches {
			rel, err := filepath.Rel(resolved, m)
			if err != nil {
				rel = m
			}
			names = append(names, rel)
			entry := map[string]any{
				"name":   rel,
				"is_dir": false,
			}
			if info, err := os.Stat(m); err == nil {
				entry["is_dir"] = info.IsDir()
				entry["size"] = info.Size()
			}
			structuredEntries = append(structuredEntries, entry)
		}
		p.emitResult(tc, strings.Join(names, "\n"), "", map[string]any{
			"path":    path,
			"entries": structuredEntries,
		})
		return
	}

	// List all entries in the directory.
	entries, err := os.ReadDir(resolved)
	if err != nil {
		p.emitResult(tc, "", fmt.Sprintf("failed to list directory: %s", err), nil)
		return
	}

	var lines []string
	structuredEntries := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		suffix := ""
		if entry.IsDir() {
			suffix = "/"
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			lines = append(lines, entry.Name()+suffix)
			structuredEntries = append(structuredEntries, map[string]any{
				"name":   entry.Name(),
				"is_dir": entry.IsDir(),
			})
			continue
		}
		lines = append(lines, fmt.Sprintf("%s%s\t%d bytes", entry.Name(), suffix, info.Size()))
		structuredEntries = append(structuredEntries, map[string]any{
			"name":   entry.Name(),
			"is_dir": entry.IsDir(),
			"size":   info.Size(),
		})
	}

	p.emitResult(tc, strings.Join(lines, "\n"), "", map[string]any{
		"path":    path,
		"entries": structuredEntries,
	})
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
