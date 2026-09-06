# Operating Object Storage

This is the page for running a Nexus deployment whose state lives in a bucket:
what the bucket looks like, what accumulates in it, how to reclaim what is dead,
what a turn costs, and what to alert on.

For getting a backend wired into a binary at all — module import, config keys,
credentials, and the feature's documented limitations — see the
[Object Storage](../guides/object-storage.md) guide. This page assumes that is
done and something is running.

> **One writing host per session.** Single-writer is assumed and not enforced.
> Nothing on this page is safe if two hosts have the same session open — see
> [Limitations → 1](../guides/object-storage.md#1-single-writer-is-assumed-and-not-enforced).

## What is in the bucket

Four key roots, all beside each other under `core.object_store.prefix`. They are
siblings, never nested, so a lifecycle rule written against one cannot
accidentally match another.

| Key prefix | Holds | Lifetime |
|---|---|---|
| `sessions/<session-id>/` | One session tree: `context/`, `files/`, `metadata/`, `plugins/`, blobs | The session's |
| `sessions/<session-id>.snapshot.json` | Commit marker: the generation that is durably present | The session's |
| `sessions/<session-id>.manifest/manifest.json` | The per-object set that generation asserts | The session's |
| `plugins/<plugin-id>/store.db` | App-scope plugin SQLite | Machine/deployment lifetime |
| `agents/<agent-id>/plugins/<plugin-id>/store.db` | Agent-scope plugin SQLite | The agent's |
| `eval/<run-id>/` | One eval run's output | The run's |

Two of those are easy to misread:

- **`<session-id>.snapshot.json` and `<session-id>.manifest/` are siblings of the
  session tree, not members of it.** That is deliberate: hydrating the tree must
  not drag the commit record down with it, and a prefix match on
  `sessions/<id>/` must not return the marker. It also means the key
  `sessions/sess-1` region contains an object *and* a prefix — see
  [MinIO's inability to represent that](../guides/object-storage.md#8-minio-cannot-hold-an-object-at-another-keys-prefix)
  if you run MinIO for real rather than as an emulator.
- **The shared roots have no session in their key at all.** `plugins/` and
  `agents/` outlive every session, which is exactly why they get no owner marker
  and no conflict detection.

## Bucket lifecycle policy

Nexus deletes objects only as part of a snapshot's committed-set prune. **It has
no retention policy, no expiry, and no notion of an old session** — it will never
delete a session because it is old, only because a newer generation of that same
session no longer names the object. Retention is entirely the operator's.

Set the lifecycle rule against the key prefixes above, not against the bucket
root, or you will expire the shared plugin stores along with the sessions. Those
have no age: `plugins/<id>/store.db` is rewritten in place and is as live on day
400 as on day 1. **An expiry rule that matches `plugins/` destroys long-term
memory, ingested corpora and every other app-scope store, silently, and the next
boot will hydrate nothing and start empty.**

### S3

```json
{
  "Rules": [
    {
      "ID": "expire-old-sessions",
      "Status": "Enabled",
      "Filter": { "Prefix": "prod/nexus/sessions/" },
      "Expiration": { "Days": 90 },
      "NoncurrentVersionExpiration": { "NoncurrentDays": 7 },
      "AbortIncompleteMultipartUpload": { "DaysAfterInitiation": 1 }
    },
    {
      "ID": "expire-eval-runs",
      "Status": "Enabled",
      "Filter": { "Prefix": "prod/nexus/eval/" },
      "Expiration": { "Days": 30 }
    }
  ]
}
```

`AbortIncompleteMultipartUpload` matters more here than it looks. A host killed
mid-snapshot leaves partial multipart uploads that are billable and invisible to
a normal listing; without the rule they accumulate for as long as the bucket
exists, and a deployment on ephemeral compute is killed mid-snapshot routinely.

Enabling versioning is a reasonable belt-and-braces measure, but **note what it
does and does not buy**: it makes an in-place overwrite recoverable *manually*,
object by object. It does not make
[limitation 4](../guides/object-storage.md#4-an-interrupted-snapshots-in-place-overwrite-is-not-undone)
go away, because hydration restores the committed object *set* and has no idea
your bucket has versions. Set `NoncurrentVersionExpiration` or the noncurrent
versions become the dominant line on the bill.

### GCS

```yaml
lifecycle:
  rule:
    - action: { type: Delete }
      condition:
        age: 90
        matchesPrefix: ["prod/nexus/sessions/"]
    - action: { type: Delete }
      condition:
        age: 30
        matchesPrefix: ["prod/nexus/eval/"]
    - action: { type: AbortIncompleteMultipartUpload }
      condition:
        age: 1
```

**Do not use `matchesStorageClass` transitions to Nearline or Coldline on
`sessions/`.** A resumed session hydrates its *whole* tree eagerly, so a session
that has aged into Coldline pays early-deletion charges and retrieval cost on
every object at the moment someone resumes it — which is precisely when latency
matters most. Transition storage classes are for data you expect not to read;
a session tree is data you expect to read all at once, unpredictably.

## Reclaiming orphans

An orphan is an object no live session names. Three ways they appear, with
different remedies:

1. **A session was abandoned rather than finished.** The tree, marker and
   manifest are all intact and consistent; nothing will ever read them again.
   Age-based lifecycle expiry is the right tool, and the only one — Nexus cannot
   tell an abandoned session from an idle one.

2. **A snapshot was interrupted after uploading and before committing.** Objects
   exist that the committed manifest does not name. Hydration ignores them by
   design, so they are correctness-neutral and cost-only. They are also the only
   class you can reclaim precisely.

3. **A session was deleted locally but not remotely.** Deleting
   `~/.nexus/sessions/<id>` on a host does not delete the bucket prefix. There is
   no `nexus session rm --remote`.

To reclaim class 2 for one session, compare the manifest against the tree:

```bash
SESSION=sess-20260901-1a2b
PREFIX=prod/nexus/sessions

# What the last successful commit asserts is present. `objects` is a sorted
# array of session-RELATIVE paths ("files/notes.md"), not full keys.
aws s3 cp "s3://$BUCKET/$PREFIX/$SESSION.manifest/manifest.json" - \
  | jq -r '.objects[]' | sort > /tmp/committed.txt

# What is actually there, with the same prefix stripped so the two lists are
# comparable. Forgetting this makes every object look like an orphan.
aws s3 ls --recursive "s3://$BUCKET/$PREFIX/$SESSION/" \
  | awk '{print $4}' | sed "s|^$PREFIX/$SESSION/||" | sort > /tmp/present.txt

# Present but not committed: safe to delete IF the session is not running.
comm -13 /tmp/committed.txt /tmp/present.txt
```

The manifest also carries `generation`, `completed_at` and `key_prefix`. Check
`generation` against the commit marker at `$PREFIX/$SESSION.snapshot.json`
before trusting the list: if they disagree, you are reading a manifest from a
snapshot that never committed, and its object set is not the live one.

**Read the last line literally.** If the session is live, that diff is not a list
of orphans — it is a list of objects a snapshot has uploaded but not yet
committed, and deleting them corrupts the generation in flight. Check the owner
marker's heartbeat first, or do this only against sessions whose host is
provably gone.

There is no built-in command for any of this. Writing one that is safe requires
answering "is this session live?" without a lock, which is the same problem
single-writer enforcement has, so it is unlikely to arrive as a small feature.

## Reading the cost off the log

Every snapshot logs one line at `INFO`. It is emitted on every turn boundary
rather than sampled, so it is a complete record and it is also the highest-volume
line the object-store code produces.

```
INFO object store: session snapshot session_id=… trigger=turn reason=…
     sequence=12 generation=12 objects=41 bytes=1839204
     objects_uploaded=4 bytes_uploaded=91232
     objects_skipped=37 bytes_skipped=1747972
     manifest_bytes=3812 db_bytes=1622016 db_duration=41ms
     shared_objects=2 shared_bytes=204800 shared_db_duration=12ms
     duration=131ms
```

| Field | What it answers |
|---|---|
| `bytes` | How big the stored session is — the number that drives **storage** cost |
| `bytes_uploaded` | What this turn actually transferred — the number that drives **request and egress** cost |
| `objects_uploaded` | PUT request count for this turn, the unit S3 and GCS bill per-request on |
| `bytes_skipped` | What immutable-skip saved. If this is near zero on a blob-heavy session, skip is not engaging and something is being rewritten |
| `db_bytes` / `db_duration` | The `VACUUM INTO` snapshot of session SQLite — usually the largest single object and the largest single cost |
| `shared_*` | The same for app- and agent-scope stores |
| `generation` | Increases across a resume onto a different host; `sequence` restarts per run |

**`bytes_uploaded`, not `bytes`, is the per-turn cost.** Confusing the two
overestimates a busy session's bill by orders of magnitude — a 91 MiB tree with a
few KiB of turn output reports `bytes=95000000 bytes_uploaded=91232`.

A rough monthly estimate, per session:

```
PUTs   = objects_uploaded × turns_per_month
egress = bytes_uploaded  × turns_per_month     (usually free inbound; outbound on resume)
GETs   = 2 per resume                          (the tree, then the manifest)
storage= bytes                                 (steady state, if the session stays live)
```

The `GETs = 2` line is not an approximation. Cold start is exactly two backend
round trips regardless of session size, and
`pkg/engine/session_objectstore_coldstart_test.go` fails if that changes.

The same numbers go out on the bus as
[`session.snapshot.result`](../events/reference.md#session-events), which is the
better source if you have somewhere to put them — it is structured, and it
carries `ok` so you can distinguish a snapshot that reported numbers from one
that failed.

## What to alert on

Three events, in descending order of how much they should wake someone.

**`session.owner.conflict` — page.** Two hosts believe they own one session.
There is no lock, so this is *detection after the fact*: state has probably
already been lost, and it will keep being lost until one of them stops. The
payload carries `holder_host`, `holder_pid`, `holder_instance_id` and
`heartbeat_age_seconds`; `holder_instance_id` is what distinguishes two
containers that happen to share a hostname and a PID. Treat a stale heartbeat as
weak evidence, not proof — the two clocks are not the same clock, which is why
the payload carries both the timestamp and the age.

**`session.storage.degraded` — alert.** The store stopped accepting this
session's state. Under `degrade` (the default) turns keep succeeding and the
durability guarantee is simply not being met until it clears, so **nothing else
will tell you**. The payload carries `since` (when the episode opened, not when
the event fired) and `consecutive_failures`. Alert on the episode lasting, not on
the first event.

**`session.storage.recovered` — the paired close.** Alerting on `degraded`
without tracking `recovered` produces an alert that never resolves. An episode
that opens and never closes is the real signal.

Under `failure_policy: strict` the degraded episode also gates the *next* turn,
so a user-visible failure follows — but only on the next turn, never the one that
failed. See
[limitation 2](../guides/object-storage.md#2-strict-gates-the-next-turn-not-the-failed-one).

Worth a dashboard rather than an alert: `bytes_uploaded` per turn trending up
(something stopped being skippable), and `duration` trending up against flat
`objects` (the store is slowing down, not the session growing).

## Kubernetes

Nothing here is required — object storage works from a bare process with ambient
credentials. These are the shapes that are easy to get subtly wrong.

> **The stock binaries cannot use object storage.** `bin/nexus` and
> `bin/nexus-broker` import no backend, by design. Every manifest below assumes
> **your own image**, built from a `main` that blank-imports a backend module.
> See [Wiring a backend into your
> binary](../guides/object-storage.md#wiring-a-backend-into-your-binary).

### EKS with IRSA

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: nexus
  namespace: agents
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::111122223333:role/nexus-session-store
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nexus
  namespace: agents
spec:
  replicas: 1                      # see the note below — this is not a default
  selector:
    matchLabels: { app: nexus }
  template:
    metadata:
      labels: { app: nexus }
    spec:
      serviceAccountName: nexus
      containers:
        - name: nexus
          image: ghcr.io/example/nexus-with-s3:v0.19.0
          args: ["-config", "/etc/nexus/config.yaml"]
          env:
            - name: AWS_REGION
              value: us-east-1
            # IMDSv2 only. Without this a pod on a node with IMDSv1 disabled
            # falls back silently and takes far longer to fail than to succeed.
            - name: AWS_EC2_METADATA_SERVICE_ENDPOINT_MODE
              value: IPv4
          volumeMounts:
            - { name: config, mountPath: /etc/nexus, readOnly: true }
            - { name: work, mountPath: /root/.nexus }
      volumes:
        - name: config
          configMap: { name: nexus-config }
        # The local working copy. emptyDir is correct: the bucket is the source
        # of truth and this is scratch that hydration refills.
        - name: work
          emptyDir: {}
```

**`replicas: 1` is load-bearing, not a starting value.** Two replicas resuming
the same session is precisely the unenforced single-writer case, and the symptom
is silent state loss rather than an error. If you need concurrency, give each
replica its own sessions — the seam is safe for many hosts writing *different*
sessions to one bucket, and unsafe for two writing the same one.

The IAM policy needs `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` and
`s3:ListBucket`. `ListBucket` is on the bucket ARN, the other three on
`<bucket>/<prefix>/*`; a policy that grants the object actions but not
`ListBucket` fails at hydration rather than at boot, which is a confusing place
to find out.

### GKE with Workload Identity

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: nexus
  namespace: agents
  annotations:
    iam.gke.io/gcp-service-account: nexus-session-store@my-project.iam.gserviceaccount.com
```

Then bind it:

```bash
gcloud iam service-accounts add-iam-policy-binding \
  nexus-session-store@my-project.iam.gserviceaccount.com \
  --role roles/iam.workloadIdentityUser \
  --member "serviceAccount:my-project.svc.id.goog[agents/nexus]"
```

The role wanted is `roles/storage.objectAdmin` scoped to the bucket. `objectUser`
is not enough: the snapshot's committed-set prune deletes.

The Deployment is otherwise identical to the EKS one, minus the AWS env vars.

### Static credentials, where identity is not available

```yaml
          env:
            - name: GOOGLE_APPLICATION_CREDENTIALS
              value: /etc/nexus-creds/key.json
          volumeMounts:
            - { name: creds, mountPath: /etc/nexus-creds, readOnly: true }
      volumes:
        - name: creds
          secret:
            secretName: nexus-gcs-key
            defaultMode: 0400
```

Prefer workload identity. A mounted key is a credential with no expiry sitting in
a filesystem, and rotating it means rolling every pod.

## Verifying workload identity for real

**No emulator reproduces any of this.** MinIO does not implement IRSA, IMDSv2 or
ECS task roles; fake-gcs-server does not implement ADC, Workload Identity or WIF.
Nothing in this repository exercises the credential path you will actually run
on, so a green CI is no evidence at all here. This is
[limitation 7](../guides/object-storage.md#7-workload-identity-is-exercised-by-nothing-in-this-repo),
and this section is how to close it yourself before you depend on it.

Run this against a real cluster, with the real image, before production:

1. **Prove the identity resolves at all**, separately from Nexus, so a failure
   has one possible cause:

   ```bash
   kubectl -n agents run cred-probe --rm -it --restart=Never \
     --overrides='{"spec":{"serviceAccountName":"nexus"}}' \
     --image=amazon/aws-cli -- sts get-caller-identity
   ```

   The ARN must be the assumed role, not the node instance role. **Getting the
   node role here is the most common failure and it looks like success** — the
   call returns 200, and the pod then has whatever the node can do, which on a
   permissive cluster may include the bucket. It will break the day node
   permissions are tightened, far from this change.

   The GCP equivalent:

   ```bash
   kubectl -n agents run cred-probe --rm -it --restart=Never \
     --overrides='{"spec":{"serviceAccountName":"nexus"}}' \
     --image=google/cloud-sdk:slim -- \
     gcloud auth print-access-token
   ```

2. **Prove the boot probe passes.** Both backends probe credentials at open and
   fail the boot rather than deferring to first write — `storage.NewClient`
   notably does *not* error on missing credentials, it returns a client that
   fails later. A clean boot with a `core.object_store` block therefore means the
   credential resolved.

3. **Prove a write lands.** A clean boot is not enough: with no backend named, no
   object-store code runs at all. Drive one turn, then look for the snapshot log
   line and confirm the object count against the bucket:

   ```bash
   kubectl -n agents logs deploy/nexus | grep 'session snapshot'
   aws s3 ls --recursive "s3://$BUCKET/prod/nexus/sessions/$SESSION/" | wc -l
   ```

4. **Prove a resume works, on a different pod.** This is the whole feature, and
   it is the step that catches a bucket that is writable but not readable, a
   prefix mismatch between environments, and a `ListBucket` permission missing
   from the policy:

   ```bash
   kubectl -n agents delete pod -l app=nexus
   # then resume the same session id on the new pod and confirm its history
   ```

5. **Prove the failure mode.** Remove the IAM permission and confirm you get
   `session.storage.degraded` and your alert fires — not silence. Under
   `degrade`, silence is exactly what an unmonitored deployment gets.

Steps 4 and 5 are the ones worth writing down the results of. They are the two
that fail for environment reasons rather than code reasons, and the two nothing
in CI can ever tell you about.

## See also

- [Object Storage](../guides/object-storage.md) — the adoption guide, and the
  full list of documented limitations
- [Configuration Reference → `core.object_store`](../configuration/reference.md#coreobject_store)
  — canonical for the keys
- [Sessions](../architecture/sessions.md) — the seam's design and why it is a
  lifecycle interface rather than a filesystem abstraction
- [Storage](../architecture/storage.md) — app- and agent-scope stores, and why
  shared roots have no owner marker
- [Session Broker](../guides/session-broker.md) — running instances that need a
  custom binary registered under `binaries:`
