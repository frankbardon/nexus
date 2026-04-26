package skills

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const pluginID = "nexus.skills"

// Plugin manages skill discovery, activation, and resource access.
type Plugin struct {
	bus     engine.EventBus
	logger  *slog.Logger
	prompts *engine.PromptRegistry

	mu           sync.RWMutex
	catalog      []SkillRecord
	activeSkills map[string]bool // name -> active
	skillsByName map[string]*SkillRecord
	skillSchemas map[string]string // skill name -> schema registry name (only skills with output_schema)
	unsubs       []func()

	// Config fields.
	scanPaths          []string
	trustProject       string // "ask", "always", "never"
	maxActiveSkills    int
	catalogInSysPrompt bool
	disabledSkills     map[string]bool
}

// New creates a new skills plugin.
func New() engine.Plugin {
	return &Plugin{
		activeSkills:       make(map[string]bool),
		skillsByName:       make(map[string]*SkillRecord),
		skillSchemas:       make(map[string]string),
		trustProject:       "ask",
		maxActiveSkills:    10,
		catalogInSysPrompt: true,
		disabledSkills:     make(map[string]bool),
	}
}

func (p *Plugin) ID() string                        { return pluginID }
func (p *Plugin) Name() string                      { return "Skills" }
func (p *Plugin) Version() string                   { return "0.1.0" }
func (p *Plugin) Dependencies() []string            { return nil }
func (p *Plugin) Requires() []engine.Requirement    { return nil }
func (p *Plugin) Capabilities() []engine.Capability { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "core.boot", Priority: 10},
		{EventType: "skill.activate", Priority: 50},
		{EventType: "skill.resource.read", Priority: 50},
		{EventType: "skill.deactivate", Priority: 50},
		{EventType: "before:llm.request", Priority: 15}, // after schema registry (5), before gates (10 on other events)
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"skill.discover",
		"skill.loaded",
		"skill.resource.result",
		"before:skill.activate",
		"schema.register",
		"schema.deregister",
	}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.prompts = ctx.Prompts

	// Read config. YAML decoding lands list values as []any, not []string,
	// so we coerce element-by-element here. The shorter v.([]string) form
	// silently drops the value at every active config and was a subtle
	// boot-time footgun.
	if v, ok := ctx.Config["scan_paths"]; ok {
		raw := coerceStringSlice(v)
		p.scanPaths = make([]string, 0, len(raw))
		for _, path := range raw {
			p.scanPaths = append(p.scanPaths, engine.ExpandPath(path))
		}
	}
	if v, ok := ctx.Config["trust_project"]; ok {
		if s, ok := v.(string); ok {
			p.trustProject = s
		}
	}
	if v, ok := ctx.Config["max_active_skills"]; ok {
		if n, ok := v.(int); ok && n > 0 {
			p.maxActiveSkills = n
		}
	}
	if v, ok := ctx.Config["catalog_in_system_prompt"]; ok {
		if b, ok := v.(bool); ok {
			p.catalogInSysPrompt = b
		}
	}
	if v, ok := ctx.Config["disabled_skills"]; ok {
		for _, name := range coerceStringSlice(v) {
			p.disabledSkills[name] = true
		}
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("core.boot", p.handleBoot, engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("skill.activate", p.handleActivate, engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("skill.resource.read", p.handleResourceRead, engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("skill.deactivate", p.handleDeactivate, engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest, engine.WithPriority(15), engine.WithSource(pluginID)),
	)

	if p.catalogInSysPrompt && p.prompts != nil {
		p.prompts.Register(pluginID, 50, p.buildCatalogSection)
	}

	p.logger.Info("skills plugin initialized")
	return nil
}

// coerceStringSlice handles the YAML→map[string]any quirk: a YAML list of
// strings decodes to []any, not []string. Accepts either form (or a single
// string) and silently skips non-string elements.
func coerceStringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	}
	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	if p.prompts != nil {
		p.prompts.Unregister(pluginID)
	}
	return nil
}

func (p *Plugin) handleBoot(_ engine.Event[any]) {
	if len(p.scanPaths) == 0 {
		p.logger.Info("skills plugin: no scan_paths configured, skipping discovery")
		_ = p.bus.Emit("skill.discover", events.SkillCatalog{Skills: nil})
		return
	}

	records := ScanForSkills(p.scanPaths, p.logger)

	p.mu.Lock()
	p.catalog = nil
	for i := range records {
		r := &records[i]
		if p.disabledSkills[r.Name] {
			p.logger.Info("skill disabled by config", "name", r.Name)
			continue
		}
		p.catalog = append(p.catalog, *r)
		p.skillsByName[r.Name] = r
	}
	p.mu.Unlock()

	// Emit discovery event with catalog summary.
	summaries := make([]events.SkillSummary, len(p.catalog))
	for i, r := range p.catalog {
		summaries[i] = events.SkillSummary{
			Name:        r.Name,
			Description: r.Description,
			Location:    r.Location,
			Scope:       r.Scope,
			Class:       r.Class,
			Subclass:    r.Subclass,
		}
	}
	_ = p.bus.Emit("skill.discover", events.SkillCatalog{Skills: summaries})
	p.logger.Info("skill discovery complete", "count", len(p.catalog))
}

func (p *Plugin) buildCatalogSection() string {
	p.mu.RLock()
	catalog := p.catalog
	p.mu.RUnlock()

	return BuildCatalogXML(catalog)
}

func (p *Plugin) handleActivate(e engine.Event[any]) {
	activation, ok := e.Payload.(events.SkillActivation)
	if !ok {
		return
	}

	p.mu.Lock()
	// Check if already active.
	if p.activeSkills[activation.Name] {
		p.mu.Unlock()
		p.logger.Debug("skill already active", "name", activation.Name)
		return
	}

	// Check max active limit.
	if len(p.activeSkills) >= p.maxActiveSkills {
		p.mu.Unlock()
		p.logger.Warn("max active skills reached", "name", activation.Name, "max", p.maxActiveSkills)
		return
	}

	record, ok := p.skillsByName[activation.Name]
	if !ok {
		p.mu.Unlock()
		p.logger.Warn("skill not found in catalog", "name", activation.Name)
		return
	}
	p.mu.Unlock()

	// Emit vetoable before:skill.activate event.
	result, err := p.bus.EmitVetoable("before:skill.activate", &activation)
	if err != nil {
		p.logger.Error("error emitting before:skill.activate", "error", err)
		return
	}
	if result.Vetoed {
		p.logger.Info("skill activation vetoed", "name", activation.Name, "reason", result.Reason)
		return
	}

	p.mu.Lock()
	p.activeSkills[activation.Name] = true
	p.mu.Unlock()

	// Load and register output schema if declared.
	schema := p.resolveOutputSchema(record)
	if schema != nil {
		schemaName := "skill." + record.Name + ".output"
		p.mu.Lock()
		p.skillSchemas[record.Name] = schemaName
		p.mu.Unlock()

		_ = p.bus.Emit("schema.register", events.SchemaRegistration{
			Name:   schemaName,
			Schema: schema,
			Source: pluginID,
		})
		p.logger.Info("registered output schema for skill", "name", record.Name, "schema", schemaName)
	}

	// Build content XML and emit skill.loaded.
	contentXML := BuildSkillContentXML(*record)
	_ = p.bus.Emit("skill.loaded", events.SkillContent{
		Name:      record.Name,
		Body:      contentXML,
		Scope:     record.Scope,
		BaseDir:   record.BaseDir,
		Resources: func() []string { r, _ := ListResources(record.BaseDir); return r }(),
	})

	p.logger.Info("skill activated", "name", activation.Name)
}

func (p *Plugin) handleResourceRead(e engine.Event[any]) {
	req, ok := e.Payload.(events.SkillResourceReq)
	if !ok {
		return
	}

	p.mu.RLock()
	record, ok := p.skillsByName[req.SkillName]
	p.mu.RUnlock()
	if !ok {
		p.logger.Warn("skill not found for resource read", "skill", req.SkillName)
		return
	}

	// Resolve path relative to skill base directory.
	fullPath := filepath.Join(record.BaseDir, req.Path)

	// Safety: ensure the resolved path is within the skill directory.
	absBase, _ := filepath.Abs(record.BaseDir)
	absPath, _ := filepath.Abs(fullPath)
	if len(absPath) < len(absBase) || absPath[:len(absBase)] != absBase {
		p.logger.Warn("resource path escapes skill directory",
			"skill", req.SkillName, "path", req.Path)
		return
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		p.logger.Warn("failed to read skill resource",
			"skill", req.SkillName, "path", req.Path, "error", err)
		return
	}

	_ = p.bus.Emit("skill.resource.result", events.SkillResourceData{
		SkillName: req.SkillName,
		Path:      req.Path,
		Content:   data,
		MimeType:  inferMimeType(req.Path),
	})
}

func (p *Plugin) handleDeactivate(e engine.Event[any]) {
	ref, ok := e.Payload.(events.SkillRef)
	if !ok {
		return
	}

	p.mu.Lock()
	delete(p.activeSkills, ref.Name)
	schemaName, hasSchema := p.skillSchemas[ref.Name]
	if hasSchema {
		delete(p.skillSchemas, ref.Name)
	}
	p.mu.Unlock()

	if hasSchema {
		_ = p.bus.Emit("schema.deregister", events.SchemaDeregistration{
			Name:   schemaName,
			Source: pluginID,
		})
		p.logger.Info("deregistered output schema for skill", "name", ref.Name)
	}

	p.logger.Info("skill deactivated", "name", ref.Name)
}

// handleBeforeLLMRequest tags LLM requests with _expects_schema when an active
// skill has a registered output schema.
func (p *Plugin) handleBeforeLLMRequest(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	req, ok := vp.Original.(*events.LLMRequest)
	if !ok {
		return
	}

	// Don't tag if already set.
	if req.ResponseFormat != nil {
		return
	}
	if _, tagged := req.Metadata["_expects_schema"]; tagged {
		return
	}

	// Find active skill with schema. If multiple active, first match wins.
	p.mu.RLock()
	var schemaName string
	for skillName, active := range p.activeSkills {
		if active {
			if sn, ok := p.skillSchemas[skillName]; ok {
				schemaName = sn
				break
			}
		}
	}
	p.mu.RUnlock()

	if schemaName == "" {
		return
	}

	if req.Metadata == nil {
		req.Metadata = make(map[string]any)
	}
	req.Metadata["_expects_schema"] = schemaName
}

// resolveOutputSchema loads the output schema for a skill record.
// Inline output_schema takes precedence over output_schema_file.
func (p *Plugin) resolveOutputSchema(record *SkillRecord) map[string]any {
	if record.OutputSchema != nil {
		return record.OutputSchema
	}

	if record.OutputSchemaFile == "" {
		return nil
	}

	// Resolve path relative to skill directory.
	schemaPath := record.OutputSchemaFile
	if !filepath.IsAbs(schemaPath) {
		schemaPath = filepath.Join(record.BaseDir, schemaPath)
	}

	data, err := os.ReadFile(schemaPath)
	if err != nil {
		p.logger.Warn("failed to read output_schema_file",
			"skill", record.Name, "path", schemaPath, "error", err)
		return nil
	}

	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		p.logger.Warn("failed to parse output_schema_file",
			"skill", record.Name, "path", schemaPath, "error", err)
		return nil
	}

	return schema
}

func inferMimeType(path string) string {
	ext := filepath.Ext(path)
	switch ext {
	case ".md":
		return "text/markdown"
	case ".txt":
		return "text/plain"
	case ".json":
		return "application/json"
	case ".yaml", ".yml":
		return "text/yaml"
	case ".sh":
		return "application/x-sh"
	case ".py":
		return "text/x-python"
	case ".go":
		return "text/x-go"
	default:
		return "application/octet-stream"
	}
}
