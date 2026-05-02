# Nexus

Nexus is a modular AI agent harness built on a pure event-driven architecture in Go. The core manages only the event lifecycle and plugin registry — **all behavior is delivered through composable plugins**.

This means you can assemble exactly the agent you need by choosing which plugins to activate, how to configure them, and optionally writing your own.

## Why Nexus?

- **Event-driven core** — Plugins never call each other directly. All communication flows through a central typed event bus, making the system loosely coupled and easy to extend.
- **Composable by design** — Pick an agent strategy (ReAct, Plan & Execute, Orchestrator), pair it with tools, memory, and an I/O interface, and you have a working agent.
- **Minimal dependencies** — Only `gopkg.in/yaml.v3` beyond the Go standard library. The Anthropic API is called via raw HTTP — no SDK required.
- **Configuration-driven** — YAML profiles let you define entirely different agent behaviors without changing code.
- **Session persistence** — Every session captures conversation history, thinking steps, plans, and file artifacts to a structured workspace on disk.

## What You Can Build

- **Coding assistants** with shell access, file I/O, and planning capabilities
- **Research agents** with large context windows and no tool access
- **Multi-agent workflows** using the orchestrator to decompose tasks across worker subagents
- **Document analysis pipelines** with PDF extraction and skill-based instructions
- **Custom domain agents** by writing your own plugins and skills

## How This Documentation is Organized

| Section | What you'll find |
|---------|-----------------|
| **Getting Started** | Installation, building from source, and creating your first config |
| **Architecture** | Deep dive into the engine, event bus, plugin system, and session management |
| **Plugin Reference** | Every built-in plugin with its configuration, events, and use cases |
| **Reference** | Complete event type catalog and configuration reference |
| **[Eval Harness](./eval/overview.md)** | Golden-trace replay, baseline diffs, online sampling, Inspect-AI-compatible JSON protocol |
| **Guides** | Tutorials for writing skills and creating custom plugins |

## Quick Start

```bash
# Clone and build
git clone https://github.com/frankbardon/nexus.git
cd nexus
make build

# Set your API key
export ANTHROPIC_API_KEY="sk-ant-..."

# Run with the default profile
bin/nexus -config configs/default.yaml
```

See [Installation](./getting-started/installation.md) for full details.
