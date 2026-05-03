// Package allplugins provides a single RegisterAll function that registers
// every built-in plugin factory with an engine.PluginRegistry. Both cmd/nexus
// and pkg/testharness use this to stay in sync.
package allplugins

import (
	"github.com/frankbardon/nexus/pkg/engine"

	// Agent plugins.
	"github.com/frankbardon/nexus/plugins/agents/orchestrator"
	"github.com/frankbardon/nexus/plugins/agents/planexec"
	"github.com/frankbardon/nexus/plugins/agents/react"
	"github.com/frankbardon/nexus/plugins/agents/subagent"

	// Control plugins.
	cancelplugin "github.com/frankbardon/nexus/plugins/control/cancel"
	hitlplugin "github.com/frankbardon/nexus/plugins/control/hitl"
	hitlsynthplugin "github.com/frankbardon/nexus/plugins/control/hitl_synthesizer"

	// IO plugins.
	browserplugin "github.com/frankbardon/nexus/plugins/io/browser"
	oneshotplugin "github.com/frankbardon/nexus/plugins/io/oneshot"
	testioplugin "github.com/frankbardon/nexus/plugins/io/test"
	tuiplugin "github.com/frankbardon/nexus/plugins/io/tui"

	// LLM coordination plugins.
	batchplugin "github.com/frankbardon/nexus/plugins/llm/batch"

	// Memory plugins.
	"github.com/frankbardon/nexus/plugins/memory/capped"
	compactionplugin "github.com/frankbardon/nexus/plugins/memory/compaction"
	longtermplugin "github.com/frankbardon/nexus/plugins/memory/longterm"
	"github.com/frankbardon/nexus/plugins/memory/simple"
	"github.com/frankbardon/nexus/plugins/memory/summary_buffer"
	vectorplugin "github.com/frankbardon/nexus/plugins/memory/vector"

	// Observer plugins.
	otelplugin "github.com/frankbardon/nexus/plugins/observe/otel"
	samplerplugin "github.com/frankbardon/nexus/plugins/observe/sampler"
	thinkingplugin "github.com/frankbardon/nexus/plugins/observe/thinking"

	// Planner plugins.
	dynamicplanner "github.com/frankbardon/nexus/plugins/planners/dynamic"
	staticplanner "github.com/frankbardon/nexus/plugins/planners/static"

	// Provider plugins.
	"github.com/frankbardon/nexus/plugins/providers/anthropic"
	fallbackplugin "github.com/frankbardon/nexus/plugins/providers/fallback"
	fanoutplugin "github.com/frankbardon/nexus/plugins/providers/fanout"
	"github.com/frankbardon/nexus/plugins/providers/gemini"
	"github.com/frankbardon/nexus/plugins/providers/openai"

	// Search provider plugins (advertise the "search.provider" capability).
	anthropicnativesearch "github.com/frankbardon/nexus/plugins/search/anthropic_native"
	bravesearch "github.com/frankbardon/nexus/plugins/search/brave"
	geminativesearch "github.com/frankbardon/nexus/plugins/search/gemini_native"
	openainativesearch "github.com/frankbardon/nexus/plugins/search/openai_native"

	// Embeddings provider plugins (advertise the "embeddings.provider" capability).
	mockembeddings "github.com/frankbardon/nexus/plugins/embeddings/mock"
	openaiembeddings "github.com/frankbardon/nexus/plugins/embeddings/openai"

	// Vector store plugins (advertise the "vector.store" capability).
	chromemvector "github.com/frankbardon/nexus/plugins/vectorstore/chromem"

	// RAG plugins.
	ragingest "github.com/frankbardon/nexus/plugins/rag/ingest"

	// Discovery plugins.
	progressiveplugin "github.com/frankbardon/nexus/plugins/discovery/progressive"

	// Skill plugins.
	"github.com/frankbardon/nexus/plugins/skills"

	// System plugins.
	dynvarsplugin "github.com/frankbardon/nexus/plugins/system/dynvars"

	// Tool plugins.
	catalogplugin "github.com/frankbardon/nexus/plugins/tools/catalog"
	codeexecplugin "github.com/frankbardon/nexus/plugins/tools/codeexec"
	"github.com/frankbardon/nexus/plugins/tools/fileio"
	knowledgesearchplugin "github.com/frankbardon/nexus/plugins/tools/knowledge_search"
	openerplugin "github.com/frankbardon/nexus/plugins/tools/opener"
	pdfplugin "github.com/frankbardon/nexus/plugins/tools/pdf"
	"github.com/frankbardon/nexus/plugins/tools/shell"
	webplugin "github.com/frankbardon/nexus/plugins/tools/web"

	// Gate plugins.
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

	// Sandbox backends. Side-effect imports register the factories with
	// pkg/engine/sandbox so configs can opt into stricter isolation tiers.
	_ "github.com/frankbardon/nexus/pkg/engine/sandbox/wasm"
)

// RegisterAll registers every built-in plugin factory with the given registry.
// Call this once after engine.New / engine.NewFromBytes to wire up all plugins
// before Boot.
func RegisterAll(r *engine.PluginRegistry) {
	// Agents
	r.Register("nexus.agent.react", react.New)
	r.Register("nexus.agent.planexec", planexec.New)
	r.Register("nexus.agent.orchestrator", orchestrator.New)
	r.Register("nexus.agent.subagent", subagent.New)

	// Control
	r.Register("nexus.control.cancel", cancelplugin.New)
	r.Register("nexus.control.hitl_synthesizer", hitlsynthplugin.New)

	// IO
	r.Register("nexus.io.tui", tuiplugin.New)
	r.Register("nexus.io.browser", browserplugin.New)
	r.Register("nexus.io.oneshot", oneshotplugin.New)
	r.Register("nexus.io.test", testioplugin.New)

	// LLM coordination
	r.Register("nexus.llm.batch", batchplugin.New)

	// Memory
	r.Register("nexus.memory.simple", simple.New)
	r.Register("nexus.memory.capped", capped.New)
	r.Register("nexus.memory.summary_buffer", summary_buffer.New)
	r.Register("nexus.memory.compaction", compactionplugin.New)
	r.Register("nexus.memory.longterm", longtermplugin.New)
	r.Register("nexus.memory.vector", vectorplugin.New)

	// Observers
	r.Register("nexus.observe.thinking", thinkingplugin.New)
	r.Register("nexus.observe.otel", otelplugin.New)
	// Sampler is registered but off by default — only fires when
	// `nexus.observe.sampler` is in plugins.active and `enabled: true`.
	r.Register("nexus.observe.sampler", samplerplugin.New)

	// Planners
	r.Register("nexus.planner.dynamic", dynamicplanner.New)
	r.Register("nexus.planner.static", staticplanner.New)

	// Providers
	r.Register("nexus.llm.anthropic", anthropic.New)
	r.Register("nexus.llm.openai", openai.New)
	r.Register("nexus.llm.gemini", gemini.New)
	r.Register("nexus.provider.fallback", fallbackplugin.New)
	r.Register("nexus.provider.fanout", fanoutplugin.New)

	// Search providers (capability: search.provider)
	r.Register("nexus.search.brave", bravesearch.New)
	r.Register("nexus.search.anthropic_native", anthropicnativesearch.New)
	r.Register("nexus.search.openai_native", openainativesearch.New)
	r.Register("nexus.search.gemini_native", geminativesearch.New)

	// Embeddings providers (capability: embeddings.provider)
	r.Register("nexus.embeddings.openai", openaiembeddings.New)
	r.Register("nexus.embeddings.mock", mockembeddings.New)

	// Vector stores (capability: vector.store)
	r.Register("nexus.vectorstore.chromem", chromemvector.New)

	// RAG
	r.Register("nexus.rag.ingest", ragingest.New)

	// Discovery
	r.Register("nexus.discovery.progressive", progressiveplugin.New)

	// Skills
	r.Register("nexus.skills", skills.New)

	// System
	r.Register("nexus.system.dynvars", dynvarsplugin.New)

	// Tools
	r.Register("nexus.tool.shell", shell.New)
	r.Register("nexus.tool.file", fileio.New)
	r.Register("nexus.tool.catalog", catalogplugin.New)
	r.Register("nexus.tool.pdf", pdfplugin.New)
	r.Register("nexus.tool.opener", openerplugin.New)
	r.Register("nexus.control.hitl", hitlplugin.New)
	r.Register("nexus.tool.code_exec", codeexecplugin.New)
	r.Register("nexus.tool.web", webplugin.New)
	r.Register("nexus.tool.knowledge_search", knowledgesearchplugin.New)

	// Gates
	r.Register("nexus.gate.content_safety", contentsafetygate.New)
	r.Register("nexus.gate.context_window", contextwindowgate.New)
	r.Register("nexus.gate.endless_loop", endlessloopgate.New)
	r.Register("nexus.gate.json_schema", jsonschemagate.New)
	r.Register("nexus.gate.output_length", outputlengthgate.New)
	r.Register("nexus.gate.prompt_injection", promptinjectiongate.New)
	r.Register("nexus.gate.rate_limiter", ratelimitergate.New)
	r.Register("nexus.gate.stop_words", stopwordsgate.New)
	r.Register("nexus.gate.token_budget", tokenbudgetgate.New)
	r.Register("nexus.gate.tool_filter", toolfiltergate.New)
}
