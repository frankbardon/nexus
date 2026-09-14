# Dynamic Variables Plugin

Injects dynamic system information (date, time, OS, working directory) into the
turn's **user message**, as an XML block prepended on the way to the provider.

## Details

| | |
|---|---|
| **ID** | `nexus.system.dynvars` |
| **Dependencies** | None |

## Configuration

Each variable is **opt-in** — defaults to `false` and must be explicitly
enabled:

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `date` | bool | `false` | Include current date |
| `time` | bool | `false` | Include current time |
| `timezone` | bool | `false` | Include timezone |
| `cwd` | bool | `false` | Include current working directory |
| `session_dir` | bool | `false` | Include session directory path |
| `os` | bool | `false` | Include operating system |
| `request_headers` | list of strings | `[]` | Normalized `X-Nexus-*` request header names to surface — see [Request Headers](../guides/request-headers.md) |

## Events

Subscribes to `before:llm.request` (priority 20) and emits nothing. It applies
its block to the outbound request in place; it registers no Prompt Registry
section.

Priority 20 puts it after the handlers that rewrite a request's shape — fanout
(2), fallback (3), the schema registry (5), progressive discovery (8) and skills
(15) — so none of them sees a message this plugin has already decorated.

## Output

The block is prepended to the request's **last user message**:

```
<runtime_context>
Current date: 2026-04-08
Current time: 10:30:00
Timezone: EST
Working directory: /Users/frank/projects/myapp
Session directory: ~/.nexus/sessions/abc123
OS: darwin/arm64
</runtime_context>

what files changed today?
```

A request with no user message — a planner prompt, a gate repair prompt — is
left alone rather than having one invented to hold the block.

For a multimodal message the block is also inserted as a leading text part,
because a provider may serialize `Parts` instead of `Content`.

### Why the user message and not the system prompt

These values change every turn, the clock most obviously. A system prompt is
the most cache-stable region of a request and sits at the very front of it, so
a per-turn value there invalidates the prompt cache at position zero and
re-bills the whole conversation on every turn. On the live user message the only
volatile bytes sit at the end of the prompt, which is where a cache wants its
variance.

### It never enters conversation history

The block is applied to the outbound `llm.request`, never to the message a
memory plugin persisted — the plugin clones the message slice before writing,
so a provider that hands out its internal slice cannot be edited through it.

That is what rules out the usual failure of per-turn injection: a history ten
turns deep carrying ten increasingly stale copies of "Current time",
contradicting each other. Nothing needs to detect or strip an old block,
because no old block is ever written. Do **not** add a cleanup pass that
rewrites stored messages — it would break prompt caching (the stable prefix
would change every turn), diverge journal and eval golden traces from what was
actually sent, and risk eating a user's own text that happened to look like a
block.

## Example Configuration

```yaml
# Empty config → no variables emitted (every flag defaults to false).
nexus.system.dynvars: {}

# Enable only the variables you want.
nexus.system.dynvars:
  date: true
  cwd: true
```
