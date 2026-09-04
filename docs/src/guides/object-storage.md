# Object Storage

Nexus keeps everything it persists on local disk: session trees, per-plugin
SQLite, eval run output. `core.object_store` optionally makes a remote object
store the source of truth for that state **between** runs, so a session can be
killed on one host and resumed on another with no shared filesystem — the case
for containers, Cloud Run, Lambda and Kubernetes Jobs, where there is no disk
between invocations.

This page is the adoption path. It covers what the seam is, how to wire a
backend into your own binary, credentials for each shipped backend, what happens
when the store is unreachable, and — at the end, in full — the things it
deliberately does not do.

[Configuration Reference → `core.object_store`](../configuration/reference.md#coreobject_store)
is canonical for the keys, their defaults and their validation behaviour. This
page adds narrative; where the two disagree, the reference page wins.

---

## Read this first: one writing host per session

**The seam assumes a session has exactly one writing host at a time. Nothing
enforces that.**

There is no lock, no lease, no fencing token and no expiry the engine waits on.
If two processes hydrate the same session ID and both keep running, both
snapshot the whole tree at their own turn boundaries, and the loser's
conversation history, journal and per-plugin `store.db` are **overwritten at
whole-file granularity with no error anywhere**. The consequence is silent state
loss, not a failed request.

An **owner marker** at `sessions/<id>.owner/owner.json` makes the situation
*diagnosable*. On `Boot` the engine writes its own host, PID, instance ID and a
heartbeat, and reads whatever was already there. A marker that still looks live
produces an error-level log line and a
[`session.owner.conflict`](../events/reference.md#session-events) event.

That is **detection, not prevention**. By the time the event is emitted the
engine has already claimed the session and is running normally. Nothing is
refused, nothing waits, and no subsequent write is blocked. A subscriber that
wants to act — page an operator, stop the run — has to do so itself.

If your deployment can produce two live processes for one session ID — a
scheduler that presumed an instance dead while it was still running is the
routine case on ephemeral compute, not an exotic one — **single-writer is yours
to arrange**, upstream of Nexus. Subscribe to `session.owner.conflict` and treat
it as a page.

The same assumption applies to the app- and agent-scope plugin stores, with less
help: those are shared across sessions by definition and **have no owner marker
at all**. Two processes on one host share the local file and SQLite serialises
them, so the later upload is a superset of the earlier and no data is lost. Two
processes on *different* hosts each hold their own copy, and the later flush
overwrites the other's whole database. See
[Per-Plugin Storage → Concurrency](../architecture/storage.md#concurrency).

---

## What the seam is

`pkg/engine/objectstore.Backend` is a **lifecycle** interface, not an
abstraction over `os.*`:

```go
Hydrate(ctx context.Context, keyPrefix, destDir string) error
Put(ctx context.Context, key, localPath string) error
Delete(ctx context.Context, key string) error
List(ctx context.Context, keyPrefix string) ([]Object, error)
Flush(ctx context.Context) error
```

Core and every plugin keep reading and writing **ordinary local files**. The
engine calls the backend at defined lifecycle points and nowhere else, which is
why "behaves exactly like local disk" is a guarantee rather than an aspiration,
and why SQLite keeps running against a real file with a real WAL.

Nothing about this is exposed to plugins. There is no `PluginContext` method, no
interface for a plugin to implement, and no plugin in the tree knows an object
store exists.

| When | What happens |
|---|---|
| Top of `Boot` | The backend named in config is opened once. A failure fails the boot. |
| Top of `Boot`, before any plugin can open storage | App- and agent-scope plugin stores are hydrated. |
| Before a resumed workspace is opened | The whole session tree is hydrated under `sessions/<id>`, then pruned to exactly the object set the committed manifest names. |
| Once the workspace exists | The owner marker is claimed and a conflict is detected (see above). |
| Every turn boundary (`agent.turn.end`) | The whole tree is snapshotted and made durable, a per-object manifest and then a commit marker are published, then the shared plugin stores are snapshotted. |
| End of `Stop` | A final snapshot, the owner marker is removed, `Flush`, then the backend is released. |
| `Abandon` | Workers stop, the handle is dropped. No snapshot, no `Flush`, marker left in place. |

Hydration is **eager and whole-tree** and completes before the first turn runs.
There is no lazy or faulting read path — see
[limitation 5](#5-cold-start-grows-with-session-size).

The snapshot is **synchronous**: it blocks the goroutine that ended the turn
until the bytes are durable, because a turn reported complete while its state is
still in flight is exactly the guarantee the snapshot exists to provide.

### Four roots, one backend

The session tree is one of four roots. The same backend — no per-root methods,
no per-root config — carries all four:

| Root | Local path | Object key |
|---|---|---|
| Session tree | `<core.sessions.root>/<id>/` | `sessions/<id>/…` |
| App-scope plugin storage | `<core.storage.root>/plugins/<pluginID>/store.db` | `plugins/<pluginID>/store.db` |
| Agent-scope plugin storage | `<core.storage.root>/agents/<agent_id>/plugins/<pluginID>/store.db` | `agents/<agent_id>/plugins/<pluginID>/store.db` |
| Eval run output | `<eval.reports_dir>/<run-id>/` | `eval/<run-id>/…` |

Keys mirror the on-disk layout, one key segment per directory, under
`core.object_store.prefix`. Nothing is encoded, hashed or flattened, so the
bucket is browsable and
`<prefix>/sessions/<id>/plugins/nexus.scene/scene.jsonl` is exactly the path it
came from. Both shipped backends produce byte-identical layouts, so a deployment
migrating between clouds can use the vendors' own copy tools with no translation
step.

### What never crosses the seam

- **`session.lock`** — it records the PID of the process holding the session on
  one machine. A lock that travelled would make every rehydrated session look
  permanently locked.
- **SQLite sidecars** (`store.db-wal`, `store.db-shm`, `store.db-journal`) —
  they describe a machine, not a session. Each `store.db` is
  WAL-checkpointed (`wal_checkpoint(TRUNCATE)` then `VACUUM INTO`) and uploaded
  as a standalone file, so the restored database needs no sidecars beside it.

---

## Wiring a backend into your binary

Two backends ship in-repo, each as its own Go module so the root module's
dependency list never grows:

| Module | Backend name | Covers |
|---|---|---|
| `github.com/frankbardon/nexus/modules/objectstore-s3` | `s3` | Amazon S3, and every S3-compatible store: MinIO, Cloudflare R2, Ceph RGW, Backblaze B2 |
| `github.com/frankbardon/nexus/modules/objectstore-gcs` | `gcs` | Google Cloud Storage, and the Cloud Storage emulators |

**Neither is in `bin/nexus`.** Adopting object storage means building your own
host binary — see [limitation 6](#6-the-shipped-binaries-cannot-use-object-storage).

### 1. Add the module to your program

```console
$ go get github.com/frankbardon/nexus@v0.19.0
$ go get github.com/frankbardon/nexus/modules/objectstore-s3@v0.1.0
```

The backend module is versioned independently of the core module and its tags
are cut on demand, not on every core release (see
[Repository Go Modules → Versioning and tagging](./go-modules.md#versioning-and-tagging)).
So a `modules/objectstore-s3/vX.Y.Z` may not exist for every core version — when
the one you want has no tag, Go resolves a branch or a commit SHA to a
pseudo-version (`@main`), and you pin a real tag once one is cut.

**The backend module requires a core version that has the seam.** Each backend's
`go.mod` names one, and `objectstore-s3/v0.1.0` requires core `v0.19.0`, the
release the seam first shipped in. Asking for an older core than the backend
declares does not silently degrade — it fails to build, because
`pkg/engine/objectstore` is not there to import.

**Go 1.26 or newer.** The core module's floor moved there when three dependencies
did; there is no supported build on 1.25.

### 2. Blank-import it

The module registers itself under its backend name from `init`, in the
`database/sql` driver style. Importing it for its side effect is the entire
wiring step — no factory to construct, no option to pass, nothing to hand to the
engine:

```go
import _ "github.com/frankbardon/nexus/modules/objectstore-s3"
```

### 3. Name it in config

```yaml
core:
  object_store:
    backend: s3
    bucket: nexus-sessions
    prefix: prod/nexus
    region: eu-west-2
    failure_policy: degrade
```

That is all of it. If you skip step 2 and keep step 3, the boot fails with the
diagnosis rather than a runtime surprise:

```
core.object_store.backend "s3" is not a registered object-store backend
(registered: none — no backend module is imported into this build);
add the backend module to your build and import it for its side effect
```

### The whole program

A minimal host that is `nexus` plus a bucket:

```go
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/allplugins"

	// Registers the "s3" object-store backend under that name. Nothing else in
	// the program references this package — naming it in config is the wiring.
	_ "github.com/frankbardon/nexus/modules/objectstore-s3"
)

func main() {
	eng, err := engine.New("config.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "engine: %v\n", err)
		os.Exit(1)
	}

	// Resuming is what makes the bucket load-bearing: with an ID set, Boot
	// hydrates that session's whole tree before the first turn runs.
	eng.RecallSessionID = os.Getenv("NEXUS_RECALL")

	allplugins.RegisterAll(eng.Registry)

	if err := eng.Run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "run: %v\n", err)
		os.Exit(1)
	}
}
```

Swap `objectstore-s3` for `objectstore-gcs` and `backend: s3` for `backend: gcs`
to run against Google Cloud Storage. Nothing else in the program changes.

`engine.NewFromBytes` is the alternative constructor for a host that
`//go:embed`s its `config.yaml` and wants no filesystem dependency at boot.
Embedders that own their own lifecycle (a desktop shell, a test harness) call
`Boot` and `Stop` directly rather than `Run`, which owns signal handling.

### Verifying the wiring

With no backend named — the default — **no object-store code runs at all**: no
handle is opened, no snapshot handler is subscribed, and a backend sitting
registered in the process is never touched. So "it booted" is not evidence the
store is engaged. Look for the snapshot log line, which is emitted on every
turn boundary rather than sampled:

```
INFO object store: session snapshot session_id=… trigger=turn generation=3
     objects=41 bytes=1839204 objects_uploaded=4 bytes_uploaded=91232
     objects_skipped=37 db_bytes=1622016 duration=131ms
```

(Abridged — the real line also carries `reason`, `sequence`, `manifest_bytes`,
`bytes_skipped`, `db_duration` and the `shared_*` counters for the app- and
agent-scope stores.)

`bytes_uploaded`, not `bytes`, is the per-turn cost. The same numbers go out on
the bus as
[`session.snapshot.result`](../events/reference.md#session-events).

---

## Credentials

Neither backend creates the bucket, and neither checks it at boot. A boot-time
round trip to the store would make `failure_policy: degrade` structurally unable
to do its job, so what is validated at boot is the *configuration* — a malformed
endpoint, a `credentials_file` that is not there, an unresolvable region — and
nothing remote.

### `s3` — ambient credentials (production)

Leave `credentials_file` empty. The backend uses the AWS SDK's default
credential chain, neither reordered nor narrowed: environment variables, the
shared config and credentials files, IRSA / EKS Pod Identity, ECS task roles,
and the EC2 instance role via IMDSv2 — with expiry-aware refresh.

```yaml
core:
  object_store:
    backend: s3
    bucket: nexus-sessions
    prefix: prod/nexus
    region: eu-west-2
```

The principal needs read, write, delete and list on the bucket. `region` is
required against real AWS: it is signed into every request.

### `s3` — a static credentials file

An ordinary AWS INI file, so the same file works with the AWS CLI and can be
mounted as a Kubernetes secret unchanged. `AWS_PROFILE` selects the profile.

```ini
# ~/.config/nexus/aws-credentials
[default]
aws_access_key_id = AKIA…
aws_secret_access_key = …
```

```yaml
core:
  object_store:
    backend: s3
    bucket: nexus-sessions
    credentials_file: ~/.config/nexus/aws-credentials
```

A `credentials_file` that does not exist **fails the boot**. That check is
deliberate: the SDK on its own *ignores* a missing shared credentials file and
falls through to ambient credentials, so an operator who typo'd the path would
silently authenticate as the wrong principal.

### `s3` — S3-compatible endpoints

Setting `endpoint` to an absolute `http://` or `https://` URL both points the
client somewhere else and switches it to **path-style addressing**
(`https://host/bucket/key`). That is what makes self-hosted stores work
unmodified: virtual-host addressing needs wildcard DNS and a wildcard
certificate they do not have. There is no separate path-style key and none is
needed — real AWS, which prefers virtual-host addressing, is the case where no
endpoint is set.

With `endpoint` set, `region` defaults to `us-east-1`, which every
S3-compatible store accepts and none of them interprets.

```yaml
# MinIO on a laptop. Works unchanged against Ceph RGW or Backblaze B2.
core:
  object_store:
    backend: s3
    bucket: nexus
    endpoint: http://127.0.0.1:9000
    credentials_file: ~/.config/nexus/minio-credentials
```

```yaml
# Cloudflare R2.
core:
  object_store:
    backend: s3
    bucket: nexus-sessions
    endpoint: https://<account-id>.r2.cloudflarestorage.com
    credentials_file: ~/.config/nexus/r2-credentials
```

One trap is closed for you: the backend sets the SDK's
`RequestChecksumCalculation` to `WhenRequired` whenever a custom endpoint is in
play. Without it `PutObject` switches to `aws-chunked` transfer encoding, which
several S3-compatible stores reject with a signature error that mentions nothing
about checksums.

### `gcs` — Application Default Credentials (production)

Leave `credentials_file` empty. The backend uses ADC, neither reordered nor
narrowed: `GOOGLE_APPLICATION_CREDENTIALS`, the `gcloud` well-known file, GKE
Workload Identity and the GCE service account via the metadata server,
service-account impersonation, and Workload Identity Federation — with
expiry-aware refresh and no key material on disk.

```yaml
core:
  object_store:
    backend: gcs
    bucket: nexus-sessions
    prefix: prod/nexus
```

The principal needs `storage.objects.get`, `create`, `delete` and `list` on the
bucket; `roles/storage.objectAdmin` covers exactly those. **No project ID is
needed anywhere** — a project is required to create or list *buckets*, and this
backend does neither.

`region` is **accepted and ignored**, with a warning logged once at boot: a GCS
bucket's location is fixed when the bucket is created and no client ever names
one. It is not an error because the same `core.object_store` block is shared
with `s3`, and a config that travels between the two clouds should not fail to
boot over a key that cannot change behaviour here.

### `gcs` — a static service-account key

The JSON file `gcloud iam service-accounts keys create` produces, so the same
file works with `gcloud` and can be mounted as a Kubernetes secret unchanged.

```yaml
core:
  object_store:
    backend: gcs
    bucket: nexus-sessions
    credentials_file: ~/.config/nexus/gcs-service-account.json
```

**Only that credential type is accepted.** An external-account (Workload
Identity Federation) or impersonation configuration names a URL the auth library
will fetch a token from, and accepting one from a path that may have come from a
shared config repository would hand an attacker a credential-exfiltration
primitive. Those belong on the ambient path above, via
`GOOGLE_APPLICATION_CREDENTIALS`, where an operator opts into them at the
environment level.

Credential resolution is: `credentials_file` if set; otherwise ADC if it
resolves; otherwise, **if `endpoint` is set**, an unauthenticated client, logged
at warn — the emulator path. Anything else **fails the boot**. That last step is
deliberately stricter than the Google SDK, which builds a client happily when it
cannot find credentials and fails at the first request instead; under
`failure_policy: degrade` that would be a run that starts, looks healthy and
persists nothing.

### `gcs` — emulator endpoints

Unlike `s3`, `endpoint` here is **an emulator switch, not a way to reach an
alternative provider**. GCS has one production service, reached by leaving
`endpoint` empty; a VPC using Private Google Access or Private Service Connect
gets there by DNS and routing policy, not by a client-side override.

```yaml
core:
  object_store:
    backend: gcs
    bucket: nexus
    endpoint: http://127.0.0.1:4443
```

The JSON API path (`/storage/v1/`) is appended for you when the URL has none, so
the key is spelled the same way for both backends. A URL that already carries a
path is left alone, for an emulator behind a reverse proxy on a sub-path.

---

## When the store is unreachable

`core.object_store.failure_policy` is the one durability trade-off you own
rather than the implementation. Both values retry with exponential backoff
(1 s, doubling, capped at 60 s), both surface the outage on the bus as a
`session.storage.degraded` / `session.storage.recovered` pair, and **both
recover with no operator action**. What differs is whether the session keeps
taking turns while the store is down.

| | `degrade` (default) | `strict` |
|---|---|---|
| Turn that hit the outage | completes | completes — **it is not un-run** |
| Further turns | accepted | refused until the state is stored |
| `core.error` | not raised | raised on every failed snapshot |
| Recovery | automatic | automatic |
| Boot-time hydration failure | fails the boot | fails the boot |

Pick `degrade` when an object-store outage should not take down an interactive
agent that still has a perfectly good local tree — and read
[limitation 3](#3-degrade-means-the-guarantee-is-not-being-met) for what you are
trading. Pick `strict` when running against unstored state is worse than
refusing input — and read
[limitation 2](#2-strict-gates-the-next-turn-not-the-failed-one) for what it does
not buy you.

Under `strict`, the veto runs at `before:io.input` priority 200, behind every
other subscriber, so slash commands and cancellation still work while the gate
is closed.

One thing is deliberately not policy-governed: a blob write-through failure
never closes the `strict` gate. Write-through is an optimisation in front of the
turn-boundary snapshot, which re-uploads anything the store is missing; failing
a turn because that optimisation stumbled on an object the very next snapshot
repairs would make `strict` fire on transients it is not there to catch.

Full detail, including the retry queue bounds and why they are compiled-in
constants rather than config keys:
[Configuration Reference → Failure policy](../configuration/reference.md#failure-policy).

---

## Trying it without a cloud account

Both backends have an emulator suite in-repo that starts and stops the emulator
itself, needs no cloud account and no repo secret, and runs the same
conformance suite plus a real kill-and-resume cycle:

```console
$ make test-objectstore-minio      # modules/objectstore-s3 against MinIO (needs Docker)
$ make test-objectstore-fake-gcs   # modules/objectstore-gcs against fake-gcs-server (no container runtime)
```

Both run in CI as their own jobs, and both **fail rather than skip** when an
emulator was provisioned, so a green job means the tests actually ran. Neither
covers IAM — see
[limitation 7](#7-workload-identity-is-exercised-by-nothing-in-this-repo).

---

## Limitations

Every item here is an accepted trade-off rather than a known bug, and every one
of them will look like a bug to an operator who meets it without warning.

### 1. Single-writer is assumed and not enforced

Covered at length [at the top of this page](#read-this-first-one-writing-host-per-session),
and repeated here because it is the one that loses data. Detection is
best-effort logging plus a `session.owner.conflict` event; nothing is refused.
The consequence of two writers is **silent state loss** — the loser's
`store.db`, history and journal are overwritten whole, with no error raised
anywhere.

A marker is treated as stale, and stays silent, when it belongs to this run,
when its host matches and its PID is gone, or when its heartbeat stopped
advancing more than five minutes ago. That is what keeps an ordinary
crash-resume from alarming — and it is also why a genuinely concurrent second
host on a *different* machine has to miss ten heartbeats before it is called
stale. Both thresholds are compiled-in constants.

### 2. `strict` gates the next turn, not the failed one

When a turn's state cannot be persisted, **the turn has already happened**: its
output was streamed to the user, its tools ran, and its side effects are in the
world. Nothing in Nexus can un-run it and no configuration makes it not have
happened.

What `strict` guarantees is that **no turn ever runs against state whose
predecessor was not durably stored, and the divergence is never silent.** It
does not guarantee that the turn which hit the outage was prevented. A genuine
pre-commit gate would need a vetoable turn-boundary event that does not exist,
and would not help even if it did — by the time an agent loop can report a turn,
the work is done.

### 3. `degrade` means the guarantee is not being met

Turns keep succeeding against the local working copy, and that is the point. The
honest caveat: **during an outage the durability guarantee is not being met even
though nothing is failing.** Work the user watched happen exists only on local
disk, so a host that dies while degraded loses it. On ephemeral compute, where
there is no disk between invocations, "loses it" is unqualified.

`session.storage.degraded` is the signal that this window is open, and
`session.storage.recovered` is the signal that it closed. Exactly one of each
per outage.

### 4. An interrupted snapshot's in-place overwrite is not undone

Hydration restores the committed object **set**, not per-object versions. The
manifest names paths, not versions — so a snapshot that died partway after
already re-uploading `conversation.jsonl`, the active journal segment or a
`store.db` has **replaced the committed bytes at that key**, and hydration will
restore those newer bytes because the key is in the committed set.

What you do get is that objects the committed manifest does not name are not
materialised into the tree, so a partial generation cannot add files. What you
do not get is "the previous good remote state remains restorable". Closing this
needs per-generation object keys, which was costed and rejected.

Orphaned objects a manifest no longer names are **left in the bucket, never
deleted**. Reclamation is the operator's.

### 5. Cold start grows with session size

Hydration is eager and whole-tree, and completes before the first turn runs.
There is deliberately no lazy or faulting read path: threading one through the
engine and ~60 plugins would be impossible to get right, and SQLite could not
use it at all — so "behaves exactly like local disk" would degrade from a
guarantee to an aspiration.

The consequence is that **time-to-first-turn on a resume scales with the size of
the stored session**. A long-running session that has accumulated a large
`files/` tree or a large per-plugin database will pay for it on every resume
onto a fresh host. Measure it for your own workload before assuming a resume is
cheap.

**No wall-clock budget is asserted, deliberately.** A millisecond threshold on a
shared CI runner is either flaky or so loose it catches nothing, and it fails
for reasons unrelated to this code. What *is* asserted is the cost *shape*, in
`pkg/engine/session_objectstore_coldstart_test.go`: resuming costs exactly two
backend round trips — the tree, then the committed-object manifest — regardless
of session size, zero `List` calls, and zero writes back to the store; a warm
tree costs no traffic at all; and hydration pulls only this session's key
prefix, proven with a larger neighbouring session in the same bucket. Those
numbers are exactly reproducible, and they are what turns into latency and
egress against a real store.

So a refactor toward per-object fetching — one request per file instead of one
for the tree — fails the suite rather than quietly turning one round trip into
thousands. What still will not fail the suite is the same two round trips
carrying steadily more bytes, which is the growth this section is about.

The snapshot side *is* measured: 0.05 MiB of tree costs ~12.5 ms of local engine
work, 6 MiB ~30 ms, 91 MiB ~170 ms, plus network on top. Immutable-by-identity
files (content-addressed blobs, sealed journal segments) are skipped rather than
re-uploaded, which took a blob-heavy 91 MiB tree from 2007 objects and 90.5 MiB
per turn to 7 objects and 27.9 MiB. Ordinary artifact output under `files/` is
not immutable and does re-upload.

### 6. The shipped binaries cannot use object storage

`bin/nexus` and `bin/nexus-broker` blank-import no backend, and they never will:
that is the whole reason backends are separate modules. **Setting
`core.object_store` in a config handed to the stock `nexus` binary fails the
boot** with the "no backend module is imported into this build" message — which
is the correct outcome, but it means object storage is a **library-only**
feature. Adopting it means building your own binary, as
[above](#the-whole-program).

This has a direct consequence for the session broker: the broker cold-spawns a
`nexus` subprocess, and the binary it spawns is whatever `binaries:` names. A
broker deployment that wants object-store-backed session pods must point that
config at a **custom binary** with a backend compiled in. The stock one cannot
do it.

### 7. Workload identity is exercised by nothing in this repo

**IRSA, EKS Pod Identity, IMDSv2 and ECS task roles on AWS; ADC resolution, GKE
Workload Identity and Workload Identity Federation on GCP — none of these are
covered by any test in this repository, and no emulator reproduces them.** MinIO
and fake-gcs-server exercise the data plane, not the credential chain.

That chain is precisely the reason both backends take a cloud SDK rather than
hand-rolled `net/http`, so the code being trusted here is the vendor's. It is
still untested *in this configuration*. **A manual live check against a real
cluster is warranted before relying on the workload-identity path in
production**, and it is the one part of adoption this repo cannot do for you.

[Operating Object Storage → Verifying workload identity for
real](../operations/object-storage.md#verifying-workload-identity-for-real) is
the five-step runbook for that check, including the failure that looks like
success: a pod picking up the *node* instance role instead of the assumed one
returns 200 and works, until node permissions are tightened.

### 8. MinIO cannot hold an object at another key's prefix

The engine's key scheme can produce an object at key `sessions/sess-1` beside
objects under `sessions/sess-1/…`. S3 and GCS hold that state fine — their key
spaces are flat and `/` has no meaning beyond being a byte. **MinIO cannot
represent it**: the PUT returns 200, the child objects stop appearing in any
listing, and the bucket cannot even be emptied by listing it. Measured on both
single-drive and 4-drive erasure modes.

This is emulator divergence, not a backend bug, and it is accommodated in the
conformance suite by `objectstoretest.WithoutObjectAtPrefix()`. A test fails and
tells you to remove the option if MinIO ever gains a flat key space. **If you
run MinIO in production rather than as an emulator, this is a real constraint on
your deployment, not a test detail.**

### 9. A misspelled block used to boot with object storage silently disabled

**Fixed.** Recorded here because the failure mode is worth knowing, and because
anyone running a build from before the fix still has it.

Plugin config has always been schema-validated with `additionalProperties:
false`, so a plugin-level typo failed the boot. The engine's own `core:` block
was not: `LoadConfigFromBytes` is non-strict, and the validator rebuilds the map
from the already-decoded typed config, so a key YAML decoding dropped never
reached the schema. `core: { object_stor: { … } }` therefore booted clean, with
`enabled=false backend=""` and no error — every turn succeeding, nothing ever
uploaded, and the first symptom an empty bucket after the host was replaced.

`checkUnknownConfigKeys` now walks the raw YAML against the config structs' yaml
tags and rejects an unknown key at any depth, naming the path and listing what
was valid there:

```
config: unknown key "core.object_stor" (valid keys here: agent_id, log_level,
logging, max_concurrent_events, models, object_store, sessions, storage,
tick_interval)
```

Blocks whose keys are data rather than field names are exempt and unaffected:
`plugins:` (plugin IDs, guarded by their own schemas), `core.models` (role names)
and `capabilities:` (capability names).

It is still worth verifying the wiring by looking for the snapshot log line
rather than by observing a clean boot — see [Verifying the
wiring](#verifying-the-wiring). A config can name a backend correctly and still
be pointed at the wrong bucket.

---

## Writing your own backend

The seam is public. `pkg/engine/objectstore` imports nothing outside the
standard library and names no bucket API, credential type or HTTP client, so a
backend can live in a module this repository never sees — no PR required.

Two rules are easy to read past, and both corrupt sessions rather than producing
an error:

- **Keys are validated, not merely documented.** Every method must reject a
  malformed key or prefix with an error wrapping `objectstore.ErrInvalidKey`,
  before touching the store or the filesystem. `objectstore.ValidateKey` and
  `objectstore.ValidateKeyPrefix` implement the rule; the `..` ban is what stops
  a hostile key from writing outside a hydration destination.
- **Prefixes match whole segments.** Raw string matching — the native behaviour
  of `ListObjectsV2` and its GCS equivalent — makes the prefix `sessions/sess-1`
  select `sessions/sess-10`'s objects, which mixes two sessions into one tree.
  `objectstore.TrimKeyPrefix` is the rule in code.

Hold your backend to the shared conformance suite:

```go
func TestContract(t *testing.T) {
    objectstoretest.RunSuite(t, func(t *testing.T) objectstore.Backend {
        return newMyBackend(t) // empty, cleaned up via t.Cleanup
    })
}
```

Passing the interface suite does not prove a session resumes from it.
`pkg/engine/objectstore/enginetest.RunResumeSuite` is the second half: it drives
a real engine through a kill-and-resume cycle against your store, and asks you
for four hooks — register a factory, make an empty bucket, list the bucket, read
one object. Both shipped backends run it.

Full detail:
[Sessions → Writing a backend](../architecture/sessions.md#writing-a-backend),
and [Repository Go Modules](./go-modules.md) for the module layout, the
no-`go.work` decision and the tagging policy.

---

## See also

- [Operating Object Storage](../operations/object-storage.md) — bucket lifecycle
  policy, orphan reclamation, reading cost off the snapshot log, what to alert
  on, Kubernetes manifests, and how to verify workload identity for real
- [Configuration Reference → `core.object_store`](../configuration/reference.md#coreobject_store) — canonical keys, defaults and validation
- [Sessions → Object-Store Backing](../architecture/sessions.md#object-store-backing-optional) — design and rationale
- [Per-Plugin Storage → Object storage for app and agent scope](../architecture/storage.md#object-storage-for-app-and-agent-scope)
- [Event Types → Session Events](../events/reference.md#session-events) — every event this feature emits
- [Repository Go Modules](./go-modules.md) — why backends are separate modules
