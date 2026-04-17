# Writing Skills

Skills are reusable instruction sets that extend the agent's behavior without writing Go code. A skill is a directory containing a `SKILL.md` file with YAML frontmatter and markdown instructions.

## Skill Structure

```
skills/
  my-skill/
    SKILL.md         # Required: frontmatter + instructions
    resources/       # Optional: supporting files
      template.txt
      schema.json
```

## SKILL.md Format

```markdown
---
name: my-skill
description: >-
  A concise description of what this skill does and when it should
  be used. This appears in the skill catalog shown to the agent.
metadata:
  author: your-name
  version: "1.0"
---

# My Skill

## When to use
Describe the situations where this skill should be activated.

## Instructions
1. Step one...
2. Step two...
3. Step three...
```

### Frontmatter Fields

| Field | Required | Description |
|-------|----------|-------------|
| `name` | Yes | Unique skill identifier (used in activation) |
| `description` | Yes | What the skill does — shown in the catalog. Write this so the agent knows when to use it. |
| `metadata.author` | No | Who created this skill |
| `metadata.version` | No | Version string |
| `output_schema` | No | Inline JSON Schema for structured output (see [Output Schema](#output-schema)) |
| `output_schema_file` | No | Path to a `.json` schema file, relative to the skill directory |

### Body Content

The markdown body is loaded into the agent's context when the skill is activated. Write it as instructions the agent should follow.

## Skill Locations

Skills are discovered from these directories:

| Location | Scope | Trust Level |
|----------|-------|-------------|
| `./skills/` | Project | Configurable (`ask`/`always`/`never`) |
| `~/.agents/skills/` | User | Always trusted |
| Custom paths in `scan_paths` | Config | Configurable |

## Example: Code Review Skill

```markdown
---
name: code-review
description: >-
  Review code for quality, bugs, security issues, and style.
  Use when the user asks for a code review or wants feedback
  on their code changes.
metadata:
  author: nexus
  version: "1.0"
---

# Code Review

## When to use
Use this skill when the user asks you to review code, check a PR,
or provide feedback on code quality.

## Instructions
1. Read all changed files thoroughly before commenting
2. Check for these categories of issues:
   - **Bugs**: Logic errors, off-by-one, null/nil handling, race conditions
   - **Security**: Injection, XSS, hardcoded secrets, unsafe deserialization
   - **Performance**: Unnecessary allocations, N+1 queries, missing indexes
   - **Style**: Naming, formatting, idiomatic patterns for the language
   - **Design**: SOLID violations, coupling, missing abstractions
3. Prioritize findings by severity (critical > major > minor > nit)
4. For each finding, explain the issue AND suggest a fix
5. Start with a high-level summary before detailed findings
6. Acknowledge what's done well
```

## Resources

Skills can include resource files in a `resources/` subdirectory. The agent can request these via the `skill.resource.read` event.

```
skills/
  doc-analysis/
    SKILL.md
    resources/
      analysis-template.md
      output-format.json
```

## Skill Catalog in System Prompt

When `catalog_in_system_prompt: true` is set on the skills plugin, discovered skills are listed in the system prompt as XML:

```xml
<skills>
  <skill name="code-review" scope="project">Review code for quality, bugs, security issues, and style.</skill>
  <skill name="doc-analysis" scope="project">Analyze documents and extract structured information.</skill>
</skills>
```

The agent can then decide to activate a skill based on the user's request.

## Output Schema

Skills can declare an output schema to enforce structured LLM output when the skill is active. The schema is registered with the Schema Registry on activation and deregistered on deactivation.

### Inline Schema

For simple schemas, define `output_schema` directly in the frontmatter:

```yaml
---
name: code-review
description: Review code for quality and bugs.
output_schema:
  type: object
  required: [summary, issues]
  properties:
    summary:
      type: string
    issues:
      type: array
      items:
        type: object
        required: [file, line, severity, message]
        properties:
          file: { type: string }
          line: { type: integer }
          severity: { type: string, enum: [critical, major, minor, nit] }
          message: { type: string }
---
```

### File-Referenced Schema

For complex schemas, reference a `.json` file:

```yaml
---
name: code-review
description: Review code for quality and bugs.
output_schema_file: resources/review.schema.json
---
```

```
skills/
  code-review/
    SKILL.md
    resources/
      review.schema.json
```

Paths are resolved relative to the skill directory. Absolute paths are also accepted.

### Precedence

If both `output_schema` and `output_schema_file` are present, `output_schema` (inline) wins.

### Lifecycle

1. Skill activates → schema loaded (inline or from file) → `schema.register` emitted
2. While skill is active, LLM requests are tagged with `_expects_schema` metadata
3. Schema Registry attaches `ResponseFormat` to tagged requests
4. Provider maps to native structured output or simulates it
5. Skill deactivates → `schema.deregister` emitted → tagging stops

### When to Use Inline vs File

- **Inline**: Simple schemas under ~10 fields. Easy to read alongside instructions.
- **File**: Complex schemas, schemas shared across skills, schemas generated or validated by external tooling.

## Best Practices

- **Write clear descriptions** — The description is how the agent decides whether to activate the skill. Make it specific about the trigger conditions.
- **Be prescriptive in instructions** — Tell the agent exactly what to do, in what order, and what output to produce.
- **Use numbered steps** — Structured instructions are easier for the agent to follow.
- **Specify output format** — If you want structured output, describe the format explicitly.
- **Keep skills focused** — One skill should do one thing well. Compose multiple skills rather than creating one mega-skill.
- **Test with different inputs** — Verify the skill produces good results across various scenarios.
