# Prompt Registry

The prompt registry allows plugins to inject dynamic sections into the system prompt at runtime. This is how skills catalogs, dynamic variables, and other context get appended to the agent's system prompt without hardcoding.

## How It Works

1. Plugins register **prompt sections** during initialization, each with a name and priority
2. When the LLM provider builds a request, it calls `prompts.Apply(systemPrompt)` to assemble the final prompt
3. Each registered section function is called — if it returns a non-empty string, that content is wrapped in an XML `<prompt_section>` tag and appended
4. Sections are appended in priority order (lower priority numbers first)
5. If a base system prompt is provided, it's wrapped in `<system_instructions>` tags

## XML Structure

All prompt content uses XML tag boundaries for clean structural separation. The assembled prompt looks like:

```xml
<system_instructions>
You are a helpful assistant.
</system_instructions>

<prompt_section name="skill-catalog">
<available_skills>
  <skill name="code-review" scope="project">
    <description>Review code for quality, bugs, security issues, and style.</description>
  </skill>
</available_skills>
</prompt_section>

<prompt_section name="dynvars">
- Date: 2026-04-08
- OS: darwin
- CWD: /Users/frank/projects/myapp
</prompt_section>
```

## Registering a Section

```go
func (p *MyPlugin) Init(ctx engine.PluginContext) error {
    ctx.Prompts.Register("my-context", 50, func() string {
        return "Some dynamic information here."
    })
    return nil
}
```

The function is called every time a prompt is assembled, so it can return different content based on current state. The returned content is automatically wrapped in `<prompt_section name="my-context">` tags by the registry.

## Built-in Sections

| Plugin | Section Name | Priority | Content |
|--------|-------------|----------|---------|
| `nexus.system.dynvars` | `dynvars` | 90 | Current date, time, timezone, CWD, OS |
| `nexus.skills` | `skill-catalog` | 80 | XML-formatted list of available skills |

## Agent-Level Semantic Tags

Beyond the structural `<prompt_section>` wrapping, each agent type uses semantic XML tags for its dynamic content:

| Tag | Content | Agents |
|-----|---------|--------|
| `<skill_context>` | Grouped loaded skill bodies | ReAct, PlanExec, Orchestrator |
| `<execution_plan>` | Plan summary + step list | ReAct |
| `<current_task>` | Current step instructions | ReAct, PlanExec, Orchestrator workers |
| `<prior_results>` | Completed step/dependency outputs | PlanExec, Orchestrator workers |
| `<user_request>` | Original user input (CDATA-wrapped) | PlanExec, Orchestrator |
| `<subtask_results>` | Worker outputs in synthesis prompts | Orchestrator |

User-provided content and LLM outputs are wrapped in CDATA blocks to prevent parsing conflicts.

## XML Helpers

Shared XML utilities in `pkg/engine/xml.go`:

```go
engine.XMLWrap("tag", content, "attr", "value")  // wrap content in <tag attr="value">...</tag>
engine.XMLTag(&builder, "tag", "attr", "value")   // write opening tag
engine.XMLClose(&builder, "tag")                   // write closing tag
engine.XMLCDATA(content)                            // wrap in <![CDATA[...]]>
engine.XMLEscape(s)                                 // escape &, <, >, "
```

## API

```go
type PromptSectionFunc func() string

type PromptRegistry struct {
    Register(name string, priority int, fn PromptSectionFunc)
    Unregister(name string)
    Apply(systemPrompt string) string
}
```

- **`Register`** — Adds a named section. If a section with the same name already exists, it is replaced.
- **`Unregister`** — Removes a named section.
- **`Apply`** — Takes the base system prompt, wraps it in `<system_instructions>`, and appends all registered sections wrapped in `<prompt_section>` tags, in priority order.

## Use Cases

- **Skill catalogs** — The skills plugin registers available skills so the agent knows what's available
- **Dynamic variables** — The dynvars plugin injects current date/time and system info
- **Custom context** — Your plugins can inject any context the agent should be aware of
