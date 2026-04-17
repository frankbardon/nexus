package engine

import "strings"

// ModelConfig describes a specific model available through a provider.
type ModelConfig struct {
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	MaxTokens int    `yaml:"max_tokens"`
}

// ModelRegistry resolves model role names to concrete model configurations.
// Each role maps to an ordered chain of ModelConfigs. The first entry is the
// primary; subsequent entries are fallbacks tried in order when the primary
// (or prior fallback) fails with a non-retryable error or exhausts retries.
type ModelRegistry struct {
	chains      map[string][]ModelConfig
	defaultRole string
}

// parseModelConfig extracts a ModelConfig from a raw config map.
func parseModelConfig(m map[string]any) ModelConfig {
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
	return cfg
}

// NewModelRegistry builds a ModelRegistry from the core.models config section.
// The modelsRaw map may contain string values (for "default" alias),
// map[string]any values (single model — backward compatible), and
// []any values (ordered fallback chain).
func NewModelRegistry(modelsRaw map[string]any) *ModelRegistry {
	r := &ModelRegistry{
		chains:      make(map[string][]ModelConfig),
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

		switch v := val.(type) {
		case map[string]any:
			// Single model config (backward compatible).
			r.chains[key] = []ModelConfig{parseModelConfig(v)}

		case []any:
			// Ordered fallback chain.
			chain := make([]ModelConfig, 0, len(v))
			for _, entry := range v {
				if m, ok := entry.(map[string]any); ok {
					chain = append(chain, parseModelConfig(m))
				}
			}
			if len(chain) > 0 {
				r.chains[key] = chain
			}
		}
	}

	return r
}

// Resolve returns the primary ModelConfig for the given role name (index 0).
// If role is empty, the default role is used.
// If the role is not found but looks like a raw model ID (contains "-"),
// it is returned as-is with an empty provider for backward compatibility.
// The second return value indicates whether the role was found.
func (r *ModelRegistry) Resolve(role string) (ModelConfig, bool) {
	if role == "" {
		role = r.defaultRole
	}

	if chain, ok := r.chains[role]; ok && len(chain) > 0 {
		return chain[0], true
	}

	// Backward compat: treat as raw model ID if it contains a hyphen.
	if strings.Contains(role, "-") {
		return ModelConfig{Model: role}, true
	}

	return ModelConfig{}, false
}

// Fallback returns the ModelConfig at the given attempt index in the fallback
// chain for the specified role. Attempt 0 is the primary (same as Resolve).
// Returns false if the chain is exhausted or the role doesn't exist.
func (r *ModelRegistry) Fallback(role string, attempt int) (ModelConfig, bool) {
	if role == "" {
		role = r.defaultRole
	}

	chain, ok := r.chains[role]
	if !ok || attempt < 0 || attempt >= len(chain) {
		return ModelConfig{}, false
	}

	return chain[attempt], true
}

// ChainLen returns the number of entries in the fallback chain for a role.
// Returns 0 if the role doesn't exist.
func (r *ModelRegistry) ChainLen(role string) int {
	if role == "" {
		role = r.defaultRole
	}
	return len(r.chains[role])
}

// Default returns the ModelConfig for the default role.
func (r *ModelRegistry) Default() ModelConfig {
	cfg, _ := r.Resolve("")
	return cfg
}

// Roles returns the names of all registered roles.
func (r *ModelRegistry) Roles() []string {
	roles := make([]string, 0, len(r.chains))
	for k := range r.chains {
		roles = append(roles, k)
	}
	return roles
}
