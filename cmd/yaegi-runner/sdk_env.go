//go:build wasip1

package main

import "encoding/json"

// EnvGet returns a sandbox-scoped env value. Never reads the host's real
// environment; values come from the configured env block.
func EnvGet(key string) string {
	resp, err := bridgeCall("env.get", map[string]any{"key": key})
	if err != nil {
		return ""
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return ""
	}
	return out.Value
}
