package client

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// refreshTools fetches the server's current tool set and synchronises the
// Nexus catalog projection. Newly visible tools emit tool.register; tools
// that vanished are dropped from the routing table.
func (s *server) refreshTools(ctx context.Context) error {
	sess := s.getSession()
	if sess == nil {
		return fmt.Errorf("not connected")
	}

	res, err := sess.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return fmt.Errorf("tools/list: %w", err)
	}

	seen := map[string]bool{}
	for _, t := range res.Tools {
		if !s.cfg.Tools.allowed(t.Name) {
			continue
		}
		seen[t.Name] = true
		s.registerMCPTool(t)
	}

	s.mu.Lock()
	for raw := range s.tools {
		if !seen[raw] {
			s.parent.unregisterToolRoute(toolName(s.cfg.Name, raw))
			delete(s.tools, raw)
		}
	}
	s.mu.Unlock()

	_ = s.parent.bus.Emit("mcp.tools.refreshed", map[string]any{
		"server": s.cfg.Name,
		"count":  len(seen),
	})
	return nil
}

// registerMCPTool emits a tool.register for one MCP tool, after wrapping its
// inputSchema into the Nexus-flavoured Parameters map. Tool names land
// under mcp__<server>__<raw> so they never collide with native tools.
func (s *server) registerMCPTool(t *mcp.Tool) {
	s.mu.Lock()
	s.tools[t.Name] = t
	s.mu.Unlock()

	catalog := toolName(s.cfg.Name, t.Name)
	s.parent.registerToolRoute(catalog, s.cfg.Name)

	params := schemaFromTool(t)

	_ = s.parent.bus.Emit("tool.register", events.ToolDef{
		Name:        catalog,
		Description: firstNonEmpty(t.Description, t.Title, t.Name),
		Parameters:  params,
		Class:       "mcp",
		Subclass:    s.cfg.Name,
		Tags:        []string{"mcp", "mcp:" + s.cfg.Name},
	})
}

// schemaFromTool produces a JSON Schema map from an MCP tool definition.
// From the client the SDK delivers InputSchema as the server's raw JSON
// schema (typically a map[string]any), so we marshal+unmarshal it into a
// plain map regardless of its concrete type.
func schemaFromTool(t *mcp.Tool) map[string]any {
	if t.InputSchema == nil {
		return map[string]any{"type": "object"}
	}
	data, err := json.Marshal(t.InputSchema)
	if err != nil {
		return map[string]any{"type": "object"}
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return map[string]any{"type": "object"}
	}
	if _, ok := out["type"]; !ok {
		out["type"] = "object"
	}
	return out
}

// handleToolInvoke routes any tool.invoke whose name belongs to this plugin.
// The dispatch table records which server owns each registered name; native
// tools (shell, file, etc.) flow past untouched.
func (p *Plugin) handleToolInvoke(e engine.Event[any]) {
	tc, ok := e.Payload.(events.ToolCall)
	if !ok {
		return
	}
	srvName, ok := p.toolRoute(tc.Name)
	if !ok {
		return
	}
	srv, ok := p.servers[srvName]
	if !ok {
		p.emitToolError(tc, fmt.Sprintf("mcp.client: unknown server %q", srvName))
		return
	}

	switch {
	case tc.Name == listResourcesToolName(srvName):
		p.dispatchListResources(srv, tc)
	case tc.Name == readResourceToolName(srvName):
		p.dispatchReadResource(srv, tc)
	default:
		// Either an auto-registered static/template resource or a raw MCP tool.
		if uri, isStatic := srv.staticResourceURI(tc.Name); isStatic {
			p.dispatchReadResourceURI(srv, tc, uri)
			return
		}
		if tmplName, isTemplate := srv.templateForCatalog(tc.Name); isTemplate {
			p.dispatchTemplateResource(srv, tc, tmplName)
			return
		}
		p.dispatchTool(srv, tc)
	}
}

// dispatchTool sends an MCP tools/call and emits the resulting tool.result.
func (p *Plugin) dispatchTool(s *server, tc events.ToolCall) {
	sess := s.getSession()
	if sess == nil {
		p.emitToolError(tc, "mcp.client: server not connected")
		return
	}

	rawName := ""
	prefix := "mcp__" + s.cfg.Name + "__"
	if len(tc.Name) > len(prefix) {
		rawName = tc.Name[len(prefix):]
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
	defer cancel()

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      rawName,
		Arguments: tc.Arguments,
	})
	if err != nil {
		p.emitToolError(tc, fmt.Sprintf("mcp tools/call: %v", err))
		return
	}

	output, parts := p.renderToolContent(res.Content)
	result := events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            tc.ID,
		Name:          tc.Name,
		Output:        output,
		TurnID:        tc.TurnID,
		OutputParts:   parts,
	}
	if structured, ok := res.StructuredContent.(map[string]any); ok {
		result.OutputStructured = structured
	}
	if res.IsError {
		// MCP returns IsError=true for tool-level errors with diagnostic
		// content inside res.Content; surface the rendered text as Error
		// so the LLM sees a clear failure signal.
		if result.Error == "" {
			result.Error = output
		}
	}
	p.emitToolResult(result)
}

// renderToolContent converts MCP content blocks into a Nexus
// (output, multimodal parts) pair. Text blocks concatenate into Output;
// image/audio blocks become MessageParts, blob-stored when above the
// inline threshold.
func (p *Plugin) renderToolContent(blocks []mcp.Content) (string, []events.MessagePart) {
	var output string
	var parts []events.MessagePart
	for _, b := range blocks {
		switch c := b.(type) {
		case *mcp.TextContent:
			if output != "" {
				output += "\n"
			}
			output += c.Text
		case *mcp.ImageContent:
			parts = append(parts, p.partFromBytes("image", c.MIMEType, c.Data))
		case *mcp.AudioContent:
			parts = append(parts, p.partFromBytes("audio", c.MIMEType, c.Data))
		case *mcp.ResourceLink:
			if output != "" {
				output += "\n"
			}
			output += fmt.Sprintf("[resource: %s] %s", c.URI, firstNonEmpty(c.Name, c.URI))
		case *mcp.EmbeddedResource:
			text, embedParts := p.renderResourceContents([]*mcp.ResourceContents{c.Resource})
			if text != "" {
				if output != "" {
					output += "\n"
				}
				output += text
			}
			parts = append(parts, embedParts...)
		}
	}
	return output, parts
}

// partFromBytes takes a decoded MCP payload and either inlines the bytes or
// routes them through the blob store, depending on the configured cutoff.
// The official SDK delivers image/audio data pre-decoded as []byte, so no
// base64 step is needed.
func (p *Plugin) partFromBytes(kind, mime string, data []byte) events.MessagePart {
	part := events.MessagePart{Type: kind, MimeType: mime}
	if p.blobs != nil && int64(len(data)) > defaultBlobInlineCutoff {
		h, err := p.blobs.Put(data, mime)
		if err == nil {
			part.URI = h.URI()
			return part
		}
		p.logger.Warn("mcp.client: blob store put failed, inlining", "error", err)
	}
	part.Data = data
	return part
}

func (p *Plugin) emitToolResult(result events.ToolResult) {
	if veto, err := p.bus.EmitVetoable("before:tool.result", &result); err == nil && veto.Vetoed {
		p.logger.Info("mcp tool.result vetoed", "tool", result.Name, "reason", veto.Reason)
		return
	}
	_ = p.bus.Emit("tool.result", result)
}

func (p *Plugin) emitToolError(tc events.ToolCall, msg string) {
	p.emitToolResult(events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            tc.ID,
		Name:          tc.Name,
		Error:         msg,
		TurnID:        tc.TurnID,
	})
}
