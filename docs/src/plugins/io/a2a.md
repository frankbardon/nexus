# A2A serve transport

`nexus.io.a2a` exposes a Nexus instance as an [Agent2Agent
(A2A)](https://a2aproject.github.io/A2A/) agent. It stands up one HTTP listener
carrying three surfaces:

| Surface | Path | Purpose |
|---|---|---|
| Discovery | `GET /.well-known/agent-card.json` | The Agent Card: who this agent is, where its bindings live, and how to authenticate. |
| JSON-RPC 2.0 binding | `POST <jsonrpc_path>` (default `/a2a`) | A2A specification §9. |
| HTTP+JSON/REST binding | `<rest_prefix>/…` (default `/a2a/v1`) | A2A specification §11. |

The wire format is entirely [`pkg/a2a`](https://github.com/frankbardon/nexus),
Nexus's hand-rolled A2A codec targeting specification 1.0.x. This plugin
contributes the listener, the credential guard, the card assembly and the
routing.

It is the A2A sibling of [`nexus.io.agui`](../io-agui.md) and inherits that
plugin's exemption from the browser/wails transport parity rule: an external
interop transport is not a Nexus UI, so nothing here is back-ported into
`nexus.io.browser` or `nexus.io.wails`.

## Current maturity

> Every operation is routed, version-negotiated and authenticated — and then
> answered with `UnsupportedOperationError`. **No A2A operation drives an agent
> turn yet.** The plugin declares no bus subscriptions and emits no bus events;
> its contract test asserts exactly that, so the declaration cannot quietly
> drift ahead of the behaviour.

The Agent Card reports this honestly rather than advertising an intention:
`capabilities.streaming`, `pushNotifications` and `extendedAgentCard` are all
derived from the set of operations the plugin actually implements, which is
currently empty. Wiring an operation flips its capability in the same edit, so
the card and the behaviour cannot disagree.

The **discovery and authentication surfaces are complete**: an A2A client can
fetch the card, learn which credentials to present, present them, and receive a
well-formed protocol error rather than a wrong answer.

## Details

| | |
|---|---|
| **ID** | `nexus.io.a2a` |
| **Dependencies** | None |
| **Requires** | None |
| **Subscriptions** | *(none yet)* |
| **Emissions** | *(none yet)* |
| **Listens?** | Yes — loopback by default (`127.0.0.1:8091`) |

## Configuration

The [Configuration Reference](../../configuration/reference.md#nexusioa2a) is
canonical for every key, its type and its default. A minimal working block:

```yaml
plugins:
  active:
    - nexus.io.a2a

  nexus.io.a2a:
    bind: "127.0.0.1:8091"
    bearer_token_env: NEXUS_A2A_TOKEN
    card:
      name: "Nexus Research Agent"
      description: "Runs research turns with web search and file tools."
      version: "1.2.0"
      skills:
        - id: research
          name: "Research a topic"
          description: "Searches the web and summarizes findings with citations."
          tags: ["research", "search"]
```

Exactly one of `card:` (inline) or `card_file:` (a JSON Agent Card document on
disk, `~`-expanded through `engine.ExpandPath`) is required. They are mutually
exclusive rather than merged: a card is a public contract, and a field-level
merge means the document an operator reads in one place is not the one that gets
served.

## Three decisions worth knowing about

### The card is half hand-authored, half derived

`card:` supplies identity, provider, modes and **skills**. Everything that
describes what the listener actually does — `supportedInterfaces`,
`capabilities`, `securitySchemes`, `securityRequirements` — is **derived** and
overwrites whatever the card source carried, `card_file` included. There are no
config keys for those, deliberately: a card naming a URL nothing is bound to, a
capability nothing implements, or a scheme nothing enforces is worse than no
card at all.

Skills in particular are **not** taken from `nexus.skills` or the tool catalog.
An internal catalog churns with every plugin an operator enables; a discovery
document that churned with it would leak internal structure and break clients
that keyed off it.

### The card endpoint is public by default

Specification §8.2 makes the well-known URI a pre-authentication bootstrap step
— a client fetches the card precisely to learn which credentials to obtain — so
gating it behind those same credentials is circular. The card stays
unauthenticated even when every operation is guarded.

That is safe because the listener binds loopback by default, and because the
card's contents are hand-authored, so what it reveals is what an operator chose
to reveal. Move `bind` off loopback and still need the card private? Set
`card_requires_auth: true` and distribute the document out-of-band, which §8.2
sanctions as "Direct Configuration" — at the cost of being undiscoverable to
clients that have not already been told about you.

### An absent `A2A-Version` header is read as 1.0, not 0.3

Specification §3.6.2 says an agent MUST read an empty `A2A-Version` as `0.3`.
That rule protects clients that predate the parameter from an agent that
silently upgraded under them. This listener has never served `0.3` and its card
advertises `1.0` on every interface, so a header-less request is not a `0.3`
client — there are none — it is a `1.0` client whose HTTP layer omitted a
header. Refusing it buys no compatibility and costs interop.

Every response therefore carries `A2A-Version: 1.0` so the client can see what
it was processed as. Set `strict_version_header: true` to restore the literal
behaviour (useful for a conformance harness). An *explicit* `A2A-Version: 0.3`
is refused either way — the policy only governs absence.

## Authentication

Two spellings, mutually exclusive, identical to `nexus.io.agui`:

- `bearer_token` / `bearer_token_env` — one shared secret, desugared into a
  one-entry `static` validator (which makes the comparison constant-time).
- `auth:` — the full `pkg/nexusauth` validator chain: `static`, `jwks`,
  `introspect`, `proxy_headers`.

Setting both is a boot error naming both keys. Setting neither disables
authentication entirely, which is safe only because the bind address defaults to
loopback — change `bind` and configure auth in the same commit.

The card's `securitySchemes` are derived from the chain, so what a client is
told to present is what is enforced. Note that a `proxy_headers` validator
publishes **no** scheme: it accepts no client credential at all, only an
identity a trusted fronting proxy already established.

See [Agent Card security
schemes](../../configuration/reference.md#agent-card-security-schemes) for the
full mapping.

## Trying it

```bash
bin/nexus -config configs/test-a2a-serve.yaml
```

```bash
# Discovery needs no credentials.
curl -s localhost:18191/.well-known/agent-card.json | jq

# Operations do.
curl -s localhost:18191/a2a \
  -H 'Authorization: Bearer test-a2a-token' \
  -H 'A2A-Version: 1.0' \
  -H 'Content-Type: application/a2a+json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"t-1"}}' | jq
```

The second call returns an `UnsupportedOperationError` (`-32004`) today; that is
the documented end state of the current stage, not a misconfiguration. Drop the
`Authorization` header to see the `401` and its RFC 6750 challenge.

## See also

- [Configuration Reference — `nexus.io.a2a`](../../configuration/reference.md#nexusioa2a)
- [AG-UI transport](../io-agui.md) — the structural sibling
- [Authentication (`auth:`)](../../configuration/reference.md#authentication-auth)
