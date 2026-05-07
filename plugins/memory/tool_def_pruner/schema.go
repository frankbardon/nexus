package tool_def_pruner

import _ "embed"

//go:embed schema.json
var configSchemaBytes []byte

// ConfigSchema implements engine.ConfigSchemaProvider.
func (p *Plugin) ConfigSchema() []byte { return configSchemaBytes }
