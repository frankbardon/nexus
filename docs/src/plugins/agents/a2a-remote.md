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
    hitl:
      input_timeout: 10m
      max_rounds: 3
    agents:
      - name: researcher
        base_url: https://research.internal
        description: A specialist research agent reachable over A2A.
      - name: legal
        base_url: https://legal.internal
        tool_name: ask_legal
        stream: false
        timeout: 30s
        progress: false
        hitl:
          enabled: false
```

Every transport key (`binding`, `stream`, the four timeouts, `retry`,
`extensions`, `validate_card`, `progress`, `hitl`) exists at both levels: at the
plugin level as a default and inside an `agents[]` entry as an override. The
`hitl` block inherits key by key, so an agent that sets only `enabled` keeps the
plugin-level `input_timeout` and `max_rounds`.

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
| Remote narrates on a non-terminal status message | `io.output` (under the delegated run's own turn id, `a2a_remote_<spawn>`) |
| Remote reports a tool call or subagent progress via the Nexus extension | `subagent.iteration` |
| Remote parks at `INPUT_REQUIRED` | `before:hitl.requested` (vetoable), then `hitl.requested` |
| A question is abandoned or the turn is cancelled | `hitl.cancel` |
| Call ends | `subagent.complete` — carries the folded result or the error |
| Result published | `before:tool.result` (vetoable), then `tool.result` |

Subscriptions: `tool.invoke`, `hitl.responded` (the human's answer, from
whichever transport rendered the question) and `cancel.active`.

`hitl.responded` is conspicuously **absent** from the emissions and must stay
absent — this plugin asks questions and waits, it never answers one. The contract
test asserts it.

## Live progress

A delegated call takes as long as the remote's work does. Without republishing,
the only thing a local transport sees is a `subagent.started`, a long silence and
a `subagent.complete` — an operator cannot tell a remote that is working from one
that has hung, and the browser, AG-UI and A2A-serve transports have nothing to
render either.

So each frame the remote streams is mapped onto the bus as it arrives, following
the [`agui_remote`](agui-remote.md) precedent:

| Frame | Republished as | Why |
|---|---|---|
| Non-terminal status update carrying a message | `io.output` | A2A's own extension-free progress channel (§3.1.1): the remote narrating. |
| Nexus extension `tool_call` telemetry | `subagent.iteration` with the call | The remote's own tool use, which A2A has no canonical field for. |
| Nexus extension `subagent` telemetry | `subagent.iteration` with the phase and detail | The remote's own delegations. |
| Artifact frames | *(nothing)* | An artifact is **output**, and all of it is folded into the tool result. Emitting it twice would put the remote's answer in the local conversation before the delegating agent had decided what to do with it. |
| Terminal status message | *(nothing)* | That is the answer, and it rides in the tool result. |
| `INPUT_REQUIRED` status message | *(nothing)* | That is a question for a human, not progress — see below. |
| Nexus extension `thinking` / `usage` telemetry | *(nothing)* | Reasoning belongs in the remote's transcript; tokens are the remote's spend under the remote's budget. Surfacing either locally would misattribute it. |

Because the tool-call and subagent rows depend on the Nexus extension, the
`extensions` key **defaults to the Nexus extension URI**. A remote that has never
heard of it answers exactly as it would have (§8.4 requires a server to activate
only extensions it recognizes, and this one declares itself optional); a remote
Nexus instance answers with the telemetry that makes the table above useful. Set
`extensions: []` to send none.

Set `progress: false` (plugin-wide or per agent) to silence the republishing for
a chatty remote. Task identity is still tracked, so cancellation and resumption
are unaffected.

## Chained human-in-the-loop

When a remote parks its task at `TASK_STATE_INPUT_REQUIRED`, the question travels
on the status message (§3.1.1) and the task stays **live**. A2A has no resume
operation: the task is continued by sending an **ordinary message carrying the
same `taskId` and `contextId`** (§3.4), and that identity is what makes the
message a continuation rather than a new conversation.

```
remote parks at INPUT_REQUIRED
      -> before:hitl.requested (vetoable)  -> hitl.requested
      -> [ a human answers, via any transport ]  -> hitl.responded
      -> SendStreamingMessage with the SAME taskId + contextId
      -> remote continues to a terminal state
```

**The delegating model never sees the question.** That is the point. A question a
remote agent cannot answer for itself is almost always one only a person can
settle — which deployment, which fiscal year, whose budget — and handing it to
the model that asked for the delegation invites it to invent an answer and then
act on it. There is no code path that gives the model one.

`nexus.control.hitl` is reached **only over the bus**, exactly as the approval
gates and memory plugins reach it. It need not even be active: any transport that
renders `hitl.requested` and answers with `hitl.responded` serves.

**It composes.** A Nexus instance serving over [`nexus.io.a2a`](../io/a2a.md)
turns its own `hitl.requested` into an `INPUT_REQUIRED` status; this plugin turns
an inbound `INPUT_REQUIRED` into a local `hitl.requested`. Chain two of them and a
question raised two hops down arrives in front of the human at the top, each hop
resuming its own task under its own `taskId`.

### Deadlines while parked

Two run concurrently and the earlier one wins.

| Deadline | Default | What it bounds |
|---|---|---|
| `timeout` (the whole-call budget) | `5m` | The entire delegation. **It keeps running while the task is parked** — a remote waiting on a human is still work this session authorized. |
| `hitl.input_timeout` | `15m` | One question waiting on a human. The outbound twin of `nexus.io.a2a`'s `tasks.input_timeout`. |

With the defaults the **call budget expires first**, which makes `input_timeout`
the looser of the two; raise `timeout` for a remote you expect to ask questions.
Whichever fires:

1. the question is retracted with `hitl.cancel`, so no stale prompt is left in a
   UI or in the hitl registry's on-disk queue;
2. the remote task is cancelled with `CancelTask`, so nobody is left working for
   a caller that has gone away;
3. the delegation ends as a clean tool error naming **which** deadline fired,
   carrying the question, and telling the model explicitly not to answer it.

`hitl.max_rounds` (default `4`) bounds a remote that answers every answer with
another question; `0` removes the cap and leaves the call budget as the only one.

### Chaining needs `stream: false` against a remote that holds the stream open

**Known limitation, and it bites the Nexus→Nexus case at the default settings.**

A2A leaves it to the server whether an `INPUT_REQUIRED` park closes the SSE
stream or holds it open, and both readings are legal. This plugin reads a stream
to its **end** before it acts on what it read, so:

| Remote's behaviour at `INPUT_REQUIRED` | `stream: true` (default) | `stream: false` |
|---|---|---|
| Closes the stream (§11.7 permits it) | Chaining works | Chaining works |
| Holds the stream open — **which is what [`nexus.io.a2a`](../io/a2a.md) does**, with keep-alive comments | The question never reaches a human; the delegation ends when the call budget or `stream_idle_timeout` fires | Chaining works |

So a Nexus instance delegating to another Nexus instance that may ask questions
must set `stream: false` for that remote today. The blocking `SendMessage`
binding returns the parked Task as soon as it parks (§3.2.2), which is exactly
the signal the chaining path needs. The cost is losing [live
progress](#live-progress) for that remote, since there are no frames to
republish.

`tests/integration/a2a_loopback_test.go` pins the working shape; the streaming
shape is a recorded defect, not a design decision.

`AUTH_REQUIRED` is **not** routed to a human. The remote is asking for a
credential, and no answer a person types is one — the fix is a `credentials`
block, and the tool error says so.

Outcomes a human answered for are **never cached**: a person's answer is a
decision made at a moment, and replaying it for a later identical task would
apply that decision again without asking.

## Cancellation

`cancel.active` — the event `nexus.control.cancel` emits once a cancellation is
actually happening, and the same one the LLM providers abort on — propagates to
every remote in flight:

1. any question this delegation put in front of a human is retracted with
   `hitl.cancel`;
2. `CancelTask` (§3.3) is issued for every remote task whose id is known;
3. the call's context is cancelled, so the stream reader unblocks and the tool
   result is published as a cancellation rather than a hang.

The same abandonment runs on the ordinary exits — an exhausted budget, a broken
stream, an unanswered question, engine shutdown. The rule is one sentence: if
this instance walks away from a remote task that has not reached a terminal
state, it tells the remote. A task that already finished is left alone.

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
| HTTP `401`/`403` | "The credentials this instance presents are not accepted; an operator must fix the configuration." — *when the remote reports the refusal as an HTTP status.* A remote that answers a refusal inside a JSON-RPC error envelope instead (which `nexus.io.a2a` does) surfaces as the protocol-error row above; either way the delegation fails cleanly and the message names the refusal. |
| HTTP `429` / `5xx` | "The agent is rate limiting / failing on its side; try again later." |
| Whole-call budget exhausted | "the agent did not finish within the `<budget>` budget for this call. Any output above is partial." |
| Task ends `FAILED` / `REJECTED` | "ended its task in state `TASK_STATE_FAILED`: `<the remote's explanation>`" |
| Task ends `CANCELED` | "cancelled its task" |
| Task parks at `INPUT_REQUIRED`, chaining **off** | "paused … and is waiting for input: `<the question>`. Re-delegate with the answer included in the task." |
| Task parks at `INPUT_REQUIRED`, question unanswered | "asked a question and no answer arrived within `<deadline>` … It was put to a human and is unanswered — do NOT answer it on their behalf." |
| Task parks at `INPUT_REQUIRED`, question declined | "asked a question and it was declined: `<the human's reason>` … do NOT answer it on their behalf." |
| Remote asks more than `hitl.max_rounds` times | "asked for input `<n>` times in one delegation, which is the configured limit … Try a more specific task, or raise `hitl.max_rounds`." |
| Task parks at `AUTH_REQUIRED` | "it needs credentials this instance did not present … An operator must configure this agent's credentials; retrying will not fix it." |
| Delegation depth cap reached | "delegation depth limit reached … Answer from what you already have, or delegate from a shallower point." |
| Named posture missing or unenforceable | An error naming the posture and the key at fault. |

## Caching

Identical calls replay from a bounded in-process LRU keyed by a content hash of
the remote's identity, the posture version, the task, and the canonicalized
context — mirroring the local [`delegate`](delegate.md) cache, so a posture edit
invalidates stale entries.

**Only successes are cached, and only ones no human answered for.** A failed
outcome is never stored, so a remote that was briefly down, rate limited or
mid-deploy is genuinely retried on the next call rather than answering from a
cached failure until the process restarts. A delegation a human answered a
question for is not stored either — see [Chained
human-in-the-loop](#chained-human-in-the-loop).

A cache hit still emits the `subagent.started` / `subagent.complete` pair so
observers see the call. Set `cache: false` to disable, `cache_size: 0` to disable
eviction.

## Credentials

Each remote names the credential this instance presents to it in its own
`credentials:` block. Four types are supported — `none`, `bearer`,
`oauth2_client_credentials` and `mtls` — and the full key list is in the
[configuration reference](../../configuration/reference.md#nexusagenta2a_remote).

```yaml
  nexus.agent.a2a_remote:
    agents:
      # An open endpoint: a loopback peer, a development agent.
      - name: local_peer
        base_url: http://127.0.0.1:8091

      # A static token, the same api_key / api_key_env shape the LLM
      # providers use.
      - name: researcher
        base_url: https://research.internal
        credentials:
          type: bearer
          token_env: RESEARCH_AGENT_TOKEN

      # Machine-to-machine OAuth2. token_url is optional: it is discovered
      # from the remote's own card on first use.
      - name: legal
        base_url: https://legal.internal
        credentials:
          type: oauth2_client_credentials
          client_id_env: LEGAL_CLIENT_ID
          client_secret_env: LEGAL_CLIENT_SECRET
          scopes: [a2a.invoke]

      # Client-certificate authentication. Paths take ~.
      - name: finance
        base_url: https://finance.internal
        credentials:
          type: mtls
          cert_file: ~/.nexus/certs/finance-client.pem
          key_file: ~/.nexus/certs/finance-client-key.pem
          ca_file: ~/.nexus/certs/internal-ca.pem
```

### Per remote, never inherited

Unlike every transport key, `credentials:` exists **only** inside an `agents[]`
entry. There is no plugin-level default and there must not be one: a default
credential silently applied to a remote an operator added later is how a token
reaches a host it was never issued for.

### Validated at boot, not on first delegation

An unset environment variable, a key belonging to a different `type`, an
unreadable client certificate, a key that does not match its certificate, an
OAuth2 remote with neither a `token_url` nor a `base_url` to discover one from —
each stops the engine at `Init` with a message naming the agent and the key.
None of them waits to become a `401` the first time a model happens to delegate.

What is *not* checked at boot is anything only the remote can answer: whether
the token is accepted, whether the certificate is trusted, whether the token
endpoint exists. Those need the network, and this plugin
[does not touch the network at boot](#discovery-is-lazy).

### No credential value is ever logged

On any path, including failures. Failure messages name the agent, the key and
the kind of failure and stop there. A token endpoint's free-text
`error_description` is dropped wholesale rather than scrubbed — a server is free
to echo the client secret into it — and only the fixed RFC 6749 `error` code is
reported, which is what an operator actually needs.

### OAuth2: one token per burst

The access token is cached and replaced `refresh_leeway` (default `30s`) ahead
of its stated expiry. A fetch is **single-flight**: a model that fans out
produces a burst of tool calls that all reach the credential source within
microseconds, and an authorization server answers a burst of identical grants
with a `429`. The first caller fetches; the rest wait on it and share the
result, including a failure, so a token endpoint that is down is hit once per
burst rather than once per call.

When `token_url` is discovered from the card rather than configured, exactly one
request necessarily precedes the token: the well-known Agent Card fetch, which
goes out unauthenticated. Specification §8.2 makes that document public. A
remote that protects its card wants `token_url` set explicitly, and the `401` it
answers with says so.

### Card mismatch is a warning, not a refusal

On the **first** call to a remote — never at boot, since
[the card is fetched lazily](#discovery-is-lazy) — the configured credential is
compared against the card's `securitySchemes`. An obvious mismatch, such as a
bearer token against a card declaring only `mutualTls`, logs one warning naming
the schemes the card declares; a remote that declares schemes while this
instance sends nothing logs one too.

It warns rather than refuses because a card's `securitySchemes` block is
optional and routinely incomplete — a remote behind a gateway that terminates
mTLS may declare nothing at all — and refusing on that evidence would break
working deployments over a documentation defect. What the warning buys is that
the far more common case, a credential configured against the wrong remote, is
diagnosed in a log line instead of an opaque `401`.

## Loopback (serve ↔ consume)

Because [`nexus.io.a2a`](../io/a2a.md) speaks the same wire, you can point
`nexus.agent.a2a_remote` at another Nexus instance's serve endpoint:

```yaml
  nexus.agent.a2a_remote:
    agents:
      - name: local_peer
        base_url: http://127.0.0.1:8091
        credentials:
          type: bearer
          token_env: PEER_A2A_TOKEN
```

This loopback topology is the cheapest faithful end-to-end proof of the outbound
path — one Nexus instance delegating to another over A2A, with no third-party
implementation in the test path. It ships as three runnable configs and one
integration test:

| File | Role |
|---|---|
| `configs/test-a2a-loopback-caller.yaml` | The delegating engine: `nexus.agent.a2a_remote` pointed at the callee, bearer credential, mock LLM. |
| `configs/test-a2a-loopback-server.yaml` | The callee: `nexus.io.a2a` on `127.0.0.1:18192`, bearer-guarded, mock LLM. |
| `configs/test-a2a-loopback-hitl-server.yaml` | The same callee, but its mocked agent calls `ask_user`, so the task parks at `INPUT_REQUIRED`. |
| `tests/integration/a2a_loopback_test.go` | Boots both engines and drives card fetch, a streaming run to `COMPLETED`, artifact return, bearer acceptance *and* refusal, a chained question answered on the caller's side, the two input deadlines racing each other, and cancellation crossing the boundary. |

```bash
go test -tags integration ./tests/integration/ -run TestA2ALoopback -v
```

**What the loopback does and does not prove.** It proves the two Nexus mappings
are self-consistent — that what one emits, the other reads. It does **not**
prove third-party interoperability: no external A2A implementation and no
conformance test kit is in that path. The expectations that are not
self-referential live in the shared corpus at `pkg/a2a/a2aconform`, which
`nexus.io.a2a` is driven against separately; see [Conformance: one corpus, two
mappings](../../guides/a2a.md#conformance-one-corpus-two-mappings).

## See also

- [A2A Interoperability](../../guides/a2a.md) — the protocol mapping and a
  worked client walkthrough
- [A2A Serve (`nexus.io.a2a`)](../io/a2a.md) — the serve side of the same wire
- [Delegate](delegate.md) / [Remote AG-UI Agents](agui-remote.md) — the sibling
  delegation primitives this mirrors
- [Posture Registry](postures.md) — where a remote's budget comes from
- [Configuration Reference — `nexus.agent.a2a_remote`](../../configuration/reference.md#nexusagenta2a_remote)
  — canonical key list
