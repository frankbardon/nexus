# Session Tags

Every session carries a small key/value label store — `SessionMeta.Labels`,
persisted as part of `metadata/session.json` alongside the rest of
[session metadata](./sessions.md#session-metadata). This page documents the
label store as its own mechanism: the two namespaces a key can belong to, the
four bus events that write and announce them, and the design rule that keeps
the two namespaces from ever being conflated.

## Two namespaces, one map

Labels live in a single `map[string]string`, but a key's first character
decides which of two disjoint namespaces it belongs to:

| Namespace | Key shape | Who can write it | How |
|-----------|-----------|-------------------|-----|
| **Reserved** | starts with `_` (e.g. `_principal_id`) | Trusted infrastructure only — today, an identity-aware transport such as `nexus.io.agui` | A direct Go method, never the bus |
| **General** | anything else (e.g. `tenant`, `project`, a workflow's own bookkeeping key) | Any plugin, tool call, or the opt-in [`nexus.tool.session_tags`](../plugins/tools/session_tags.md) tool set | The vetoable bus events below |

Exactly one function decides which namespace a key belongs to:
`engine.IsReservedLabelKey` (a `strings.HasPrefix(key, "_")` check). Every
consumer that needs the same decision calls this one definition rather than
re-implementing the prefix check — the core write handlers, the reads in
`nexus.tool.session_tags`, ICM's `OperatorTemplateCtx.Context` projection, and
the `<session_context>` prompt builders in the ReAct, Orchestrator, and
Subagent agent loops all share it. That means there is no way for two call
sites in the codebase to disagree about what counts as reserved.

## The request/announce events

Four event types cover every write to the label store:

| Event | Payload | Vetoable | Purpose |
|-------|---------|----------|---------|
| `before:session.tag.set` | `events.SessionTagSetRequest{Key, Value}` | Yes | Ask to write one general-namespace key |
| `before:session.tag.delete` | `events.SessionTagDeleteRequest{Key}` | Yes | Ask to remove one general-namespace key |
| `session.tag.set` | `events.SessionTagSet{SessionID, Key, Value}` | No (announce) | A label was written — by either path, general or reserved |
| `session.tag.deleted` | `events.SessionTagDeleted{SessionID, Key}` | No (announce) | A label was removed — by either path |

The two `before:*` events follow the same shape every other vetoable request
in Nexus does (`before:io.input`, `before:tool.invoke`, ...): a subscriber can
veto with a reason, and a caller emitting one of these gets exactly one
outcome back — applied, or rejected with a reason. There is no second
"apply" step after the veto check passes: the one handler in the core engine
that checks the veto is also the only place that touches `SessionMeta.Labels`
for the general path, because the engine is the only thing that knows how to
mutate session metadata safely.

The two announce events fire from **every** successful write, regardless of
which path produced it — the vetoable general path, or the reserved
direct-Go-call path described below. A subscriber that wants to know "this
session's tags changed" only ever needs to watch these two event types; it
never has to also know about the reserved namespace's separate write
mechanism to see a reserved key change.

## Reading and enumerating

There is no `before:session.tag.get`/`list` request event — reads are
synchronous and go straight to `SessionMetadata().Labels`, the same struct the
write path persists to. `nexus.tool.session_tags`'s `session_tag_get` and
`session_tag_list` tools read it directly rather than round-tripping through
the bus. A reserved key is invisible to both: `session_tag_get` reports it as
not found, identical to a key that was never set, and `session_tag_list`
omits it from the result set entirely — not redacted, not flagged, simply
absent, so a general-namespace enumeration carries no signal that a reserved
key even exists.

## Why the split is structural, not conventional

An identity tag like `_principal_id` and a business-context tag like `tenant`
look identical on the wire: a string key, a string value, sitting in the same
map. The tempting design is to let one code path handle both and trust
callers to behave — never overwrite the identity key, never let an
attacker-controlled tool argument reach it. Nexus does not take that bet.

The reserved-key check is enforced in exactly one place: the core engine's
handler for `before:session.tag.set` / `before:session.tag.delete`. It
rejects a `_`-prefixed key unconditionally, with **no veto exception and no
caller-trust distinction** — a request from a fully-trusted internal plugin
and one from an untrusted tool call an agent invoked because a document told
it to are rejected identically. The only sanctioned way to write a reserved
key is a direct Go method call — `SessionWorkspace.SetReservedLabel` /
`DeleteReservedLabel` — that never touches the bus at all, so there is no
vetoable request an ordinary plugin could emit to reach it even if it wanted
to.

That structural wall exists to protect one design rule:

> **Identity and general context must never be conflatable.**

A `tenant` tag an agent set to steer its own behavior must never be able to
silently become — or overwrite — the `_principal_id` an authenticated
transport bound for the run. If both lived in one flat, unpartitioned
namespace, "don't touch the identity key" would only ever be a comment asking
callers to behave, and the first tool call, misconfigured plugin, or
prompt-injected request that happened to name that key would win by
coincidence. Making the split a prefix the engine itself enforces — rather
than a convention plugin authors are trusted to honor — turns "please don't"
into "cannot", at the one point where every write, from any source, is
forced to pass.

A second, narrower direct-Go seam exists for the same reason on the general
side: `SessionWorkspace.SetLabel` writes a general-namespace key without a
veto hop, for a caller that already sits on trusted, already-authenticated,
already-decoded input and gains nothing from re-litigating it through a gate
built for untrusted general writes. It still rejects a `_`-prefixed key
defensively — a caller mistake here bypasses the general validation
completely, and without the same check a client could smuggle a
`_`-prefixed key in through this second path and land in the reserved
namespace anyway.

## Where general tags surface in prompts

A tag in the general namespace is not just a piece of session bookkeeping —
several agent loops expose it to the LLM as ordinary prompt context, always
through the shared `engine.XMLWrap("session_context", ...)` convention (see
[Prompt Registry](./prompts.md#agent-level-semantic-tags)) and always
built by reading `SessionMetadata().Labels` fresh, filtering out every
reserved key, and rendering what's left as sorted `key: value` lines. A
reserved key is filtered before rendering, not redacted after: it is never
present in the string that gets wrapped, so there is no code path that first
builds an unfiltered prompt and then removes the sensitive part.

Because filtering happens at read time rather than at write time, a tag
written mid-session by any general writer is visible on the very next render
— there is no cache to invalidate. Every one of these builders emits nothing
at all (not even an empty `<session_context/>` tag) when there are no
general-namespace labels currently set, so a session that never wrote a tag
never adds noise to its own prompt.

- **ReAct** and **Orchestrator** (both the per-worker and the synthesis
  system prompt) each add a `<session_context>` section alongside their
  existing `<skill_context>` / `<execution_plan>` / `<current_task>`
  sections.
- **Subagent** prepends a `<session_context>` block ahead of its configured
  system prompt, when one applies.
- **ICM** exposes the same filtered map two ways: `OperatorTemplateCtx.Context`
  is available to the workspace's `operator.md` template as `{{ .Context.<key> }}`,
  and — because that template only renders once, at posture-registration
  time, not on every turn — the per-turn `<icm_turn>` XML payload also carries
  a `<session_context>` block, rebuilt fresh on every dispatch. See
  [ICM: Workspace layout](../plugins/workflows-icm.md#workspace-layout) and
  [ICM: XML payload reference](../plugins/workflows-icm.md#xml-payload-reference).

This is deliberately the only thing a general tag is used for by the core
agent loops: influencing what the LLM sees. Nothing in the reserved namespace
is ever exposed this way — an identity binding is metadata about *who is
running the session*, not something the model should be told about or asked
to reason over, and the same `IsReservedLabelKey` filter that keeps it off
the bus keeps it out of every prompt too.

## Who writes what today

| Writer | Namespace | Mechanism |
|--------|-----------|-----------|
| `nexus.io.agui` | Reserved (`_principal_id`) | Direct `SetReservedLabel` at run start/resume, `DeleteReservedLabel` at run end — see [AG-UI Serve Transport](../plugins/io-agui.md) |
| `nexus.io.agui` | General (each `RunAgentInput.context` item) | Direct `SetLabel`, no veto hop, since the input already arrived over an authenticated transport |
| `nexus.tool.session_tags` (opt-in) | General only | `before:session.tag.set` / `before:session.tag.delete`, the same vetoable path any other bus caller uses — see [Session Tags Tool](../plugins/tools/session_tags.md) |
| Any other plugin | General only | `before:session.tag.set` / `before:session.tag.delete` |

No plugin can write the reserved namespace except through the direct Go
methods, which are only ever called from core-adjacent, trusted code — never
from a bus handler, and never from anything an agent's tool calls can reach.

## See also

- [Sessions](./sessions.md) — the session tree and metadata this store is
  part of.
- [Event Types → Session Tag Events](../events/reference.md#session-tag-events)
  — the full payload field tables for all four events.
- [Configuration Reference → `nexus.tool.session_tags`](../configuration/reference.md#nexustoolsession_tags)
  — the opt-in tool plugin's config keys.
- [AG-UI Serve Transport](../plugins/io-agui.md) — the identity bind/clear
  behavior that motivated the reserved namespace.
