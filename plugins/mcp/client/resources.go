package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/yosida95/uritemplate/v3"

	"github.com/frankbardon/nexus/pkg/events"
)

// refreshResources reconciles the catalog with the server's current
// resources + resource templates. Generic browse/read tools are registered
// once; static resources auto-register up to ResourceConfig.AutoRegisterMax;
// templates always auto-register because the count is typically small and
// each template requires args that the LLM should see.
func (s *server) refreshResources(ctx context.Context) error {
	c := s.getClient()
	if c == nil {
		return fmt.Errorf("not connected")
	}

	resList, err := c.ListResources(ctx, mcp.ListResourcesRequest{})
	if err != nil {
		return fmt.Errorf("resources/list: %w", err)
	}

	tmplList, err := c.ListResourceTemplates(ctx, mcp.ListResourceTemplatesRequest{})
	if err != nil {
		// Some servers don't implement templates; log and continue.
		s.logger.Debug("mcp: resources/templates/list failed", "error", err)
		tmplList = &mcp.ListResourceTemplatesResult{}
	}

	// Always register the generic browse + read tools so an LLM can fall
	// back to dynamic discovery even when auto-registration is off.
	s.registerGenericResourceTools()

	// Static resources.
	if s.cfg.Resources.AutoRegisterStatic {
		s.reconcileStaticResources(ctx, resList.Resources)
	}

	// Templates.
	if s.cfg.Resources.AutoRegisterTemplate {
		s.reconcileTemplates(tmplList.ResourceTemplates)
	}

	return nil
}

func (s *server) registerGenericResourceTools() {
	listName := listResourcesToolName(s.cfg.Name)
	readName := readResourceToolName(s.cfg.Name)

	s.parent.registerToolRoute(listName, s.cfg.Name)
	s.parent.registerToolRoute(readName, s.cfg.Name)

	_ = s.parent.bus.Emit("tool.register", events.ToolDef{
		Name:        listName,
		Description: fmt.Sprintf("List MCP resources exposed by the %q server.", s.cfg.Name),
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Class:    "mcp",
		Subclass: s.cfg.Name,
		Tags:     []string{"mcp", "mcp:" + s.cfg.Name, "mcp:resources"},
	})

	_ = s.parent.bus.Emit("tool.register", events.ToolDef{
		Name:        readName,
		Description: fmt.Sprintf("Read an MCP resource by URI from the %q server.", s.cfg.Name),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"uri": map[string]any{
					"type":        "string",
					"description": "Resource URI to read.",
				},
			},
			"required": []string{"uri"},
		},
		Class:    "mcp",
		Subclass: s.cfg.Name,
		Tags:     []string{"mcp", "mcp:" + s.cfg.Name, "mcp:resources"},
	})
}

func (s *server) reconcileStaticResources(ctx context.Context, resources []mcp.Resource) {
	limit := s.cfg.Resources.AutoRegisterMax
	if limit <= 0 || len(resources) > limit {
		s.logger.Info("mcp: skipping static-resource auto-registration above limit",
			"server", s.cfg.Name, "count", len(resources), "limit", limit)
		s.mu.Lock()
		for slug := range s.staticResources {
			s.parent.unregisterToolRoute(staticResourceToolName(s.cfg.Name, slug))
		}
		s.staticResources = map[string]mcp.Resource{}
		s.resourceURIs = map[string]string{}
		s.mu.Unlock()
		return
	}

	seen := map[string]bool{}
	for _, r := range resources {
		slug := resourceSlug(r.Title, r.Name, r.URI)
		seen[slug] = true
		s.registerStaticResource(slug, r)
		if s.cfg.Resources.SubscribeUpdates {
			subCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
			req := mcp.SubscribeRequest{}
			req.Params.URI = r.URI
			if c := s.getClient(); c != nil {
				if err := c.Subscribe(subCtx, req); err != nil {
					s.logger.Debug("mcp: resources/subscribe failed", "uri", r.URI, "error", err)
				}
			}
			cancel()
		}
	}

	s.mu.Lock()
	for slug := range s.staticResources {
		if !seen[slug] {
			s.parent.unregisterToolRoute(staticResourceToolName(s.cfg.Name, slug))
			delete(s.staticResources, slug)
			delete(s.resourceURIs, slug)
		}
	}
	s.mu.Unlock()
}

func (s *server) registerStaticResource(slug string, r mcp.Resource) {
	s.mu.Lock()
	s.staticResources[slug] = r
	s.resourceURIs[slug] = r.URI
	s.mu.Unlock()

	catalog := staticResourceToolName(s.cfg.Name, slug)
	s.parent.registerToolRoute(catalog, s.cfg.Name)

	desc := firstNonEmpty(r.Description, r.Title, r.Name)
	if desc == "" {
		desc = "Read MCP resource " + r.URI
	}

	_ = s.parent.bus.Emit("tool.register", events.ToolDef{
		Name:        catalog,
		Description: desc,
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Class:    "mcp",
		Subclass: s.cfg.Name,
		Tags:     []string{"mcp", "mcp:" + s.cfg.Name, "mcp:resource"},
	})
}

func (s *server) reconcileTemplates(templates []mcp.ResourceTemplate) {
	seen := map[string]bool{}
	for _, t := range templates {
		slug := resourceSlug(t.Title, t.Name, templateString(t))
		seen[slug] = true
		s.registerTemplate(slug, t)
	}

	s.mu.Lock()
	for slug := range s.templates {
		if !seen[slug] {
			s.parent.unregisterToolRoute(templateResourceToolName(s.cfg.Name, slug))
			delete(s.templates, slug)
		}
	}
	s.mu.Unlock()
}

func (s *server) registerTemplate(slug string, t mcp.ResourceTemplate) {
	s.mu.Lock()
	s.templates[slug] = t
	s.mu.Unlock()

	catalog := templateResourceToolName(s.cfg.Name, slug)
	s.parent.registerToolRoute(catalog, s.cfg.Name)

	props := map[string]any{}
	var required []string
	for _, name := range templateVarNames(t) {
		props[name] = map[string]any{
			"type":        "string",
			"description": fmt.Sprintf("Value for {%s} in the MCP resource URI template.", name),
		}
		required = append(required, name)
	}

	desc := firstNonEmpty(t.Description, t.Title, t.Name)
	if desc == "" {
		desc = "Read MCP resource matching template " + templateString(t)
	}

	def := events.ToolDef{
		Name:        catalog,
		Description: desc,
		Parameters: map[string]any{
			"type":       "object",
			"properties": props,
		},
		Class:    "mcp",
		Subclass: s.cfg.Name,
		Tags:     []string{"mcp", "mcp:" + s.cfg.Name, "mcp:resource_template"},
	}
	if len(required) > 0 {
		def.Parameters["required"] = required
	}
	_ = s.parent.bus.Emit("tool.register", def)
}

// staticResourceURI returns the URI of an auto-registered static resource
// keyed by its catalog tool name, or "", false if no such mapping exists.
func (s *server) staticResourceURI(catalog string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := "mcp__" + s.cfg.Name + "__resource__"
	if len(catalog) <= len(prefix) {
		return "", false
	}
	slug := catalog[len(prefix):]
	uri, ok := s.resourceURIs[slug]
	return uri, ok
}

// templateForCatalog returns the catalog->slug match for a template tool.
func (s *server) templateForCatalog(catalog string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := "mcp__" + s.cfg.Name + "__template__"
	if len(catalog) <= len(prefix) {
		return "", false
	}
	slug := catalog[len(prefix):]
	if _, ok := s.templates[slug]; ok {
		return slug, true
	}
	return "", false
}

// templateString returns the canonical URI-template expression for an MCP
// ResourceTemplate. mcp-go wraps the parsed template inside a URITemplate
// pointer whose String() representation is the original RFC 6570 template.
func templateString(t mcp.ResourceTemplate) string {
	if t.URITemplate == nil || t.URITemplate.Template == nil {
		return ""
	}
	return t.URITemplate.Template.Raw()
}

// templateVarNames extracts variable names from an RFC 6570 URI template.
func templateVarNames(t mcp.ResourceTemplate) []string {
	if t.URITemplate == nil || t.URITemplate.Template == nil {
		return nil
	}
	return t.URITemplate.Template.Varnames()
}

// dispatchListResources answers the generic per-server list tool by sending
// resources/list and rendering the result as a JSON catalog.
func (p *Plugin) dispatchListResources(s *server, tc events.ToolCall) {
	c := s.getClient()
	if c == nil {
		p.emitToolError(tc, "mcp.client: server not connected")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
	defer cancel()

	res, err := c.ListResources(ctx, mcp.ListResourcesRequest{})
	if err != nil {
		p.emitToolError(tc, fmt.Sprintf("mcp resources/list: %v", err))
		return
	}

	entries := make([]map[string]any, 0, len(res.Resources))
	for _, r := range res.Resources {
		entry := map[string]any{
			"uri":  r.URI,
			"name": r.Name,
		}
		if r.Title != "" {
			entry["title"] = r.Title
		}
		if r.Description != "" {
			entry["description"] = r.Description
		}
		if r.MIMEType != "" {
			entry["mime_type"] = r.MIMEType
		}
		entries = append(entries, entry)
	}

	output := jsonString(map[string]any{"resources": entries})
	p.emitToolResult(events.ToolResult{
		SchemaVersion:    events.ToolResultVersion,
		ID:               tc.ID,
		Name:             tc.Name,
		Output:           output,
		OutputStructured: map[string]any{"resources": entries},
		TurnID:           tc.TurnID,
	})
}

func (p *Plugin) dispatchReadResource(s *server, tc events.ToolCall) {
	uri, _ := tc.Arguments["uri"].(string)
	if uri == "" {
		p.emitToolError(tc, "mcp.client: read_resource requires uri argument")
		return
	}
	p.dispatchReadResourceURI(s, tc, uri)
}

func (p *Plugin) dispatchReadResourceURI(s *server, tc events.ToolCall, uri string) {
	c := s.getClient()
	if c == nil {
		p.emitToolError(tc, "mcp.client: server not connected")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
	defer cancel()

	req := mcp.ReadResourceRequest{}
	req.Params.URI = uri
	res, err := c.ReadResource(ctx, req)
	if err != nil {
		p.emitToolError(tc, fmt.Sprintf("mcp resources/read: %v", err))
		return
	}

	text, parts := p.renderResourceContents(res.Contents)
	p.emitToolResult(events.ToolResult{
		SchemaVersion: events.ToolResultVersion,
		ID:            tc.ID,
		Name:          tc.Name,
		Output:        text,
		OutputParts:   parts,
		TurnID:        tc.TurnID,
	})
}

func (p *Plugin) dispatchTemplateResource(s *server, tc events.ToolCall, slug string) {
	s.mu.Lock()
	tmpl, ok := s.templates[slug]
	s.mu.Unlock()
	if !ok {
		p.emitToolError(tc, "mcp.client: unknown resource template")
		return
	}

	args := map[string]string{}
	for _, name := range templateVarNames(tmpl) {
		v, _ := tc.Arguments[name].(string)
		args[name] = v
	}
	uri, err := expandURITemplate(tmpl, args)
	if err != nil {
		p.emitToolError(tc, fmt.Sprintf("mcp.client: expand template: %v", err))
		return
	}
	p.dispatchReadResourceURI(s, tc, uri)
}

func (p *Plugin) renderResourceContents(contents []mcp.ResourceContents) (string, []events.MessagePart) {
	var text string
	var parts []events.MessagePart
	for _, rc := range contents {
		switch v := rc.(type) {
		case mcp.TextResourceContents:
			if text != "" {
				text += "\n"
			}
			text += v.Text
		case mcp.BlobResourceContents:
			data, err := base64.StdEncoding.DecodeString(v.Blob)
			if err != nil {
				if text != "" {
					text += "\n"
				}
				text += "[mcp.client: invalid blob payload: " + err.Error() + "]"
				continue
			}
			part := events.MessagePart{Type: classifyMime(v.MIMEType), MimeType: v.MIMEType}
			if p.blobs != nil && int64(len(data)) > defaultBlobInlineCutoff {
				h, err := p.blobs.Put(data, v.MIMEType)
				if err == nil {
					part.URI = h.URI()
					parts = append(parts, part)
					continue
				}
				p.logger.Warn("mcp.client: blob put failed, inlining", "error", err)
			}
			part.Data = data
			parts = append(parts, part)
		}
	}
	return text, parts
}

// classifyMime maps a MIME type to the events.MessagePart Type the
// providers expect ("image", "audio", "video", "file", or "text").
func classifyMime(mime string) string {
	switch {
	case startsWith(mime, "image/"):
		return "image"
	case startsWith(mime, "audio/"):
		return "audio"
	case startsWith(mime, "video/"):
		return "video"
	default:
		return "file"
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// expandURITemplate fills the template's variables with the supplied
// arguments using the SDK's underlying RFC 6570 implementation.
func expandURITemplate(t mcp.ResourceTemplate, args map[string]string) (string, error) {
	if t.URITemplate == nil || t.URITemplate.Template == nil {
		return "", fmt.Errorf("template has no expression")
	}
	values := uritemplate.Values{}
	for k, v := range args {
		values.Set(k, uritemplate.String(v))
	}
	return t.URITemplate.Template.Expand(values)
}

// jsonString marshals a payload with stable formatting for tool output.
func jsonString(v any) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(data)
}
