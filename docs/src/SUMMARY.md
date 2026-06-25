# Summary

[Introduction](./introduction.md)

# Getting Started

- [Installation](./getting-started/installation.md)
- [Your First Configuration](./getting-started/first-config.md)

# Architecture

- [Overview](./architecture/overview.md)
- [Event Bus](./architecture/event-bus.md)
- [Events](./architecture/events.md)
- [Causation](./architecture/causation.md)
- [Plugin System](./architecture/plugin-system.md)
- [Sessions](./architecture/sessions.md)
- [Per-Plugin Storage](./architecture/storage.md)
- [Model Registry](./architecture/models.md)
- [Prompt Registry](./architecture/prompts.md)
- [Hot Reload](./architecture/hot-reload.md)
- [Context Engineering](./architecture/context-engineering.md)
- [Postures](./architecture/postures.md)
- [Sub-agent Delegation](./architecture/delegate.md)
- [Scenes](./architecture/scenes.md)
- [Streaming Tools](./architecture/streaming-tools.md)
- [Replay](./architecture/replay.md)

# Desktop Shell

- [Overview](./desktop/overview.md)
- [Building Your App](./desktop/building-your-app.md)
- [API Reference](./desktop/reference.md)
- [Design System (Brief)](./desktop/design-system-brief.md)
- [Design System (Full Reference)](./desktop/design-system.md)

# ICM Workflows

- [Overview](./icm/overview.md)
- [End-to-End Walkthrough](./icm/walkthrough.md)
- [Plugin Reference](./plugins/workflows-icm.md)

# Plugin Reference

- [Overview](./plugins/overview.md)
- [Agents](./plugins/agents/index.md)
  - [ReAct Agent](./plugins/agents/react.md)
  - [Plan & Execute Agent](./plugins/agents/planexec.md)
  - [Subagent](./plugins/agents/subagent.md)
  - [Orchestrator](./plugins/agents/orchestrator.md)
  - [Posture Registry](./plugins/agents/postures.md)
  - [Delegate](./plugins/agents/delegate.md)
- [LLM Providers](./plugins/providers/index.md)
  - [Anthropic (Claude)](./plugins/providers/anthropic.md)
  - [OpenAI](./plugins/providers/openai.md)
  - [Gemini (Google)](./plugins/providers/gemini.md)
  - [Fallback](./plugins/providers/fallback.md)
  - [Fanout](./plugins/providers/fanout.md)
- [Tools](./plugins/tools/index.md)
  - [Shell](./plugins/tools/shell.md)
  - [File I/O](./plugins/tools/file.md)
  - [PDF Reader](./plugins/tools/pdf.md)
  - [File Opener](./plugins/tools/opener.md)
  - [Code Exec (run_code)](./plugins/tools/code_exec.md)
  - [Knowledge Search](./plugins/tools/knowledge_search.md)
- [Memory](./plugins/memory/index.md)
  - [Simple History](./plugins/memory/simple.md)
  - [Capped History](./plugins/memory/capped.md)
  - [Summary-Buffer History](./plugins/memory/summary_buffer.md)
  - [Context Compaction](./plugins/memory/compaction.md)
  - [Tool-Result Clearing](./plugins/memory/tool_result_clear.md)
  - [Tool-Definition Pruner](./plugins/memory/tool_def_pruner.md)
  - [Topic-Aware Pruner](./plugins/memory/topic_pruner.md)
  - [Long-Term Memory](./plugins/memory/longterm.md)
  - [Vector Memory](./plugins/memory/vector.md)
- [Embeddings](./plugins/embeddings/index.md)
  - [OpenAI](./plugins/embeddings/openai.md)
  - [Mock](./plugins/embeddings/mock.md)
- [Vector Stores](./plugins/vectorstore/index.md)
  - [Chromem-go](./plugins/vectorstore/chromem.md)
- [RAG](./plugins/rag/index.md)
  - [Ingest](./plugins/rag/ingest.md)
- [I/O Interfaces](./plugins/io/index.md)
  - [Terminal UI (TUI)](./plugins/io/tui.md)
  - [Browser UI](./plugins/io/browser.md)
  - [Oneshot](./plugins/io/oneshot.md)
  - [Test IO](./plugins/io/test.md)
  - [Wails Desktop](./plugins/io/wails.md)
  - [Broker IO (dial-back)](./plugins/io/broker.md)
- [Observers](./plugins/observers/index.md)
  - [Thinking Persistence](./plugins/observers/thinking.md)
  - [OpenTelemetry](./plugins/observers/otel.md)
  - [Online Sampler](./plugins/observers/sampler.md)
- [Planners](./plugins/planners/index.md)
  - [Dynamic Planner](./plugins/planners/dynamic.md)
  - [Static Planner](./plugins/planners/static.md)
- [Gates](./plugins/gates/index.md)
- [Skills](./plugins/skills.md)
- [Scenes](./plugins/scene.md)
- [System](./plugins/system.md)
- [Control](./plugins/control.md)
- [MCP Client](./plugins/mcp-client.md)

# Reference

- [Event Types](./events/reference.md)
- [Configuration Reference](./configuration/reference.md)
- [Sandboxing](./security/sandboxing.md)
- [Native Realtime API — deferred](./multimodal/native-realtime-deferred.md)

# Eval Harness

- [Overview](./eval/overview.md)
- [Quickstart](./eval/quickstart.md)
- [Case Format](./eval/case-format.md)
- [Promoting a Session](./eval/promotion.md)
- [Inspect-Mode Protocol](./eval/inspect-protocol.md)

# Guides

- [Writing Skills](./skills/authoring.md)
- [Structured Output](./guides/structured-output.md)
- [Web Search & Fetch](./guides/web-search.md)
- [Retrieval-Augmented Generation (RAG)](./guides/rag.md)
- [Integration Testing](./guides/integration-testing.md)
- [Plugin Contract Tests](./guides/plugin-contracts.md)
- [Session Broker](./guides/session-broker.md)
- [Creating a Custom Plugin](./skills/custom-plugin.md)
