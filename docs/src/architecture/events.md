# Events

The `pkg/events` package defines every typed payload that flows over the
engine bus. Plugins import these types directly; the journal
(`pkg/engine/journal/`) writes them to disk; out-of-tree consumers
(replay tools, dashboards, MCP servers, embedders) read those journals
and depend on the same struct shapes.

Because the contract is observable to many parties, every change to a
payload struct is a potential compatibility event. Nexus uses a simple
per-event-type versioning scheme to make those events explicit and
auditable.

## Versioning convention

Every top-level event-payload struct carries two things:

1. A version constant: `<StructName>Version = 1`.
2. A `SchemaVersion int \`json:"_schema_version"\`` field as the first
   declared field on the struct.

Producers stamp `SchemaVersion = <StructName>Version` on every emitted
literal. The journal records the stamped value verbatim, so downstream
consumers can branch on the contract version without correlating to
build metadata or git revisions.

Example:

```go
const LLMRequestVersion = 1

type LLMRequest struct {
    SchemaVersion int `json:"_schema_version"`

    Role     string
    Model    string
    Messages []Message
    // ...
}

// Producer:
_ = bus.Emit("llm.request", events.LLMRequest{
    SchemaVersion: events.LLMRequestVersion,
    Role:          "balanced",
    // ...
})
```

Versions start at `1`. Nexus is a fresh project — there is no
historical drift to encode in a `0`-baseline. The only place `0`
appears is the deserialization rule below.

### Why per-type versions, not a global one

Because event payloads churn at very different rates. `llm.request` may
gain a field every quarter; `core.tick` is unlikely to ever change.
Coupling them into a single global version forces a cascade of
consumer-side compatibility code that is mostly noise.

## The v0 == v1 deserialization rule

When a journal record (or a third-party producer that hasn't yet
adopted the field) leaves out `_schema_version`, JSON deserializes it
to Go's zero value: `0`. Consumers MUST treat `0` as `v1` — the
running code's contract — rather than reject the payload.

This keeps:

- **Journals written before the field existed replayable.** Idea 01
  (durable journal) shipped before Phase 4 of Idea 10. Replay must
  flow through the new code path without rewriting old records.
- **Embedders that haven't pulled the latest Nexus minor
  interoperable.** Producers running an older Nexus emit payloads
  without the field; the new bus should accept them.

The rule is implicit while v1 is the only shipped version: nothing
special happens during unmarshal. The first `v2` to ship will register
a `{Type, From: 0, To: 1}` no-op in `pkg/events/compat/` plus the real
`{Type, From: 1, To: 2}` migrator chained after it. `compat.Apply` is
the single entry point.

## Compat package

`pkg/events/compat/` holds field-level migrations between versions. The
public surface is small:

```go
type Key struct {
    EventType string  // "llm.request"
    From, To  int
}

type Migrator func(payload map[string]any) (map[string]any, error)

var Migrations = map[Key]Migrator{}

func Apply(eventType string, from, to int, payload map[string]any) (map[string]any, error)
```

`Apply` chains one-step migrators from `from` up to `to`. With the
registry empty — today's state — Apply is a no-op pass-through.

Compat is wired into the journal-replay path in two places:

- `pkg/engine/engine.go` — `replayPayloadConverter` calls `compat.Apply`
  before `journal.PayloadAs[T]` re-types map payloads back into structs
  for live re-emission during `engine.ReplaySession`.
- `pkg/eval/runner/runner.go` — same pattern for the eval harness's
  case-driven replay path.

When a future PR ships `v2` of an event type, the migrator goes into
`pkg/events/compat/` and replay-time data flows through it without
touching the engine. No engine code change required.

## Lint rule guarantee

`make check-events` (alias of `scripts/check-event-versions.sh`,
backed by `internal/cmd/check-event-versions/`) compares the working
tree's `pkg/events/*.go` against a base git revision (default
`HEAD~1`, override via `CHECK_EVENTS_BASE`). It fails the build when:

- A field was **removed** from an existing struct without bumping the
  matching `<Name>Version` constant.
- A field was **renamed** (heuristic: same position + same type, but
  different name) without a bump.
- A field's **type** changed without a bump.

Additive changes (new fields with sensible zero defaults) pass without
a bump because they are forward-compatible — older consumers ignore
the unknown field, JSON round-trips preserve it.

The rule wires into `make lint` so existing Go-quality CI gates
catch schema regressions automatically.

False positives (e.g., reordering fields with identical types) are
tolerable — the operator just bumps the version trivially. False
negatives (a rename slipping through) are the failure case the rule
guards against; the position+type+name comparator catches that class.

## Author guide

### Adding a new event type

1. Define the struct in the appropriate per-domain file
   (`core.go`, `llm.go`, `agent.go`, …) with `SchemaVersion int
   \`json:"_schema_version"\`` as the first declared field.
2. Add `<Name>Version = 1` to that file's version-constants block.
3. List the struct in `pkg/events/version_test.go`'s
   `versionedPayloads()` table so the round-trip test covers it.
4. Producers must stamp `SchemaVersion: events.<Name>Version` on every
   literal they emit.

### Mutating an existing event type

- **Adding a field** with a sensible zero default — go ahead. No bump
  needed; the lint check passes.
- **Removing a field** — bump `<Name>Version` and register a
  `{Type, From: oldVer, To: newVer}` migrator in
  `pkg/events/compat/` that drops the field from old payloads (or
  rewrites it onto a replacement field).
- **Renaming a field** — same as removal: bump the version, register a
  migrator that copies the old key to the new key.
- **Changing a field's type** — same: bump and register a converter.

The `pkg/events/compat/compat_test.go` placeholder test demonstrates
the registration pattern; copy it for new migrators.

### What NOT to version

Helper structs that exist only as nested fields inside top-level
payloads (`Message`, `Citation`, `ToolCallRequest`, `Usage`, etc.)
deliberately do **not** carry `SchemaVersion`. They have no
independent identity on the wire — their version is the version of
the enclosing payload. Versioning them would double-count migrations
and clutter every literal.

When in doubt, version it; the cost of an extra `int` is dwarfed by
the cost of an undetected breaking change.
