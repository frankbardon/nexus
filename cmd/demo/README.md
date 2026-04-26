# Nexus Compete — feature showcase demo

A competitive-intelligence workbench built on Nexus. Three specialized agents
share one vector knowledge base. The competitive-intel framing is just the
showcase — each agent is a generic Nexus pattern you can repurpose by swapping
the watched folder, the system prompts, and (for the Drafter) the skills.

| Agent | Role | What it shows off |
|---|---|---|
| **Librarian** | Curates the KB | RAG ingestion (watch mode), longterm memory, content-safety gate (redact mode), tool-filter gate, capability auto-resolution |
| **Researcher** | Multi-step research over web + KB | Provider fallback chain (Anthropic→OpenAI), parallel tool dispatch, dynamic planner with auto-approval, summary-buffer memory, vector memory, web tools, rate-limiter + prompt-injection + token-budget + context-window gates, `search.provider` capability pinning |
| **Drafter** | Writes structured deliverables | Skills with `output_schema`, structured output (Anthropic tool-sim path), json-schema gate retry loop, output-length gate, content-safety gate (block mode), file_write tool |

All three exercise: desktop shell framework, multi-agent isolation, agent-contributed
settings + keychain secrets, the chat envelope protocol over `nexus.io.wails`,
the observe/logger + observe/thinking observers, and shared vector storage.

## The agents in detail

### Librarian — knowledge base curator

**What it is.** A read-and-ingest assistant tied to a watched folder on
disk. Drop a markdown file in, it gets chunked, embedded, and upserted
into the shared `compete-kb` vector namespace within seconds. The
Librarian itself can query the KB (`knowledge_search`), record durable
catalog notes between sessions (`memory_write` / `memory_read`), and
read raw files inside its watched folder (`file_read`). It cannot
browse the web or run shell commands.

**Example prompts.**

- *"What competitors do you have on file, and when was each last updated?"*
- *"Summarize what we have on Vortex AI in the KB."*
- *"I just dropped `crewai.md` in the folder — confirm it's queryable
  and tell me what topics it covers."*
- *"Record a catalog note: 'pricing pages for ACME and Loom both went
  behind a contact-sales wall on 2026-04-20'."*
- *"List the catalog notes you've recorded in the last month."*

**Adaptation ideas — swap the seed docs, keep the agent.**

- **Personal Zettelkasten / second brain.** Point `input_dir` at your
  Obsidian vault. Ask: *"What have I written about caching strategies?"*
- **Engineering postmortems archive.** Watch a folder of incident
  reports. Ask: *"How many incidents have we had involving the
  payments service in the last six months?"*
- **Sales enablement.** Watch a folder of product one-pagers and
  battlecards. Ask: *"What's our differentiator vs. Vendor X according
  to the latest battlecard?"*
- **Runbook library for ops.** Watch a folder of SOPs. Ask: *"Walk me
  through the failover playbook for the primary database."*

### Researcher — multi-step web + KB research

**What it is.** A read-only researcher that fans out parallel
`knowledge_search` and `web_search` calls, fetches full page bodies
when needed, and synthesizes a cited answer. A dynamic planner runs
first (auto-approved) so you can see the shape of the work before it
starts. The provider fallback chain (Anthropic primary, OpenAI
fallback) keeps long sessions alive through rate-limit blips. Vector
memory carries findings between sessions; the summary-buffer keeps
context fresh on long conversations. Read-only on the filesystem (no
shell, no file writes) — its output is a chat answer, not a file.

**Example prompts.**

- *"Compare ACME, Vortex, and Loom on pricing model, target buyer, and
  time-to-value. Cite sources."*
- *"Vortex AI just announced a Series B. What changed about their
  positioning in the last 90 days? Use web sources."*
- *"Build a one-page market map of the agentic-RAG vendor space:
  group competitors by pricing model, list each one's primary wedge."*
- *"What's the consensus stance on prompt-caching among the major
  agentic frameworks today?"*
- *"You found three claims about Loom's pricing last week — re-check
  them against the live website and flag any that have changed."*
  (Exercises vector memory recall.)

**Adaptation ideas.**

- **Pre-meeting prep agent.** Point at a folder of customer call notes
  + your CRM exports. Ask: *"Summarize everything we know about Acme
  Corp going into Thursday's renewal call. Pull in any LinkedIn or
  press updates from the last quarter."*
- **OSS evaluation agent.** Watch a folder where you save GitHub
  README dumps and ADRs. Ask: *"Compare LangGraph, Mastra, and
  CrewAI on extension points and license terms. Use the docs I've
  saved plus current GitHub READMEs."*
- **Due-diligence agent.** Watch a folder of public filings + analyst
  reports. Ask: *"What are the three biggest risks Pulp Inc. flagged
  in their last two earnings calls that aren't on Wikipedia?"*
- **Academic literature review.** Watch a folder of paper notes. Ask:
  *"What's the current state of evidence for retrieval-augmented
  generation outperforming long-context for document QA? Limit to
  2025–2026 sources."*

### Drafter — structured deliverable writer

**What it is.** A skill-driven publisher. The `competitor-brief`
skill (in [skills/competitor-brief/SKILL.md](skills/competitor-brief/SKILL.md))
declares an `output_schema` so the LLM is forced into a strict JSON
shape; the `json_schema` gate retries via the LLM when the model
deviates; the `output_length` gate enforces a slide-sized budget; the
`content_safety` gate blocks PII / secrets before publication. The
Drafter has `knowledge_search` (read-only over the same KB the
Librarian populates) and `file_write` (constrained to your configured
`output_dir`). Cannot browse the web — it composes facts that are
already in the KB.

**Example prompts.**

- *"Write a competitor brief on Vortex AI."* (Activates the skill,
  produces schema-conformant JSON, prompts to save.)
- *"Draft briefs on all four competitors in the KB and write each one
  to `output_dir`."*
- *"I want to stress-test the schema gate — try writing a brief that
  drops the `headline` field."* (Watch the retry loop kick in.)
- *"The brief on Loom is too long; tighten it to fit on one slide
  without dropping any required fields."* (Watch the output-length
  retry loop.)
- *"Write a brief on a competitor we don't have in the KB yet."*
  (Watch the agent refuse rather than fabricate — the system prompt
  forbids inventing facts.)

**Adaptation ideas — write a new skill, get a new deliverable type.**

- **Customer-success readout.** Write a `customer-readout` skill with
  fields like `health_score`, `risks`, `expansion_opportunities`.
  Drop call-note docs into the Librarian; ask the Drafter to publish
  weekly readouts.
- **Structured incident report.** Write an `incident-report` skill
  with `timeline`, `impact`, `root_cause`, `remediation`. Drop
  Slack-export incident threads into the Librarian; have the Drafter
  produce a publishable post-mortem.
- **RFP response sections.** Write an `rfp-section` skill with
  `requirement_id`, `our_capability`, `evidence_links`. Drop your
  product capability docs into the KB; have the Drafter draft RFP
  answers grounded in real product behavior.
- **Investor update.** Write an `investor-update` skill with
  `highlights`, `lowlights`, `metrics`, `asks`. Drop weekly-status
  exports into the KB; produce monthly investor letters.

## Workflows

The three agents compose. Each can also stand alone if a single
agent is all you need.

| Workflow | Path |
|---|---|
| **Curate-then-publish** | Librarian ingests → Drafter cites |
| **Research-then-publish** | Librarian ingests → Researcher digs deeper → Drafter cites the Researcher's findings (after the human re-files them as docs) |
| **Pure ingestion + Q&A** | Librarian alone, used like a personal knowledge graph |
| **Web-only research** | Researcher alone, KB empty — the planner just routes everything through `web_search` |
| **Skill-driven content** | Drafter alone, against a Librarian-curated KB; the human queues up "write a brief on X" requests |

## Data layout

All demo persistence is rooted under `~/.nexus/demo/`, keeping it isolated
from `cmd/desktop` and any other Nexus app on the same machine:

```
~/.nexus/demo/
  settings.json          # shell-level + per-agent settings (plaintext)
  sessions.json          # session metadata index
  sessions/<id>/         # per-session engine workspace
  vectors/               # shared chromem store (namespace: compete-kb)
  vectors/_cache/        # embedding cache
  longterm/librarian/    # Librarian's longterm memory notes
```

The desktop framework's `Shell.DataDir` controls settings + session-index
location; per-agent YAMLs point `core.sessions.root`, `nexus.memory.longterm.path`,
and `nexus.vectorstore.chromem.path` at subdirectories of the same root.
Secrets (API keys) still live in the OS keychain, not on disk.

## Cross-agent knowledge sharing

The three agents read/write the same chromem namespace at
`~/.nexus/demo/vectors/` (namespace `compete-kb`).

**Caveat:** chromem-go is in-memory with on-disk JSON persistence. Each engine
loads its own snapshot at boot, so writes from the Librarian don't appear in
Researcher / Drafter sessions until those sessions are restarted (or a new
session is opened). Swapping in `sqlite-vec` / `pgvector` removes that limit.

For the demo, the natural flow already accommodates this:
1. Drop docs into Librarian's watched folder → ingest happens.
2. Open Researcher → it sees the new content at boot.
3. Ask Researcher to enrich the KB → Researcher writes... no, wait — Researcher
   is read-only. To add new findings to the KB, drop them as docs into
   Librarian's folder. (This keeps responsibility clean: the Librarian owns
   what's in the KB.)

## Running

The demo uses the Wails toolchain (same as `cmd/desktop`):

```bash
# Dev (hot-reload):
cd cmd/demo
wails dev

# Production build:
wails build
```

A plain `go build ./cmd/demo` produces a binary that compiles but cannot launch
the webview — Wails-native build steps are required for the desktop shell to run.

### Required settings

On first launch the app redirects you to Settings if anything required is missing:

| Setting | Used by | Where |
|---|---|---|
| `shell.anthropic_api_key` | All three agents (Claude Sonnet) | OS keychain |
| `shell.openai_api_key` | All agents (embeddings) + Researcher fallback | OS keychain |
| `researcher.brave_api_key` | Researcher (web_search) | OS keychain |
| `librarian.input_dir` | Librarian (watched folder for ingest) | Plaintext setting |
| `drafter.output_dir` | Drafter (where briefs are saved) | Plaintext setting |

Brave's free tier covers the demo: <https://brave.com/search/api/>.

### Try it

1. **Set up the KB.** Point Librarian's `input_dir` at `cmd/demo/seed/competitors/`.
   Open the Librarian; the watcher auto-ingests `acme-corp.md`, `vortex-ai.md`,
   `loom-systems.md` into the `compete-kb` namespace. Ask: *"What competitors do
   you have on file?"*

2. **Research a question.** Open the Researcher. Ask: *"Compare ACME, Vortex,
   and Loom on pricing and target buyer."* You'll see (a) the planner spit out
   a multi-step plan, (b) parallel `knowledge_search` calls fan out, (c) maybe
   a `web_search` if the KB is thin, (d) a final synthesis with cited sources.

3. **Draft a deliverable.** Open the Drafter. Ask: *"Write a competitor brief
   on Vortex AI."* It activates the `competitor-brief` skill, produces JSON
   matching the schema, and (with your nudge) writes it to `output_dir`. Try
   triggering a schema-retry loop by asking it to *"include only the headline
   field"* — the gate will block and ask for a retry.

## Where the showcase lives

If you want to read the demo to understand a specific Nexus feature:

| Feature | File / line |
|---|---|
| Per-agent engine isolation, factories, settings | [main.go](main.go) |
| RAG watch-mode ingestion | [config-librarian.yaml](config-librarian.yaml) — `nexus.rag.ingest.watch` |
| Capability pinning (search.provider → Brave) | [config-researcher.yaml](config-researcher.yaml) — `capabilities` block |
| Provider fallback chain | [config-researcher.yaml](config-researcher.yaml) — `core.models.balanced` (list form) |
| Parallel tool dispatch | [config-researcher.yaml](config-researcher.yaml) — `nexus.agent.react.parallel_tools` |
| Skills with output_schema | [skills/competitor-brief/SKILL.md](skills/competitor-brief/SKILL.md) |
| Schema gate + retry loop | [config-drafter.yaml](config-drafter.yaml) — `nexus.gate.json_schema.max_retries` |
| Tool-filter (read-only Researcher) | [config-researcher.yaml](config-researcher.yaml) — `nexus.gate.tool_filter.exclude` |
| Memory hierarchy (capped + summary_buffer + longterm + vector all in use) | each `config-*.yaml` plus `commonFactories()` in [main.go](main.go) |
| Chat envelope protocol (frontend side) | [frontend/dist/index.html](frontend/dist/index.html) — `chatView()` and `createBus()` |

## Differences from `cmd/desktop`

- Three agents instead of two; all conversational ReAct, no custom domain plugins.
- No file browser panel (kept the frontend lean — drop files in Librarian's
  watched folder via Finder instead).
- No session list panel (each agent restart opens a fresh session; the desktop
  framework still persists session metadata under `~/.nexus/sessions/`).
- Skills are scanned from `./cmd/demo/skills` — the demo currently expects to
  be launched from the repo root for that path to resolve.
