// Package events defines the typed payload structs that flow over the
// engine bus. Plugins import these types directly; the engine serializes
// them to the journal as JSON; out-of-tree consumers (replay tools,
// dashboards, MCP servers) read those journals.
//
// # Schema versioning
//
// Each top-level payload struct carries an `_schema_version` JSON field
// (the `SchemaVersion int` Go field) and a sibling `<Name>Version`
// constant declaring the running code's version of that payload. Producers
// stamp `SchemaVersion = <Name>Version` at emit time so the journal
// records which contract the payload was written under.
//
// Versions start at 1 — Nexus is a fresh project with no historical drift
// to encode. Field-level compatibility migrations between versions live in
// the sibling pkg/events/compat package; the empty registry today is the
// scaffold journal-replay code calls into when a future PR ships v2.
//
// # The v0 == v1 deserialization rule
//
// When a journal record (or a third-party producer) leaves out the
// `_schema_version` field, JSON deserializes it to Go's zero value: 0.
// Consumers MUST treat 0 as v1 — i.e., the current contract — rather
// than reject the payload. This keeps journals written before the field
// existed replayable, and keeps producers that haven't been updated yet
// (e.g. an embedder still on the previous Nexus minor) interoperable
// during the transition window.
//
// The rule is implicit while v1 is the only shipped version: nothing
// special happens during unmarshal. When a v2 ships, the compat package
// gains a `{Type, From: 0, To: 1}` migrator that is a no-op pass-through
// plus the real `{Type, From: 1, To: 2}` migrator chained after it. Apply
// is the entry point.
//
// # Shipped versions above 1
//
// Most payloads are still at v1. The ones that have moved, and why:
//
//   - io.input / UserInput — v2 added PreloadMessages, v3 added Headers.
//   - llm.request / LLMRequest — v2 added Overrides, the carrier for the
//     `core.models` chain entry a request is actually being served by
//     (native provider blocks plus the API-surface selector). Both
//     directions are compatible: a v1 payload parses into the v2 struct
//     with every axis unset, and a v1 consumer ignores the extra object.
//
// Each of those additions is an optional field, so neither needed a
// compat migrator — see the mutation rules below.
//
// # Consumer-side pinning
//
// Versions returns the whole manifest — payload struct name to its
// `<Name>Version` constant — so an embedder pinned to a released Nexus
// can name the payloads it reads in one table test and get a single
// failure listing every one that moved on a pin bump. `make check-events`
// is the producer-side guard; it diffs git revisions and cannot run from
// a module cache. Versions is the fact a consumer asserts against.
//
// A version pin is NOT a complete guard. An additive field is not required
// to bump the version and usually does not (see below; the io.input and
// llm.request bumps above were the author's choice, not the rule), so a
// payload can gain a field a consumer ought to start reading while its
// version stays put. A consumer that must react
// to new fields reads the release notes (the GitHub Release for each tag)
// for the payloads it pins; the version only signals a breaking change.
//
// # Adding a new event type
//
//  1. Define the struct in the appropriate per-domain file
//     (core.go, llm.go, agent.go, …) with a SchemaVersion int field
//     tagged `json:"_schema_version"` as the first declared field.
//  2. Add a <Name>Version = 1 constant alongside the struct.
//  3. List it in the versions manifest in versions.go (what Versions
//     returns); TestVersionsManifestCoversEveryConstant fails until you do.
//  4. Producers must stamp `SchemaVersion = <Name>Version` on every
//     literal they emit. The `make check-events` lint rule catches
//     mutations to existing structs that forget to bump the constant;
//     net-new structs are not lint-enforced (no diff).
//
// # Mutating an existing event type
//
//   - Adding a field with a sensible zero value is forward-compatible —
//     bump the version constant only when consumers must distinguish
//     pre/post-add records (e.g., to reject empty defaults from old
//     producers). The lint check accepts additive-only diffs without a
//     bump.
//   - Removing or renaming a field is a breaking change — bump the
//     constant and register a compat migrator. The lint check fails the
//     PR if you forget.
//   - Changing a field's type is a breaking change — same as remove/rename.
package events
