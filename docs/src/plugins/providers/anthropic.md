# Anthropic (Claude) Provider

The Anthropic provider calls the Claude API via direct HTTP requests — no SDK dependency. It supports streaming, tool use, request cancellation, automatic retries, extended thinking and Anthropic's named reasoning-depth control, `effort`.

## Details

| | |
|---|---|
| **ID** | `nexus.llm.anthropic` |
| **Dependencies** | None |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `api_key_env` | string | `ANTHROPIC_API_KEY` | Name of the environment variable containing the API key |
| `debug` | bool | `false` | Log raw request/response bodies to the session plugin directory |
| `thinking` | map | `off` *(no block)* | Extended thinking: `mode`, `budget_tokens`, `display`, `include_thoughts`; see **Thinking** |
| `output_config` | map | *(unset)* | Mirrors the wire object of the same name; carries `effort`. See **Effort** |
| `pricing` | map | (embedded defaults) | Per-model pricing overrides. Keys are model IDs, values have `input_per_million` and `output_per_million` (USD) |

This table is a summary. Every key this plugin accepts, with its exact default,
lives in the [configuration reference](../../configuration/reference.md#nexusllmanthropic),
which is canonical wherever the two disagree.

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `llm.request` | 10 | Receives LLM requests from agents |
| `cancel.active` | 5 | Cancels in-flight API requests |

### Emits

| Event | When |
|-------|------|
| `llm.response` | Non-streaming response received |
| `llm.stream.chunk` | Each chunk of a streaming response |
| `llm.stream.end` | Streaming response complete |
| `thinking.step` | A `thinking` block carries text and `thinking.include_thoughts` is on |
| `core.error` | API errors |

## Features

### Model Resolution

The provider uses the Model Registry to resolve role names. When an `llm.request` specifies a `Role` (e.g., `"reasoning"`), the provider looks up the concrete model config. If no role is specified, the default model is used.

### Streaming

When `llm.request.Stream` is `true`, the provider uses Server-Sent Events (SSE) to stream the response. Each content chunk and tool use block generates a `llm.stream.chunk` event. When streaming completes, `llm.stream.end` carries the full usage statistics.

### Tool Calling

The provider translates Nexus tool definitions into the Anthropic `tool_use` format. Tool call responses from the API are parsed and included in the `llm.response` or final `llm.stream.end` event.

### Prompt Assembly

Before sending a request, the provider calls `PromptRegistry.Apply()` to append dynamic sections (skills catalog, system variables, etc.) to the system prompt.

### Request Cancellation

Subscribes to `cancel.active` at priority 5. When a cancellation arrives, the in-flight HTTP request context is cancelled, aborting the API call.

### Retry Logic

Transient errors (rate limits, server errors) are retried with exponential backoff.

### Structured Output (Simulated)

Anthropic does not natively support `response_format`. When `ResponseFormat` is set with `Type: "json_schema"`, the provider simulates structured output via tool-use-as-schema:

1. A synthetic tool named `_structured_output` is injected alongside any real tools. Its `input_schema` is the output schema from `ResponseFormat.Schema`.
2. `tool_choice` is forced to `{"type": "tool", "name": "_structured_output"}`, overriding any existing tool choice.
3. Claude returns the structured data as tool call arguments.
4. The provider unwraps the tool call arguments back into `LLMResponse.Content`, so downstream consumers see structured output (not a tool call).
5. `LLMResponse.Metadata["_structured_output"]` is set to `true`.

During streaming, the synthetic tool's `input_json_delta` chunks are emitted as `llm.stream.chunk` content events, so the UI can stream structured output in real time.

### Thinking

Anthropic's extended thinking is one config block with one deciding key. `thinking.mode`
names the shape of the request's `thinking` field, and nothing else chooses it:

| `mode`     | What goes in the request body |
|------------|-------------------------------|
| `adaptive` | `"thinking": {"type": "adaptive"}` — the model sizes its own thinking |
| `budget`   | `"thinking": {"type": "enabled", "budget_tokens": N}` — the legacy fixed budget |
| `disabled` | `"thinking": {"type": "disabled"}` — an explicit opt out |
| `off`      | *(no `thinking` field at all)* |

An absent `thinking` block resolves to `off`. A block that is present with no `mode`
resolves to `adaptive`.

**The provider never inspects the model id.** There is no model-capability table in
this plugin and no inference from the model string — deliberately, so the provider
cannot go stale the week Anthropic ships a family it has never heard of. The price is
that a `mode` the target model does not accept is an Anthropic **HTTP 400** with no
local diagnosis: nothing fails at boot, nothing warns, the first request just comes
back rejected. The table below is the entire mitigation. Read it before you pick a
`mode`.

#### Which mode for which model

| Model family | `mode` | Notes |
|---|---|---|
| Fable 5 / 5.1 (and the Mythos counterparts) | `adaptive` | Thinking is always on. `budget_tokens` is a 400, and so is an explicit `disabled` |
| Opus 5 | `adaptive` | Thinks by default. `disabled` is accepted only at effort `high` or below |
| Sonnet 5 | `adaptive` | Thinks by default. `disabled` is accepted |
| Opus 4.8 / 4.7 | `adaptive` | Does **not** think unless asked. `disabled` is accepted |
| Opus 4.6 / Sonnet 4.6 | `adaptive` *(recommended)* | Also auto-enables interleaved thinking with no beta header. `budget` still works here, deprecated |
| Sonnet 4.5, Haiku 4.5 and older | `budget` | `adaptive` is a 400. `budget_tokens` is required, at least `1024` and below `max_tokens` |

Two asymmetries carry most of the real-world breakage:

- **`budget_tokens` is a 400 on every current family** — Fable 5/5.1, Opus 5, Opus 4.8,
  Opus 4.7 and Sonnet 5 all reject it. A config that has been quietly working since
  Sonnet 4.5 stops working the moment the role's model is bumped.
- **`adaptive` is a 400 on Haiku 4.5 and Sonnet 4.5.** The two modes are not a
  compatibility ladder; they are two disjoint eras, and `budget` remains the only
  correct mode on the older families.

Opus 4.6 and Sonnet 4.6 are the one overlap, which is why they are the place to
migrate from rather than the place to stay.

```yaml
# Current families
plugins:
  nexus.llm.anthropic:
    thinking:
      mode: adaptive
      include_thoughts: true
```

```yaml
# Sonnet 4.5 / Haiku 4.5 and older
plugins:
  nexus.llm.anthropic:
    thinking:
      mode: budget
      budget_tokens: 8192
      include_thoughts: true
```

`budget_tokens` has **no default** and is **required** under `mode: budget` — a
`budget` block without it fails at `Init`, naming the key, rather than at the user's
first turn. In every other mode it is ignored.

#### `off` and `disabled` are different requests

They are not two spellings of the same thing, and neither is a superset of the other:

- `off` sends **no `thinking` field**, leaving the model's own default standing.
- `disabled` sends **`{"type":"disabled"}`**, an explicit instruction not to think.

Which one turns thinking off depends on what the model does when the field is absent,
and the current families changed that answer:

| Model family | How to turn thinking off |
|---|---|
| Fable 5 / 5.1 | **Not possible.** Omitting still runs adaptive, and `disabled` is a 400 |
| Opus 5 | `mode: disabled` — and only at effort `high` or below; `xhigh` and `max` reject it |
| Sonnet 5 | `mode: disabled` |
| Opus 4.8 / 4.7 | `mode: off` — these do not think unless asked. `mode: disabled` also works |
| Opus 4.6 / Sonnet 4.6 and older | `mode: off` |

So on Opus 5, `mode: off` does **not** turn thinking off: the field is simply absent
and the model thinks anyway. `disabled` is the only way off, and that is the whole
reason the two values exist separately.

#### `display`, and why `include_thoughts` alone is not enough

`thinking.display` controls whether the API returns *readable* reasoning text. It is a
visibility control only — the model thinks, and is billed, identically under every
value, and the raw chain of thought is never exposed on any model.

| `display`    | What comes back |
|--------------|-----------------|
| `summarized` | `thinking` blocks carry a readable summary of the model's reasoning |
| `omitted`    | `thinking` blocks are still streamed, but with **empty text** |
| *(unset)*    | No `display` key is sent; the target model's own default applies |

The API default is model-dependent, and it changed quietly: it is **`omitted`** on
Fable 5/5.1, Mythos 5/5.1, Opus 5, Opus 4.8, Opus 4.7 and Sonnet 5, where it was
`summarized` on Opus 4.6 / Sonnet 4.6.

That default is a trap for `include_thoughts`, because `omitted` is not "no blocks" —
it is blocks with nothing in them. A reader that counts blocks sees thinking happening;
a reader that renders their text sees a long silent pause and no error anywhere. So
Nexus infers one key from the other:

> When `display` is unset and `include_thoughts` resolves true, the provider sends
> `display: "summarized"` under `mode: adaptive` and `mode: budget`.

Without it, `include_thoughts: true` would emit contentless `thinking.step` events on
every current model and look like a broken UI rather than a config mistake. The
inference is limited to the two modes that actually think — there is no reasoning to
display under `disabled`, and `off` sends no `thinking` object for `display` to ride
in. **An explicit `display` always wins, in both directions**, including the
deliberate `include_thoughts: true` with `display: omitted`.

#### Migrating from `enabled` and a bare `budget_tokens`

`thinking.enabled` is a **deprecated** alias for `mode` and is still honoured. Every
path below logs a warning at boot naming the mode it resolved to, so boot logs are the
inventory of configs still to migrate.

| Old block | Resolves to | Write instead |
|---|---|---|
| `enabled: true` | `mode: adaptive` | `mode: adaptive` |
| `enabled: false` | `mode: off` | `mode: off` — or `mode: disabled` on a model that thinks by default |
| `budget_tokens: N` alone (non-zero) | `mode: budget`, inferred, with a warning | `mode: budget` + `budget_tokens: N` |
| `budget_tokens: 0` alone | `mode: off`, with a warning | `mode: off` |
| `mode:` and `enabled:` together | the `mode`; `enabled` is ignored and warned | drop `enabled` |

`budget_tokens: 0` is the one exception to the budget inference, and it is preserved on
purpose: `0` has always meant "disable thinking" on this provider. Inferring `budget`
there would put `{"type":"enabled","budget_tokens":0}` on the wire, which every model
rejects — turning a documented way to switch thinking off into an unconditional 400.
Under an *explicit* `mode: budget` the `0` is passed through unchanged and the API owns
the rejection, because an explicit mode is the operator naming the wire shape.

`-1` has no special meaning here. It is Gemini's dynamic-thinking literal; on this
provider `adaptive` is what hands sizing back to the model, and a `-1` copied out of a
Gemini block is just an invalid budget.

#### Per-role thinking

Everything above describes the **plugin-level** `thinking:` block. A
`core.models` role entry may carry one of its own, which **merges over** it key
by key — the role wins on every key it names, and a plugin key the role is
silent about survives. That is what lets one plugin instance serve a Haiku role
on `mode: budget` and an Opus role on `mode: adaptive` at the same time, which is
impossible with a single plugin-level block.

The block on the role is spelled in **this provider's own vocabulary** — the same
`mode`, `budget_tokens`, `display` and `include_thoughts` documented above, not a
translated cross-provider shape. It can be, because a `core.models` entry names
its own `provider:` one line above the block, so there is never any doubt about
whose `mode` is meant. (The one key that *is* written without knowing the
provider is [`effort`](#per-role-effort), which is why it still exists.)

```yaml
plugins:
  nexus.llm.anthropic:
    thinking:
      mode: adaptive
      display: summarized

core:
  models:
    deep:
      provider: nexus.llm.anthropic
      model: claude-opus-4-7
    legacy:
      provider: nexus.llm.anthropic
      model: claude-haiku-4-5
      thinking:
        mode: budget          # role wins on `mode`
        budget_tokens: 4096   # `display: summarized` survives from the plugin
```

The merged block goes through the same parser as the plugin one, so it inherits
every check, inference and deprecation warning documented above. Because a block
written on a `core.models` entry never passes through this plugin's
`schema.json`, `Init` sweeps every role — and every entry of every chain — this
provider could serve and **fails the boot naming the role** when a merged block
is invalid, rather than waiting for the first request that uses it.

**Precedence is the one rule every provider now follows** —

```text
request-stamped  >  role's thinking: block  >  role's effort  >  plugin block
```

— where "request-stamped" is what the `fallback` or `fanout` coordinator put on
the request for the chain entry actually being served. On this provider steps 2
and 3 never actually contend: a role's `effort` lands in `output_config.effort`
and the thinking block lands in `thinking`, two different wire fields, so a role
can set both and neither displaces the other. (On `nexus.llm.gemini` they *do*
contend, because a role's `effort` becomes `thinkingLevel` — the same field a
role's `thinking.level` writes — and the block wins there for exactly the reason
the ordering says it should.) A named role does **not** inherit the default
role's block: these are one provider's vocabulary and a different role may be
served by a different provider.

Two things to watch: `thinking: {}` on a role is a statement rather than a
silence (it overrides no key, but a present block with no `mode` is `adaptive`),
and a role that switches `mode` inherits plugin-level keys that may be
meaningless under the new mode.

**A known gap: `include_thoughts` on a role changes the wire, not the events.**
It feeds the `display` inference like any other merged key, so it does alter the
`thinking` object this provider sends. But the gate deciding whether
`thinking.step` events are emitted at all reads the **plugin-level**
`include_thoughts`, because the response-parse path never sees the resolved
per-role block. A role that turns it on where the plugin has it off gets the
thinking text from Anthropic and no events on the bus. Set the plugin-level value
to whatever you want the events to do, and use the role block for wire shape.

**`thinking:` and `cache:` are the per-entry *blocks* this provider reads today.**
A role's `retry:` and `api:` parse and travel — see
[Native provider blocks on a role](../../configuration/reference.md#native-provider-blocks-on-a-role)
— but nothing here reads them back. The scalar axes are all wired: `model`,
`max_tokens` and `effort` through this provider's own resolution pass, and
`temperature` alongside the blocks, on every path.

See the [configuration reference](../../configuration/reference.md#per-role-thinking)
for the canonical account.

#### Thinking on the bus, and round-tripping

`thinking` blocks that carry text are emitted as `thinking.step` events (`Source:
nexus.llm.anthropic`, `Phase: reasoning`) when `include_thoughts` is on — on the
streaming path one per `thinking_delta`, on the sync path one per block — and every one
of them lands in the per-session journal. `redacted_thinking` blocks are passed through
opaquely and never emitted.

Those blocks must also be **echoed back unchanged**, signatures intact, on the next
assistant turn — the API rejects the follow-up request with a 400 otherwise. That
forwarding is handled for you: the provider stashes them on
`LLMResponse.Metadata["thinking_blocks"]`, which `pkg/roundtrip` carries onto the
assistant message a later request replays. Nothing in the thinking config turns it on
or off.

#### Temperature

When thinking is actually on (`adaptive` or `budget`), the provider **strips
`temperature`** from the request body, warning when it drops a user-set value that is
not `1.0` — Anthropic requires `temperature: 1` with thinking, and omitting the field
is the cleanest way to satisfy that.

Be aware the newer families go further than the provider does: `temperature`, `top_p`
and `top_k` are rejected outright on Fable 5/5.1, Opus 5, Opus 4.8, Opus 4.7 and
Sonnet 5 **regardless of thinking**, including under `mode: disabled` and `mode: off`,
where the provider does not strip them. On those models, do not set sampling
parameters at all.

A `core.models` entry may carry a `temperature:` of its own, and this provider
resolves one on every path — the role a request names, the default role, the
recovery after a router rewrote `model`, and the fallback/fanout stamp. A
temperature already on the request wins over the role's, so an agent posture and
the `approval_policy` gate still outrank `core.models`. Both strippings above
apply to a role's value exactly as they do to a posture's, and on the families
that reject the field outright a role setting it is simply a 400 — the provider
does not strip it for you.

#### Per-role caching

A `core.models` role may carry its own `cache:` block, and it merges over the
plugin-level one exactly the way `thinking:` does — key by key, role wins, plugin
keys the role is silent about survive, and the merged block goes back through the
same `parseCacheConfig` the plugin block uses. Every role this provider could
serve is swept at `Init`: an unknown key or a wrongly typed one fails the boot
naming the role, because these blocks never pass through `schema.json`. The
`extended-cache-ttl-2025-04-11` beta header follows the *resolved* TTL, so a role
that lifts a `5m` plugin default to `1h` gets the gate its own markers need.

```yaml
core:
  models:
    cheap:
      provider: nexus.llm.anthropic
      model: claude-haiku-4-5
      cache:
        ttl: "1h"        # the plugin's system/tools breakpoints survive
```

**Caching is stateful in a way thinking is not.** An Anthropic cache entry is
addressed by the exact prefix bytes, breakpoint placement and TTL included. Two
roles that share a model and a system prompt but differ in `system`, `tools`,
`message_prefix` or `ttl` therefore do not share an entry: the second misses,
pays the cache-write premium (1.25× for `5m`, 2× for `1h`) to write its own, and
both then expire on their own clocks. Varying only `enabled` per role is always
safe — a role that caches nothing simply stops reading and writing. Varying the
breakpoints or the TTL is safe when the roles also differ in model or system
prompt, and is a silent cost multiplier when they do not.

### Effort

`output_config.effort` is Anthropic's named reasoning-depth control, and the
replacement for `thinking.budget_tokens` on the current families. Anthropic's own
vocabulary is five values, and the key goes on the wire **nested inside
`output_config`** — never as a top-level request field:

```yaml
plugins:
  nexus.llm.anthropic:
    output_config:
      effort: xhigh          # low | medium | high | xhigh | max
```

```json
{"model": "...", "output_config": {"effort": "xhigh"}}
```

The YAML block is spelled to mirror that wire object, which also carries `format`
(structured outputs) and, in beta, `task_budget`. Nexus **merges** into the object
rather than assigning over it, so effort and a future `format` emission coexist.

**`effort` is independent of `thinking.mode`.** It is emitted whatever the thinking
configuration is, `mode: off` included: depth governs how hard the model works on the
answer, `mode` governs whether thinking blocks come back. Setting one never implies the
other.

Unset is not the same as `high`. Unset sends no `effort` key and no `output_config`
object at all, and the API then applies its own default, which is currently `high`. The
distinction matters only in that an absent key stays correct if Anthropic changes that
default.

**A sixth word is accepted: `minimal`.** It is Gemini's floor, not an Anthropic level,
and it never reaches the wire — the provider **clamps it to `low`** and warns once at
`Init` naming the configured and the effective value. The Gemini provider does the
mirror image, clamping Anthropic's `xhigh` and `max` down to its own `high`:

| Value | This provider sends | `nexus.llm.gemini` sends |
|---|---|---|
| `minimal` | `low` — **clamped** | `minimal` |
| `low` / `medium` / `high` | as written | as written |
| `xhigh` / `max` | as written | `high` — **clamped** |

So all six words configure either provider, and a
[`core.models`](../../configuration/reference.md#reasoning-depth-effort) role shared
between them — a `fanout` role dispatching to both at once, say — stays configurable
whichever provider's word the operator reaches for. Neither leg silently ignores the
setting, and neither rejects the other's vocabulary. `low` is already this provider's
floor, so clamping `minimal` onto it loses nothing there is a word for.

Clamping is about cross-*provider* vocabulary, never cross-*model* capability. No clamp
— and no code path in this provider — inspects the model id; the table below is advice
for the operator, not something the provider enforces.

As with `thinking.mode`, the provider never inspects the model id, and not every model
accepts every level:

| Model family | Accepted levels |
|---|---|
| Fable 5 / 5.1, Opus 5, Opus 4.8, Opus 4.7, Sonnet 5 | `low`, `medium`, `high`, `xhigh`, `max` |
| Opus 4.6 / Sonnet 4.6 | `low`, `medium`, `high`, `max` — **no `xhigh`** |
| Opus 4.5 | `low`, `medium`, `high` |
| Sonnet 4.5, Haiku 4.5 | none — `effort` is rejected outright |

`xhigh` arrived with Opus 4.7, so it is the level most likely to 400 on an older model.
A level the target model does not accept is an Anthropic HTTP 400 the operator owns. A
value outside the accepted six is a different matter and *is* caught locally: it fails
at `Init`, naming the accepted set.

#### Per-role effort

A [`core.models`](../../configuration/reference.md#coremodels) role may carry its own
`effort:`, in the same vocabulary, and it reaches this provider per request:

```yaml
core:
  models:
    default: balanced
    balanced:
      provider: nexus.llm.anthropic
      model: claude-opus-4-5
    deep:
      provider: nexus.llm.anthropic
      model: claude-opus-4-5
      effort: max            # this role thinks hard

plugins:
  nexus.llm.anthropic:
    output_config:
      effort: low            # every other role's default
```

**The role wins** over the plugin-level `output_config.effort`. A request on `deep`
sends `{"effort": "max"}`; a request on any role without its own `effort` sends
`{"effort": "low"}`; with neither set, nothing is emitted.

That precedence is now **the same on Gemini**, where a role's `effort` likewise beats
the plugin-level `thinking.level`. It used to be the inverse there; see the
behaviour-change note on the
[Gemini page](gemini.md#reasoning-depth-from-a-role-effort).

An `effort` **already on the request** beats both. The fallback and fanout coordinators
stamp the chain entry they are actually serving onto the outgoing request, so a fallback
entry's or a non-first fanout leg's own `effort` is what this provider answers with,
rather than the role's first entry's. Beyond that, the role's effort is found on every
path that resolves a role — the named role, the `default` role, and the late recovery
after a router rewrote `model` without touching the rest.

A role's `effort` cannot fail at `Init`, because it only exists per request: an
unusable value **fails that request** with a `core.error` naming the role and the
accepted set. A clamp warns at request time instead, once per (role, value) — warning
every turn would flood a busy role's log.

The full cross-provider account, including a worked `fanout` role and the fact that
`nexus.llm.openai` ignores `effort` entirely, is in the configuration reference under
[Reasoning depth: `effort`](../../configuration/reference.md#reasoning-depth-effort).

### Cost Tracking

The provider computes `CostUSD` on every `llm.response` using per-model pricing rates. Embedded defaults cover common Claude models. Override via config for enterprise pricing tiers or new models:

```yaml
plugins:
  nexus.llm.anthropic:
    pricing:
      claude-sonnet-4-6-20250514:
        input_per_million: 3.0
        output_per_million: 15.0
```

Config overrides are merged with embedded defaults — only override the models you need to change. Cost is accumulated into `SessionMeta.CostUSD` by the engine.

### Debug Mode

When `debug: true`, raw request and response JSON bodies are written to the session's plugin directory for inspection.

## HTTP Configuration

- **Timeout**: 5 minutes per request
- **API endpoint**: `https://api.anthropic.com/v1/messages`

## Example Configuration

```yaml
plugins:
  nexus.llm.anthropic:
    api_key_env: ANTHROPIC_API_KEY
    debug: false
```

To use a different environment variable for the API key:

```yaml
plugins:
  nexus.llm.anthropic:
    api_key_env: MY_CLAUDE_KEY
```

A current-family model with thinking on, reasoning text surfaced, and reasoning
depth raised:

```yaml
plugins:
  nexus.llm.anthropic:
    api_key_env: ANTHROPIC_API_KEY
    thinking:
      mode: adaptive
      include_thoughts: true
    output_config:
      effort: xhigh
```
