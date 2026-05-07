package events

// Schema-version constants for schema.* registry payloads. See doc.go.
//
// Note: this file holds payloads for the schema-registry events
// ("schema.register" / "schema.deregister") that JSON-schema gates use to
// publish output schemas. It is unrelated to the per-event-type
// `_schema_version` field added to every payload struct in this package
// — that is a versioning mechanism for the event payloads themselves.
const (
	SchemaRegistrationVersion   = 1
	SchemaDeregistrationVersion = 1
)

// SchemaRegistration is emitted to register an output schema with the schema registry.
// Event type: schema.register
type SchemaRegistration struct {
	SchemaVersion int `json:"_schema_version"`

	Name   string         // e.g. "skill.code_review.output"
	Schema map[string]any // JSON Schema
	Source string         // plugin ID that registered it
}

// SchemaDeregistration is emitted to remove a schema from the registry.
// Event type: schema.deregister
type SchemaDeregistration struct {
	SchemaVersion int `json:"_schema_version"`

	Name   string // schema name to remove
	Source string // plugin ID that registered it
}
