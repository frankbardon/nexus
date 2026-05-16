package client

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// refreshPrompts pulls the server's prompt set and reconciles the slash
// command registry. Removed prompts have their commands unregistered.
func (s *server) refreshPrompts(ctx context.Context) error {
	c := s.getClient()
	if c == nil {
		return fmt.Errorf("not connected")
	}

	res, err := c.ListPrompts(ctx, mcp.ListPromptsRequest{})
	if err != nil {
		return fmt.Errorf("prompts/list: %w", err)
	}

	seen := map[string]bool{}
	prefix := s.parent.cfg.Defaults.CommandPrefix
	for _, p := range res.Prompts {
		seen[p.Name] = true
		s.registerMCPPrompt(p, prefix)
	}

	s.mu.Lock()
	for raw := range s.prompts {
		if !seen[raw] {
			cmd := "/" + prefix + "." + s.cfg.Name + "." + promptSlug(raw)
			s.parent.unregisterPrompt(cmd)
			delete(s.prompts, raw)
		}
	}
	s.mu.Unlock()

	_ = s.parent.bus.Emit("mcp.prompts.refreshed", map[string]any{
		"server": s.cfg.Name,
		"count":  len(seen),
	})
	return nil
}

func (s *server) registerMCPPrompt(p mcp.Prompt, prefix string) {
	s.mu.Lock()
	s.prompts[p.Name] = p
	s.mu.Unlock()

	cmd := "/" + prefix + "." + s.cfg.Name + "." + promptSlug(p.Name)
	s.parent.registerPrompt(cmd, s.cfg.Name, p.Name)
}

// handleInputIntercept watches before:io.input for /mcp.<srv>.<prompt> style
// invocations. When one matches we veto the original input, call
// prompts/get, and re-emit a new io.input whose PreloadMessages carry the
// server's response in the right order with their original roles.
func (p *Plugin) handleInputIntercept(e engine.Event[any]) {
	vp, ok := e.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	input, ok := vp.Original.(*events.UserInput)
	if !ok {
		return
	}
	content := strings.TrimSpace(input.Content)
	if content == "" || content[0] != '/' {
		return
	}

	cmd, rest := splitCommand(content)
	cmd = strings.ToLower(cmd)

	// Alias rewrite: "/review pr=123" -> "/mcp.gh.review_pr pr=123"
	if target, ok := p.aliasReverse[cmd]; ok {
		cmd = "/" + target
	}

	p.mu.RLock()
	route, ok := p.promptCommands[cmd]
	p.mu.RUnlock()
	if !ok {
		return
	}

	srv, ok := p.servers[route.server]
	if !ok {
		p.logger.Warn("mcp.client: prompt server vanished", "server", route.server)
		return
	}
	prompt, ok := srv.getPrompt(route.prompt)
	if !ok {
		p.logger.Warn("mcp.client: prompt not found on server", "server", route.server, "prompt", route.prompt)
		return
	}

	args, err := parseSlashArgs(rest, prompt.Arguments)
	if err != nil {
		p.reportSlashError(cmd, err)
		vp.Veto = engine.VetoResult{Vetoed: true, Reason: "mcp prompt argument error"}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), srv.cfg.Timeout)
	defer cancel()

	c := srv.getClient()
	if c == nil {
		p.reportSlashError(cmd, fmt.Errorf("server %q is not connected", route.server))
		vp.Veto = engine.VetoResult{Vetoed: true, Reason: "mcp server disconnected"}
		return
	}

	req := mcp.GetPromptRequest{}
	req.Params.Name = route.prompt
	req.Params.Arguments = args

	resp, err := c.GetPrompt(ctx, req)
	if err != nil {
		p.reportSlashError(cmd, fmt.Errorf("mcp prompts/get: %w", err))
		vp.Veto = engine.VetoResult{Vetoed: true, Reason: "mcp prompts/get failed"}
		return
	}

	preload := p.promptMessagesToPreload(resp.Messages)

	vp.Veto = engine.VetoResult{Vetoed: true, Reason: "consumed by mcp.client prompt"}

	go func() {
		_ = p.bus.Emit("io.input", events.UserInput{
			SchemaVersion:   events.UserInputVersion,
			SessionID:       input.SessionID,
			PreloadMessages: preload,
		})
	}()
}

// promptMessagesToPreload converts MCP PromptMessages into Nexus messages.
// Each MCP message carries a single content block; we map text inline, and
// route image/audio/embedded resources through MessagePart with blob-store
// offload above the inline threshold.
func (p *Plugin) promptMessagesToPreload(msgs []mcp.PromptMessage) []events.Message {
	out := make([]events.Message, 0, len(msgs))
	for _, m := range msgs {
		role := string(m.Role)
		if role == "" {
			role = "user"
		}
		msg := events.Message{Role: role}
		switch c := m.Content.(type) {
		case mcp.TextContent:
			msg.Content = c.Text
		case mcp.ImageContent:
			msg.Parts = []events.MessagePart{p.partFromBase64("image", c.MIMEType, c.Data)}
		case mcp.AudioContent:
			msg.Parts = []events.MessagePart{p.partFromBase64("audio", c.MIMEType, c.Data)}
		case mcp.EmbeddedResource:
			text, parts := p.renderResourceContents([]mcp.ResourceContents{c.Resource})
			msg.Content = text
			msg.Parts = parts
		case mcp.ResourceLink:
			msg.Content = fmt.Sprintf("[resource: %s] %s", c.URI, firstNonEmpty(c.Name, c.URI))
		}
		out = append(out, msg)
	}
	return out
}

// reportSlashError surfaces parse / dispatch failures to the user via the
// usual io.output channel rather than swallowing them.
func (p *Plugin) reportSlashError(cmd string, err error) {
	_ = p.bus.Emit("io.output", events.AgentOutput{
		SchemaVersion: events.AgentOutputVersion,
		Role:          "system",
		Content:       fmt.Sprintf("MCP %s: %v", cmd, err),
	})
}

// splitCommand returns the leading "/word" of an input string plus the
// remainder. Whitespace around the boundary is trimmed.
func splitCommand(content string) (cmd, rest string) {
	for i := 0; i < len(content); i++ {
		if content[i] == ' ' || content[i] == '\t' {
			return content[:i], strings.TrimLeft(content[i+1:], " \t")
		}
	}
	return content, ""
}

// handlePromptsListQuery answers the synchronous mcp.prompts.list query.
func (p *Plugin) handlePromptsListQuery(e engine.Event[any]) {
	q, ok := e.Payload.(*events.MCPPromptsList)
	if !ok {
		return
	}
	q.Prompts = p.listPrompts()
}
