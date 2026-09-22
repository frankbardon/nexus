# Gemini Provider

The Gemini provider calls Google's Gemini API via direct HTTP — no SDK dependency. It supports both the public Generative Language API (api-key auth) and Vertex AI (Google Application Default Credentials) and ships feature parity with the OpenAI and Anthropic providers (sync + streaming, tool use, structured output, retry, cancellation, debug logs, fallback hooks). On top of that it adds Gemini-only features: thinking ("reasoning") parts, multimodal inputs, the built-in code execution tool, and prompt caching via the `cachedContents` API.

## Details

|  |  |
|---|---|
| **ID** | `nexus.llm.gemini` |
| **Dependencies** | None |

## Configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `auth` | string | `api_key` | `api_key` for the public endpoint, `vertex` for Vertex AI |
| `api_key` | string | — | Direct API key (overrides `api_key_env`) |
| `api_key_env` | string | `GEMINI_API_KEY`, then `GOOGLE_API_KEY` | Env var holding the API key |
| `credentials` | string | `google-adc` | (Vertex) Name of a registered `nexuscreds` credential source. Ignored under `auth: api_key` |
| `project_id` | string | *(env `GOOGLE_CLOUD_PROJECT`, then metadata)* | (Vertex) GCP project. Config → `GOOGLE_CLOUD_PROJECT` → GCE metadata server; fails at `Init` only if all three miss |
| `location` | string | *(GCE zone-derived region, then `us-central1`)* | (Vertex) GCP region for the AI Platform endpoint. Config → region derived from this process's GCE zone → `us-central1` |
| `service_account_json` | string | *(unset — full ADC chain)* | (Vertex) Path to a credentials JSON file. Forwarded to the credential source, not parsed by the provider |
| `service_account_json_env` | string | *(unset — full ADC chain)* | (Vertex) Env var holding that path. Also forwarded; naming an unset variable is an error, not a fall-through |
| `debug` | bool | `false` | Log raw request/response bodies into the session plugin directory |
| `pricing` | map | embedded defaults | Per-model pricing overrides; see **Cost Tracking** below |
| `retry` | map | disabled | Retry/backoff config; see **Retry Logic** |
| `thinking` | map | `off` | Reasoning config: `mode: level` (Gemini 3.x) or `mode: budget` (Gemini 2.5); see **Thinking** |
| `code_execution` | bool | `false` | Enable Gemini's built-in code execution tool |
| `cache` | map | disabled | Prompt cache config; see **Prompt Caching** |

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `llm.request` | 10 | LLM requests from agents |
| `cancel.active` | 5 | Cancels in-flight API requests |

### Emits

| Event | When |
|-------|------|
| `llm.response` | Non-streaming response received (also after a stream completes) |
| `llm.stream.chunk` | Each chunk of a streaming response |
| `llm.stream.end` | Streaming response complete |
| `thinking.step` | A `thought: true` part is observed (sync or stream) |
| `tool.invoke` / `tool.result` | When the built-in code execution tool is used |
| `before:core.error` / `core.error` | API errors (vetoable so fallback can intercept) |

## Features

### Auth Modes

- **`api_key`** (default) — Sends `x-goog-api-key` header. URL: `https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent`.
- **`vertex`** — Signs every request with a Google OAuth2 access token obtained from a `nexuscreds` credential source (`credentials`, default `google-adc`), and routes requests to `https://{location}-aiplatform.googleapis.com/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:generateContent`. Minting, caching and refreshing the token belong to the source, not to this provider.

The headline for Vertex: **on GKE with Workload Identity, `auth: vertex` alone is a complete Vertex configuration.**

```yaml
plugins:
  nexus.llm.gemini:
    auth: vertex
```

`project_id` and `location` come from the pod's own GCE metadata, and `google-adc` picks up the token the metadata server hands out for the Google service account the pod's Kubernetes service account is bound to. No key file, no project id, no region. (Setting up the binding itself — GSA creation, KSA annotation, the `roles/iam.workloadIdentityUser` grant — is Google's documentation, not ours.)

#### Credential precedence

`google-adc` runs Google's Application Default Credentials chain. The first rung that answers wins:

1. **An explicitly configured key file** — `service_account_json`, or the path held in the environment variable named by `service_account_json_env`. Nexus reads this file at `Init`, so a bad path fails at boot.
2. **`GOOGLE_APPLICATION_CREDENTIALS`** — the standard ADC environment variable. Nexus never reads it itself; it is step one of the library's own chain, which is why it behaves identically whether or not any Nexus config key is set.
3. **The well-known `gcloud` file** — `~/.config/gcloud/application_default_credentials.json`, written by `gcloud auth application-default login`. This is the developer-laptop rung.
4. **The GCE metadata server** — the keyless rung: GKE Workload Identity, GCE, Cloud Run.

Rung 1 is the only one Nexus itself decides. Rungs 2–4 are the library's, in that order.

The scope is fixed at `https://www.googleapis.com/auth/cloud-platform` and is not configurable — Vertex AI accepts nothing narrower.

#### The boot log line

Vertex credentials are resolved *and exercised* during `Init`: the provider asks the source for one token before the plugin is ready. A pod whose Workload Identity binding is broken therefore fails at **boot**, not on its first user message. On success there is exactly one INFO line per process:

```
INFO vertex credentials confirmed credential_source=google-adc project=my-project location=us-central1 location_source=metadata
```

| Field | What it tells you |
|---|---|
| `credential_source` | The `credentials` name that was opened — which registered source minted the token. |
| `project` | The project every request URL will be built from. |
| `location` | The region every request URL will be built from. |
| `location_source` | Which rung of the location chain answered: `config` (you set `location`), `metadata` (derived from this process's GCE zone), or `default` (nothing answered and `us-central1` is standing in). |

`location_source` exists because the location chain falls back silently: without it, `location=us-central1` cannot be told apart from a deliberate setting and a metadata lookup that failed.

The line deliberately carries no token, no key material, no credential type and no principal.

#### The stale-key-in-image trap

A key file baked into a container image — or a `GOOGLE_APPLICATION_CREDENTIALS` left behind in a Deployment's env — **silently wins over the metadata server**, because it sits earlier in the chain above. The pod authenticates and nothing looks wrong; it simply acts as the wrong identity, and keeps doing so long after that key should have been retired.

Be aware that the boot line does **not** report which credential type won. That diagnostic was given up on purpose to keep the credential seam narrow — the provider asks its source for a token and learns nothing else about it. So the way to catch this is to know the precedence and check the image: on a pod you believe is keyless, confirm that no `service_account_json` / `service_account_json_env` is configured, that `GOOGLE_APPLICATION_CREDENTIALS` is unset in the pod environment, and that no credentials JSON was baked into the image layer.

#### Workload Identity Federation

`external_account` credential files — Workload Identity Federation from AWS, Azure or any OIDC provider — now work when pointed at by `service_account_json`. They previously failed with `missing client_email or private_key`, because the hand-rolled parser understood only `service_account`. The accepted file types are now `service_account`, `authorized_user`, `external_account`, `external_account_authorized_user`, `impersonated_service_account` and `gdch_service_account`; anything else is rejected at boot with its type named.

#### Regional model availability

Vertex model availability is per-region. A pod that derives its region from its own zone can land in a region where the configured model is not served, and that fails at **first inference with a 404**, not at boot — there is no boot-time availability probe by design. Set `location` explicitly wherever the model choice matters.

#### Migrating from a service-account key file

**No YAML edit is required.** `service_account_json` and `service_account_json_env` mean exactly what they meant before, and a config that sets either keeps working unchanged. What changed is underneath: the hand-rolled RS256 JWT exchange is gone, and the credential is now built by `golang.org/x/oauth2/google`. Three things may look different:

- **Error strings.** Failures to load or exchange a credential now surface the library's wording, wrapped by Nexus. Anything matching on the old messages needs updating.
- **Scope handling.** Fixed at `cloud-platform`, granted by the library rather than encoded into a JWT assertion by the provider.
- **Universe domain.** A key file's `universe_domain` is now honoured by the library. Behaviour against non-default universes may differ from the old code, which ignored the field entirely.

One behaviour change is deliberate and can break a working config: **`service_account_json_env` naming an unset or empty environment variable is now an error**, where the old code fell through to the next candidate. A typo in a variable name used to become a silently different identity; it now fails at boot.

#### Release note: new direct dependencies

Nexus's root direct-dependency list is deliberately defended, so this one is called out explicitly. Vertex auth promotes two modules to direct requirements of the root module:

- `golang.org/x/oauth2` — previously an indirect dependency.
- `cloud.google.com/go/compute/metadata` — previously reached indirectly via gRPC, which arrives with the OTLP trace exporter.

The measured cost was **zero new modules in the build graph**: both were already present transitively, and the change is a promotion, not an addition. Both are confined to `pkg/nexuscreds/googleadc` and `pkg/nexuscreds/gcemeta`; the parent `pkg/nexuscreds` package is stdlib-only, so a binary that blank-imports neither pays nothing for them.

`bin/nexus`, `bin/nexus-broker` and the reference desktop app all blank-import `google-adc`. A custom binary must blank-import the package registering whichever source it names in `credentials`, or boot fails with an error naming that source and listing the sources the build actually carries.

### Streaming

When `llm.request.Stream` is `true`, the provider uses `:streamGenerateContent?alt=sse` and parses SSE `data:` lines. Text deltas are emitted as `llm.stream.chunk` (Content), function calls as `llm.stream.chunk` (ToolCall), and a final `llm.stream.end` carries `usageMetadata`.

### Tool Calling

Nexus tool definitions are translated into Gemini `functionDeclarations`. Because Gemini matches tool calls and tool responses by **function name** (not an opaque ID), the provider synthesises stable IDs (`call_{seq}_{name}`) on outbound responses and resolves trailing `tool` messages back to the function name when serializing `functionResponse` parts.

Tool parameter schemas are run through the same sanitizer as `responseSchema` (see [Structured Output](#structured-output-native)) plus two repairs Gemini's schema dialect demands:

- **`type` unions collapsed.** Draft 2020-12 producers (notably `zod-to-json-schema`, used by most TypeScript MCP servers) emit nullable fields as `"type": ["string", "null"]`. Gemini's `Schema.type` is a singular enum and 400s on a list, so the union is collapsed to its first non-`null` member plus `nullable: true`.
- **Missing `items` filled.** Gemini requires `items` on every array-typed schema, at every nesting level, and rejects the whole request with `properties[<name>].items: missing field` when it is absent. JSON Schema leaves `items` optional, so free-form arrays (an RFC 6902 patch ops array, an opaque value list) commonly arrive without one. The provider inserts an empty schema `{}`, which leaves the element type unconstrained rather than inventing one — Gemini still emits objects, arrays or scalars as the tool's description directs.

Both repairs are applied recursively and only when needed; a schema that already declares a singular type and an element schema passes through untouched.

### Tool Choice

`ToolChoice.Mode` maps to `toolConfig.function_calling_config`:

- `auto` → `mode: AUTO`
- `required` → `mode: ANY`
- `none` → `mode: NONE`
- `tool` (with `Name`) → `mode: ANY` plus `allowed_function_names: [name]`

### Structured Output (Native)

When `ResponseFormat.Type` is `json_object` or `json_schema`, the provider sets `generationConfig.responseMimeType: application/json` and (for `json_schema`) `generationConfig.responseSchema`. JSON Schema fields Gemini doesn't accept (`$schema`, `$id`, `additionalProperties`, `$ref`, `definitions`, `$defs`) are stripped recursively. `LLMResponse.Metadata["_structured_output"]` is set to `true`.

### Thinking

Gemini expresses reasoning through `generationConfig.thinkingConfig`, and *which*
field inside that object is correct depends entirely on the model family. Gemini 3.x
takes `thinkingLevel` and does not support `thinkingBudget`; Gemini 2.5 is the
inverse. `thinking.mode` is the axis that picks one:

| `mode`   | What goes into `generationConfig.thinkingConfig` | Family |
|----------|--------------------------------------------------|--------|
| `level`  | `thinkingLevel: "<level>"`                        | Gemini 3.x |
| `budget` | `thinkingBudget: N`                               | Gemini 2.5 |
| `off`    | *(no `thinkingConfig` object at all)*             | — |

`include_thoughts: true` adds `includeThoughts: true` next to whichever of the two
was chosen. Under `mode: off` there is no `thinkingConfig` for it to attach to, so
it is ignored.

**The provider never inspects the model id.** There is no capability table and no
inference from the model string — the operator declares which family they mean, and
a `mode` the target model does not accept is a Gemini HTTP 400 by design.

#### Which mode for which model

| Model family | `mode` | Google's default when the field is absent |
|---|---|---|
| Gemini 3.8 / 3.7 Flash | `level` | `medium` |
| Gemini 3.6 / 3.5 Flash | `level` | `medium` |
| Gemini 3.1 Pro | `level` | `high` — thinking cannot be turned off |
| Gemini 3.5 / 3.1 Flash-Lite (including the Image variants) | `level` | `minimal` |
| Gemini 3 Flash | `level` | `high` |
| Gemini 2.5 (Pro, Flash, Flash Preview, Flash-Lite) | `budget` | dynamic (`-1`) on 2.5 Pro and 2.5 Flash |

`level` accepts exactly four values, lowercase: `minimal`, `low`, `medium`, `high`.
Anything else fails at `Init` with the accepted set named.

`level` is **optional** under `mode: level`. Omitted, the provider falls back to the
role's [`effort`](#reasoning-depth-from-a-role-effort); with neither set it sends a
`thinkingConfig` carrying no `thinkingLevel` key at all, and the model's own default —
the right-hand column above — applies. Omitting `level` is therefore a valid way to say
"whatever this model does by default", not a misconfiguration.

Set here, at the **plugin** level, `level` is a default: a role's `effort` overrides it.
Only a `level` on the role's own [`thinking:` block](#per-role-thinking) outranks that
role's `effort`.

Two asymmetries are worth keeping in mind:

- **2.5 will *accept* a `thinkingLevel`** for backward compatibility, but it degrades
  2.5 Pro. Accepted is not the same as correct: on 2.5, use `mode: budget`.
- **3.x will not accept a `thinkingBudget` at all.** `thinkingBudget` is superseded
  rather than formally deprecated — it remains the only correct parameter on 2.5,
  which is why both modes ship.

```yaml
# Gemini 3.x
plugins:
  nexus.llm.gemini:
    thinking:
      mode: level
      level: medium
      include_thoughts: true
```

```yaml
# Gemini 2.5
plugins:
  nexus.llm.gemini:
    thinking:
      mode: budget
      budget_tokens: 8192
      include_thoughts: true
```

#### Reasoning depth from a role: `effort`

A [`core.models`](../../configuration/reference.md#coremodels) role may carry its own
`effort:`, and this provider reads it as a `thinkingLevel` — but only under
`mode: level`, and only when the plugin-level `thinking.level` is unset:

```yaml
core:
  models:
    worker:
      provider: nexus.llm.gemini
      model: gemini-3-flash
      effort: low              # → "thinkingConfig": {"thinkingLevel": "low"}

plugins:
  nexus.llm.gemini:
    thinking:
      mode: level
```

The same key works on a `fanout` role spanning both providers, which is where the
clamp earns its keep:

```yaml
core:
  models:
    panel:
      fanout: true
      providers:
        - provider: nexus.llm.gemini
          model: gemini-3.1-pro
          effort: max            # Anthropic's word — clamped on this leg
        - provider: nexus.llm.anthropic
          model: claude-opus-4-7
          effort: max            # native there

plugins:
  nexus.llm.gemini:
    thinking:
      mode: level
      # a `level:` here is only the default this role's effort overrides
```

**A role's `effort` wins over the plugin-level `thinking.level`**, the same way it wins
over `output_config.effort` on the Anthropic provider. A plugin-level key is a default
and a per-role key is the more specific statement; that is the one rule, on every
provider. Only a `level` on the **role's own** [`thinking:` block](#per-role-thinking)
outranks that role's `effort`, because there the contest is between two statements at
the same level of specificity and the native, unambiguous one wins.

> **Behaviour change.** This direction was reversed until the per-role-reasoning
> release: the plugin-level `thinking.level` beat a role's `effort`, on the argument
> that `level` is Gemini's native vocabulary and `effort` the translated one. With a
> role able to write `level` itself the contest is between the layers rather than the
> vocabularies, and this provider now follows the same rule as every other. If you set
> a plugin-level `thinking.level` **and** a role `effort` and relied on the level
> winning, move that value onto the role's own `thinking:` block. Two follow-on
> consequences: a role `effort` the plugin-level `level` used to shadow now reaches the
> vocabulary gate, so a value in neither provider's vocabulary now fails `Init` where it
> was previously ignored; and a clamp that was previously silent is now warned.

Anthropic's vocabulary reaches higher than Gemini's, so two of its words clamp:

| Role `effort:` | `thinkingLevel` sent |
|---|---|
| `minimal`, `low`, `medium`, `high` | as written |
| `xhigh`, `max` | `high` — **clamped**: "as deep as this model goes" |

Clamping rather than rejecting is what keeps a role shared with the Anthropic provider
configurable whichever provider's word the operator reaches for; ignoring the value
would instead make `effort:` a silent no-op on this leg. The clamp is announced **once
at `Init`** — the provider walks every `core.models` role it could serve and logs the
role, the configured value and the effective one. A word in neither provider's
vocabulary fails `Init` for any such role, and fails the request if it arrives some
other way. The clamp warning is raised once per (role, value).

Under `mode: budget` and `mode: off`, a role's `effort` is **ignored entirely and
without warning**: there is no `thinkingLevel` on those paths for it to become, and
2.5's depth control is `budget_tokens`. The same is true behind a `level` the role's own
block named — and because a role may now carry its own block, the mode that decides this
is the role's, not necessarily the plugin's.

**How the value gets here.** Like the Anthropic provider, this one reads the
`core.models` registry itself, on every path that resolves a role: the role the request
names, the `default` role, and the late recovery after a router rewrote `model` without
touching the rest. A plain single-entry role's `effort:` therefore reaches the wire on
its own, with no coordinator involved.

An `effort` already carried **on the request** still wins over the registry, and that
ordering matters. The `fallback` coordinator stamps one when it retries onto a **later**
chain entry, and the `fanout` coordinator stamps one on **every** leg; a registry lookup
here always returns the role's **first** entry, which is the wrong entry in both cases.
So the registry is consulted only when the request arrived carrying no `effort` at all,
which is exactly the ordinary single-entry role.

The cross-provider account — the union vocabulary, both clamp directions, and the fact
that `nexus.llm.openai` ignores `effort` entirely — is in the configuration reference
under [Reasoning depth: `effort`](../../configuration/reference.md#reasoning-depth-effort).

#### Per-role thinking

Everything above describes the **plugin-level** `thinking:` block. A
[`core.models`](../../configuration/reference.md#coremodels) role entry may carry one of
its own, which **merges over** it key by key — the role wins on every key it names, and
a plugin key the role is silent about survives. That is what lets one plugin instance
serve a Gemini 2.5 role on `mode: budget` and a Gemini 3.x role on `mode: level` at the
same time, which matters here more than on any other provider: the two families take
different parameters, and this provider never inspects the model id to guess which.

The block on the role is spelled in **this provider's own vocabulary** — the same
`mode`, `level`, `budget_tokens` and `include_thoughts` documented above, not a
translated cross-provider shape. It can be, because a `core.models` entry names its own
`provider:` one line above the block, so a `level:` there is unambiguously Gemini's
`thinkingLevel` and nobody else's. The one key written *without* knowing which provider
will read it is [`effort`](#reasoning-depth-from-a-role-effort) — which is why it still
exists, and why the role's own block outranks it when the two land in the same field.

```yaml
plugins:
  nexus.llm.gemini:
    thinking:
      mode: level
      level: medium
      include_thoughts: true

core:
  models:
    default: balanced
    balanced:
      provider: nexus.llm.gemini
      model: gemini-3.1-pro
    legacy:
      provider: nexus.llm.gemini
      model: gemini-2.5-pro
      thinking:
        mode: budget           # role wins
        budget_tokens: 8192    # and displaces the inherited `level`
                               # include_thoughts: true survives
```

`balanced` sends `{"thinkingLevel": "medium", "includeThoughts": true}`; `legacy` sends
`{"thinkingBudget": 8192, "includeThoughts": true}`.

**`level` and `budget_tokens` displace each other across the merge.** Gemini rejects a
request carrying both `thinkingLevel` and `thinkingBudget`, so the two keys are mutually
exclusive in configuration — and a role naming one drops an inherited other rather than
landing beside it. Without that, the very configuration this feature exists for (a 2.5
plugin block and a 3.x role, or the reverse) would merge into a block no operator could
have written and fail the boot. A role naming **both keys itself** is still an `Init`
error: that one the operator did write. **Exactly one of the two reaches the wire,
whichever layer supplied it.**

**Precedence**, highest first:

1. a `thinking` block already on the request — what the `fallback` and `fanout`
   coordinators stamp for the chain entry they are actually serving;
2. the role's own `thinking:` block, merged over the plugin's — including a `level` it
   names, which lands in the same wire field the role's `effort` would have and so
   outranks it;
3. the role's [`effort`](#reasoning-depth-from-a-role-effort), read only under
   `mode: level`;
4. the plugin-level `thinking:` block, unchanged.

That is the same rule as on every other provider —
`request-stamped > role's native block > role's effort > plugin block` — and steps 2
and 3 used to be settled the other way round here. See the behaviour-change note under
[Reasoning depth from a role](#reasoning-depth-from-a-role-effort).

**Invalid role blocks fail the boot.** A block on a `core.models` entry never passes
through this plugin's `schema.json` — core stores these maps without looking inside — so
`Init` sweeps every role, and every entry of every chain, this provider could serve,
merges each block over the plugin's and parses the result. A role whose merged block is
invalid fails `Init` naming the role rather than failing at the first request that uses
it. The same sweep checks each entry's `effort` against the block that entry resolves
to, which is why a role on `mode: off` may carry any `effort` at all without complaint.

Two edges worth knowing:

- **`thinking: {}` on a role is a statement, not a silence.** It overrides no individual
  key, but the merged block is *present*, so on a deployment with no plugin-level block
  it turns thinking on — a present block with no `mode` is `mode: level`.
- **A role that switches `mode` must restate the key that mode needs.** A plugin-level
  `mode: budget` plus a role's bare `level:` leaves `mode: budget` with the budget
  displaced, which fails `Init` naming the role. Set `mode` on the role alongside the key.

**`include_thoughts` on a role does reach the events here**, unlike on
[`nexus.llm.anthropic`](anthropic.md#per-role-thinking), where the `thinking.step` gate
reads the plugin-level value. Nothing gates the emission on this provider: a
`thinking.step` is emitted for every thought part that comes back, and thought parts
come back only when `includeThoughts` was on the wire — which the merged role block
decides. The asymmetry is an implementation difference, not a design one; do not rely on
it staying that way if the Anthropic gap is closed by moving the gate rather than the
config.

**`thinking:` is the only per-entry *block* this provider reads today.** A role's
`cache:`, `retry:` and `api:` parse and travel — see
[Native provider blocks on a role](../../configuration/reference.md#native-provider-blocks-on-a-role)
— but nothing here reads them back. The scalar axes are all wired: `model`,
`max_tokens` and `effort` through this provider's own resolution pass, and
`temperature` alongside the `thinking` block, on every path — so a role's
`temperature:` reaches `generationConfig.temperature` whether or not a
coordinator is involved. A temperature already on the request wins over the
role's, so an agent posture and the `approval_policy` gate still outrank
`core.models`.

#### `-1` and `0` on the budget path

Both are meaningful on 2.5 and both are sent. The budget goes on the wire because
the key is *present*, not because it is positive — so a configured `0` is not
silently dropped the way an unset key is.

- **`-1` — dynamic thinking.** The model sizes its own thinking. It is the default
  on 2.5 Pro, 2.5 Flash and 2.5 Flash Preview, and is supported but not the default
  on 2.5 Flash-Lite.
- **`0` — thinking disabled.** Works on 2.5 Flash, 2.5 Flash Preview and 2.5
  Flash-Lite. **2.5 Pro cannot disable thinking** and rejects it.

`budget_tokens: 0` and `mode: off` are different requests, and both are reachable on
purpose: `0` sends an explicit "do not think" that Gemini validates against the
model, while `off` sends no `thinkingConfig` at all and leaves the model's own
default standing.

**`-1` does not travel between providers.** On the Anthropic provider `-1` has no
special meaning — there is no dynamic-budget literal there, and `adaptive` is the
mode that hands sizing back to the model. The same integer copied from a Gemini
block into an Anthropic one means something else entirely.

#### Exactly one parameter, by construction

Gemini rejects a request that carries both `thinkingLevel` and `thinkingBudget`.
Nexus does not build such a body and then check it: `mode` selects the single branch
that writes one key, so **no configuration can put both on the wire**. What is left
is caught at boot rather than at first inference —

- `level` and `budget_tokens` both set in one block → `Init` error naming both keys.
  Across the plugin/role merge they do not collide: a role naming one **displaces** an
  inherited other. See [Per-role thinking](#per-role-thinking).
- `mode: budget` with no `budget_tokens` → `Init` error. A budget is a number only the
  operator can pick, so there is no default to fall back on.
- `mode: level` with no `level` → **not** an error. Every Gemini 3.x model carries its
  own default thinking level, so the provider falls back to the role's `effort` and,
  failing that, omits `thinkingLevel` and lets the model default stand.

A misconfigured thinking block fails the process at boot, not the user's first turn.

#### Migrating from `enabled` / a bare `budget_tokens`

`thinking.enabled` is a deprecated alias for `mode` and is still honoured. Every path
below logs a warning at boot naming the mode it resolved to, so boot logs are the
inventory of configs still to migrate.

| Old block | Resolves to | Write instead |
|---|---|---|
| `enabled: true` and nothing else | `mode: level` with no level — `thinkingLevel` is omitted and the model's own default applies | `mode: level` + an explicit `level` (3.x), or `mode: budget` + `budget_tokens` (2.5) |
| `enabled: true` + `budget_tokens: N` | `mode: budget` — **the budget wins over the `enabled` alias** | `mode: budget` + `budget_tokens: N`, drop `enabled` |
| `budget_tokens: N` alone | `mode: budget`, inferred | `mode: budget` + `budget_tokens: N` |
| `enabled: false` | `mode: off` | `mode: off` |
| `mode:` and `enabled:` together | the `mode`; `enabled` is ignored | drop `enabled` |

The inference in row two matters because it is the shape most existing 2.5 configs
are already in — `enabled: true` beside a `budget_tokens` keeps behaving as a 2.5
budget config rather than flipping to a level it has no value for. The inference is
also the one place the resolved mode is *not* what the `enabled` alias alone would
give, which is why it warns.

#### Read the right Google page

Nexus speaks the **generateContent** API (`:generateContent` and
`:streamGenerateContent`), not the newer Interactions API. Google documents thinking
for both under near-identical titles, and the field names differ:

| Surface | Doc page | Fields |
|---|---|---|
| generateContent — **what this provider uses** | `ai.google.dev/gemini-api/docs/generate-content/thinking` | `thinkingConfig.thinkingLevel`, `thinkingConfig.thinkingBudget`, `thinkingConfig.includeThoughts` |
| Interactions API — *not* this provider | `ai.google.dev/gemini-api/docs/thinking` | a different shape, e.g. `thinking_summaries: "auto"` in place of `includeThoughts` |

The two URLs differ only by `/generate-content/`. If a field you are reading about
has no counterpart among this plugin's keys, check which page you landed on before
concluding the key is missing.

#### Thought parts on the bus

Response parts with `thought: true` are emitted as `thinking.step` events
(`Source: nexus.llm.gemini`, `Phase: reasoning`) and are excluded from
`LLMResponse.Content`. Every `thinking.step` event lands in the per-session journal
automatically — read it via `journal.Writer.SubscribeProjection` (live) or
`journal.ProjectFile` (post-mortem). `usageMetadata.thoughtsTokenCount` is mirrored
into `events.Usage.ReasoningTokens`.

Gemini's **thought signatures** are separate from all of this and are handled for
you: they ride back on replayed assistant messages via `pkg/roundtrip`
(`gemini_thought_signatures` on `LLMResponse.Metadata`). They are mandatory once
function calls are in play — a replayed `functionCall` that lost its signature is a
`400 INVALID_ARGUMENT` on the next turn — and nothing in the thinking config turns
that forwarding on or off.

### Multimodal

`events.Message.Parts` (text, image, audio, video, file) is serialized into Gemini parts. Inline payloads up to 18 MB use `inlineData` with base64 bytes; larger payloads must be uploaded via the Files API and referenced by URI (`fileData.fileUri`). Provider falls back to `Content` only when `Parts` is empty, so existing text-only callers are unaffected.

### Code Execution

Set `code_execution: true` to advertise Gemini's built-in code execution tool. Response parts of type `executableCode` and `codeExecutionResult` are dual-emitted: appended to `Content` as fenced markdown blocks for any UI, and emitted as `tool.invoke` / `tool.result` events under the synthetic name `_gemini_code_execution` so observers see them as ordinary tool activity.

### Prompt Caching

```yaml
nexus.llm.gemini:
  cache:
    enabled: true
    min_tokens: 32768
    ttl: "1h"
    max_entries: 64
```

The provider computes a deterministic hash of the cache-eligible prefix (model + system instruction + tool declarations + the leading run of contents up to the first tool exchange). When a hit is present in the in-memory LRU, `cachedContent` is set on the request and only the trailing delta is sent. Cache entries are populated explicitly via `Plugin.createCachedContent`; the auto-populate path is intentionally read-only in this initial release. `usageMetadata.cachedContentTokenCount` flows into `events.Usage.CachedTokens` and the cost calculation applies the cached-input discount (default 25%, override via `pricing.<model>.cached_ratio`).

#### Per-role caching

A `core.models` role may carry its own `cache:` block, and it merges over the
plugin-level one exactly the way `thinking:` does — key by key, role wins, plugin
keys the role is silent about survive, and the merged block goes back through the
same `parseCacheSettings` the plugin block uses. Every role this provider could
serve is swept at `Init`: an unknown key or a wrongly typed one fails the boot
naming the role, because these blocks never pass through `schema.json`. An
unparseable `ttl` stays soft — warned once, defaulted.

```yaml
core:
  models:
    cheap:
      provider: nexus.llm.gemini
      model: gemini-3-flash
      cache:
        enabled: false   # this role never reads or writes the shared cache
```

**Only `enabled` changes anything per request today.** The prefix-hash entry map
is shared across every role by design — a cache entry lives at Google and is
addressed by prefix alone — so a role can opt in or out of reading it, and that
is all. `min_tokens`, `ttl` and `max_entries` on a role are carried and validated
but govern the explicit `cachedContents.create` path, which is not role-scoped;
setting them per role is inert. `min_tokens` is inert everywhere for now: the
token-count probe it exists for is not implemented.

### Cost Tracking

Embedded defaults cover the 1.5, 2.0, and 2.5 model lines (single tier — the 2.5-pro >200k tier is **not** modeled; override via config when high-context billing matters):

```yaml
nexus.llm.gemini:
  pricing:
    gemini-2.5-pro:
      input_per_million: 2.50      # >200k tier
      output_per_million: 15.0
      cached_ratio: 0.25
```

`ReasoningTokens` are billed at the output rate. `CachedTokens` are billed at `input_per_million * cached_ratio`; remaining prompt tokens at the standard input rate.

### Retry Logic

Same retry surface as the OpenAI / Anthropic providers (`constant`, `linear`, `exponential`, `exponential_jitter`). Defaults retry 429 / 500 / 502 / 503 / 504. Honors `Retry-After` on 429 responses.

### Request Cancellation

Subscribes to `cancel.active` at priority 5. When a cancellation arrives, the in-flight HTTP request context is cancelled.

### Fallback Hook

Errors are emitted on the vetoable `before:core.error` event before the terminal `core.error`, letting `nexus.provider.fallback` swap to another provider in the chain.

## Example: api-key

```yaml
core:
  models:
    default: balanced
    balanced:
      provider: nexus.llm.gemini
      model: gemini-2.5-flash
      max_tokens: 8192

plugins:
  active:
    - nexus.io.tui
    - nexus.llm.gemini
    - nexus.agent.react

  nexus.llm.gemini:
    api_key_env: GEMINI_API_KEY
```

## Example: Vertex AI

Keyless — a GKE pod under Workload Identity, or any GCE/Cloud Run instance with an attached service account. `location` is pinned anyway, because model availability is per-region:

```yaml
plugins:
  nexus.llm.gemini:
    auth: vertex
    location: us-central1
```

With an explicit key file — a laptop, or anywhere outside GCP:

```yaml
plugins:
  nexus.llm.gemini:
    auth: vertex
    location: us-central1
    project_id: my-gcp-project
    service_account_json: ~/.config/gcloud/keys/nexus-sa.json
```

A runnable version of both is in `configs/demo-gemini-vertex.yaml`.

## HTTP Configuration

- **Timeout**: 5 minutes per request
- **Public endpoint**: `https://generativelanguage.googleapis.com/v1beta`
- **Vertex endpoint**: `https://{location}-aiplatform.googleapis.com/v1`

## Search Grounding

A separate plugin, `nexus.search.gemini_native`, advertises the `search.provider` capability and answers `search.request` events using Gemini's `google_search` tool. Use it independently of the LLM provider — for example, run Anthropic for chat and Gemini for grounded search lookups.
