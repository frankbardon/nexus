package events

// SchemaRegistration is emitted to register an output schema with the schema registry.
// Event type: schema.register
type SchemaRegistration struct {
	Name   string         // e.g. "skill.code_review.output"
	Schema map[string]any // JSON Schema
	Source string         // plugin ID that registered it
}

// SchemaDeregistration is emitted to remove a schema from the registry.
// Event type: schema.deregister
type SchemaDeregistration struct {
	Name   string // schema name to remove
	Source string // plugin ID that registered it
}
