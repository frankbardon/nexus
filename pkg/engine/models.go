package engine

import "strings"

// ModelConfig describes a specific model available through a provider.
type ModelConfig struct {
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	MaxTokens int    `yaml:"max_tokens"`
}

// ModelRegistry resolves model role names to concrete model configurations.
type ModelRegistry struct {
	roles       map[string]ModelConfig
	defaultRole string
}

// NewModelRegistry builds a ModelRegistry from the core.models config section.
// The modelsRaw map may contain string values (for "default" alias) and
// map[string]any values (for role definitions).
func NewModelRegistry(modelsRaw map[string]any) *ModelRegistry {
	r := &ModelRegistry{
		roles:       make(map[string]ModelConfig),
		defaultRole: "balanced",
	}

	if modelsRaw == nil {
		return r
	}

	for key, val := range modelsRaw {
		if key == "default" {
			if alias, ok := val.(string); ok {
				r.defaultRole = alias
			}
			continue
		}

		m, ok := val.(map[string]any)
		if !ok {
			continue
		}

		cfg := ModelConfig{}
		if v, ok := m["provider"].(string); ok {
			cfg.Provider = v
		}
		if v, ok := m["model"].(string); ok {
			cfg.Model = v
		}
		if v, ok := m["max_tokens"].(int); ok {
			cfg.MaxTokens = v
		} else if v, ok := m["max_tokens"].(float64); ok {
			cfg.MaxTokens = int(v)
		}

		r.roles[key] = cfg
	}

	return r
}

// Resolve returns the ModelConfig for the given role name.
// If role is empty, the default role is used.
// If the role is not found but looks like a raw model ID (contains "-"),
// it is returned as-is with an empty provider for backward compatibility.
// The second return value indicates whether the role was found.
func (r *ModelRegistry) Resolve(role string) (ModelConfig, bool) {
	if role == "" {
		role = r.defaultRole
	}

	if cfg, ok := r.roles[role]; ok {
		return cfg, true
	}

	// Backward compat: treat as raw model ID if it contains a hyphen.
	if strings.Contains(role, "-") {
		return ModelConfig{Model: role}, true
	}

	return ModelConfig{}, false
}

// Default returns the ModelConfig for the default role.
func (r *ModelRegistry) Default() ModelConfig {
	cfg, _ := r.Resolve("")
	return cfg
}

// Roles returns the names of all registered roles.
func (r *ModelRegistry) Roles() []string {
	roles := make([]string, 0, len(r.roles))
	for k := range r.roles {
		roles = append(roles, k)
	}
	return roles
}
