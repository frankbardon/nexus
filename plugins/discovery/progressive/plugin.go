// Package progressive implements hierarchical tool discovery. Tools and skills
// are organized into class/subclass taxonomy. The LLM sees class-level summaries
// and drills into specific classes via a "discover" meta-tool.
package progressive

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

const (
	pluginID   = "nexus.discovery.progressive"
	pluginName = "Progressive Discovery"
	version    = "0.1.0"
)

// classInfo holds grouped tools for a single class.
type classInfo struct {
	Name        string
	Subclasses  map[string][]events.ToolDef // subclass -> tools (empty string key for unsubclassed)
	Description string                      // auto-generated from member tools
}

func (c *classInfo) toolCount() int {
	n := 0
	for _, tools := range c.Subclasses {
		n += len(tools)
	}
	return n
}

func (c *classInfo) subclassNames() []string {
	var names []string
	for sc := range c.Subclasses {
		if sc != "" {
			names = append(names, sc)
		}
	}
	sort.Strings(names)
	return names
}

// allTools returns all tools in the class, across subclasses.
func (c *classInfo) allTools() []events.ToolDef {
	var all []events.ToolDef
	for _, tools := range c.Subclasses {
		all = append(all, tools...)
	}
	return all
}

// Plugin implements progressive tool discovery.
type Plugin struct {
	bus    engine.EventBus
	logger *slog.Logger
	unsubs []func()

	mu sync.RWMutex

	// Taxonomy: class name -> classInfo.
	classes map[string]*classInfo

	// All registered tools (raw, before classification).
	allTools []events.ToolDef

	// Tools classified as "special" (always revealed).
	specialTools []events.ToolDef

	// Tools without a class.
	classlessTools []events.ToolDef

	// Revealed tools per scope tracking.
	// Key: class name (or "class.subclass"), Value: true if revealed.
	revealed map[string]bool

	// Idle tracking for hybrid scope: class -> turns since last use.
	idleTurns map[string]int

	// Config.
	scope             string   // "session", "turn", "hybrid"
	idlePruneTurns    int      // hybrid only
	classlessBehavior string   // "include" or "exclude"
	alwaysInclude     []string // classes always fully revealed
	defaultDepth      string   // "class" or "full"
}

// New creates a new progressive discovery plugin.
func New() engine.Plugin {
	return &Plugin{
		classes:           make(map[string]*classInfo),
		revealed:          make(map[string]bool),
		idleTurns:         make(map[string]int),
		scope:             "session",
		idlePruneTurns:    5,
		classlessBehavior: "include",
		defaultDepth:      "class",
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

	// Parse config.
	if v, ok := ctx.Config["scope"].(string); ok {
		switch v {
		case "session", "turn", "hybrid":
			p.scope = v
		default:
			return fmt.Errorf("progressive discovery: invalid scope %q", v)
		}
	}
	if v, ok := ctx.Config["idle_prune_turns"].(int); ok {
		p.idlePruneTurns = v
	}
	if v, ok := ctx.Config["classless_behavior"].(string); ok {
		switch v {
		case "include", "exclude":
			p.classlessBehavior = v
		default:
			return fmt.Errorf("progressive discovery: invalid classless_behavior %q", v)
		}
	}
	if v, ok := ctx.Config["always_include"].([]any); ok {
		for _, item := range v {
			if s, ok := item.(string); ok {
				p.alwaysInclude = append(p.alwaysInclude, s)
			}
		}
	}
	if v, ok := ctx.Config["default_depth"].(string); ok {
		switch v {
		case "class", "full":
			p.defaultDepth = v
		default:
			return fmt.Errorf("progressive discovery: invalid default_depth %q", v)
		}
	}

	// Subscribe to tool registration.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.register", p.handleToolRegister,
			engine.WithPriority(5), engine.WithSource(pluginID)),
	)

	// Intercept LLM requests to replace tool list.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("before:llm.request", p.handleBeforeLLMRequest,
			engine.WithPriority(8), engine.WithSource(pluginID)),
	)

	// Handle discover tool invocations.
	p.unsubs = append(p.unsubs,
		p.bus.Subscribe("tool.invoke", p.handleToolInvoke,
			engine.WithPriority(40), engine.WithSource(pluginID)),
	)

	return nil
}

func (p *Plugin) Ready() error { return nil }

func (p *Plugin) Shutdown(_ context.Context) error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	return nil
}

func (p *Plugin) Subscriptions() []engine.EventSubscription {
	return []engine.EventSubscription{
		{EventType: "tool.register", Priority: 5},
		{EventType: "before:llm.request", Priority: 8},
		{EventType: "tool.invoke", Priority: 40},
	}
}

func (p *Plugin) Emissions() []string {
	return []string{
		"before:tool.result",
		"tool.result",
	}
}

// handleToolRegister collects and classifies tools as they register.
func (p *Plugin) handleToolRegister(event engine.Event[any]) {
	td, ok := event.Payload.(events.ToolDef)
	if !ok {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.allTools = append(p.allTools, td)

	// Classify by tier.
	if p.isSpecialClass(td.Class) {
		p.specialTools = append(p.specialTools, td)
		p.logger.Debug("tool registered as special", "name", td.Name, "class", td.Class)
		return
	}

	if td.Class == "" {
		p.classlessTools = append(p.classlessTools, td)
		p.logger.Debug("tool registered as classless", "name", td.Name)
		return
	}

	// Classified tool — group by class/subclass.
	ci, exists := p.classes[td.Class]
	if !exists {
		ci = &classInfo{
			Name:       td.Class,
			Subclasses: make(map[string][]events.ToolDef),
		}
		p.classes[td.Class] = ci
	}
	ci.Subclasses[td.Subclass] = append(ci.Subclasses[td.Subclass], td)

	// Regenerate class description from member tools.
	ci.Description = generateClassDescription(ci)

	p.logger.Debug("tool registered as classified",
		"name", td.Name, "class", td.Class, "subclass", td.Subclass)
}

// handleBeforeLLMRequest replaces the full tool list with class summaries.
func (p *Plugin) handleBeforeLLMRequest(event engine.Event[any]) {
	vp, ok := event.Payload.(*engine.VetoablePayload)
	if !ok {
		return
	}
	req, ok := vp.Original.(*events.LLMRequest)
	if !ok {
		return
	}

	// Skip internal/gate requests.
	if src, _ := req.Metadata["_source"].(string); src != "" {
		return
	}

	// full depth = legacy behavior, pass through.
	if p.defaultDepth == "full" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Turn-scoped: reset revealed set each turn.
	if p.scope == "turn" {
		p.revealed = make(map[string]bool)
	}

	// Hybrid: increment idle counters, prune stale reveals.
	if p.scope == "hybrid" {
		for cls := range p.revealed {
			p.idleTurns[cls]++
			if p.idleTurns[cls] > p.idlePruneTurns {
				delete(p.revealed, cls)
				delete(p.idleTurns, cls)
				p.logger.Debug("pruned idle class", "class", cls)
			}
		}
	}

	// Build replacement tool list.
	var tools []events.ToolDef

	// 1. Special-tier: always included.
	tools = append(tools, p.specialTools...)

	// 2. Classless-tier: per config.
	if p.classlessBehavior == "include" {
		tools = append(tools, p.classlessTools...)
	}

	// 3. Always-include classes: fully revealed.
	for _, cls := range p.alwaysInclude {
		if ci, ok := p.classes[cls]; ok {
			tools = append(tools, ci.allTools()...)
		}
	}

	// 4. Already-revealed classes: include their tools.
	for key := range p.revealed {
		parts := strings.SplitN(key, ".", 2)
		cls := parts[0]
		if p.isAlwaysIncluded(cls) {
			continue // already added above
		}
		ci, ok := p.classes[cls]
		if !ok {
			continue
		}
		if len(parts) == 2 {
			// Subclass-specific reveal.
			sc := parts[1]
			tools = append(tools, ci.Subclasses[sc]...)
		} else {
			// Full class reveal.
			tools = append(tools, ci.allTools()...)
		}
	}

	// 4.5. Tool-choice mandate: if the request demands a specific tool,
	// force-include it (and mark its class revealed for peer visibility on
	// subsequent iterations). Without this, discovery silently strips the
	// demanded tool and the provider downgrades tool_choice to "required",
	// violating the caller's contract.
	if req.ToolChoice != nil && req.ToolChoice.Mode == "tool" && req.ToolChoice.Name != "" {
		tools = p.ensureToolVisible(req.ToolChoice.Name, tools)
	}

	// 5. Add discover meta-tool if there are undiscovered classes.
	hasUndiscovered := p.hasUndiscoveredClasses()
	if hasUndiscovered {
		tools = append(tools, p.discoverToolDef())
	}

	// 6. Inject class summaries into system prompt.
	if hasUndiscovered {
		summaryXML := p.buildClassSummaryXML()
		if summaryXML != "" {
			p.injectSystemPrompt(req, summaryXML)
		}
	}

	req.Tools = tools
}

// handleToolInvoke handles the "discover" meta-tool and keeps idle counters
// fresh for hybrid scope. Every non-discover invocation resets the counter
// for whichever revealed class/subclass owns the tool — otherwise hybrid
// would prune classes mid-use because only `discover` calls refreshed them.
func (p *Plugin) handleToolInvoke(event engine.Event[any]) {
	tc, ok := event.Payload.(events.ToolCall)
	if !ok {
		return
	}
	if tc.Name != "discover" {
		if p.scope == "hybrid" {
			p.touchIdleForTool(tc.Name)
		}
		return
	}

	className, _ := tc.Arguments["class"].(string)
	subclass, _ := tc.Arguments["subclass"].(string)

	if className == "" {
		p.emitResult(tc, "", "class argument is required")
		return
	}

	p.mu.Lock()
	ci, exists := p.classes[className]
	if !exists {
		p.mu.Unlock()
		p.emitResult(tc, "", fmt.Sprintf("unknown class %q", className))
		return
	}

	var revealed []events.ToolDef
	if subclass != "" {
		tools, ok := ci.Subclasses[subclass]
		if !ok {
			p.mu.Unlock()
			p.emitResult(tc, "", fmt.Sprintf("unknown subclass %q in class %q", subclass, className))
			return
		}
		revealed = tools
		p.revealed[className+"."+subclass] = true
		p.idleTurns[className+"."+subclass] = 0
	} else {
		revealed = ci.allTools()
		p.revealed[className] = true
		p.idleTurns[className] = 0
	}
	p.mu.Unlock()

	// Build response with full tool definitions.
	output := p.buildDiscoverResponse(className, subclass, revealed)
	p.emitResult(tc, output, "")
}

// injectSystemPrompt appends discovery class summaries to the system message
// in the LLM request. If no system message exists, one is prepended.
func (p *Plugin) injectSystemPrompt(req *events.LLMRequest, xml string) {
	section := "\n\n" + engine.XMLWrap("tool_discovery",
		"Use the `discover` tool to inspect a class and reveal its tools.\n\n"+xml)

	for i := range req.Messages {
		if req.Messages[i].Role == "system" {
			req.Messages[i].Content += section
			return
		}
	}
	// No system message — prepend one.
	msg := events.Message{Role: "system", Content: strings.TrimPrefix(section, "\n\n")}
	req.Messages = append([]events.Message{msg}, req.Messages...)
}

// discoverToolDef returns the ToolDef for the discover meta-tool.
func (p *Plugin) discoverToolDef() events.ToolDef {
	return events.ToolDef{
		Name:        "discover",
		Description: "Inspect available tool classes to reveal their tools. Call with a class name to see all tools in that class. Optionally specify a subclass to narrow results.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"class": map[string]any{
					"type":        "string",
					"description": "The tool class to inspect (e.g. \"filesystem\", \"memory\")",
				},
				"subclass": map[string]any{
					"type":        "string",
					"description": "Optional subclass within the class (e.g. \"read\", \"write\")",
				},
			},
			"required": []string{"class"},
		},
	}
}

// buildClassSummaryXML generates the <tool_classes> XML for system prompt injection.
func (p *Plugin) buildClassSummaryXML() string {
	// Collect undiscovered classes.
	type classSummary struct {
		name string
		info *classInfo
	}

	var summaries []classSummary
	for name, ci := range p.classes {
		if p.isAlwaysIncluded(name) {
			continue
		}
		if p.revealed[name] {
			continue
		}
		// Check if all subclasses are revealed individually.
		allRevealed := true
		for sc := range ci.Subclasses {
			key := name
			if sc != "" {
				key = name + "." + sc
			}
			if !p.revealed[key] {
				allRevealed = false
				break
			}
		}
		if allRevealed {
			continue
		}
		summaries = append(summaries, classSummary{name: name, info: ci})
	}

	if len(summaries) == 0 {
		return ""
	}

	// Sort for deterministic output.
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].name < summaries[j].name
	})

	var b strings.Builder
	b.WriteString("<tool_classes>\n")
	for _, s := range summaries {
		fmt.Fprintf(&b, `  <class name=%q tools="%d"`, s.name, s.info.toolCount())
		if subs := s.info.subclassNames(); len(subs) > 0 {
			fmt.Fprintf(&b, ` subclasses="%s"`, strings.Join(subs, ", "))
		}
		b.WriteString(">\n")
		fmt.Fprintf(&b, "    %s\n", s.info.Description)
		b.WriteString("  </class>\n")
	}
	b.WriteString("</tool_classes>")
	return b.String()
}

// buildDiscoverResponse formats revealed tools as JSON for the discover result.
func (p *Plugin) buildDiscoverResponse(class, subclass string, tools []events.ToolDef) string {
	type toolSummary struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
		Subclass    string         `json:"subclass,omitempty"`
	}

	var summaries []toolSummary
	for _, t := range tools {
		summaries = append(summaries, toolSummary{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
			Subclass:    t.Subclass,
		})
	}

	result := map[string]any{
		"class": class,
		"tools": summaries,
	}
	if subclass != "" {
		result["subclass"] = subclass
	}

	data, _ := json.Marshal(result)
	return string(data)
}

// hasUndiscoveredClasses returns true if any classes are not yet revealed.
func (p *Plugin) hasUndiscoveredClasses() bool {
	for name, ci := range p.classes {
		if p.isAlwaysIncluded(name) {
			continue
		}
		if p.revealed[name] {
			continue
		}
		// Check partial reveals.
		allRevealed := true
		for sc := range ci.Subclasses {
			key := name
			if sc != "" {
				key = name + "." + sc
			}
			if !p.revealed[key] {
				allRevealed = false
				break
			}
		}
		if !allRevealed {
			return true
		}
	}
	return false
}

// touchIdleForTool resets the idle counter for whatever class/subclass owns
// the named tool, if that class/subclass is currently revealed. No-op for
// classless tools, special tools, unknown names, or entries not yet
// revealed. Called on every non-discover tool.invoke in hybrid scope.
func (p *Plugin) touchIdleForTool(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range p.allTools {
		if t.Name != name || t.Class == "" {
			continue
		}
		if p.isAlwaysIncluded(t.Class) {
			return
		}
		if t.Subclass != "" {
			key := t.Class + "." + t.Subclass
			if p.revealed[key] {
				p.idleTurns[key] = 0
			}
		}
		if p.revealed[t.Class] {
			p.idleTurns[t.Class] = 0
		}
		return
	}
}

// ensureToolVisible guarantees the named tool is present in the outgoing
// list. If the tool belongs to a class, that class (or subclass) is marked
// revealed so peers show up on subsequent iterations. No-op if the tool is
// already in the list or is not registered. Caller must hold p.mu.
func (p *Plugin) ensureToolVisible(name string, tools []events.ToolDef) []events.ToolDef {
	for _, t := range tools {
		if t.Name == name {
			return tools
		}
	}
	for _, t := range p.allTools {
		if t.Name != name {
			continue
		}
		tools = append(tools, t)
		if t.Class != "" && !p.isAlwaysIncluded(t.Class) {
			key := t.Class
			if t.Subclass != "" {
				key = t.Class + "." + t.Subclass
			}
			p.revealed[key] = true
			p.idleTurns[key] = 0
			p.logger.Debug("force-revealed via tool_choice mandate",
				"tool", name, "class", t.Class, "subclass", t.Subclass)
		}
		return tools
	}
	return tools
}

// isSpecialClass returns true if the class is in the always_include list
// and marked as a special-tier category.
func (p *Plugin) isSpecialClass(class string) bool {
	// Special categories bypass discovery entirely.
	return class == "communication" || class == "agents"
}

// isAlwaysIncluded returns true if a class is in the always_include config.
func (p *Plugin) isAlwaysIncluded(class string) bool {
	return slices.Contains(p.alwaysInclude, class)
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

// generateClassDescription creates a summary from member tool descriptions.
func generateClassDescription(ci *classInfo) string {
	// Collect unique description fragments.
	var descriptions []string
	seen := make(map[string]bool)
	for _, tools := range ci.Subclasses {
		for _, t := range tools {
			// Take first sentence of each tool's description.
			desc := firstSentence(t.Description)
			lower := strings.ToLower(desc)
			if !seen[lower] {
				seen[lower] = true
				descriptions = append(descriptions, desc)
			}
		}
	}

	if len(descriptions) == 0 {
		return fmt.Sprintf("Tools in the %s class.", ci.Name)
	}

	// Join with semicolons, capitalize first letter.
	joined := strings.Join(descriptions, "; ")
	if len(joined) > 200 {
		// Truncate long descriptions.
		joined = joined[:197] + "..."
	}
	return joined
}

// firstSentence extracts the first sentence from a string.
func firstSentence(s string) string {
	if idx := strings.Index(s, ". "); idx != -1 {
		return s[:idx+1]
	}
	if strings.HasSuffix(s, ".") {
		return s
	}
	return s + "."
}
