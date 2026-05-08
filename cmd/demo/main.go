// Command demo is the Nexus showcase application — a working,
// production-shaped harness app whose explicit job is to exercise as much
// of the Nexus plugin surface as possible in one cohesive product.
//
// The base product is a competitive-intelligence workbench. Six agents
// share one vector knowledge base inside a Wails desktop shell:
//
//   - Librarian        — KB curator (RAG ingest, longterm memory)
//   - Researcher       — multi-step web + KB research (RAG showroom)
//   - Drafter          — structured deliverable writer (skills, schemas)
//   - Engineer         — agent-does-work-on-disk (shell, codeexec, planexec)
//   - Orchestrator     — fan-out coordinator (subagent, fanout, gemini)
//   - Multimodal       — PDF + screenshots + multimodal embeddings
//
// In addition, `cmd/demo` doubles as a CLI when invoked with subcommands:
//
//	cmd/demo recipe <name>   # run a non-interactive showcase recipe
//
// Recipes (batch, eval, tui, browser-ui, voice, otel-trace, ...) live in
// the recipe runner registered alongside `desktop.Run`. They reuse the
// same plugin factories the Wails agents use, so anything you can build
// for one mode is buildable for the other.
//
// Why this app exists:
//   - Demo every plugin in `plugins/` somewhere reachable.
//   - Stay close to the cmd/desktop pattern so any improvements to the
//     shell framework benefit both apps.
//   - Be the canonical reference for "how do I wire feature X in Nexus".
//
// All three "core" agents (Librarian, Researcher, Drafter) share the
// chromem vector path. Cross-engine reads see a snapshot at session boot
// — chromem is in-memory with on-disk JSON persistence and does not
// hot-reload writes from sibling engines. Restart the consuming session
// (or recall it) after the Librarian ingests new content. Swapping in
// sqlite-vec / pgvector removes that limit; tracked as a follow-up.
package main

import (
	"embed"
	"log"

	"github.com/frankbardon/nexus/pkg/desktop"
	"github.com/frankbardon/nexus/pkg/engine"
	orchestratoragent "github.com/frankbardon/nexus/plugins/agents/orchestrator"
	planexecagent "github.com/frankbardon/nexus/plugins/agents/planexec"
	reactagent "github.com/frankbardon/nexus/plugins/agents/react"
	subagentplugin "github.com/frankbardon/nexus/plugins/agents/subagent"
	cancelplugin "github.com/frankbardon/nexus/plugins/control/cancel"
	hitlplugin "github.com/frankbardon/nexus/plugins/control/hitl"
	hitlsynth "github.com/frankbardon/nexus/plugins/control/hitl_synthesizer"
	progressivedisc "github.com/frankbardon/nexus/plugins/discovery/progressive"
	coheremultimodal "github.com/frankbardon/nexus/plugins/embeddings/cohere_multimodal"
	openaiembeddings "github.com/frankbardon/nexus/plugins/embeddings/openai"
	approvalpolicygate "github.com/frankbardon/nexus/plugins/gates/approval_policy"
	contentsafetygate "github.com/frankbardon/nexus/plugins/gates/content_safety"
	contextwindowgate "github.com/frankbardon/nexus/plugins/gates/context_window"
	endlessloopgate "github.com/frankbardon/nexus/plugins/gates/endless_loop"
	jsonschemagate "github.com/frankbardon/nexus/plugins/gates/json_schema"
	outputlengthgate "github.com/frankbardon/nexus/plugins/gates/output_length"
	promptinjectiongate "github.com/frankbardon/nexus/plugins/gates/prompt_injection"
	ratelimitergate "github.com/frankbardon/nexus/plugins/gates/rate_limiter"
	stopwordsgate "github.com/frankbardon/nexus/plugins/gates/stop_words"
	tokenbudgetgate "github.com/frankbardon/nexus/plugins/gates/token_budget"
	toolfiltergate "github.com/frankbardon/nexus/plugins/gates/tool_filter"
	tooltimeoutgate "github.com/frankbardon/nexus/plugins/gates/tool_timeout"
	wailsio "github.com/frankbardon/nexus/plugins/io/wails"
	"github.com/frankbardon/nexus/plugins/memory/capped"
	compactionmemory "github.com/frankbardon/nexus/plugins/memory/compaction"
	longtermplugin "github.com/frankbardon/nexus/plugins/memory/longterm"
	summarybuffer "github.com/frankbardon/nexus/plugins/memory/summary_buffer"
	tooldefpruner "github.com/frankbardon/nexus/plugins/memory/tool_def_pruner"
	toolresultclear "github.com/frankbardon/nexus/plugins/memory/tool_result_clear"
	topicpruner "github.com/frankbardon/nexus/plugins/memory/topic_pruner"
	vectormemory "github.com/frankbardon/nexus/plugins/memory/vector"
	thinkingobs "github.com/frankbardon/nexus/plugins/observe/thinking"
	dynamicplanner "github.com/frankbardon/nexus/plugins/planners/dynamic"
	staticplanner "github.com/frankbardon/nexus/plugins/planners/static"
	"github.com/frankbardon/nexus/plugins/providers/anthropic"
	fallbackprovider "github.com/frankbardon/nexus/plugins/providers/fallback"
	fanoutprovider "github.com/frankbardon/nexus/plugins/providers/fanout"
	"github.com/frankbardon/nexus/plugins/providers/gemini"
	"github.com/frankbardon/nexus/plugins/providers/openai"
	ragcitations "github.com/frankbardon/nexus/plugins/rag/citations"
	raghybrid "github.com/frankbardon/nexus/plugins/rag/hybrid"
	ragingest "github.com/frankbardon/nexus/plugins/rag/ingest"
	rerankercohere "github.com/frankbardon/nexus/plugins/rag/reranker/cohere"
	rerankerjina "github.com/frankbardon/nexus/plugins/rag/reranker/jina"
	rerankerlocal "github.com/frankbardon/nexus/plugins/rag/reranker/local"
	classifierrouter "github.com/frankbardon/nexus/plugins/router/classifier"
	metadatarouter "github.com/frankbardon/nexus/plugins/router/metadata"
	anthropicnativesearch "github.com/frankbardon/nexus/plugins/search/anthropic_native"
	bravesearch "github.com/frankbardon/nexus/plugins/search/brave"
	openainativesearch "github.com/frankbardon/nexus/plugins/search/openai_native"
	"github.com/frankbardon/nexus/plugins/skills"
	dynvarsplugin "github.com/frankbardon/nexus/plugins/system/dynvars"
	catalogplugin "github.com/frankbardon/nexus/plugins/tools/catalog"
	codeexectool "github.com/frankbardon/nexus/plugins/tools/codeexec"
	"github.com/frankbardon/nexus/plugins/tools/fileio"
	knowledgesearch "github.com/frankbardon/nexus/plugins/tools/knowledge_search"
	openertool "github.com/frankbardon/nexus/plugins/tools/opener"
	pdftool "github.com/frankbardon/nexus/plugins/tools/pdf"
	screenshottool "github.com/frankbardon/nexus/plugins/tools/screenshot"
	shelltool "github.com/frankbardon/nexus/plugins/tools/shell"
	webtool "github.com/frankbardon/nexus/plugins/tools/web"
	chromemvector "github.com/frankbardon/nexus/plugins/vectorstore/chromem"
	sqliteftsvector "github.com/frankbardon/nexus/plugins/vectorstore/sqlite_fts"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed config-librarian.yaml
var librarianConfig []byte

//go:embed config-researcher.yaml
var researcherConfig []byte

//go:embed config-drafter.yaml
var drafterConfig []byte

//go:embed config-engineer.yaml
var engineerConfig []byte

//go:embed config-orchestrator.yaml
var orchestratorConfig []byte

//go:embed config-multimodal.yaml
var multimodalConfig []byte

// commonFactories returns the plugin factories shared by every demo agent.
//
// Why one shared map instead of per-agent factory maps:
//
//	The desktop framework instantiates a fresh engine per agent. Each agent
//	independently chooses which plugin IDs to put in its `plugins.active`
//	YAML — so a factory being registered here costs ~zero if the agent
//	doesn't reference it. Sharing one map means:
//	  - new plugins get one-line registration and are immediately available
//	    to every agent
//	  - reading any agent config is enough to see which plugins it activates
//	    (no need to also read which factory map was passed in)
//	  - each agent's Factories field stays lean (just `commonFactories()`)
//
// New entries added during the showcase upgrade are grouped with comments
// explaining what they unlock so the demo stays self-documenting.
func commonFactories() map[string]func() engine.Plugin {
	return map[string]func() engine.Plugin{
		// ─── IO + agent core ───────────────────────────────────────────
		// nexus.io.wails: bridges the Wails webview JS runtime to the bus.
		// nexus.agent.react: ReAct loop (think→tool→observe).
		// nexus.control.cancel: /resume slash command + cancel capability.
		"nexus.io.wails":           wailsio.New,
		"nexus.agent.react":        reactagent.New,
		"nexus.agent.planexec":     planexecagent.New,
		"nexus.agent.orchestrator": orchestratoragent.New,
		"nexus.agent.subagent":     subagentplugin.New,
		"nexus.control.cancel":     cancelplugin.New,

		// ─── Cross-cutting human-in-the-loop (Phase 1) ─────────────────
		// hitl: owns the LLM-facing `ask_user` tool and the
		// hitl.requested/hitl.responded protocol. Any plugin (approval
		// gates, memory writes) can pause the loop and ask the user.
		// hitl_synthesizer: when emitters leave the prompt blank, the
		// synthesizer renders a context-aware approval question via a
		// small/cheap LLM (cached on disk per action signature).
		"nexus.control.hitl":             hitlplugin.New,
		"nexus.control.hitl_synthesizer": hitlsynth.New,

		// ─── Cross-cutting system var injection (Phase 1) ──────────────
		// dynvars: substitutes {{date}}, {{cwd}}, {{session_dir}} etc.
		// into system prompts at request time so prompts always reflect
		// the live environment without forcing the operator to template
		// strings by hand.
		"nexus.system.dynvars": dynvarsplugin.New,

		// ─── Providers ─────────────────────────────────────────────────
		// Three direct LLM providers (anthropic, openai, gemini) plus two
		// orchestration plugins (fallback for sequential retry chains;
		// fanout for parallel multi-provider dispatch with vote/judge
		// selection).
		"nexus.llm.anthropic":     anthropic.New,
		"nexus.llm.openai":        openai.New,
		"nexus.llm.gemini":        gemini.New,
		"nexus.provider.fallback": fallbackprovider.New,
		"nexus.provider.fanout":   fanoutprovider.New,

		// ─── Memory ────────────────────────────────────────────────────
		// capped/summary_buffer/longterm/vector are the four "history"
		// strategies. tool_result_clear and tool_def_pruner are
		// curators that run alongside any of them — they don't store
		// state themselves; they trim already-stored context based on
		// age and usage. Engineer uses both because long shell sessions
		// tend to accumulate huge stdout transcripts that would
		// otherwise blow the context window.
		"nexus.memory.capped":            capped.New,
		"nexus.memory.summary_buffer":    summarybuffer.New,
		"nexus.memory.longterm":          longtermplugin.New,
		"nexus.memory.vector":            vectormemory.New,
		"nexus.memory.tool_result_clear": toolresultclear.New,
		"nexus.memory.tool_def_pruner":   tooldefpruner.New,
		// compaction (Phase 5): event-driven external compaction
		// orchestrator. Unlike summary_buffer (which compacts inline),
		// compaction emits memory.compacted events that other history
		// buffers adopt — useful when the orchestrator's synth pass
		// processes a long worker-output transcript.
		"nexus.memory.compaction": compactionmemory.New,
		// topic_pruner (Phase 5): drops earlier turns when the user
		// pivots to a new topic. Embedding-based similarity check;
		// works with any embeddings.provider.
		"nexus.memory.topic_pruner": topicpruner.New,

		// ─── RAG primitives + consumers ────────────────────────────────
		// chromem (vector.store) + sqlite_fts (search.lexical) + rag/hybrid
		// (search.hybrid orchestrator) compose into a hybrid retrieval
		// stack. knowledge_search auto-detects search.hybrid and routes
		// queries through it; rag/ingest auto-dual-writes to sqlite_fts
		// when the lexical capability is active. Rerankers (local/cohere/
		// jina) plug in via the search.reranker capability — pin the one
		// you want under `capabilities` in the agent's YAML. citations
		// validates `<cite/>` tags or Anthropic native Citations against
		// rag.retrieved before the user-visible response is emitted.
		// embeddings.openai is the default text embedder. cohere_multimodal
		// is a separate embeddings.provider — it embeds images alongside
		// text and is used by the Multimodal Reader agent (Phase 6).
		"nexus.embeddings.openai":            openaiembeddings.New,
		"nexus.embeddings.cohere_multimodal": coheremultimodal.New,
		"nexus.vectorstore.chromem":          chromemvector.New,
		"nexus.vectorstore.sqlite_fts":       sqliteftsvector.New,
		"nexus.rag.ingest":                   ragingest.New,
		"nexus.rag.hybrid":                   raghybrid.New,
		"nexus.rag.citations":                ragcitations.New,
		"nexus.rag.reranker.local":           rerankerlocal.New,
		"nexus.rag.reranker.cohere":          rerankercohere.New,
		"nexus.rag.reranker.jina":            rerankerjina.New,
		"nexus.tool.knowledge_search":        knowledgesearch.New,

		// ─── Tools ─────────────────────────────────────────────────────
		// shell/code_exec/opener (Phase 4) are gated behind approval_policy
		// + tool_timeout when active. They are loaded as factories here so
		// any agent that lists them in `plugins.active` gets them; agents
		// that don't (Librarian/Researcher/Drafter) never instantiate them.
		"nexus.tool.file":       fileio.New,
		"nexus.tool.catalog":    catalogplugin.New,
		"nexus.tool.web":        webtool.New,
		"nexus.tool.shell":      shelltool.New,
		"nexus.tool.code_exec":  codeexectool.New,
		"nexus.tool.opener":     openertool.New,
		"nexus.tool.pdf":        pdftool.New,
		"nexus.tool.screenshot": screenshottool.New,

		// ─── Search providers (search.provider capability) ─────────────
		// Brave is the default external search backend. The two native
		// providers (anthropic_native / openai_native) wrap each LLM
		// vendor's server-side web_search tool — useful as fallbacks when
		// no Brave key is configured, or to experiment with provider-side
		// search quality. Capability resolution picks one; pin via
		// `capabilities.search.provider:` in YAML.
		"nexus.search.brave":            bravesearch.New,
		"nexus.search.anthropic_native": anthropicnativesearch.New,
		"nexus.search.openai_native":    openainativesearch.New,

		// ─── Routers ───────────────────────────────────────────────────
		// classifier: per-step LLM-judge that picks the cheapest model
		//   from a candidate list capable of answering the prompt.
		//   Recursion-guarded; cache-warmed; fires on before:llm.request.
		// metadata: deterministic rule-based routing on event metadata
		//   (e.g., subagent worker requests get tagged and routed to a
		//   cheaper model role than the orchestrator's main thread).
		//   Runs at higher priority than classifier; deterministic
		//   matches short-circuit before classifier fires.
		"nexus.router.classifier": classifierrouter.New,
		"nexus.router.metadata":   metadatarouter.New,

		// ─── Discovery ─────────────────────────────────────────────────
		// progressive: hierarchical tool discovery — the LLM only sees
		// class summaries up front; it drills into a class via a
		// "discover" meta-tool to expose the full set. Cuts tool-spec
		// token cost on agents with many tools (Engineer, Orchestrator).
		"nexus.discovery.progressive": progressivedisc.New,

		// ─── Planners ──────────────────────────────────────────────────
		// dynamic: LLM generates the plan from the user request.
		// static: plan steps come from YAML — used by the Drafter to
		//   force a deterministic retrieve→outline→draft→cite pipeline
		//   for skill-driven deliverables.
		"nexus.planner.dynamic": dynamicplanner.New,
		"nexus.planner.static":  staticplanner.New,

		// ─── Skills ────────────────────────────────────────────────────
		"nexus.skills": skills.New,

		// ─── Observers ─────────────────────────────────────────────────
		"nexus.observe.thinking": thinkingobs.New,

		// ─── Gates ─────────────────────────────────────────────────────
		// Most gates are per-agent opt-in (in their `plugins.active`
		// list). All registered factories are reusable across agents.
		// stop_words / tool_timeout were added in Phase 1 as universal
		// guard-rails.
		"nexus.gate.endless_loop":     endlessloopgate.New,
		"nexus.gate.content_safety":   contentsafetygate.New,
		"nexus.gate.token_budget":     tokenbudgetgate.New,
		"nexus.gate.rate_limiter":     ratelimitergate.New,
		"nexus.gate.context_window":   contextwindowgate.New,
		"nexus.gate.prompt_injection": promptinjectiongate.New,
		"nexus.gate.json_schema":      jsonschemagate.New,
		"nexus.gate.output_length":    outputlengthgate.New,
		"nexus.gate.tool_filter":      toolfiltergate.New,
		"nexus.gate.stop_words":       stopwordsgate.New,
		"nexus.gate.tool_timeout":     tooltimeoutgate.New,
		// approval_policy (Phase 3): config-driven HITL gate. Match a
		// tool name → render an approval prompt → wait for the user's
		// pick. Drafter uses this on file_write so unintended overwrites
		// require explicit confirmation.
		"nexus.gate.approval_policy": approvalpolicygate.New,
	}
}

func main() {
	if err := desktop.Run(&desktop.Shell{
		Title:  "Nexus Compete",
		Width:  1200,
		Height: 800,
		Assets: assets,
		// Keep all demo persistence — settings, session metadata, engine
		// session workspaces — under one tree, isolated from cmd/desktop
		// and any other Nexus apps. Per-agent configs point core.sessions.root,
		// longterm memory, and chromem storage at subdirectories of this.
		DataDir: "~/.nexus/demo",
		Agents: []desktop.Agent{
			librarianAgent(),
			researcherAgent(),
			drafterAgent(),
			engineerAgent(),
			orchestratorAgent(),
			multimodalAgent(),
		},
	}); err != nil {
		log.Fatalf("wails run: %v", err)
	}
}

func librarianAgent() desktop.Agent {
	return desktop.Agent{
		ID:          "librarian",
		Name:        "Librarian",
		Description: "Curates the competitor knowledge base",
		Icon:        "fa-solid fa-book",
		ConfigYAML:  librarianConfig,
		Factories:   commonFactories(),
		Settings: []desktop.SettingsField{
			sharedAnthropicKey(),
			sharedOpenAIKey(),
			{
				Key:         "input_dir",
				Display:     "Source documents",
				Description: "Folder watched for new competitor documents (auto-ingested into the shared knowledge base)",
				Type:        desktop.FieldPath,
				Required:    true,
			},
		},
	}
}

func researcherAgent() desktop.Agent {
	// Researcher is the demo's "RAG showroom" — it ships configured for
	// hybrid retrieval (vector + lexical) with a free local reranker by
	// default. Cohere and Jina rerankers are loaded as factories and can
	// be swapped in by editing `capabilities.search.reranker:` in the
	// YAML and providing a key here. Both keys are optional — the demo
	// runs fine without them.
	return desktop.Agent{
		ID:          "researcher",
		Name:        "Researcher",
		Description: "Multi-step research across web + KB (hybrid retrieval, citations)",
		Icon:        "fa-solid fa-magnifying-glass-chart",
		ConfigYAML:  researcherConfig,
		Factories:   commonFactories(),
		Settings: []desktop.SettingsField{
			sharedAnthropicKey(),
			sharedOpenAIKey(),
			{
				Key:         "brave_api_key",
				Display:     "Brave Search API Key",
				Description: "Required for web search via Brave. Free tier: https://brave.com/search/api/. To swap to Anthropic or OpenAI native search, edit capabilities.search.provider in config-researcher.yaml.",
				Type:        desktop.FieldString,
				Secret:      true,
				Required:    true,
			},
			{
				Key:         "cohere_api_key",
				Display:     "Cohere API Key (optional)",
				Description: "Optional: enables Cohere Rerank v3.5 as the search.reranker. Free trial: https://cohere.com/. Set capabilities.search.reranker to nexus.rag.reranker.cohere in YAML to activate.",
				Type:        desktop.FieldString,
				Secret:      true,
				Required:    false,
			},
			{
				Key:         "jina_api_key",
				Display:     "Jina API Key (optional)",
				Description: "Optional: enables Jina Reranker v2 as the search.reranker. Free tier: https://jina.ai/reranker. Set capabilities.search.reranker to nexus.rag.reranker.jina in YAML to activate.",
				Type:        desktop.FieldString,
				Secret:      true,
				Required:    false,
			},
		},
	}
}

// multimodalAgent — vision + document pipeline. Uses Gemini as primary
// (native multimodal in / out), Claude as fallback, Cohere multimodal
// embeddings for image+text joint vector search, and the read_pdf +
// take_screenshot tools to ingest non-text content.
//
// The point of this agent in the demo is twofold:
//   - Some plugins (PDF, screenshot, multimodal embeddings) only make
//     sense inside a vision/document workflow. They needed a home.
//   - Anyone evaluating Nexus for a "let me look at this" use case
//     can read this YAML and see the wiring end-to-end.
//
// Cohere is required for the multimodal embeddings; without a key the
// agent still loads, but knowledge-search style image lookups won't
// work. The PDF tool needs poppler-utils (pdftotext + pdfinfo) on PATH.
func multimodalAgent() desktop.Agent {
	return desktop.Agent{
		ID:          "multimodal",
		Name:        "Multimodal Reader",
		Description: "PDF + screenshot + image+text embeddings (Gemini primary)",
		Icon:        "fa-solid fa-file-image",
		ConfigYAML:  multimodalConfig,
		Factories:   commonFactories(),
		Settings: []desktop.SettingsField{
			sharedAnthropicKey(),
			{
				Key:         "gemini_api_key",
				Display:     "Google Gemini API Key",
				Description: "Primary LLM for the Multimodal Reader (vision + PDF native input). Free tier: https://aistudio.google.com/.",
				Type:        desktop.FieldString,
				Secret:      true,
				Required:    true,
			},
			{
				Key:         "cohere_multimodal_key",
				Display:     "Cohere API Key (multimodal embeddings)",
				Description: "Required for image+text joint embeddings. Free trial: https://cohere.com/. Same key works for Cohere Rerank if you want to enable it on the Researcher.",
				Type:        desktop.FieldString,
				Secret:      true,
				Required:    true,
			},
			{
				Key:         "input_dir",
				Display:     "Input folder",
				Description: "Folder the agent is allowed to read PDFs and other documents from.",
				Type:        desktop.FieldPath,
				Required:    true,
			},
		},
	}
}

// orchestratorAgent — fan-out coordinator. Decomposes a request into
// parallel worker subagents, then synthesizes their outputs into one
// answer. Also showcases provider fanout (same prompt to anthropic +
// openai + gemini, llm_judge picks the winner) and metadata-router
// rules that route worker traffic to the cheap model role
// deterministically.
//
// Compare with the other agents:
//   - Researcher does serial multi-step retrieval inside one ReAct loop.
//   - Engineer plans, then executes one step at a time.
//   - Orchestrator decomposes UP FRONT and runs N steps IN PARALLEL.
func orchestratorAgent() desktop.Agent {
	return desktop.Agent{
		ID:          "orchestrator",
		Name:        "Orchestrator",
		Description: "Decompose → parallel workers → synthesize (also: fanout-vote across 3 providers)",
		Icon:        "fa-solid fa-diagram-project",
		ConfigYAML:  orchestratorConfig,
		Factories:   commonFactories(),
		Settings: []desktop.SettingsField{
			sharedAnthropicKey(),
			sharedOpenAIKey(),
			{
				Key:         "gemini_api_key",
				Display:     "Google Gemini API Key",
				Description: "Required for the third leg of the fanout-vote. Free tier: https://aistudio.google.com/. Stored in OS keychain.",
				Type:        desktop.FieldString,
				Secret:      true,
				Required:    true,
			},
			{
				Key:         "brave_api_key",
				Display:     "Brave Search API Key",
				Description: "Worker subagents use this for web_search. Free tier: https://brave.com/search/api/.",
				Type:        desktop.FieldString,
				Secret:      true,
				Required:    true,
			},
		},
	}
}

// engineerAgent — agent-does-work-on-disk. Runs shell commands and Go
// code in a sandbox, plans before acting, and gates every dangerous
// operation behind an approval prompt. Compare with Drafter (skill-driven
// publishing) and Researcher (read-only investigation): the Engineer is
// the side of the demo that actually mutates the filesystem.
func engineerAgent() desktop.Agent {
	return desktop.Agent{
		ID:          "engineer",
		Name:        "Engineer",
		Description: "Plan-then-execute on shell + Go interpreter (HITL approval per call)",
		Icon:        "fa-solid fa-screwdriver-wrench",
		ConfigYAML:  engineerConfig,
		Factories:   commonFactories(),
		Settings: []desktop.SettingsField{
			sharedAnthropicKey(),
			{
				Key:         "workspace_dir",
				Display:     "Workspace folder",
				Description: "Directory the Engineer is allowed to read, write, and run shell commands in. Treat this as a sandbox — pick a fresh folder, not your home directory.",
				Type:        desktop.FieldPath,
				Required:    true,
			},
		},
	}
}

func drafterAgent() desktop.Agent {
	return desktop.Agent{
		ID:          "drafter",
		Name:        "Drafter",
		Description: "Writes structured competitor briefs",
		Icon:        "fa-solid fa-file-pen",
		ConfigYAML:  drafterConfig,
		Factories:   commonFactories(),
		Settings: []desktop.SettingsField{
			sharedAnthropicKey(),
			sharedOpenAIKey(),
			{
				Key:         "output_dir",
				Display:     "Brief output folder",
				Description: "Where finished competitor briefs are written.",
				Type:        desktop.FieldPath,
				Required:    true,
			},
		},
	}
}

func sharedAnthropicKey() desktop.SettingsField {
	return desktop.SettingsField{
		Key:         "shell.anthropic_api_key",
		Display:     "Anthropic API Key",
		Description: "Used by all agents (Claude Sonnet/Haiku). Stored in OS keychain.",
		Type:        desktop.FieldString,
		Secret:      true,
		Required:    true,
	}
}

func sharedOpenAIKey() desktop.SettingsField {
	return desktop.SettingsField{
		Key:         "shell.openai_api_key",
		Display:     "OpenAI API Key",
		Description: "Used for embeddings (RAG) by all agents and as Researcher LLM fallback. Stored in OS keychain.",
		Type:        desktop.FieldString,
		Secret:      true,
		Required:    true,
	}
}
