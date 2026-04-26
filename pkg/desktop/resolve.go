package desktop

import (
	"fmt"
	"strings"

	"github.com/frankbardon/nexus/pkg/engine"
)

// resolveConfig performs template substitution on raw config YAML bytes.
// It replaces ${key} placeholders with values from the settings store,
// using scope fallback (agent scope first, then shell scope).
//
// For each SettingsField declared by the agent, the resolver looks for
// ${field.Key} in the YAML. If the field references a shell-scoped
// value (key starts with "shell."), it strips the prefix and looks up
// in shell scope directly. Otherwise it checks agent scope first, then
// falls back to shell scope.
//
// Returns an error listing all unresolved required fields.
func resolveConfig(raw []byte, agentID string, fields []SettingsField, store SettingsStore) ([]byte, []string, error) {
	result := string(raw)
	var missing []string

	for _, f := range fields {
		placeholder := "${" + f.Key + "}"
		if !strings.Contains(result, placeholder) {
			continue
		}

		// Determine lookup scope and key.
		lookupScope := agentID
		lookupKey := f.Key
		if strings.HasPrefix(f.Key, "shell.") {
			lookupScope = "shell"
			lookupKey = strings.TrimPrefix(f.Key, "shell.")
		}

		val, found := store.Resolve(lookupScope, lookupKey, f.Secret)
		if !found {
			// Try the default value.
			if f.Default != nil {
				val = fmt.Sprintf("%v", f.Default)
				found = true
			}
		}

		if !found {
			if f.Required {
				missing = append(missing, f.Key)
				continue
			}
			// Replace unset non-required placeholders with empty string
			// so plugins receive "" (falsy) instead of the literal "${key}".
			val = ""
			// fall through to replacement below
		}

		// Tilde-expand path-typed fields so plugins receive an absolute path
		// even when the user typed "~/Work/foo" in settings.
		if f.Type == FieldPath && val != "" {
			val = engine.ExpandPath(val)
		}

		result = strings.ReplaceAll(result, placeholder, val)
	}

	if len(missing) > 0 {
		return nil, missing, fmt.Errorf("missing required settings: %s", strings.Join(missing, ", "))
	}

	return []byte(result), nil, nil
}
