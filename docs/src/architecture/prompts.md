# Prompt Registry

The prompt registry allows plugins to inject dynamic sections into the system prompt at runtime. This is how skills catalogs, dynamic variables, and other context get appended to the agent's system prompt without hardcoding.

## How It Works

1. Plugins register **prompt sections** during initialization, each with a name and priority
2. When the LLM provider builds a request, it calls `prompts.Apply(systemPrompt)` to assemble the final prompt
3. Each registered section function is called — if it returns a non-empty string, that content is appended
4. Sections are appended in priority order (lower priority numbers first)

## Registering a Section

```go
func (p *MyPlugin) Init(ctx engine.PluginContext) error {
    ctx.Prompts.Register("my-context", 50, func() string {
        return "## My Context\nSome dynamic information here."
    })
    return nil
}
```

The function is called every time a prompt is assembled, so it can return different content based on current state.

## Built-in Sections

| Plugin | Section Name | Priority | Content |
|--------|-------------|----------|---------|
| `nexus.system.dynvars` | `dynvars` | 90 | Current date, time, timezone, CWD, OS |
| `nexus.skills` | `skill-catalog` | 80 | XML-formatted list of available skills |

## API

```go
type PromptSectionFunc func() string

type PromptRegistry struct {
    Register(name string, priority int, fn PromptSectionFunc)
    Apply(systemPrompt string) string
}
```

- **`Register`** — Adds a named section. If a section with the same name already exists, it is replaced.
- **`Apply`** — Takes the base system prompt and appends all registered sections that return non-empty strings, separated by newlines.

## Example Output

Given a system prompt of `"You are a helpful assistant."` and two registered sections:

```
You are a helpful assistant.

## Available Skills
<skills>
  <skill name="code-review" scope="project">Review code for quality, bugs, security issues, and style.</skill>
</skills>

## System Info
- Date: 2026-04-08
- OS: darwin
- CWD: /Users/frank/projects/myapp
```

## Use Cases

- **Skill catalogs** — The skills plugin registers available skills so the agent knows what's available
- **Dynamic variables** — The dynvars plugin injects current date/time and system info
- **Custom context** — Your plugins can inject any context the agent should be aware of
