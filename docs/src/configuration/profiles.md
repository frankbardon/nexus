# Built-in Profiles

Nexus ships with a set of configuration profiles in `configs/` that cover common use cases. Each example file is intentionally minimal — it contains only the plugins required to exercise the part of the system it demonstrates. Use them as-is or as starting points for your own configurations.

## default.yaml

**General-purpose conversational agent** with a subagent but no tool access.

```bash
bin/nexus -config configs/default.yaml
```

| Setting | Value |
|---------|-------|
| Agent | ReAct (10 iterations) |
| System prompt | `prompts/conversationalist.md` |
| Tools | None |
| Memory | Conversation (100 messages, persisted) |
| Plugins | TUI, Anthropic, ReAct, Subagent, Conversation, Logger |

Best for: Chat-only interactions, brainstorming, Q&A.

---

## coding.yaml

**Coding assistant** with shell access, file I/O, and skills.

```bash
bin/nexus -config configs/coding.yaml
```

| Setting | Value |
|---------|-------|
| Agent | ReAct (10 iterations) |
| System prompt | `prompts/coding-assistant.md` |
| Tools | Shell (allowlisted), File I/O |
| Memory | Conversation (100 messages, persisted) |
| Plugins | TUI, Anthropic, ReAct, Shell, File, Conversation, Logger |

Shell allowlist: `go`, `git`, `ls`, `cat`, `grep`, `find`, `mkdir`, `rm`, `cp`, `mv`, `make`, `docker`, `npm`, `cargo`, `python`

Best for: Software development tasks, code generation, debugging.

---

## research.yaml

**Research agent** with no tool access but a larger iteration budget and conversation buffer.

```bash
bin/nexus -config configs/research.yaml
```

| Setting | Value |
|---------|-------|
| Agent | ReAct (15 iterations) |
| System prompt | `prompts/research-assistant.md` |
| Tools | File I/O (no shell) |
| Memory | Conversation (200 messages, persisted) |
| Plugins | TUI, Anthropic, ReAct, File, Conversation, Logger |

Best for: Analysis, research, document review, long-form reasoning.

---

## planned.yaml

**Dynamic planner** wired into the ReAct agent with auto-approval.

```bash
bin/nexus -config configs/planned.yaml
```

| Setting | Value |
|---------|-------|
| Agent | ReAct (10 iterations, planning enabled) |
| Planner | Dynamic (auto-approval, reasoning model, max 10 steps) |
| System prompt | `prompts/conversationalist.md` |
| Tools | None |
| Memory | Conversation (100 messages, persisted) |
| Plugins | TUI, Anthropic, ReAct, Dynamic Planner, Conversation, Logger |

Best for: Exercising the dynamic planner / ReAct integration. Add tool plugins (e.g. `nexus.tool.shell`, `nexus.tool.file`) if you want the generated plan to act on the system.

---

## planned-static.yaml

**Static planner** with a fixed coding workflow and no approval required.

```bash
bin/nexus -config configs/planned-static.yaml
```

| Setting | Value |
|---------|-------|
| Agent | ReAct (10 iterations, planning enabled) |
| Planner | Static (no approval) |
| System prompt | `prompts/coding-assistant.md` |
| Tools | None |
| Memory | Conversation (100 messages, persisted) |
| Plugins | TUI, Anthropic, ReAct, Static Planner, Conversation, Logger |

Static plan steps:
1. Analyze the request and identify affected files
2. Plan the implementation approach
3. Implement the changes
4. Verify correctness
5. Summarize what was done

Best for: Enforcing a consistent development workflow across all requests.

---

## oneshot.yaml

**Non-interactive single-turn agent** that emits a JSON transcript to stdout. Ideal for scripting, batch jobs, and CI. One prompt in, one transcript out, then the process exits.

```bash
echo "What is 2+2?" | bin/nexus -config configs/oneshot.yaml | jq .
```

| Setting | Value |
|---------|-------|
| Agent | ReAct (10 iterations) |
| I/O | `nexus.io.oneshot` (stdin prompt → JSON stdout) |
| System prompt | `prompts/conversationalist.md` |
| Tools | None |
| Memory | Conversation (100 messages, persisted) |
| Plugins | Oneshot, Anthropic, ReAct, Conversation, Logger |

Every approval request is auto-approved. See [Oneshot I/O](../plugins/io/oneshot.md) for the full transcript schema and prompt-resolution rules.

Best for: CI pipelines, shell scripting, feeding Nexus output into `jq` / other tools.

---

## oneshot-planned.yaml

**Oneshot variant with the dynamic planner** and `approval: always`, used to exercise the auto-approval path end-to-end.

```bash
echo "Explain the plan step" | bin/nexus -config configs/oneshot-planned.yaml
```

Same output format as `oneshot.yaml`, but the transcript additionally includes `plans`, `plan_updates`, `thinking`, and an `approvals` array logging the auto-granted plan approvals.

Best for: Testing planner workflows without a human in the loop.

---

## browser.yaml

**Minimal browser-UI example** that swaps the TUI for the in-browser IO plugin.

```bash
bin/nexus -config configs/browser.yaml
```

| Setting | Value |
|---------|-------|
| Agent | ReAct (10 iterations) |
| I/O | `nexus.io.browser` (localhost:8080, auto-opens browser) |
| System prompt | `prompts/conversationalist.md` |
| Tools | None |
| Memory | Conversation (100 messages, persisted) |
| Plugins | Browser, Anthropic, ReAct, Conversation, Logger |

Best for: Exercising the browser IO plugin.

---

## browser-planned.yaml

**Browser IO combined with the dynamic planner**, auto-approving generated plans.

```bash
bin/nexus -config configs/browser-planned.yaml
```

| Setting | Value |
|---------|-------|
| Agent | ReAct (10 iterations, planning enabled) |
| I/O | `nexus.io.browser` |
| Planner | Dynamic (auto-approval, reasoning model, max 10 steps) |
| System prompt | `prompts/conversationalist.md` |
| Memory | Conversation (100 messages, persisted) |
| Plugins | Browser, Anthropic, ReAct, Dynamic Planner, Conversation, Logger |

Best for: End-to-end planner demos rendered in the browser UI.

---

## orchestrator.yaml

**Orchestrator agent** delegating subtasks to `nexus.agent.subagent` workers.

```bash
bin/nexus -config configs/orchestrator.yaml
```

| Setting | Value |
|---------|-------|
| Agent | Orchestrator (max 3 workers, 5 subtasks) |
| Worker | `nexus.agent.subagent/main` (8 iterations) |
| Memory | Conversation (100 messages, persisted) |
| Plugins | TUI, Anthropic, Orchestrator, Subagent, Conversation, Logger |

Best for: Exercising the orchestrator / subagent fan-out pattern.

---

## planexec.yaml

**PlanExec agent** driven by the dynamic planner with auto-approval.

```bash
bin/nexus -config configs/planexec.yaml
```

| Setting | Value |
|---------|-------|
| Agent | PlanExec (10 iterations, replan on failure) |
| Planner | Dynamic (no approval, balanced model, max 6 steps) |
| System prompt | `prompts/coding-assistant.md` |
| Memory | Conversation (100 messages, persisted) |
| Plugins | TUI, Anthropic, PlanExec, Dynamic Planner, Conversation, Logger |

Best for: Exercising the PlanExec agent's plan-then-execute loop.

---

## rag.yaml

**RAG-enabled agent** wired with embeddings, vector store, ingest plugin, knowledge-search tool, and vector memory.

```bash
bin/nexus -config configs/rag.yaml
```

| Setting | Value |
|---------|-------|
| Agent | ReAct (10 iterations) |
| LLM | Anthropic (default model: balanced) |
| Embeddings | `nexus.embeddings.openai` (text-embedding-3-small) |
| Vector store | `nexus.vectorstore.chromem` (default `~/.nexus/vectors/`) |
| Tools | `knowledge_search` over namespaces `[kb, project-docs]` |
| Memory | Conversation (100 messages) + per-agent vector recall |
| Plugins | TUI, Anthropic, ReAct, OpenAI Embeddings, Chromem, RAG Ingest, Knowledge Search Tool, Vector Memory, Logger |

Best for: Q&A grounded in a knowledge base. Pre-populate the vector store with `nexus ingest --namespace=kb ./your-docs` before starting the agent. See the [RAG guide](../guides/rag.md) for the full walkthrough.

Requires both `ANTHROPIC_API_KEY` (LLM) and `OPENAI_API_KEY` (embeddings) — they're independent keys.

---

## Creating Your Own Profile

Start by copying an existing profile and modifying it:

```bash
cp configs/coding.yaml configs/my-agent.yaml
# Edit to taste
bin/nexus -config configs/my-agent.yaml
```

See the [Configuration Reference](./reference.md) for all available options.
