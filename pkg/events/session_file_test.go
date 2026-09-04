package events

import (
	"reflect"
	"strings"
	"testing"
)

// SessionFile.Map is the wire contract for session.file.created and
// session.file.updated, and this is what keeps the struct and the map from
// drifting apart.
//
// Without it the struct is decorative: `make check-events` guards field names
// and types in this package, but nothing forces those fields to reach the bus.
// A field added to SessionFile and forgotten in Map would ship a payload
// silently missing it — the same class of bug as the hand-built emitters this
// struct replaced, just one level up.
func TestSessionFileMapCoversEveryField(t *testing.T) {
	// Field name -> the wire key it must appear under. Spelled out rather than
	// derived, because a derivation would happily agree with a typo on both
	// sides.
	wireKeys := map[string]string{
		"SchemaVersion": "_schema_version",
		"SessionID":     "session_id",
		"Path":          "path",
		"Size":          "size",
		"Offset":        "offset",
		"BytesAdded":    "bytes_added",
	}

	typ := reflect.TypeOf(SessionFile{})
	got := SessionFile{}.Map()

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		key, known := wireKeys[field.Name]
		if !known {
			t.Errorf("SessionFile.%s has no wire key; add it to Map and to this test, "+
				"or the field will never reach a subscriber", field.Name)
			continue
		}
		if _, present := got[key]; !present {
			t.Errorf("SessionFile.%s maps to %q, which Map does not emit", field.Name, key)
		}
	}

	if len(got) != len(wireKeys) {
		t.Errorf("Map emits %d keys, the struct has %d mapped fields; "+
			"an unmapped key on the wire is one no struct field defines", len(got), len(wireKeys))
	}
}

// The payload keys are load-bearing beyond this package: the object-store seam
// uses path directly as an object key, and offset/bytes_added are what let a
// snapshot upload an append as a delta. Renaming one is a breaking change for
// every subscriber, so it should be a deliberate act rather than a refactor
// side effect.
func TestSessionFileWireKeysAreStable(t *testing.T) {
	got := SessionFile{
		SchemaVersion: SessionFileVersion,
		SessionID:     "sess-1",
		Path:          "files/notes.md",
		Size:          120,
		Offset:        100,
		BytesAdded:    20,
	}.Map()

	want := map[string]any{
		"_schema_version": SessionFileVersion,
		"session_id":      "sess-1",
		"path":            "files/notes.md",
		"size":            120,
		"offset":          100,
		"bytes_added":     20,
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("wire payload changed:\n got = %#v\nwant = %#v", got, want)
	}
}

// Action was removed deliberately: the action is the event type, and a copy of
// it in the payload is a second source of truth that can disagree with the
// first. nexus.tool.pdf hardcoded "created" on every write, including updates,
// for exactly as long as the field existed.
func TestSessionFileHasNoActionField(t *testing.T) {
	typ := reflect.TypeOf(SessionFile{})
	if _, found := typ.FieldByName("Action"); found {
		t.Error("SessionFile has an Action field again; the action is the event " +
			"type (session.file.created vs .updated), and duplicating it in the " +
			"payload is how nexus.tool.pdf came to report every update as a creation")
	}
	for key := range (SessionFile{}).Map() {
		if strings.Contains(key, "action") {
			t.Errorf("Map emits %q; see above", key)
		}
	}
}
