# Remote A2A Agents (`nexus.agent.a2a_remote`)

The **outbound** side of Nexus's [Agent2Agent integration](../../guides/a2a.md).
Where [`nexus.io.a2a`](../io/a2a.md) *serves* a Nexus instance as an A2A agent,
`nexus.agent.a2a_remote` lets a Nexus agent *call* remote A2A agents — any
service that speaks A2A, including another Nexus instance running
`nexus.io.a2a`.

Each configured remote is registered as an LLM-facing tool (default
`delegate_a2a_<name>`). When the parent agent calls it, the plugin resolves the
remote's Agent Card, sends the delegated task over the A2A wire through
`pkg/a2a/a2aclient`, and folds the remote task's final text and artifacts back
into the `tool.result` the parent expects.

From the parent agent's perspective a remote A2A call is a single tool call,
exactly like the local [`delegate`](delegate.md) and
[`agui_remote`](agui-remote.md) primitives — the transport just happens to be
the A2A wire.

## Details

| | |
|---|---|
| **ID** | `nexus.agent.a2a_remote` |
| **Dependencies** | *(none)* |
| **Requires** | `posture.registry` *(optional)* |
| **Source** | `plugins/agents/a2aremote/` |

`posture.registry` is optional here, unlike on [`delegate`](delegate.md): a
posture is one way to bound a remote and the plugin is fully usable without one.
A remote that *names* a posture with no registry active fails that **call**, with
an error naming the plugin to activate — it does not fail boot.

## Configuration

The full, authoritative key list lives in the
[configuration reference](../../configuration/reference.md#nexusagenta2a_remote).
In brief:

```yaml
plugins:
  active:
    - nexus.io.tui
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.agent.a2a_remote
    - nexus.memory.capped

  nexus.agent.a2a_remote:
    timeout: 3m
    agents:
      - name: researcher
        base_url: https://research.internal
        description: A specialist research agent reachable over A2A.
      - name: legal
        base_url: https://legal.internal
        tool_name: ask_legal
        stream: false
        timeout: 30s
```

Every transport key (`binding`, `stream`, the four timeouts, `retry`,
`extensions`, `validate_card`) exists at both levels: at the plugin level as a
default and inside an `agents[]` entry as an override.

### `base_url`, not an endpoint

`base_url` is the **origin** (plus optional path prefix) the remote agent is
served under, not an operation URL. The Agent Card is fetched from
`/.well-known/agent-card.json` beneath it and names the per-binding endpoints, so
an operator configures one URL rather than one URL per binding.

An operator who was handed a card out of band, or who knows the endpoint
outright, pins it with `jsonrpc_endpoint` or `rest_endpoint`, which skips
discovery for that binding entirely.

### Model-supplied URLs are out of scope

The tool schema exposes **no** `url`, `endpoint` or `host` parameter, and must
not grow one. Which remotes this instance can reach is an operator decision: a
model-chosen address is a server-side request forgery surface and an unbounded
spend surface at the same time, and neither is worth the flexibility.

## Discovery is lazy

A remote agent is somebody else's process. Its Agent Card is therefore fetched on
**first use**, never during `Ready()`:

- A remote that is down — restarting, not deployed yet, behind a VPN nobody has
  connected to — **cannot fail this instance's startup**.
- Until the card resolves, the tool carries the configured `description`, or a
  generic one naming the agent and saying the remote has not been contacted.
- The first successful call rebuilds the description from the card's own `name`,
  `version`, `description` and `skills`, and re-registers the tool **once**. The
  tool catalog replaces an entry registered under an existing name, so this is an
  update rather than a duplicate.
- `a2aclient` caches a card only on success, so a remote that comes up later
  resolves on the next call with no retry logic here.

## Tool definition

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `task` | string | yes | Natural-language description of what the remote agent should accomplish. The remote does not see the caller's conversation, so the task must stand alone. |
| `context` | object | no | Structured context passed alongside the task, serialized into the outbound message under an XML `<delegate_context>` boundary. |
| `timeout_seconds` | int | no | Override this call's time budget. |

## Budgets and depth

An `agents[]` entry may name a `posture`, in which case the registered
[`AgentPosture`](postures.md) bounds the call the same way it bounds a local
delegate.

Only two of a posture's dimensions cross an A2A boundary:

| Posture field | Effect |
|---|---|
| `default_budget.timeout` | The call's whole-run deadline. |
| `max_recursion_depth` | Narrows the plugin-level `max_depth` for this remote. |
| `default_budget.max_tokens` | **Refused.** |
| `default_budget.max_tool_calls` | **Refused.** |

The remote runs its own loop under its own budget; A2A gives a client no say over
its token or tool-call spend. A posture that sets either is refused with an error
naming the key rather than silently half-honoured — accepting a budget that
cannot be enforced would be the worse failure.

Timeout precedence, first match wins:

1. the tool's `timeout_seconds` argument
2. the posture's `default_budget.timeout`
3. the `timeout` key (agent-level, else plugin-level)
4. the `5m` built-in default

Delegation depth rides the bus's causation stack, so a remote call slots beneath
its caller in the causation tree exactly as a local delegate does.

## The result the model sees

A2A splits an answer between the terminal status message and the task's
artifacts (§3.7), and a remote is free to put its whole answer in either. Both
are folded into one XML-tagged document, per the house convention for
prompt-injected content:

```xml
<remote_agent name="researcher" state="TASK_STATE_COMPLETED" task_id="t-1" context_id="c-1">
<final_response>
<![CDATA[the agent's closing summary]]>
</final_response>
<artifacts count="2">
<artifact id="turn-answer" name="answer">
<text media_type="text/plain">
<![CDATA[the answer text]]>
</text>
</artifact>
<artifact id="tool-1" name="web_search result">
<data media_type="application/json">
<![CDATA[{ "hits": 3 }]]>
</data>
</artifact>
</artifacts>
</remote_agent>
```

- Remote-authored text rides in `CDATA`, and a remote-supplied `]]>` is split
  across two sections, so a remote cannot break the framing the model reads.
- Binary (`raw`) and external (`url`) parts are **described**, not inlined:
  `<binary bytes="9182" media_type="application/pdf" filename="report.pdf"/>`.
  Base64 in a prompt costs tokens and tells the model nothing it can use.
- An oversized part is truncated at 16 KiB and marked `truncated="true"` with its
  `original_bytes`, so the model can tell a fragment from a whole document.
- Parts carrying [Nexus extension](../../guides/a2a.md#the-nexus-extension-telemetry-a2a-has-no-field-for)
  telemetry are dropped: they are observability, not output.

## Event mapping

| Point | Emitted |
|---|---|
| `Ready()`, and once per remote after its card resolves | `tool.register` |
| Call starts (including a cache hit) | `subagent.started` |
| Call ends | `subagent.complete` — carries the folded result or the error |
| Result published | `before:tool.result` (vetoable), then `tool.result` |

Republishing a remote run's *incremental* progress — its text deltas, its own
tool calls, its subagent activity — is deliberately **not** done here yet, and
`Emissions()` does not claim it.

## Failure behavior

Every failure surfaces as a clean `tool.result` error carrying a sentence the
calling model can act on, alongside whatever partial output did arrive. None of
them is an engine-level failure, and the parent agent's loop continues normally.

| Condition | What the model is told |
|---|---|
| Agent Card unreachable / non-2xx | "the agent is unreachable — its agent card at `<url>` could not be fetched … The remote may be down; try again later or proceed without it." |
| Agent Card unparseable or non-conformant | "the agent card … is not usable … This is a misconfiguration on the remote, not something retrying will fix." |
| Card exposes no interface for the configured binding | "the agent does not expose the `<binding>` binding this instance is configured for" |
| Stream goes silent past `stream_idle_timeout` | "the agent went silent mid-run … Any output above is partial." |
| Stream never opens past `stream_open_timeout` | "the agent did not accept the streaming request in time." |
| Stream ends before a terminal state | "the agent closed the stream … without finishing its task. Any output above is partial." |
| Malformed / non-conformant frames | "the agent sent a response this client cannot read … a defect in the remote" |
| A2A protocol error from the remote | "the agent refused the request (`<ErrorType>`): …" |
| HTTP `401`/`403` | "The credentials this instance presents are not accepted; an operator must fix the configuration." |
| HTTP `429` / `5xx` | "The agent is rate limiting / failing on its side; try again later." |
| Whole-call budget exhausted | "the agent did not finish within the `<budget>` budget for this call. Any output above is partial." |
| Task ends `FAILED` / `REJECTED` | "ended its task in state `TASK_STATE_FAILED`: `<the remote's explanation>`" |
| Task ends `CANCELED` | "cancelled its task" |
| Task parks at `INPUT_REQUIRED` / `AUTH_REQUIRED` | "paused … and is waiting for input: `<the question>`. Re-delegate with the answer included in the task." |
| Delegation depth cap reached | "delegation depth limit reached … Answer from what you already have, or delegate from a shallower point." |
| Named posture missing or unenforceable | An error naming the posture and the key at fault. |

A remote parked at `INPUT_REQUIRED` is reported rather than chained into the
local human-in-the-loop surface; that chaining is separate work.

## Caching

Identical calls replay from a bounded in-process LRU keyed by a content hash of
the remote's identity, the posture version, the task, and the canonicalized
context — mirroring the local [`delegate`](delegate.md) cache, so a posture edit
invalidates stale entries.

**Only successes are cached.** A failed outcome is never stored, so a remote that
was briefly down, rate limited or mid-deploy is genuinely retried on the next
call rather than answering from a cached failure until the process restarts.

A cache hit still emits the `subagent.started` / `subagent.complete` pair so
observers see the call. Set `cache: false` to disable, `cache_size: 0` to disable
eviction.

## Credentials

The A2A client takes credentials through a `CredentialSource` seam, applied per
HTTP attempt so a source that refreshes an expired token does so on the retry
rather than replaying a stale one. The seam is wired
(`plugins/agents/a2aremote/credentials.go`) and every remote currently gets the
no-credential source, which is correct for a loopback or open endpoint.
Concrete bearer, OAuth2 and mTLS sources — and the config block that selects
between them — are separate work; the plugin page and the
[configuration reference](../../configuration/reference.md#nexusagenta2a_remote)
will grow a `credentials:` key when they land.

## Loopback (serve ↔ consume)

Because [`nexus.io.a2a`](../io/a2a.md) speaks the same wire, you can point
`nexus.agent.a2a_remote` at another Nexus instance's serve endpoint:

```yaml
  nexus.agent.a2a_remote:
    agents:
      - name: local_peer
        base_url: http://127.0.0.1:8091
```

This loopback topology is the cheapest faithful end-to-end proof of the outbound
path — one Nexus instance delegating to another over A2A, with no third-party
implementation in the test path.

## See also

- [A2A Interoperability](../../guides/a2a.md) — the protocol mapping and a
  worked client walkthrough
- [A2A Serve (`nexus.io.a2a`)](../io/a2a.md) — the serve side of the same wire
- [Delegate](delegate.md) / [Remote AG-UI Agents](agui-remote.md) — the sibling
  delegation primitives this mirrors
- [Posture Registry](postures.md) — where a remote's budget comes from
- [Configuration Reference — `nexus.agent.a2a_remote`](../../configuration/reference.md#nexusagenta2a_remote)
  — canonical key list
