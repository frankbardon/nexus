// Package compat carries field-level migrations between event-payload
// schema versions.
//
// The events package stamps every emitted payload with `_schema_version`
// (see ../doc.go for the convention). When the journal is later replayed
// — by `engine.ReplaySession`, the eval harness, or external tooling —
// the running code may have moved to a newer version than the recorded
// payload. The replay path consults this package to lift the on-disk
// payload up to the in-memory contract before delivering it to live
// subscribers.
//
// # Wiring
//
// `engine.replayPayloadConverter` (in pkg/engine/engine.go) calls
// `compat.Apply(eventType, recordedVersion, currentVersion, payload)`
// before re-typing the map[string]any back into a struct via
// `journal.PayloadAs[T]`. With the registry empty (today's state), Apply
// is a no-op and the converter returns the payload unchanged. When a
// future PR ships v2 of an event type, the migrator goes here and
// replay-time data flows through it without touching the engine.
//
// # Why it lives in its own package
//
// Migrations are scoped data, not engine logic — they accrete over time
// as types evolve, are cited in PR templates, and get exercised by
// targeted tests. Keeping them out of pkg/engine and out of pkg/events
// itself prevents an import cycle (engine imports events; events does
// not import engine; compat depends only on map[string]any).
//
// # The v0 == v1 rule
//
// When a payload arrives without `_schema_version`, JSON deserializes it
// to 0. Per the package-level rule documented in ../doc.go, consumers
// treat 0 as v1 — the running code's contract. The first migration that
// ever lands here will register a `{Type, From: 0, To: 1}` no-op so
// chained Apply calls stay symmetric across the synthetic boundary.
package compat

import "fmt"

// Key uniquely identifies one migration step: take a payload of the named
// event type from version From to version To.
type Key struct {
	EventType string // e.g. "llm.request"
	From, To  int
}

// Migrator transforms a payload (always a map[string]any after JSON
// round-trip via the journal) from one version to the next. Returning
// (payload, nil) is fine for additive changes — drop a sensible zero
// value into the new field and pass through.
type Migrator func(payload map[string]any) (map[string]any, error)

// Migrations is the active registry. Empty today — see package doc.
//
// Authors registering new entries must also add a unit test in
// compat_test.go demonstrating the migrator on a representative payload.
var Migrations = map[Key]Migrator{}

// Apply lifts a payload from version `from` up to version `to` for the
// named event type by chaining one-step migrators registered in
// Migrations. When `from == to` (the common case) or no migrators are
// registered for a step, the payload is returned unchanged.
//
// Returns an error when a step is missing in the middle of a chain —
// e.g., Migrations has 1->2 but not 2->3, and the caller asked for 1->3.
// This forces explicit, contiguous migration registration rather than
// silent drops.
func Apply(eventType string, from, to int, payload map[string]any) (map[string]any, error) {
	if from == to {
		return payload, nil
	}
	// Downgrades aren't a thing here: replay only ever lifts older records
	// up to the running code's version. Reject so a logic bug at the call
	// site can't quietly return a payload at the wrong version.
	if to < from {
		return payload, fmt.Errorf("compat.Apply: downgrade not supported (%s: from=%d to=%d)", eventType, from, to)
	}
	cur := payload
	for v := from; v < to; v++ {
		k := Key{EventType: eventType, From: v, To: v + 1}
		m, ok := Migrations[k]
		if !ok {
			// No registered step. With an empty registry today this is the
			// hot path: the replay payload is on the running code's
			// version, just lacking a stamped field. Pass through.
			//
			// When the registry has at least one entry, a missing step in
			// the middle of a chain is a real bug. Distinguish by checking
			// whether ANY migrator exists for this event type — if so,
			// missing intermediate is fatal.
			if hasMigratorFor(eventType) {
				return payload, fmt.Errorf("compat.Apply: no migrator registered for %s v%d->v%d", eventType, v, v+1)
			}
			return cur, nil
		}
		next, err := m(cur)
		if err != nil {
			return payload, fmt.Errorf("compat.Apply %s v%d->v%d: %w", eventType, v, v+1, err)
		}
		cur = next
	}
	return cur, nil
}

func hasMigratorFor(eventType string) bool {
	for k := range Migrations {
		if k.EventType == eventType {
			return true
		}
	}
	return false
}
