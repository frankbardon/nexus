package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/frankbardon/nexus/pkg/engine"

	// Import all plugin packages.
	"github.com/frankbardon/nexus/plugins/agents/orchestrator"
	cancelplugin "github.com/frankbardon/nexus/plugins/control/cancel"
	"github.com/frankbardon/nexus/plugins/agents/planexec"
	"github.com/frankbardon/nexus/plugins/agents/react"
	"github.com/frankbardon/nexus/plugins/agents/subagent"
	browserplugin "github.com/frankbardon/nexus/plugins/io/browser"
	oneshotplugin "github.com/frankbardon/nexus/plugins/io/oneshot"
	tuiplugin "github.com/frankbardon/nexus/plugins/io/tui"
	compactionplugin "github.com/frankbardon/nexus/plugins/memory/compaction"
	"github.com/frankbardon/nexus/plugins/memory/conversation"
	"github.com/frankbardon/nexus/plugins/observe/logger"
	otelplugin "github.com/frankbardon/nexus/plugins/observe/otel"
	thinkingplugin "github.com/frankbardon/nexus/plugins/observe/thinking"
	dynamicplanner "github.com/frankbardon/nexus/plugins/planners/dynamic"
	staticplanner "github.com/frankbardon/nexus/plugins/planners/static"
	"github.com/frankbardon/nexus/plugins/providers/anthropic"
	"github.com/frankbardon/nexus/plugins/providers/openai"
	"github.com/frankbardon/nexus/plugins/skills"
	dynvarsplugin "github.com/frankbardon/nexus/plugins/system/dynvars"

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

	"github.com/frankbardon/nexus/plugins/tools/ask"
	"github.com/frankbardon/nexus/plugins/tools/fileio"
	openerplugin "github.com/frankbardon/nexus/plugins/tools/opener"
	pdfplugin "github.com/frankbardon/nexus/plugins/tools/pdf"
	"github.com/frankbardon/nexus/plugins/tools/shell"
)

func main() {
	configPath := flag.String("config", "nexus.yaml", "path to config file")
	recallSession := flag.String("recall", "", "session ID to recall and resume")
	flag.Parse()

	// When recalling, load config from the session's snapshot.
	effectiveConfig := *configPath
	if *recallSession != "" {
		snapshotPath, err := engine.SessionConfigSnapshotPath(*configPath, *recallSession)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to locate session config: %v\n", err)
			os.Exit(1)
		}
		effectiveConfig = snapshotPath
	}

	// Create engine.
	eng, err := engine.New(effectiveConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create engine: %v\n", err)
		os.Exit(1)
	}

	eng.RecallSessionID = *recallSession

	// Register all plugins.
	eng.Registry.Register("nexus.llm.anthropic", anthropic.New)
	eng.Registry.Register("nexus.llm.openai", openai.New)
	eng.Registry.Register("nexus.agent.react", react.New)
	eng.Registry.Register("nexus.agent.planexec", planexec.New)
	eng.Registry.Register("nexus.agent.orchestrator", orchestrator.New)
	eng.Registry.Register("nexus.agent.subagent", subagent.New)
	eng.Registry.Register("nexus.tool.shell", shell.New)
	eng.Registry.Register("nexus.tool.file", fileio.New)
	eng.Registry.Register("nexus.tool.pdf", pdfplugin.New)
	eng.Registry.Register("nexus.tool.opener", openerplugin.New)
	eng.Registry.Register("nexus.tool.ask", ask.New)
	eng.Registry.Register("nexus.memory.conversation", conversation.New)
	eng.Registry.Register("nexus.memory.compaction", compactionplugin.New)
	eng.Registry.Register("nexus.io.tui", tuiplugin.New)
	eng.Registry.Register("nexus.io.browser", browserplugin.New)
	eng.Registry.Register("nexus.io.oneshot", oneshotplugin.New)
	eng.Registry.Register("nexus.skills", skills.New)
	eng.Registry.Register("nexus.observe.logger", logger.New)
	eng.Registry.Register("nexus.planner.dynamic", dynamicplanner.New)
	eng.Registry.Register("nexus.planner.static", staticplanner.New)
	eng.Registry.Register("nexus.observe.thinking", thinkingplugin.New)
	eng.Registry.Register("nexus.observe.otel", otelplugin.New)
	eng.Registry.Register("nexus.control.cancel", cancelplugin.New)
	eng.Registry.Register("nexus.system.dynvars", dynvarsplugin.New)

	// Gate plugins.
	eng.Registry.Register("nexus.gate.content_safety", contentsafetygate.New)
	eng.Registry.Register("nexus.gate.context_window", contextwindowgate.New)
	eng.Registry.Register("nexus.gate.endless_loop", endlessloopgate.New)
	eng.Registry.Register("nexus.gate.json_schema", jsonschemagate.New)
	eng.Registry.Register("nexus.gate.output_length", outputlengthgate.New)
	eng.Registry.Register("nexus.gate.prompt_injection", promptinjectiongate.New)
	eng.Registry.Register("nexus.gate.rate_limiter", ratelimitergate.New)
	eng.Registry.Register("nexus.gate.stop_words", stopwordsgate.New)
	eng.Registry.Register("nexus.gate.token_budget", tokenbudgetgate.New)
	eng.Registry.Register("nexus.gate.tool_filter", toolfiltergate.New)

	// Run handles SIGINT/SIGTERM internally; embedders call Boot/Stop directly.
	if err := eng.Run(context.Background()); err != nil {
		eng.Logger.Error("engine error", "error", err)
		os.Exit(1)
	}
}
