package promote

import (
	"encoding/json"

	"gopkg.in/yaml.v3"
)

// Tiny indirection layer so the rest of the package never imports yaml.v3
// or encoding/json directly. Centralizing makes test stubs and future
// formatter swaps cheap.

func yamlMarshal(v any) ([]byte, error) {
	return yaml.Marshal(v)
}

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
