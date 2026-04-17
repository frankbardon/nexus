# Skills Plugin

Discovers, catalogs, and manages skills — reusable instruction sets that extend the agent's behavior without writing code.

## Details

| | |
|---|---|
| **ID** | `nexus.skills` |
| **Dependencies** | None |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `scan_paths` | string[] | *(none)* | Additional directories to scan for skills |
| `trust_project` | string | `ask` | Trust level for project-scoped skills: `ask` (prompt user), `always`, `never` |
| `max_active_skills` | int | `10` | Maximum number of concurrently active skills |
| `catalog_in_system_prompt` | bool | `true` | Include skill catalog in the system prompt |
| `disabled_skills` | string[] | *(none)* | Skills to exclude from discovery |

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `core.boot` | 10 | Scans for skills at startup |
| `skill.activate` | 50 | Activates a skill by name |
| `skill.deactivate` | 50 | Deactivates a skill |
| `skill.resource.read` | 50 | Reads a skill's resource files |
| `before:llm.request` | 15 | Tags requests with `_expects_schema` for active skills that declare `output_schema` |

### Emits

| Event | When |
|-------|------|
| `skill.discover` | Skills catalog assembled |
| `skill.loaded` | Skill content loaded for the agent |
| `skill.resource.result` | Skill resource content |
| `before:skill.activate` | Before activation (vetoable for trust checks) |
| `schema.register` | Skill with `output_schema` activated |
| `schema.deregister` | Skill with `output_schema` deactivated |

## Skill Discovery

At boot, the plugin scans these directories:

1. `./skills/` — Project-scoped skills
2. `~/.agents/skills/` — User-scoped skills
3. Any paths in `scan_paths` config

Each directory containing a `SKILL.md` file is recognized as a skill.

## System Prompt Catalog

When `catalog_in_system_prompt: true`, the plugin registers a prompt section listing available skills in XML format:

```xml
<skills>
  <skill name="code-review" scope="project">Review code for quality, bugs, security issues, and style.</skill>
  <skill name="git-workflow" scope="project">Standard git workflow with branching and PR creation.</skill>
</skills>
```

This lets the agent know which skills exist and can request activation when appropriate.

## Trust Levels

Project-scoped skills (from `./skills/`) may be untrusted. The `trust_project` setting controls behavior:

| Mode | Behavior |
|------|----------|
| `ask` | Show approval dialog before activating project skills |
| `always` | Trust all project skills automatically |
| `never` | Block all project skill activation |

User-scoped skills (from `~/.agents/skills/`) are always trusted.

## Example Configuration

```yaml
nexus.skills:
  trust_project: ask
  max_active_skills: 5
  catalog_in_system_prompt: true
  scan_paths:
    - /shared/team-skills
  disabled_skills:
    - experimental-skill
```

For details on creating skills, see [Writing Skills](../skills/authoring.md).
