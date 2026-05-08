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
	reactagent "github.com/frankbardon/nexus/plugins/agents/react"
	cancelplugin "github.com/frankbardon/nexus/plugins/control/cancel"
	hitlplugin "github.com/frankbardon/nexus/plugins/control/hitl"
	hitlsynth "github.com/frankbardon/nexus/plugins/control/hitl_synthesizer"
	openaiembeddings "github.com/frankbardon/nexus/plugins/embeddings/openai"
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
	longtermplugin "github.com/frankbardon/nexus/plugins/memory/longterm"
	summarybuffer "github.com/frankbardon/nexus/plugins/memory/summary_buffer"
	vectormemory "github.com/frankbardon/nexus/plugins/memory/vector"
	thinkingobs "github.com/frankbardon/nexus/plugins/observe/thinking"
	dynamicplanner "github.com/frankbardon/nexus/plugins/planners/dynamic"
	"github.com/frankbardon/nexus/plugins/providers/anthropic"
	fallbackprovider "github.com/frankbardon/nexus/plugins/providers/fallback"
	"github.com/frankbardon/nexus/plugins/providers/openai"
	ragingest "github.com/frankbardon/nexus/plugins/rag/ingest"
	bravesearch "github.com/frankbardon/nexus/plugins/search/brave"
	"github.com/frankbardon/nexus/plugins/skills"
	dynvarsplugin "github.com/frankbardon/nexus/plugins/system/dynvars"
	catalogplugin "github.com/frankbardon/nexus/plugins/tools/catalog"
	"github.com/frankbardon/nexus/plugins/tools/fileio"
	knowledgesearch "github.com/frankbardon/nexus/plugins/tools/knowledge_search"
	webtool "github.com/frankbardon/nexus/plugins/tools/web"
	chromemvector "github.com/frankbardon/nexus/plugins/vectorstore/chromem"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed config-librarian.yaml
var librarianConfig []byte

//go:embed config-researcher.yaml
var researcherConfig []byte

//go:embed config-drafter.yaml
var drafterConfig []byte

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
		"nexus.io.wails":       wailsio.New,
		"nexus.agent.react":    reactagent.New,
		"nexus.control.cancel": cancelplugin.New,

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
		"nexus.llm.anthropic":     anthropic.New,
		"nexus.llm.openai":        openai.New,
		"nexus.provider.fallback": fallbackprovider.New,

		// ─── Memory ────────────────────────────────────────────────────
		"nexus.memory.capped":         capped.New,
		"nexus.memory.summary_buffer": summarybuffer.New,
		"nexus.memory.longterm":       longtermplugin.New,
		"nexus.memory.vector":         vectormemory.New,

		// ─── RAG primitives + consumers ────────────────────────────────
		"nexus.embeddings.openai":     openaiembeddings.New,
		"nexus.vectorstore.chromem":   chromemvector.New,
		"nexus.rag.ingest":            ragingest.New,
		"nexus.tool.knowledge_search": knowledgesearch.New,

		// ─── Tools ─────────────────────────────────────────────────────
		"nexus.tool.file":    fileio.New,
		"nexus.tool.catalog": catalogplugin.New,
		"nexus.tool.web":     webtool.New,

		// ─── Search providers (search.provider capability) ─────────────
		"nexus.search.brave": bravesearch.New,

		// ─── Planner ───────────────────────────────────────────────────
		"nexus.planner.dynamic": dynamicplanner.New,

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
	return desktop.Agent{
		ID:          "researcher",
		Name:        "Researcher",
		Description: "Multi-step research across web + KB",
		Icon:        "fa-solid fa-magnifying-glass-chart",
		ConfigYAML:  researcherConfig,
		Factories:   commonFactories(),
		Settings: []desktop.SettingsField{
			sharedAnthropicKey(),
			sharedOpenAIKey(),
			{
				Key:         "brave_api_key",
				Display:     "Brave Search API Key",
				Description: "Required for web search. Free tier at https://brave.com/search/api/.",
				Type:        desktop.FieldString,
				Secret:      true,
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
