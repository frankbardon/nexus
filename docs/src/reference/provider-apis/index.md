# Provider API surface tracking

**Purpose.** Record which version of each LLM provider's API Nexus speaks, which
features of that API it implements, and what a provider release would have to
change for Nexus to need work. This is standing reference, reviewed on a cadence
— not a one-off note.

It answers three questions:

1. **What do we pin?** The exact version strings, headers and endpoints Nexus
   sends today, with the file that sends them.
2. **What do we support?** Feature by feature, against what the API offers.
3. **What would break us?** The specific shapes of change that need
   accommodation, so a release note can be skimmed against a known list.

## How to use this

Each provider page carries a `Last verified` date and a `References` section of
canonical URLs. To review:

1. Open each provider's changelog (linked per page) and read entries newer than
   that page's `Last verified` date.
2. For anything that touches a row in that page's feature matrix, update the row
   and note it under **Changes needing accommodation**.
3. Bump `Last verified`, even when nothing changed — a current date with no
   changes is itself information.

**Cadence.** Review before cutting a release, and whenever a provider ships a
model family Nexus is asked to target. Provider APIs in this space have moved
faster than quarterly.

**Verification basis.** Pages are not equally well sourced, and each says so in
its own *Verification basis* section. A page grounded in a directly fetched
changelog is stronger evidence than one assembled from search summaries plus our
own code. When the difference matters for a decision, fetch before relying.

**Accuracy rule.** Every claim about Nexus behaviour cites a file. Every claim
about a provider's API cites a URL. A row that cannot cite either is marked
`unverified` rather than asserted — an unverified row is a research task, not a
fact.

## Pinned versions at a glance

| Provider | Version mechanism | What Nexus pins | Set in |
|---|---|---|---|
| Anthropic | `anthropic-version` request header, plus `anthropic-beta` opt-ins | `2023-06-01` | `plugins/providers/anthropic/auth.go:330`, `files.go:155,201`, `plugins/llm/batch/anthropic.go:419` |
| Anthropic (Bedrock) | version in request body | `bedrock-2023-05-31` | `plugins/providers/anthropic/auth.go:51` |
| Anthropic (Vertex) | version in request body | `vertex-2023-10-16` | `plugins/providers/anthropic/auth.go:52` |
| OpenAI | none on direct API — surface choice is the version axis | `chat_completions` or `responses`, operator-declared | `plugins/providers/openai/api.go` |
| OpenAI (Azure) | `api-version` query, per-surface | operator-supplied, required; `/openai/v1/responses` is implicitly versioned | `plugins/providers/openai/auth.go:189` |
| Gemini (AI Studio) | version in URL path | `v1beta` | `plugins/providers/gemini/auth.go:358` |
| Gemini (Vertex) | version in URL path | `v1` | `plugins/providers/gemini/auth.go:351` |

**Beta opt-ins Nexus sends to Anthropic** (`anthropic-beta`): `files-api-2025-04-14`
(`files.go:28`), `message-batches-2024-09-24` (`plugins/llm/batch/anthropic.go:23`),
`extended-cache-ttl-2025-04-11` and `pdfs-2024-09-25` (`beta.go:113,119`), plus
whatever the structured-output block declares (`structured.go:37`).

Each beta header is a dated contract with its own graduation or removal path.
When one graduates to GA, sending it is usually still accepted but becomes
noise; when one is withdrawn, requests fail. Both are review items.

## Scope

The three LLM provider plugins Nexus ships: `nexus.llm.anthropic`,
`nexus.llm.openai`, `nexus.llm.gemini`, plus the surfaces
`nexus.llm.batch` speaks on their behalf.

Out of scope for now, same template if wanted later: the search adapters
(Brave, provider-native), embeddings (OpenAI), rerankers (Cohere, Jina), and
the MCP client's protocol version.
