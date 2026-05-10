# Nexus Compete — feature showcase demo

A competitive-intelligence workbench built on Nexus. Three specialized agents
share one vector knowledge base. The competitive-intel framing is just the
showcase — each agent is a generic Nexus pattern you can repurpose by swapping
the watched folder, the system prompts, and (for the Drafter) the skills.

| Agent | Role | What it shows off |
|---|---|---|
| **Librarian** | Curates the KB | RAG ingestion (watch mode), longterm memory, content-safety gate (redact mode), tool-filter gate, capability auto-resolution |
| **Researcher** | Multi-step research over web + KB | Hybrid retrieval (chromem vector + sqlite_fts BM25 with RRF fusion), search.reranker (local default; Cohere / Jina swap-in), rag/citations validation, per-step model routing via router/classifier, three search.provider options (Brave, Anthropic native, OpenAI native), provider fallback chain, parallel tool dispatch, dynamic planner with auto-approval, summary-buffer memory, vector memory, web tools, rate-limiter + prompt-injection + token-budget + context-window gates |
| **Drafter** | Writes structured deliverables | Skills with `output_schema`, structured output (Anthropic tool-sim path), static planner (deterministic 5-step pipeline), approval_policy gate (HITL on file_write), json-schema gate retry loop, output-length gate, content-safety gate (block mode), file_write tool |
| **Engineer** | Plan-then-execute on shell + Go interpreter | `nexus.agent.planexec` plan-execute loop, `tools/shell` (allowlisted commands, sandboxed env), `tools/code_exec` (Yaegi Go interpreter), `tools/opener`, approval_policy gate per call (HITL via synthesized prompts), tool_timeout per call, discovery/progressive (hierarchical tool exposure), memory/tool_result_clear (drops big stale stdout from history), memory/tool_def_pruner (drops unused tool defs from per-turn list) |
| **Orchestrator** | Decompose → parallel workers → synthesize | `nexus.agent.orchestrator` (decomposition), `nexus.agent.subagent` (worker primitive), `nexus.agent.react` (worker inner loop), `providers/fanout` with `llm_judge` strategy (same prompt → anthropic+openai+gemini in parallel → judged synthesis), `providers/gemini` (third LLM), `router/metadata` (deterministic routing of worker traffic to the cheap haiku role), `memory/compaction` (external, event-driven), `memory/topic_pruner` (drops earlier turns on topic shift) |
| **Multimodal Reader** | Vision + document pipeline | `tools/pdf` (pdftotext + pdfinfo, document/text modes), `tools/screenshot` (per-session blob store), `embeddings/cohere_multimodal` (image+text joint embedding), Gemini primary (native multimodal in/out), provider fallback to Anthropic, separate chromem path for the multimodal vector namespace |

All three exercise: desktop shell framework, multi-agent isolation, agent-contributed
settings + keychain secrets, the chat envelope protocol over `nexus.io.wails`,
the observe/logger + observe/thinking observers, and shared vector storage.

**Cross-cutting features (every agent):** human-in-the-loop `ask_user` tool
(`nexus.control.hitl` + `nexus.control.hitl_synthesizer`), dynamic system-prompt
variables like `{{date}}` / `{{session_dir}}` (`nexus.system.dynvars`),
`tool_timeout` safety net, and a `stop_words` banlist tuned per agent.

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

### Engineer — agent that does work on disk

**What it is.** A plan-then-execute agent that runs sandboxed shell
commands and Go code in your configured `workspace_dir`. It generates
a plan first (via `nexus.planner.dynamic`), then executes step by step
through `nexus.agent.planexec`. Every shell + `run_code` + `file_write`
call goes through `nexus.gate.approval_policy`, which renders an HITL
prompt in the chat panel so you confirm or skip before anything
touches the filesystem. `tools/shell` is locked to a small allowlist
(ls, cat, head, tail, wc, grep, find, git, go, make, tree, file, stat,
which, pwd, echo, date) and runs with `env_restrict: true`.
`tools/code_exec` uses Yaegi (in-process Go interpreter) with `net:
deny`. The agent has no web access and no `knowledge_search`; it's a
"bounded scratchpad" for inspecting and producing files. Long shell
sessions trim themselves with `memory/tool_result_clear` (drops
stdout-heavy turns) and `memory/tool_def_pruner` (drops unused tool
defs). Tool catalog uses `discovery/progressive` so the LLM only sees
the tools it's drilled into.

**Example prompts.**

- *"List the markdown files in the workspace and tell me which one
  was modified most recently."* (Single shell step; you approve `ls`,
  it runs, agent reports.)
- *"Compile a quick word-count of every .md file in here."* (Multi-
  step plan: `find` → `wc -l` → format output.)
- *"Write a small Go function that computes Fibonacci numbers, run it
  with input 20, and save the script to `fib.go`. Print the result."*
  (Demonstrates `code_exec` + `file_write` + approval gates on each.)
- *"Run `git status` and summarize what's uncommitted."*
- *"Try `rm -rf /` on the workspace."* (Watch the `stop_words` gate
  block it before approval is even asked.)

**Adaptation ideas — change the allowlist, get a new persona.**

- **Build-and-test agent.** Add `npm`, `pytest`, `cargo` to the shell
  allowlist; point `workspace_dir` at a project root. Ask: *"Run the
  full test suite and tell me which tests failed."*
- **Repo migrator.** Allowlist `git`, `gofmt`, `find`, `xargs`. Ask:
  *"Rename every occurrence of `OldType` to `NewType` across the
  repo, then verify the build still passes."*
- **Data wrangler.** Allowlist `jq`, `awk`, `sort`, `uniq`. Ask:
  *"Take the JSON in `events.jsonl` and produce a CSV summary by
  event type."*
- **Notebook-style scratchpad.** Lean entirely on `code_exec`; ask
  "show me a Go snippet that pretty-prints the first 100 prime
  numbers" and watch Yaegi run it without spawning a process.

### Orchestrator — fan-out coordinator

**What it is.** The "decompose, fan out, synthesize" pattern made
explicit. `nexus.agent.orchestrator` reads the user's request, splits
it into 2–8 independent subtasks, spawns parallel worker subagents
(via `nexus.agent.subagent`) capped by `max_workers: 4`, then runs a
synthesis pass to merge their outputs into one cited answer. Workers
run on the cheap `worker` model role (Claude Haiku) — `nexus.router.metadata`
deterministically rewrites every worker `llm.request` to that role —
so a 4-worker fan-out costs roughly the same as one Sonnet turn.
Workers can call `knowledge_search` and `web_search`. Long sessions
get external compaction (`memory/compaction`) and topic-shift pruning
(`memory/topic_pruner`). The agent also exposes a `vote` model role
that uses `nexus.provider.fanout` to send the same prompt to
Anthropic + OpenAI + Gemini in parallel, with `strategy: llm_judge`
picking the strongest answer.

**Example prompts.**

- *"Compare ACME, Vortex, Loom, and Pulp on pricing model, target
  buyer, time-to-value, and primary wedge."* (Splits into 4 worker
  subtasks — one per competitor — runs them in parallel, synthesizes
  a comparison table.)
- *"Build a market map of the agentic-RAG vendor space: group
  vendors by pricing model and list each one's primary wedge."*
- *"What are the three most-cited risks across our last six incident
  postmortems?"* (Fan out across the postmortems folder, synthesize.)
- *"Use the `vote` role to answer: 'In one sentence, what is the
  canonical use case for retrieval-augmented generation?'"* (Fans out
  to all three providers; the judge picks the best response.)

**Adaptation ideas.**

- **Multi-doc summarizer.** Drop a folder of long docs in via the
  Librarian; ask the Orchestrator to produce one-paragraph summaries
  per doc, then a meta-summary. The decomposition is automatic.
- **Comparative analyst.** Watch a folder of analyst reports; ask:
  *"What do all three analysts agree on about Vendor X, and where
  do they disagree?"*
- **Ensemble voting for tough questions.** Use the `vote` role on
  ambiguous prompts where you want a "second opinion" before acting.
- **Decompose-then-act.** Combine with the Engineer in a workflow:
  Orchestrator produces a plan list, you copy specific items into
  the Engineer for execution.

### Multimodal Reader — vision + document pipeline

**What it is.** A Gemini-primary agent built for non-text inputs.
`tools/pdf` extracts text via `pdftotext` (fast, lightweight) or
passes the raw PDF bytes to the LLM as a `file` MessagePart in
`document` mode (when layout matters: tables, math, signed forms).
`tools/screenshot` captures the current desktop and surfaces the PNG
back to the LLM as an image part. `embeddings/cohere_multimodal`
embeds images and text into a single vector space, stored in a
separate chromem path so the vectors don't collide with the text-only
`compete-kb` namespace used by the other agents. Provider fallback
goes Gemini → Claude on errors; long document-review sessions use
`memory/summary_buffer` to keep context fresh. Requires
`poppler-utils` on PATH (`pdftotext`, `pdfinfo`).

**Example prompts.**

- *"Read `quarterly-report.pdf` and tell me the three biggest risks
  flagged in the management discussion."* (Uses `text` mode by
  default — fast and cheap.)
- *"Look at the table on page 5 of `pricing-deck.pdf` and explain
  what's changed vs page 4."* (Switches to `document` mode so Gemini
  sees the layout natively.)
- *"Take a screenshot of my screen and tell me what's broken in the
  UI."* (Uses `take_screenshot`; Gemini reasons over the image.)
- *"Compare the two contracts in `vendor-a.pdf` and `vendor-b.pdf` —
  which one has stricter SLA terms?"* (Multi-doc, multimodal.)
- *"I uploaded a screenshot of an architecture diagram — describe
  what each component does."*

**Adaptation ideas.**

- **Receipt / invoice triage.** Watch a folder of receipt PDFs; ask
  the agent to extract vendor + total + date for each, write them to
  a CSV.
- **Contract review.** Drop NDAs / MSAs into the input folder; ask
  *"Flag any clause that limits our right to share redacted output
  with subprocessors."*
- **UI bug reporter.** Pair `take_screenshot` with a "describe what's
  broken" loop; produces issue tickets with attached screenshots.
- **Diagram explainer.** Drop architecture diagrams or flowcharts as
  PNGs; ask: *"Walk me through the request flow from end-user to
  database, naming every component."*

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
| `researcher.brave_api_key` | Researcher (web_search via Brave; can swap to vendor-native search) | OS keychain |
| `researcher.cohere_api_key` *(optional)* | Researcher (Cohere Rerank v3.5 — set `capabilities.search.reranker: nexus.rag.reranker.cohere` to use) | OS keychain |
| `researcher.jina_api_key` *(optional)* | Researcher (Jina Reranker v2 — set `capabilities.search.reranker: nexus.rag.reranker.jina` to use) | OS keychain |
| `librarian.input_dir` | Librarian (watched folder for ingest) | Plaintext setting |
| `drafter.output_dir` | Drafter (where briefs are saved) | Plaintext setting |
| `engineer.workspace_dir` | Engineer (sandbox folder for shell + file_write) | Plaintext setting |
| `orchestrator.gemini_api_key` | Orchestrator (third leg of fanout-vote) | OS keychain |
| `orchestrator.brave_api_key` | Orchestrator (web_search by worker subagents) | OS keychain |
| `multimodal.gemini_api_key` | Multimodal Reader (primary LLM) | OS keychain |
| `multimodal.cohere_multimodal_key` | Multimodal Reader (image+text embeddings) | OS keychain |
| `multimodal.input_dir` | Multimodal Reader (folder of PDFs / images) | Plaintext setting |

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

## CLI recipes

`cmd/demo` doubles as a CLI when invoked with the `recipe` subcommand.
Recipes are non-interactive scenarios that exercise plugins which don't
fit a chat UI — batch jobs, alternative IO transports, eval golden
traces, telemetry exports, voice loops, etc. They reuse the same
plugin factories the Wails desktop app uses.

```bash
cmd/demo                          # launch the desktop app (default)
cmd/demo recipe                   # list available recipes
cmd/demo recipe embeddings-mock   # run the named recipe
```

Phase 7 ships these recipes:

| Recipe | What it shows |
|---|---|
| `embeddings-mock` | `embeddings/mock` + chromem ingest pipeline; deterministic, no API key, CI-safe |
| `tui` | `io/tui` transport against the Researcher RAG showroom; same plugin set as the Wails Researcher, terminal interaction |
| `browser-ui` | `io/browser` HTTP/WS transport (default `127.0.0.1:8889`) — a no-Wails fallback for the Researcher |
| `batch-briefs` | `llm/batch` against Anthropic's Messages Batches API; submits N brief-generation requests, prints the batch ID, exits (real batches take 1+ hour to complete; state persists to `~/.nexus/batches`) |
| `eval` | `pkg/eval` golden-trace runner. Loads a case from `tests/eval/cases/<id>/`, replays it under the engine's stash mode (no API calls), and prints per-assertion pass/fail. Hermetic; CI-safe. Default case: `skills-discovery`. |
| `otel-trace` | `observe/otel` + `observe/sampler` enabled on a minimal Researcher engine. Exports OTLP spans to `127.0.0.1:4317` by default. Companion `recipe-otel-trace-docker-compose.yaml` boots Jaeger; bring it up first, then `open http://localhost:16686`. |
| `voice` | `io/voice` (VAD + Whisper ASR + OpenAI TTS) over `io/realtime` (low-latency WebSocket). Listens on `ws://127.0.0.1:8890/`. Connect a voice client and speak. |
| `fanout-vote` | `providers/fanout` with `llm_judge` strategy across anthropic + openai + gemini. Pure-CLI repro of the Orchestrator's vote behavior. Prints each leg's response plus the judge's pick. |

API keys for recipes come from environment variables (`ANTHROPIC_API_KEY`,
`OPENAI_API_KEY`, `BRAVE_API_KEY`) — recipes don't read the desktop
app's keychain settings.

## Differences from `cmd/desktop`

- Three agents instead of two; all conversational ReAct, no custom domain plugins.
- No file browser panel (kept the frontend lean — drop files in Librarian's
  watched folder via Finder instead).
- No session list panel (each agent restart opens a fresh session; the desktop
  framework still persists session metadata under `~/.nexus/sessions/`).
- Skills are scanned from `./cmd/demo/skills` — the demo currently expects to
  be launched from the repo root for that path to resolve.
