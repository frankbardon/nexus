# Summary

[Introduction](./introduction.md)

# Getting Started

- [Installation](./getting-started/installation.md)
- [Your First Configuration](./getting-started/first-config.md)

# Architecture

- [Overview](./architecture/overview.md)
- [Event Bus](./architecture/event-bus.md)
- [Plugin System](./architecture/plugin-system.md)
- [Sessions](./architecture/sessions.md)
- [Model Registry](./architecture/models.md)
- [Prompt Registry](./architecture/prompts.md)

# Desktop Shell

- [Overview](./desktop/overview.md)
- [Building Your App](./desktop/building-your-app.md)
- [API Reference](./desktop/reference.md)
- [Design System (Brief)](./desktop/design-system-brief.md)
- [Design System (Full Reference)](./desktop/design-system.md)

# Plugin Reference

- [Overview](./plugins/overview.md)
- [Agents](./plugins/agents/index.md)
  - [ReAct Agent](./plugins/agents/react.md)
  - [Plan & Execute Agent](./plugins/agents/planexec.md)
  - [Subagent](./plugins/agents/subagent.md)
  - [Orchestrator](./plugins/agents/orchestrator.md)
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
  - [Ask User](./plugins/tools/ask.md)
  - [Code Exec (run_code)](./plugins/tools/code_exec.md)
  - [Knowledge Search](./plugins/tools/knowledge_search.md)
- [Memory](./plugins/memory/index.md)
  - [Simple History](./plugins/memory/simple.md)
  - [Capped History](./plugins/memory/capped.md)
  - [Summary-Buffer History](./plugins/memory/summary_buffer.md)
  - [Context Compaction](./plugins/memory/compaction.md)
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
- [Observers](./plugins/observers/index.md)
  - [Event Logger](./plugins/observers/logger.md)
  - [Thinking Persistence](./plugins/observers/thinking.md)
  - [OpenTelemetry](./plugins/observers/otel.md)
- [Planners](./plugins/planners/index.md)
  - [Dynamic Planner](./plugins/planners/dynamic.md)
  - [Static Planner](./plugins/planners/static.md)
- [Gates](./plugins/gates/index.md)
- [Skills](./plugins/skills.md)
- [System](./plugins/system.md)
- [Control](./plugins/control.md)

# Reference

- [Event Types](./events/reference.md)
- [Configuration Reference](./configuration/reference.md)

# Guides

- [Writing Skills](./skills/authoring.md)
- [Structured Output](./guides/structured-output.md)
- [Web Search & Fetch](./guides/web-search.md)
- [Retrieval-Augmented Generation (RAG)](./guides/rag.md)
- [Integration Testing](./guides/integration-testing.md)
- [Creating a Custom Plugin](./skills/custom-plugin.md)
