// Command demo is the Nexus showcase desktop app: a competitive-intelligence
// workbench with three specialized agents (Librarian, Researcher, Drafter)
// that share one vector knowledge base.
//
// Goals:
//   - Exercise as much of Nexus's surface as possible in one cohesive product:
//     desktop shell, multi-agent isolation, RAG primitives, capabilities,
//     gates (input/output/cost), planner+approval, parallel tool dispatch,
//     skills with output schemas, provider fallback, structured output,
//     long-term + vector memory.
//   - Stay close to the cmd/desktop pattern so any improvements to the shell
//     framework benefit both apps.
//
// All three agents share the chromem vector path (see VectorPath constant).
// Cross-engine reads see a snapshot at session boot — chromem is in-memory
// with on-disk JSON persistence and does not hot-reload writes from sibling
// engines. Restart the consuming session (or recall it) after the Librarian
// ingests new content. Swapping in sqlite-vec / pgvector removes that limit.
package main

import (
	"embed"
	"log"

	"github.com/frankbardon/nexus/pkg/desktop"
	"github.com/frankbardon/nexus/pkg/engine"
	reactagent "github.com/frankbardon/nexus/plugins/agents/react"
	cancelplugin "github.com/frankbardon/nexus/plugins/control/cancel"
	openaiembeddings "github.com/frankbardon/nexus/plugins/embeddings/openai"
	contentsafetygate "github.com/frankbardon/nexus/plugins/gates/content_safety"
	contextwindowgate "github.com/frankbardon/nexus/plugins/gates/context_window"
	endlessloopgate "github.com/frankbardon/nexus/plugins/gates/endless_loop"
	jsonschemagate "github.com/frankbardon/nexus/plugins/gates/json_schema"
	outputlengthgate "github.com/frankbardon/nexus/plugins/gates/output_length"
	promptinjectiongate "github.com/frankbardon/nexus/plugins/gates/prompt_injection"
	ratelimitergate "github.com/frankbardon/nexus/plugins/gates/rate_limiter"
	tokenbudgetgate "github.com/frankbardon/nexus/plugins/gates/token_budget"
	toolfiltergate "github.com/frankbardon/nexus/plugins/gates/tool_filter"
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

// commonFactories returns the plugin factories shared by all three demo
// agents. Each Agent's Factories map starts from this and adds anything
// agent-specific (none currently — every plugin is reusable).
func commonFactories() map[string]func() engine.Plugin {
	return map[string]func() engine.Plugin{
		// IO + agent core
		"nexus.io.wails":       wailsio.New,
		"nexus.agent.react":    reactagent.New,
		"nexus.control.cancel": cancelplugin.New,

		// Providers
		"nexus.llm.anthropic":     anthropic.New,
		"nexus.llm.openai":        openai.New,
		"nexus.provider.fallback": fallbackprovider.New,

		// Memory
		"nexus.memory.capped":         capped.New,
		"nexus.memory.summary_buffer": summarybuffer.New,
		"nexus.memory.longterm":       longtermplugin.New,
		"nexus.memory.vector":         vectormemory.New,

		// RAG primitives + consumers
		"nexus.embeddings.openai":     openaiembeddings.New,
		"nexus.vectorstore.chromem":   chromemvector.New,
		"nexus.rag.ingest":            ragingest.New,
		"nexus.tool.knowledge_search": knowledgesearch.New,

		// Tools
		"nexus.tool.file":    fileio.New,
		"nexus.tool.catalog": catalogplugin.New,
		"nexus.tool.web":     webtool.New,

		// Search providers (search.provider capability)
		"nexus.search.brave": bravesearch.New,

		// Planner
		"nexus.planner.dynamic": dynamicplanner.New,

		// Skills
		"nexus.skills": skills.New,

		// Observers
		"nexus.observe.thinking": thinkingobs.New,

		// Gates
		"nexus.gate.endless_loop":     endlessloopgate.New,
		"nexus.gate.content_safety":   contentsafetygate.New,
		"nexus.gate.token_budget":     tokenbudgetgate.New,
		"nexus.gate.rate_limiter":     ratelimitergate.New,
		"nexus.gate.context_window":   contextwindowgate.New,
		"nexus.gate.prompt_injection": promptinjectiongate.New,
		"nexus.gate.json_schema":      jsonschemagate.New,
		"nexus.gate.output_length":    outputlengthgate.New,
		"nexus.gate.tool_filter":      toolfiltergate.New,
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
