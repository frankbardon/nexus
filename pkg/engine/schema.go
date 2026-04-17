package engine

import (
	"log/slog"
	"sync"

	"github.com/frankbardon/nexus/pkg/events"
)

type schemaEntry struct {
	Schema map[string]any
	Source string
}

// SchemaRegistry collects named output schemas registered by plugins
// and attaches them to LLM requests tagged with _expects_schema metadata.
type SchemaRegistry struct {
	mu      sync.RWMutex
	schemas map[string]schemaEntry
	logger  *slog.Logger

	// logUnresolved controls whether warnings are logged when a request
	// tags _expects_schema but no matching schema is registered.
	logUnresolved bool
}

// NewSchemaRegistry creates an empty SchemaRegistry.
func NewSchemaRegistry(logger *slog.Logger) *SchemaRegistry {
	return &SchemaRegistry{
		schemas:       make(map[string]schemaEntry),
		logger:        logger,
		logUnresolved: true,
	}
}

// Register adds a named schema. If a schema with the same name exists, it is replaced.
func (r *SchemaRegistry) Register(name string, schema map[string]any, source string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.schemas[name] = schemaEntry{Schema: schema, Source: source}
	r.logger.Debug("schema registered", "name", name, "source", source)
}

// Deregister removes a named schema. Only the original source can remove it.
func (r *SchemaRegistry) Deregister(name string, source string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.schemas[name]; ok && entry.Source == source {
		delete(r.schemas, name)
		r.logger.Debug("schema deregistered", "name", name, "source", source)
	}
}

// Lookup returns the schema for a given name, or nil if not found.
func (r *SchemaRegistry) Lookup(name string) (map[string]any, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.schemas[name]
	if !ok {
		return nil, false
	}
	return entry.Schema, true
}

// HandleSchemaRegister processes schema.register events.
func (r *SchemaRegistry) HandleSchemaRegister(event Event[any]) {
	reg, ok := event.Payload.(events.SchemaRegistration)
	if !ok {
		return
	}
	r.Register(reg.Name, reg.Schema, reg.Source)
}

// HandleSchemaDeregister processes schema.deregister events.
func (r *SchemaRegistry) HandleSchemaDeregister(event Event[any]) {
	dereg, ok := event.Payload.(events.SchemaDeregistration)
	if !ok {
		return
	}
	r.Deregister(dereg.Name, dereg.Source)
}

// HandleBeforeLLMRequest attaches ResponseFormat to requests tagged with _expects_schema.
// Skips if ResponseFormat is already set directly on the request.
func (r *SchemaRegistry) HandleBeforeLLMRequest(event Event[any]) {
	vp, ok := event.Payload.(*VetoablePayload)
	if !ok {
		return
	}
	req, ok := vp.Original.(*events.LLMRequest)
	if !ok {
		return
	}

	// Don't override explicit ResponseFormat.
	if req.ResponseFormat != nil {
		return
	}

	// Check for _expects_schema tag in metadata.
	schemaName, _ := req.Metadata["_expects_schema"].(string)
	if schemaName == "" {
		return
	}

	schema, found := r.Lookup(schemaName)
	if !found {
		if r.logUnresolved {
			r.logger.Warn("request tagged _expects_schema but no matching schema registered",
				"schema", schemaName)
		}
		return
	}

	req.ResponseFormat = &events.ResponseFormat{
		Type:   "json_schema",
		Name:   schemaName,
		Schema: schema,
		Strict: true,
	}
}

// Install subscribes the registry to the event bus for schema management
// and request attachment. Returns unsubscribe functions.
func (r *SchemaRegistry) Install(bus EventBus) []func() {
	return []func(){
		bus.Subscribe("schema.register", r.HandleSchemaRegister),
		bus.Subscribe("schema.deregister", r.HandleSchemaDeregister),
		bus.Subscribe("before:llm.request", r.HandleBeforeLLMRequest,
			WithPriority(5)), // before gates (10) and agents (50)
	}
}
