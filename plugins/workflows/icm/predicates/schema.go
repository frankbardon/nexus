package predicates

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	jsc "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// evalSchema validates a JSON artifact against a registered JSON
// schema. The predicate's SchemaPath field carries the registered
// schema name (not a filesystem path) — the icm plugin registers each
// schema under PredicateSchemaName(...) at Ready() and the evaluator
// looks it up under the same key. Compilation results cache so repeat
// evaluations skip the (non-trivial) compile step.
func (e *Evaluator) evalSchema(p *workspace.Predicate, artifact []byte, sc StageEvalContext, res Result) Result {
	if e.Schemas == nil {
		res.Verdict = false
		res.Feedback = "schema registry not configured"
		return res
	}
	name := p.SchemaPath
	if name == "" {
		// The output-schema synthesizer names its predicate "output";
		// look that up under the canonical PredicateSchemaName.
		name = PredicateSchemaName(sc.InstanceID, sc.StageID, p.Name)
	}
	schema, err := e.compileSchema(name)
	if err != nil {
		res.Verdict = false
		res.Feedback = fmt.Sprintf("schema %q failed to compile: %v", name, err)
		return res
	}
	if schema == nil {
		res.Verdict = false
		res.Feedback = fmt.Sprintf("schema %q is not registered", name)
		return res
	}
	inst, err := jsc.UnmarshalJSON(bytes.NewReader(artifact))
	if err != nil {
		res.Verdict = false
		res.Feedback = fmt.Sprintf("output is not valid JSON: %v", err)
		return res
	}
	if err := schema.Validate(inst); err != nil {
		res.Verdict = false
		res.Feedback = "schema validation failed: " + formatSchemaError(err)
		return res
	}
	res.Verdict = true
	return res
}

// compileSchema returns the compiled schema for the registered name,
// caching the result. A return of (nil, nil) means the name is unknown
// to the registry — the caller surfaces that as a not-registered
// feedback string.
func (e *Evaluator) compileSchema(name string) (*jsc.Schema, error) {
	e.schemaCacheMu.Lock()
	defer e.schemaCacheMu.Unlock()
	if cached, ok := e.schemaCompileCache[name]; ok {
		return cached, nil
	}
	raw, found := e.Schemas.Lookup(name)
	if !found {
		return nil, nil
	}
	schemaBytes, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal schema: %w", err)
	}
	doc, err := jsc.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		return nil, fmt.Errorf("unmarshal schema: %w", err)
	}
	c := jsc.NewCompiler()
	if err := c.AddResource(name, doc); err != nil {
		return nil, fmt.Errorf("add resource: %w", err)
	}
	compiled, err := c.Compile(name)
	if err != nil {
		return nil, err
	}
	e.schemaCompileCache[name] = compiled
	return compiled, nil
}

// formatSchemaError flattens a jsonschema/v6 ValidationError tree into
// a single-line human-readable string so failure feedback fits in chat
// context without losing meaning.
func formatSchemaError(err error) string {
	ve, ok := err.(*jsc.ValidationError)
	if !ok {
		return err.Error()
	}
	if len(ve.Causes) == 0 {
		return ve.Error()
	}
	var parts []string
	for _, cause := range ve.Causes {
		parts = append(parts, formatSchemaError(cause))
	}
	return strings.Join(parts, "; ")
}
