package skills

import (
	"context"
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
		trustProject:       "ask",
		maxActiveSkills:    10,
		catalogInSysPrompt: true,
		disabledSkills:     make(map[string]bool),
	}
}

func (p *Plugin) ID() string             { return pluginID }
func (p *Plugin) Name() string           { return "Skills" }
func (p *Plugin) Version() string        { return "0.1.0" }
func (p *Plugin) Dependencies() []string { return nil }

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "core.boot", Priority: 10},
		{EventType: "skill.activate", Priority: 50},
		{EventType: "skill.resource.read", Priority: 50},
		{EventType: "skill.deactivate", Priority: 50},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"skill.discover",
		"skill.loaded",
		"skill.resource.result",
		"before:skill.activate",
	}
}

func (p *Plugin) Init(ctx engine.PluginContext) error {
	p.bus = ctx.Bus
	p.logger = ctx.Logger
	p.prompts = ctx.Prompts

	// Read config.
	if v, ok := ctx.Config["scan_paths"]; ok {
		if paths, ok := v.([]string); ok {
			p.scanPaths = paths
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
		if list, ok := v.([]string); ok {
			for _, name := range list {
				p.disabledSkills[name] = true
			}
		}
	}

	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("core.boot", p.handleBoot, engine.WithPriority(10), engine.WithSource(pluginID)),
		p.bus.Subscribe("skill.activate", p.handleActivate, engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("skill.resource.read", p.handleResourceRead, engine.WithPriority(50), engine.WithSource(pluginID)),
		p.bus.Subscribe("skill.deactivate", p.handleDeactivate, engine.WithPriority(50), engine.WithSource(pluginID)),
	)

	if p.catalogInSysPrompt && p.prompts != nil {
		p.prompts.Register(pluginID, 50, p.buildCatalogSection)
	}

	p.logger.Info("skills plugin initialized")
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
	cwd, _ := os.Getwd()
	paths := append(DefaultScanPaths(cwd), p.scanPaths...)

	records := ScanForSkills(paths, p.logger)

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
	p.mu.Unlock()

	p.logger.Info("skill deactivated", "name", ref.Name)
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
