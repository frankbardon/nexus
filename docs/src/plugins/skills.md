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
| `scan_paths` | string[] | *(none)* | Directories to scan for skills. **Required** — no implicit defaults. If empty, no skills are loaded. |
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

At boot, the plugin scans only the directories listed in the `scan_paths` config — there are no implicit defaults. If `scan_paths` is empty or unset, no skills are loaded.

Each directory containing a `SKILL.md` file under a configured scan path is recognized as a skill. Scope is inferred from the resolved path: directories under the user's home `.nexus` or `.agents` trees are treated as `user` scope; everything else is treated as `project` scope.

Tilde paths (`~`, `~/...`) are expanded to the user's home directory automatically.

```yaml
nexus.skills:
  scan_paths:
    - ./skills              # project skills, relative to cwd
    - ~/.agents/skills      # user-scope skills (tilde is expanded)
    - /shared/team-skills   # any other directory you want to include
```

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

Project-scoped skills may be untrusted. The `trust_project` setting controls behavior:

| Mode | Behavior |
|------|----------|
| `ask` | Show approval dialog before activating project skills |
| `always` | Trust all project skills automatically |
| `never` | Block all project skill activation |

User-scoped skills (paths under the user's home `.nexus` or `.agents` trees) are always trusted.

## Example Configuration

```yaml
nexus.skills:
  trust_project: ask
  max_active_skills: 5
  catalog_in_system_prompt: true
  scan_paths:
    - ./skills
    - /shared/team-skills
  disabled_skills:
    - experimental-skill
```

For details on creating skills, see [Writing Skills](../skills/authoring.md).
