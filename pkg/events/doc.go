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
// # Adding a new event type
//
//  1. Define the struct in the appropriate per-domain file
//     (core.go, llm.go, agent.go, …) with a SchemaVersion int field
//     tagged `json:"_schema_version"` as the first declared field.
//  2. Add a <Name>Version = 1 constant alongside the struct.
//  3. Producers must stamp `SchemaVersion = <Name>Version` on every
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
