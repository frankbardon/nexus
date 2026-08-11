# Session Broker

The **session broker** (`cmd/nexus-broker`) is a standalone service that fronts
many OS-isolated `nexus` instances behind a single HTTP/WebSocket ingress.
Callers *claim* an instance, talk to it over a WebSocket, and *release* it when
done. Each instance is a separate `nexus` process, so tenant isolation is
**process isolation**.

It is a protocol-aware gateway, not a blind TCP proxy: it decodes every frame
and routes by lease and signal, which is how it tracks readiness, idleness, and
crashes.

> The broker is **not** an engine plugin. It lives under `cmd/nexus-broker` and
> is built separately from `cmd/nexus`. The only plugin involved is
> [`nexus.io.broker`](../plugins/io/broker.md), which runs *inside* each spawned
> instance and dials back to the broker.

## How it works

```
                          ┌───────────────────────────────────────┐
                          │            nexus-broker               │
   client ──HTTP POST────▶│  /claim /release/{id} /ticket/{id}    │
                          │  /leases                              │
          ◀──lease+ws_url─│                                       │
          ──WebSocket────▶│  /lease/{id}  ◀──frames──▶  /instance │
                          └───────────────────────────────────────┘
                                       │ exec() with env             ▲
                                       ▼                             │ dials back
                          ┌───────────────────────────────────────┐ │
                          │  nexus instance (own process)         │─┘
                          │   nexus.io.broker plugin              │
                          └───────────────────────────────────────┘
```

1. A caller `POST /claim`s with a full nexus config, optionally naming which
   `binaries:` entry to run.
2. The broker resolves that name against its registry, acquires a capacity slot,
   mints a lease, writes the config to a temp file, and **cold-spawns** that
   entry's `nexus` binary, injecting the broker address and lease id as
   environment variables.
3. The instance's [`nexus.io.broker`](../plugins/io/broker.md) plugin dials
   **back** to the broker's `/instance` endpoint, registers its lease, and
   signals ready. The broker is the only listening socket.
4. `POST /claim` returns the lease id and a `ws_url`. The caller opens that
   WebSocket and IO frames flow client ↔ broker ↔ instance.
5. The instance is released on demand (`POST /release`), on idle
   (`idle_timeout`), or on crash. The session persists on disk and is resumable.

## Running the broker

The broker reads its own YAML config file (default `broker.yaml`, override with
`-config <path>`):

```yaml
# broker.yaml
listen_addr: ":8080"          # HTTP/WS gateway bind address
advertise_addr: ""            # address CLIENTS use to reach this broker; required behind a proxy/LB
binaries:                     # named nexus variants this broker may spawn (see below)
  nexus:                      # reserved name; always present, declare it to override the path
    path: "nexus"
# nexus_binary_path: "nexus"  # DEPRECATED alias for binaries.nexus.path; still honoured
max_concurrent: 8             # max live instances; <=0 = unlimited
idle_timeout: 5m              # release a lease after this much inactivity; <=0 disables
queue_wait_timeout: 30s       # how long an over-cap claim waits in the FIFO queue; <=0 = no waiting
release_grace: 10s            # graceful-shutdown grace before force-kill
state_dir: ""                 # per-broker dir for the lease journal; empty = in-memory only
broker_id: ""                 # stamped on every lease record; generated + persisted when empty
reattach_window: 60s          # how long a lease restored after a restart waits for its instance
# auth:                       # optional; omit the block to run unauthenticated (see Authentication)
```

```bash
# build both binaries
go build -o bin/nexus ./cmd/nexus
go build -o bin/nexus-broker ./cmd/nexus-broker

# run the broker
bin/nexus-broker -config broker.yaml
```

Every config key, its type, and its default are listed in the authoritative
[Configuration Reference](../configuration/reference.md#session-broker-nexus-broker).

### Serving several `nexus` variants: the binary registry

`binaries:` is a **registry of named `nexus` variants** this broker may spawn,
keyed by the name a claim selects it by. A base build, a vision-enabled build, a
pinned older release and a wrapper script that pre-sets an environment can all
live behind one ingress.

Every entry is a **variant of `nexus`, not an arbitrary program**, because the
broker spawns them all identically and expects the same behaviour back:

- it exec()s the entry's `path` with `-config <temp file>`, plus
  `-recall <session_id>` when the claim is a [resume](#new-vs-resume);
- it injects `NEXUS_BROKER_ADDR`, `NEXUS_BROKER_LEASE_ID` and
  `NEXUS_BROKER_SPAWN_SECRET` into the child's environment;
- it waits for the process's [`nexus.io.broker`](../plugins/io/broker.md) plugin
  to dial **back** to `/instance`, register that lease, and signal ready.

A binary that does not honour that contract never signals ready, so the claim
fails with `504 instance did not become ready in time` rather than misbehaving
quietly. The registry chooses *which* build runs; it does not change the
protocol any of them speak.

```yaml
# broker.yaml
binaries:
  nexus:                                 # reserved name; declare it only to pin the path
    path: "/usr/local/bin/nexus"

  vision:
    path: "~/builds/nexus-vision"        # `~` is expanded
    label: "Nexus (vision)"              # presentational only
    description: "Multimodal build with the image tools compiled in"
    args: ["-profile", "vision"]         # appended AFTER the broker's -config / -recall
    env:
      NEXUS_VISION: "1"                  # layered UNDER the broker's NEXUS_BROKER_* vars

  pinned:
    path: "nexus-0.9"                    # no path separator → looked up on the broker's PATH
    description: "Pinned 0.9 build for regression triage"
```

`label` and `description` are documentation for operator and client surfaces —
nothing routes on them, and consumers fall back to the entry name when `label`
is empty. `path` is the only required field. The per-field table is in the
[Binary registry reference](../configuration/reference.md#binary-registry-binaries).

`args` are **that variant's own flags**, so only set them for a build that
defines them: stock `nexus` accepts `-config`, `-recall` and `-replay` and
nothing else, and an unknown flag makes the process exit before it can dial back
— which the claim sees as `502 instance exited before signalling ready`.

**`nexus` is reserved and always resolves.** After a successful load the registry
always contains a `nexus` entry, whatever the config says — declare it to pin its
path, omit it and it is synthesized as `path: "nexus"` (a `PATH` lookup). Omit
the whole `binaries:` block and that synthesized entry is the entire registry,
which is exactly the pre-registry behaviour. There is deliberately **no
`default:` field**: a claim that names no binary always means `nexus`, so an
operator cannot silently change what an existing client spawns.

**Every entry is verified at boot**, before the gateway listens: the path is
expanded (`~`), looked up on the broker process's `PATH` when it contains no path
separator, made absolute, then stat'd and checked for an execute bit. An entry
that is missing, is a directory, or carries no execute bit **refuses the boot**,
naming the entry, the path that was resolved, and the reason. The broker logs the
resolved absolute path of every entry at startup, so a stale build shadowing the
one you meant is visible in the boot log rather than inferred later.

> **This includes the reserved `nexus` entry.** A zero-config broker whose `PATH`
> has no `nexus` now fails to start, where it previously started fine and failed
> at the first claim. The exec-bit check is a mode check, not a "can *this* user
> run it" check, so a binary executable only by another user still passes boot and
> fails at exec.

**A variant can extend a spawn, never redirect it.** `args` are appended *after*
the broker's own `-config` / `-recall` arguments, so an entry can add flags but
cannot displace the contract the instance protocol depends on. `env` is merged
over the broker process's environment, but the three `NEXUS_BROKER_*` dial-back
variables are applied **last and always win** — an entry cannot point an instance
at a different broker, hand it another lease id, or supply its own spawn secret.

**Which entry ran a session is remembered**, so a resume re-uses it instead of
falling back to `nexus`, and a resume that names a *different* variant is refused
with a `409`. That check is best-effort, with limits worth knowing before you
depend on it — see
[A resume re-uses the binary that created the session](#a-resume-re-uses-the-binary-that-created-the-session).

Full resolution rules and the boot-failure messages are in
[Binary resolution](../configuration/reference.md#binary-resolution).

#### Migrating from `nexus_binary_path`

**Nothing to do.** `nexus_binary_path` still works and existing deployments boot
unchanged — it is **deprecated**, not removed:

| Your `broker.yaml` | What happens |
|---|---|
| Neither key | `nexus` is synthesized with path `nexus`; unchanged zero-config behaviour. |
| `nexus_binary_path` only | Its value becomes `binaries.nexus.path`, and the broker logs one `WARN` naming the replacement key. |
| `binaries.nexus` only | Taken as written. This is the form to move to. |
| **Both** | **Boot failure** naming both keys. |

Setting both is refused rather than resolved by precedence: whichever rule the
broker picked, half of the operators who hit it would silently spawn the binary
they did not mean, and the mistake would only ever surface as instances behaving
oddly. Setting `nexus_binary_path: ""` is likewise a boot error, not a silent
fallback — remove the key to take the default.

Migrating is a mechanical rewrite:

```yaml
# before
nexus_binary_path: "/usr/local/bin/nexus"

# after
binaries:
  nexus:
    path: "/usr/local/bin/nexus"
```

#### What the registry deliberately does not do

- **No per-binary authentication or scoping.** [`auth:`](#authentication) gates
  routes, not entries. Any caller allowed to claim may name any registered entry.
- **No per-binary capacity.** `max_concurrent` is one global cap across every
  variant; there is no per-entry limit or reservation.
- **No broker-side default configs.** A claim still supplies the whole engine
  config as YAML text. An entry contributes `args` and `env`, never config
  content.
- **No hot reload.** The registry is read, folded and resolved once at startup.
  Adding, changing or removing an entry means **restarting the broker**.

### Behind a proxy: set `advertise_addr`

A lease is in-memory state on **one** broker process, so the `ws_url` a claim
returns has to name that process. With a wildcard bind and no `advertise_addr`,
the broker can only guess — it derives the host from the claim request's `Host`
header.

**That is the failure this key prevents.** Behind a reverse proxy or load
balancer the `Host` header names the **intermediary**, so the returned `ws_url`
points at the load balancer rather than at the broker holding the lease. The
client then reconnects through the LB, lands on whichever broker it picks, and is
told `404 unknown lease` — by a broker that is working perfectly and simply does
not have that lease. Set `advertise_addr` to the address clients actually use to
reach **this** broker:

```yaml
listen_addr: ":8080"                              # bind wide
advertise_addr: "wss://broker-1.example.com"      # tell clients where THIS broker is
```

A bare `host:port` keeps the `ws://` scheme; the scheme-qualified form is for a
TLS-terminating proxy. Malformed values (no port, a wildcard host, a path) fail
startup, and the wildcard-bind-without-`advertise_addr` shape logs a `WARN` at
boot. A directly-reachable broker can leave the key empty. Full precedence table:
[`ws_url` resolution](../configuration/reference.md#ws_url-resolution).

### Surviving a restart: set `state_dir`

Lease state — which instances this broker spawned, who claimed them, and what
session each is running — lives in memory by default, so a restart loses it and
the spawned `nexus` processes become orphans nobody can account for. Point
`state_dir` at a directory and the broker journals every lease transition to
`<state_dir>/leases.jsonl`, **and reclaims its running instances when it comes
back**:

```yaml
state_dir: "~/.nexus/broker"     # per-broker; never share one dir between brokers
broker_id: ""                    # optional name; generated and persisted when empty
reattach_window: 60s             # bound on how long a restored lease waits for its instance
```

A record is appended when a lease is minted, when its pid and session id first
become known, and when it is torn down — including on **idle sweep and crash**,
not just a manual `POST /release`. Records carry the lease id, the claiming
principal, the session id, the `binaries` entry name the instance was spawned
from, the pid, and this broker's `broker_id` / `advertise_addr`. **No secret is
ever written**: not the per-spawn secret, not a WebSocket ticket, not a bearer
token.

The durable **session → binary** mapping lives in its own file beside the
journal, `<state_dir>/session-binaries.jsonl`, and not in the journal itself. The
journal is compacted down to live leases, and a resume always arrives *after* the
original lease was released, so a binding kept only there would be gone exactly
when it is wanted. A line is written at claim time for a resume and on the
session-id report for a new session; the file is capped at 4096 bindings, oldest
dropped first, and rewritten on open and every 256 appends.

It is best-effort by design: an unknown session means *no opinion, proceed*, never
a mismatch. A session that predates the file, one whose binding was pruned, and a
broker with no `state_dir` all resume exactly as they did before it existed. A
corrupt or torn index is skipped line by line and **never prevents the broker from
booting** — an index that cannot be opened at all only logs a `WARN` and turns the
mapping off. See
[Session → binary index](../configuration/reference.md#session--binary-index-session-binariesjsonl).

The journal is compacted on open and every 512 appends, so it holds roughly the
live lease set rather than the whole history. A write that fails is logged and
never fails the claim or release that produced it, and a record torn by a `kill
-9` is skipped with a warning instead of failing the file.

Leave `state_dir` empty and the broker behaves exactly as it always has,
logging one `WARN` at startup to say lease state is in-memory only. This is
about **lease** bookkeeping, not sessions: an instance's session under
`~/.nexus/sessions/<id>/` is persisted and `-recall`-able either way.

#### What a restart actually does

At boot — before any route is served — the broker replays the journal and, for
each lease that was live when it stopped:

- **The pid is still alive** → the lease is **restored**, with its original owner,
  session id and creation time, and it **re-takes its capacity slot** so
  `max_concurrent` stays honest.
- **The pid is gone**, or the lease never got a process → the record is closed out
  and forgotten.
- **The record belongs to another `broker_id`** → it is left completely alone.
  Nothing is adopted and nothing is killed.

A restored lease shows as `spawning` in `GET /leases` and is **not yet usable**.
Meanwhile the surviving instance's `nexus.io.broker` transport is already
reconnecting with exponential backoff, so it re-dials `/instance` on its own — no
instance-side configuration and no client action are involved. When it does, and
it presents the right lease id **and** the right spawn secret, the lease goes
`active` and existing clients can reconnect to the same `ws_url` and carry on.

> **A restored lease always requires the spawn secret, even on a broker with no
> `auth:` block.** A live pid is not proof of identity: the recorded pid may have
> been recycled to an unrelated process while the broker was down, and the
> portable liveness probe cannot tell. This one check is not configurable.

The secret survives the restart without ever being written down: it is **derived**
as `HMAC-SHA256(<state_dir>/spawn-key, lease_id)` rather than randomly minted, so
the restarted broker recomputes exactly what the running instance is still
holding. `spawn-key` is 32 random bytes, mode `0600`, created on first boot. It is
a *key*, not a credential — presenting it to `/instance` authenticates nothing —
and losing or rotating it is safe: derived secrets simply stop matching, and the
affected leases are reaped rather than reattached.

**Nothing waits forever.** A restored lease that no instance reconnects to within
`reattach_window` (default 60s) is reaped through the ordinary release path: the
process is signalled (`SIGTERM`, escalating to `SIGKILL`), the slot is freed, and
the record is closed out. Setting `reattach_window` to `0` does **not** disable
this — it falls back to the default, because an unbounded wait is the orphan the
feature exists to remove.

For the per-record detail, the exact reasons written to the journal, and the full
trade-off discussion around the derivation key, see
[Restart recovery](../configuration/reference.md#restart-recovery-reattach_window).

### Health check

```bash
curl -s http://localhost:8080/healthz
# {"status":"ok"}
```

## Authentication

**An absent `auth:` block means authentication is disabled**, and every route
behaves exactly as it did before authentication existed. That is the default for
an upgrading deployment, and the broker says so once at boot:

```
WARN client authentication is DISABLED: broker config has no auth block, so any
     caller that can reach this broker can claim, release and list leases
```

Opting in means adding an `auth:` block to `broker.yaml`. A **malformed** block
is a boot failure naming the offending key — it never falls back to disabled, and
unknown keys are rejected at every level.

### What the block protects

| Route | With `auth:` configured |
|-------|-------------------------|
| `POST /claim`, `POST /release/{lease_id}`, `POST /ticket/{lease_id}`, `GET /leases` | Middleware validates the credential **before** the handler runs. A refused claim spawns nothing. |
| `WS /lease/{lease_id}` | The same validator chain, resolved by the handler itself so it can also accept a single-use `?ticket=` (see [the ticket flow](#connecting-the-ticket-flow)). |
| `WS /instance` (the dial-back) | Not a client route: a spawned instance proves itself with its [per-spawn secret](#the-instance-dial-back-secret), not with an operator-configured credential. |
| `GET /healthz` | **Never authenticated.** A load balancer or container probe has no credential to present, and liveness leaks nothing. |

### The four validators

The `auth.validators` list is **ordered, and the first validator that accepts
wins** — so put the cheap ones first.

| `type` | Verifies | Key settings |
|--------|----------|--------------|
| `static` | A table of shared bearer tokens, compared in constant time | `tokens[]`, each with `token` + `principal` |
| `jwks` | An OIDC JWT, against the signing keys the issuer publishes | `issuer`, `jwks_url`, `audience`, `principal_claim` |
| `introspect` | An **opaque** token, by asking the issuer (RFC 7662) | `introspection_url`, `client_id`, a client secret, `principal_claim` |
| `proxy_headers` | An identity a fronting authenticating proxy already established | `trusted_proxy_cidrs`, `principal_header` |

A worked example: a shared token for CI, and OIDC access tokens for everyone
else. It is deliberately vendor-neutral — the broker is generic OIDC with **no
provider-specific defaults**, so every value below comes from your own issuer.

```yaml
# broker.yaml
listen_addr: ":8080"
advertise_addr: "wss://broker-1.example.com"

auth:
  admin_scope: "nexus.broker.admin"    # scope that unlocks the operator view of GET /leases
  validators:
    # Tried first: no network round trip.
    - type: static
      tokens:
        - token: "replace-me"
          principal: "ci-runner"
          tenant: "acme"
          scopes: "nexus.broker.admin"   # whitespace-separated, or a YAML list

    # Everyone else presents an OIDC access token.
    - type: jwks
      issuer: "https://id.example.com/"                        # exact `iss` value
      jwks_url: "https://id.example.com/.well-known/jwks.json"  # explicit; no discovery
      audience: "nexus-broker"                                  # or a list
      algorithms: ["RS256"]
      principal_claim: sub                                      # required
      tenant_claim: org_id
      scopes_claim: scope
```

Four things worth knowing before you deploy that:

- **`principal_claim` is required and has no default guess.** Lease ownership is
  principal-`ID` equality, so silently defaulting to `sub` for an issuer that
  mints a different stable identifier would bind ownership to the wrong field. A
  token whose mapped claim is absent, empty, or not a scalar is rejected.
- **There is no OIDC discovery**, on purpose — `jwks_url` is configured
  explicitly. For an issuer that documents only its discovery URL, read the value
  out once by hand:
  ```bash
  curl -s https://id.example.com/.well-known/openid-configuration | jq -r .jwks_uri
  ```
- **Key rotation needs no restart**, and an unreachable issuer never turns into an
  allow: a `kid` already in the cache keeps verifying, a `kid` that is not cached
  is denied.
- **Secrets follow one convention**: `<key>` holds the literal value, `<key>_env`
  holds the *name* of an environment variable to read it from — so
  `client_secret_env: "NEXUS_BROKER_INTROSPECTION_SECRET"` keeps the
  `introspect` client secret out of the file. **Setting both is a boot error**,
  not a precedence rule. A `broker.yaml` carrying `static` tokens or an inline
  `client_secret` is itself a secret: restrict its permissions.

An `introspect` validator that cannot reach its endpoint answers **`503` with a
`Retry-After` header**, not `401`. "We could not find out" is not a statement
about your token, and telling every client at once to re-authenticate against an
identity provider that is already failing would make an outage worse. It is still
a refusal: no lease is claimed and nothing is released.

Every key, default and validation rule is in the authoritative
[Authentication reference](../configuration/reference.md#authentication-auth).

### `proxy_headers` needs a CIDR allowlist

`type: proxy_headers` makes the broker's original deployment story — "put your own
authenticating proxy in front of it" — first-class: the proxy authenticates the
user and passes the identity down in a header.

> **⚠️ A wrong `trusted_proxy_cidrs` turns this validator into an open door.**
> A header is not a credential. Anyone who can open a TCP connection to
> `listen_addr` can send `X-Forwarded-User: <anybody>` — no signature, no expiry,
> nothing to verify. The CIDR allowlist is the *entire* security model.
>
> - **Never write `0.0.0.0/0` or `::/0`.** That is not "allow the ingress", it is
>   "let every caller on the network name themselves".
> - **List the proxy's own address, not the client's** — the allowlist is matched
>   against the peer that opened the connection, and `X-Forwarded-For` is never
>   read.
> - **Do not point it at a network you share with anything else.** A `10.0.0.0/8`
>   that also holds other workloads means any of them can impersonate any broker
>   user. Prefer the proxy's `/32` (or `/128`).
> - **Bind the broker where only the proxy can reach it**, so the CIDR check is a
>   second line of defence rather than the only one.
> - If some callers arrive directly, **chain** a token validator after this one —
>   do not widen the CIDR to accommodate them.

```yaml
auth:
  validators:
    - type: proxy_headers
      trusted_proxy_cidrs: ["10.4.0.0/16"]   # required; an empty list fails the boot
      principal_header: X-Forwarded-User     # required; no default is right for everyone
      tenant_header: X-Auth-Request-Org
      scopes_header: X-Forwarded-Groups
    - type: jwks                             # direct callers still need a real token
      issuer: "https://id.example.com/"
      jwks_url: "https://id.example.com/.well-known/jwks.json"
      audience: "nexus-broker"
      principal_claim: sub
```

### Who may touch a lease

Every lease records the principal that claimed it. Authorization is that
ownership, plus one read-only admin scope — there are no roles and no policy
engine.

- **`POST /release/{lease_id}`, `WS /lease/{lease_id}` and
  `POST /ticket/{lease_id}`** require the caller's principal `ID` to equal the
  owner's. It is checked *before* anything happens: a refused release sends no
  shutdown frame and frees no slot, and a refused connect never reaches `101`.
- **A lease that is not yours answers exactly like a lease that never existed** —
  `404 {"error":"unknown lease"}`, byte for byte — so live lease ids cannot be
  enumerated by differencing responses.
- **`GET /leases` is filtered, not refused.** A non-operator sees only its own
  leases, and the capacity aggregates are **omitted rather than zeroed** (treat a
  missing `max_concurrent` as "not disclosed", never as `0`).
- **`auth.admin_scope`** (default `nexus.broker.admin`) unlocks the whole-registry
  view of `GET /leases`. It grants **visibility only — there is no admin bypass on
  release or connect**, so a leaked operator credential cannot tear down or hijack
  another principal's session. Set it to `""` to mean nobody is an operator.
- The broker's **own** teardown paths — the `idle_timeout` sweeper and crash
  detection — act with no principal at all and bypass ownership entirely.

With no `auth:` block, the caller and every lease owner are the same anonymous
identity, so nothing is refused and nothing is filtered.

### Connecting: the ticket flow

Browser JavaScript **cannot** set headers on a WebSocket handshake, so the bearer
token a claim was made with can never reach `WS /lease/{lease_id}` from a browser.
A **ticket** is the one credential the broker itself mints. End to end:

1. **Claim with your credential.** `POST /claim` returns `ws_url` **and**
   `ticket` — the `ticket` key is present only when the broker is configured with
   an `auth:` block.
2. **Connect with it**: `ws_url + "?ticket=" + ticket`.
3. **It is single-use and lives 30 seconds**, and the TTL is a constant rather
   than a config key: the value travels in a URL, so it lands in proxy access logs
   and browser history, and the tight window plus single use are the whole
   mitigation for that.
4. **Reconnecting needs a fresh one.** `POST /ticket/{lease_id}`, authenticated
   with the credential the lease was claimed with, mints a replacement. Do *not*
   re-claim: that spawns a new instance and abandons the live session.
5. **Non-browser clients can skip tickets entirely** and send
   `Authorization: Bearer <token>` on the handshake instead — the same token the
   lease was claimed with.

A non-empty `?ticket=` **wins exclusively**: when both are presented the
`Authorization` header is not consulted at all, and a ticket failure is final
rather than falling back to the header. Send one credential, or mint a fresh
ticket. Every way a ticket can fail — unknown, expired, already redeemed, minted
for another lease — answers identically with `401 {"error":"credential
rejected"}`.

Tickets are in-memory only, so they do not survive a broker restart, and every
ticket for a lease is destroyed the moment the lease goes away.

### The instance dial-back secret

The `auth:` block also gates the **instance** side. With it configured, an
instance's `register` frame on `WS /instance` must carry both a known `lease_id`
**and** the per-spawn secret the broker injected into its environment at exec;
anything else is closed with the same `unknown lease` policy-violation close. The
secret is never logged, never returned by `GET /leases`, and never passed in
argv. The instance side needs no configuration — the
[`nexus.io.broker`](../plugins/io/broker.md) plugin reads
`NEXUS_BROKER_SPAWN_SECRET` from the environment the broker set.

> **In an authenticated deployment, an out-of-date registry entry is rejected.**
> A `nexus` build that predates the spawn-secret protocol cannot
> echo a secret, so its `register` frame is refused and the claim eventually fails
> with `504 instance did not become ready in time` while the child process looks
> alive and connects fine. The broker logs a `WARN` naming the version skew
> explicitly, because that symptom otherwise reads as a network fault. The fix is
> to upgrade the binary that registry entry points at — the check is per **spawn**,
> so one stale variant fails while the rest of the registry keeps working. With
> **no** `auth:` block the secret is not
> checked at all, so an older binary keeps working — **except** on a lease
> [restored after a restart](#what-a-restart-actually-does), which always requires
> it.

## HTTP API

All control-plane calls are plain HTTP/JSON. Whether they need a credential
depends on the [`auth:` block](#authentication): with one configured, every route
below requires one (`GET /healthz` does not); with it absent, none of them do.

### `POST /claim` — claim an instance

Body:

```jsonc
{
  "config": "engine:\n  name: example\n",  // required: full nexus config (YAML text)
  "session_id": "prior-session-id",         // optional: resume a persisted session
  "binary": "vision"                        // optional: which `binaries:` entry to spawn
}
```

Success (`200`):

```jsonc
{
  "lease_id": "…",                          // handle for this instance
  "ws_url": "ws://host:port/lease/<lease>",  // client WebSocket endpoint
  "session_id": "…",                         // engine session id (see new-vs-resume below)
  "ticket": "…"                              // single-use, 30s WebSocket credential;
                                             // present only when `auth:` is configured
}
```

```bash
curl -s -X POST http://localhost:8080/claim \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <token>' \
  -d '{"config":"engine:\n  name: example\n"}'
```

The `Authorization` header is required when the broker is configured with an
[`auth:` block](#authentication) and ignored when it is not. The principal it
resolves to becomes the lease's **owner**, and only that principal can release,
connect to, or mint a ticket for the lease.

Error responses:

| Condition | Status | Body |
|-----------|--------|------|
| Missing/empty `config` | `400` | `{"error":"claim requires a non-empty config"}` |
| Unknown `binary` name | `400` | `{"error":"unknown binary \"…\"; this broker spawns: …"}` |
| Resume whose `binary` differs from the one recorded for the session | `409` | `{"error":"session \"…\" was created by binary \"…\" but this claim requests \"…\"; …"}` |
| Resume whose recorded binary is no longer in `binaries:` | `409` | `{"error":"session \"…\" was created by binary \"…\", which this broker no longer offers; …"}` |
| Over capacity, queue wait elapsed | `503` | `{"error":"capacity wait timed out"}` |
| At capacity, queueing disabled (`queue_wait_timeout <= 0`) | `503` | `{"error":"no capacity"}` |
| Instance exited before ready (e.g. resume of a missing/invalid session) | `502` | `{"error":"instance exited before signalling ready"}` |
| Instance did not become ready within the boot window | `504` | `{"error":"instance did not become ready in time"}` |

### Choosing a binary

`binary` names an entry of the broker's
[binary registry](#serving-several-nexus-variants-the-binary-registry):

```bash
# spawn the "vision" variant
curl -s -X POST http://localhost:8080/claim \
  -H 'Content-Type: application/json' \
  -d '{"config":"engine:\n  name: example\n","binary":"vision"}'

# omit `binary` and the claim spawns the reserved `nexus` entry
curl -s -X POST http://localhost:8080/claim \
  -H 'Content-Type: application/json' \
  -d '{"config":"engine:\n  name: example\n"}'
```

**Omitting `binary` means `nexus`**, the entry every broker is guaranteed to
have, so a client written before the registry existed keeps getting exactly what
it got before. The field is trimmed of surrounding whitespace, the same way entry
names are trimmed at load, so the two always agree. **On a resume this changes**:
omitting `binary` alongside a `session_id` means the entry that created the
session, not `nexus` — see
[A resume re-uses the binary that created the session](#a-resume-re-uses-the-binary-that-created-the-session).

An unknown name returns `400` naming the rejected value and listing the entries
this broker actually has — never a silent fallback to `nexus`. The check runs
before the claim allocates anything, so a typo consumes no lease, no capacity
slot, no temp config file, and spawns no process.

The entry's `args` are appended after the broker's own `-config` / `-recall`
arguments, and its `env` is layered under the broker-owned `NEXUS_BROKER_*`
variables — an entry can extend a spawn but never redirect it at another broker
or supply its own spawn secret.

### New vs. resume

- **New session** — omit `session_id`. The engine generates a fresh session id
  and the instance reports it back; the broker returns it in the response.
  **Capture that id** if you want to resume the session later.
- **Resume** — set `session_id` to a previously returned id. The broker spawns
  the instance with `-recall <id>`, so the engine reloads that session from
  `~/.nexus/sessions/<id>/` and replays its history. The response echoes the
  requested id.

Resuming a session id that does not exist on disk makes the engine fail to boot;
the instance never signals ready and the claim returns **`502`** rather than
silently starting a new session.

#### A resume re-uses the binary that created the session

A session directory is engine state written by **one particular build**, and
replaying it under a different variant does not fail loudly: the engine boots,
the transcript loads, and the session simply behaves as though capabilities it
once had have vanished. The claim is the last point at which that mistake is
still attributable, so the broker records which
[`binaries:` entry](#serving-several-nexus-variants-the-binary-registry) ran each
session — the entry **name**, never a path — and reconciles a resume against it.

Recording happens only when [`state_dir`](#surviving-a-restart-set-state_dir) is
configured, in two places:

- the **lease journal** stamps the entry name on the lease record, so
  `leases.jsonl` says which variant a live lease is running;
- the **session → binary index**, `session-binaries.jsonl`, keeps the pairing
  after the lease is gone. This is the one a resume actually consults, because a
  resume always arrives *after* the original lease was released and the journal is
  compacted down to live leases. The broker checks live leases first and falls
  back to the index.

On a claim that carries a `session_id`:

| You send | Recorded binding | What happens |
|---|---|---|
| no `binary` | `vision` | Spawns **`vision`** — the recorded entry is inherited with its `path`, its `args` **and** its `env`. Not the reserved `nexus`. |
| `"binary": "vision"` | `vision` | Proceeds normally. |
| `"binary": "nexus"` | `vision` | **`409`**, naming **both** the recorded entry and the one you asked for. |
| anything | *none recorded* | **Falls through** — spawns what you asked for, or `nexus` if you asked for nothing. **No error.** |
| no `binary` | `nocturne`, no longer in `binaries:` | **`409`** naming the missing entry, so it can be restored. Deliberately *not* a silent fallback to `nexus`. |

A `binary` that is empty or only whitespace counts as **omitted**. An *unknown*
name is still a `400`, not a `409`, even on a bound session — a typo is reported
as a typo, and only the `400` lists the entries this broker actually has. Every
rejection happens **before anything is allocated**: no lease, no capacity slot, no
place in the queue, no temp config file, and no process.

The exact error strings and the reasoning behind `409` over `400`/`500` are in
[Resume inherits the recorded binary](../configuration/reference.md#resume-inherits-the-recorded-binary).

#### The 409 is best-effort, not an invariant

**Do not build a client that relies on the mismatch check catching every
mistake.** An unrecorded session is *no opinion, proceed* — never a mismatch — so
a resume runs completely unchecked whenever the binding is missing. Concretely,
nothing is checked when:

- **the broker has no `state_dir`.** Nothing is recorded durably, so once the
  original lease is gone so is the pairing — which is every resume, since a resume
  by definition follows a release.
- **the binding was evicted.** The index holds at most **4096** pairings and drops
  the **oldest first**, so a session resumed after a lot of other traffic can find
  its binding gone.
- **the session predates the feature**, or was created against a *different*
  broker — no broker reads another's index.

In all three the claim proceeds silently under whatever binary it asked for. The
check is a safety net over the common mistake, not a guarantee about what a
session directory is replayed under.

**The recommended client pattern is therefore: capture `session_id` *and* the
`binary` you claimed with, and send both back on resume.** That way the correct
variant is selected by your own request rather than by a broker-side record that
may not exist:

```bash
# 1. claim, remembering BOTH values
claim=$(curl -s -X POST http://localhost:8080/claim \
  -H 'Content-Type: application/json' \
  -d '{"config":"engine:\n  name: example\n","binary":"vision"}')
session_id=$(printf '%s' "$claim" | jq -r .session_id)
binary=vision                      # persist this next to the session id

# 2. resume, restating the binary you recorded
curl -s -X POST http://localhost:8080/claim \
  -H 'Content-Type: application/json' \
  -d "{\"config\":\"engine:\n  name: example\n\",\"session_id\":\"$session_id\",\"binary\":\"$binary\"}"
```

Restating a matching `binary` costs nothing when the broker also has the binding
(the claim simply proceeds), and it is the only thing that keeps the resume
correct when the broker does not.

### `POST /release/{lease_id}` — release an instance

Gracefully tears a live instance down: the broker sends a `shutdown` frame, the
instance's `nexus.io.broker` plugin emits `io.session.end`, and the engine
performs a clean `Stop` that flushes and **persists the session before exit**.
The broker waits up to `release_grace` and force-kills the process if that
window elapses (orphan prevention). The session directory under
`~/.nexus/sessions/<id>/` is left intact and remains resumable via `-recall`.

```bash
curl -s -X POST http://localhost:8080/release/lease-abc123
# {"status":"released","lease_id":"lease-abc123"}
```

| Outcome | Status | Body |
|---------|--------|------|
| Released (graceful or killed) | `200` | `{"status":"released","lease_id":"…"}` |
| Unknown / already-released lease | `404` | `{"error":"unknown lease"}` |
| Missing lease id in path | `400` | `{"error":"release requires a lease id"}` |

Release is **idempotent**: releasing an already-gone lease returns `404` rather
than erroring, and concurrent releases of the same lease collapse to one
teardown.

### `POST /ticket/{lease_id}` — mint a fresh WebSocket ticket

Only relevant when the broker is configured with an `auth:` block. A browser
cannot put an `Authorization` header on a WebSocket handshake, so a claim hands
back a **single-use, 30-second** `ticket` bound to that lease and to the claiming
principal. Because the window is deliberately tight, a reconnect needs a fresh
one — that is what this route is for; re-claiming would spawn a *new* instance and
abandon the live session.

```bash
curl -s -X POST http://localhost:8080/ticket/lease-abc123 \
  -H 'Authorization: Bearer <the token the lease was claimed with>'
# {"lease_id":"lease-abc123","ticket":"…"}
```

| Outcome | Status | Body |
|---------|--------|------|
| Ticket issued | `200` | `{"lease_id":"…","ticket":"…"}` |
| Unknown, already-released, **or** another principal's lease | `404` | `{"error":"unknown lease"}` (identical in all three cases, so live lease ids cannot be enumerated) |
| Missing lease id in path | `400` | `{"error":"ticket requires a lease id"}` |

Tickets are in-memory only — they do **not** survive a broker restart — and every
ticket for a lease is destroyed the moment the lease goes away, whether by
`POST /release`, idle reaping, or a crash. With no `auth:` block the route is
inert: it answers `200` with the `ticket` key **omitted**, and the lease socket
keeps accepting a connection with no ticket at all. Full detail, including why the
TTL is not configurable, is in the
[configuration reference](../configuration/reference.md#post-ticketlease_id-http-api-not-yaml).

### `GET /leases` — list live instances

A read-only introspection surface, sorted by `created_at` then `lease_id`. It
performs no mutation.

**What comes back depends on who is asking.** A caller holding
[`auth.admin_scope`](#who-may-touch-a-lease) — and every caller when auth is
disabled — gets the **operator** shape: every live lease plus the capacity and
queue aggregates. Any other authenticated caller gets only its own leases, with
the aggregates **omitted**.

```bash
curl -s http://localhost:8080/leases -H 'Authorization: Bearer <token>'
```

Operator shape:

```jsonc
{
  "max_concurrent": 8,     // configured cap (0 = unlimited)
  "slots_in_use": 2,       // live instances currently holding a slot
  "queue_depth": 0,        // claims parked in the FIFO capacity wait queue
  "leases": [
    {
      "lease_id": "lease-abc123",
      "session_id": "…",
      "pid": 41234,
      "state": "active",           // "spawning" | "active" | "draining"
      "reason": "",                 // teardown reason once draining (e.g. "manual release", "idle")
      "last_activity": "2026-06-25T12:00:00Z",
      "created_at": "2026-06-25T11:59:30Z"
    }
  ]
}
```

Caller-scoped shape — same lease objects, same ordering, **no aggregate keys at
all**:

```jsonc
{
  "leases": [
    {
      "lease_id": "lease-abc123",
      "session_id": "…",
      "pid": 41234,
      "state": "active",
      "last_activity": "2026-06-25T12:00:00Z",
      "created_at": "2026-06-25T11:59:30Z"
    }
  ]
}
```

The aggregates are **absent, not zeroed** — read a missing `max_concurrent`,
`slots_in_use` or `queue_depth` as "not disclosed to you", never as `0`. A caller
that owns no live lease gets `200` with `{"leases": []}`, never a `404`.

Lease states:

| State | Meaning |
|-------|---------|
| `spawning` | The lease exists but its instance has not yet dialed back and registered — the claim is still booting an engine. |
| `active` | The instance has registered; frames can flow. |
| `draining` | A teardown (manual release, idle, or crash) has latched; the lease is on its way out. |

### Connecting over WebSocket

After a successful claim, open the returned `ws_url` and exchange IO frames. A
minimal browser sketch, carrying the [ticket](#connecting-the-ticket-flow) the
claim returned:

```javascript
const auth = { Authorization: "Bearer <token>" };   // omit when auth is disabled

const { lease_id, ws_url, ticket } = await (await fetch("http://localhost:8080/claim", {
  method: "POST",
  headers: { "Content-Type": "application/json", ...auth },
  body: JSON.stringify({ config: "engine:\n  name: example\n" }),
})).json();

// `ticket` is present only when the broker is configured with an `auth:` block.
const url = ticket ? `${ws_url}?ticket=${encodeURIComponent(ticket)}` : ws_url;

const ws = new WebSocket(url);
ws.onmessage = (e) => console.log("frame:", e.data);
ws.onopen = () => {
  // send a user input message into the instance
  ws.send(JSON.stringify({ type: "input", content: "hello" }));
};

// reconnecting later? the ticket is single-use and 30s-lived — mint a fresh one
// rather than re-claiming, which would spawn a NEW instance.
// const { ticket: next } = await (await fetch(
//   `http://localhost:8080/ticket/${lease_id}`, { method: "POST", headers: auth })).json();

// later: release the instance (the session persists on disk)
await fetch(`http://localhost:8080/release/${lease_id}`, { method: "POST", headers: auth });
```

A client that can set request headers — Go, a CLI, anything that is not a browser
— may skip tickets and send `Authorization: Bearer <token>` on the handshake
instead.

The IO message shapes carried inside broker frames (`output`, `stream.delta`,
`input`, `approval.response`, …) are documented on the
[`nexus.io.broker` plugin page](../plugins/io/broker.md#how-it-works).

## Capacity and queueing

`max_concurrent` caps live instances. Each claim acquires a slot **before**
spawning, so the live count can never exceed the cap. When the cap is full a
claim does not fail immediately — it parks in a **FIFO wait queue** bounded by
`queue_wait_timeout`. The moment a slot frees (release, idle, or crash) it is
handed to the oldest waiter. Set `queue_wait_timeout` to `0` to disable waiting
(at-capacity claims are rejected immediately with `503 no capacity`); set
`max_concurrent` to `0` for unlimited instances.

## Idle reaping

If an instance receives no real client input for `idle_timeout`, the broker
releases it through the same teardown path as `POST /release` (so the session is
persisted). Only inbound `io` input frames (client → instance) reset the idle
timer — output, pings, and control frames do not. Set `idle_timeout` to `0` to
disable idle reaping.

## v1 caveats

The session broker is a **v1**. Understand these boundaries before deploying it:

- **Identity is verified, but authorization is thin.** With an
  [`auth:` block](#authentication) configured the broker validates a credential on
  every client-facing route, stamps the resulting principal on each lease as its
  **owner**, refuses release / connect / ticket-mint to anyone else, and scopes
  `GET /leases` to the caller unless it holds `auth.admin_scope`. What it does
  **not** do:
  - **It verifies identity; it never issues it.** There is no login, no user
    store, no token endpoint. Credentials come from a static table you write, or
    from your own identity provider, or from a proxy you already trust. The one
    credential the broker mints is a WebSocket [ticket](#connecting-the-ticket-flow),
    which is a lease-scoped capability, not an identity.
  - **Authorization is lease ownership plus one read-only admin scope.** No roles,
    no policy engine, no per-tenant quotas. `tenant` is carried on the principal
    and recorded, but nothing enforces it.
  - **No mTLS, and no TLS at all.** The broker speaks plain HTTP on
    `listen_addr`; terminate TLS at a proxy and set
    [`advertise_addr`](#behind-a-proxy-set-advertise_addr) to the `wss://` address
    clients use. Client certificates are not a supported credential.
  - **No per-tenant rate limiting.** `max_concurrent` is a global cap — not a
    per-principal one and not a per-binary one — so one caller, or one variant,
    can fill the queue for everybody. Nor is the
    [binary registry](#serving-several-nexus-variants-the-binary-registry) part
    of the access-control surface: any caller allowed to claim may name any
    registered entry.
  - **No OS-level sandboxing of instances**, either — access control does not
    change what a spawned process can do to the host (see the last caveat below).
  - With **no** `auth:` block, none of the above is enforced at all: any client
    that can reach the broker can claim, connect to, and release any instance. The
    broker logs one `WARN` at boot saying so.
- **Single broker, single host.** Restart-reattach works; genuine clustering does
  not. There is **no shared lease registry**, no cross-broker `GET /leases`, and
  no routing of a request to the broker that owns the lease.
  With `state_dir` **unset**,
  a broker **restart orphans running instances** and loses all lease tracking —
  the orphaned `nexus` processes must be cleaned up manually. **With `state_dir`
  set, a restart no longer orphans them**: the broker replays its journal, drops
  the leases whose process is gone, restores the rest with their owners and
  capacity slots, and the surviving instances reattach on their own reconnect
  backoff — reaped after `reattach_window` if they do not (see
  [Surviving a restart](#surviving-a-restart-set-state_dir)). Recovery is
  strictly single-broker: it only ever reclaims leases stamped with **this**
  broker's `broker_id`. Running several brokers behind one load balancer does
  **not** work as a cluster: a lease lives on exactly one process, so each broker
  must be individually addressable via `advertise_addr`, clients must reconnect to
  the URL the claim returned rather than to the LB, and each broker needs its
  **own** `state_dir` — they never read each other's.
- **The session → binary check is advisory.** A resume is reconciled against the
  variant recorded for that session, but the recording is best-effort: a broker
  with no `state_dir` keeps nothing, the index is capped at 4096 bindings, and
  sessions created before the feature (or by another broker) have none. An
  unrecorded session resumes unchecked, so
  [do not treat the `409` as a guarantee](#a-resume-re-uses-the-binary-that-created-the-session).
- **Cold-spawn per claim.** There is no pre-warm pool, so each claim pays full
  engine boot latency before the instance signals ready.
- **No OS-level per-tenant sandboxing.** Instances are separate processes but
  are not otherwise sandboxed from each other or the host beyond what the OS
  user provides.

## See also

- [`nexus.io.broker` plugin](../plugins/io/broker.md) — the dial-back transport
  inside each instance.
- [Configuration Reference](../configuration/reference.md#session-broker-nexus-broker)
  — authoritative broker + plugin config keys.
- [Authentication (`auth:`)](../configuration/reference.md#authentication-auth) —
  every validator key, default and validation rule, plus the per-route status
  mapping and the audit-record shape.
- [Sessions](../architecture/sessions.md) — on-disk session layout and `-recall`.
