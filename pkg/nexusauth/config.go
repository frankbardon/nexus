package nexusauth

import (
	"fmt"
	"sort"
	"strings"
)

// Config is the parsed form of an `auth:` block. It is decoder-agnostic on
// purpose: ParseConfig takes a map[string]any, which is what both
// gopkg.in/yaml.v3 (broker.yaml) and a plugin's ctx.Config hand over, so the two
// hosts share one code path and one set of error messages.
type Config struct {
	// Validators are the configured validators, in the order the operator listed
	// them. Empty means auth is not configured — BuildChain returns a disabled
	// Chain, not a permissive one.
	Validators []ValidatorConfig
}

// ValidatorConfig is one entry of the `validators:` list.
type ValidatorConfig struct {
	// Type selects the implementation, e.g. ValidatorTypeStatic.
	Type string

	// Name is the label the Chain reports in denial records. ParseConfig derives
	// it from Type, disambiguating repeats as "type#2", "type#3", …, so an
	// operator with two issuers can tell the two apart in one log line without
	// having to name them.
	Name string

	// ClaimMapping is the shared claim-name mapping. It is parsed for every
	// entry even though only claim-bearing validators consume it.
	ClaimMapping ClaimMapping

	// Options holds the remaining, type-specific keys verbatim. The per-type
	// builder consumes them and rejects anything it does not recognize, so a
	// typo fails loudly instead of being silently ignored.
	Options map[string]any
}

// Shared keys every validator entry accepts, handled by ParseConfig itself
// rather than by a per-type builder.
const (
	keyType           = "type"
	keyPrincipalClaim = "principal_claim"
	keyTenantClaim    = "tenant_claim"
	keyScopesClaim    = "scopes_claim"
)

// knownValidatorTypes lists the types BuildChain can construct, for the
// unknown-type error message. Extend it alongside the switch in buildValidator.
func knownValidatorTypes() []string {
	return []string{ValidatorTypeStatic}
}

// ParseConfig parses an `auth:` block. A nil or empty map is valid and yields a
// Config with no validators — "absent" is a legitimate deployment state that
// BuildChain turns into a disabled Chain.
//
// Unknown keys are rejected at every level, matching the repo's
// additionalProperties:false convention for config schemas: a misspelled auth key
// that is silently ignored is a security bug, not a cosmetic one.
func ParseConfig(raw map[string]any) (Config, error) {
	var cfg Config
	if len(raw) == 0 {
		return cfg, nil
	}
	if err := rejectUnknownKeys(raw, "validators"); err != nil {
		return Config{}, fmt.Errorf("nexusauth: config: %w", err)
	}

	rawList, present := raw["validators"]
	if !present || rawList == nil {
		return cfg, nil
	}
	list, ok := rawList.([]any)
	if !ok {
		return Config{}, fmt.Errorf("nexusauth: config: validators: want a list, got %T", rawList)
	}

	counts := make(map[string]int, len(list))
	for i, rawEntry := range list {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			return Config{}, fmt.Errorf("nexusauth: config: validators[%d]: want a mapping, got %T", i, rawEntry)
		}
		vc, err := parseValidatorEntry(entry)
		if err != nil {
			return Config{}, fmt.Errorf("nexusauth: config: validators[%d]: %w", i, err)
		}
		counts[vc.Type]++
		vc.Name = vc.Type
		if n := counts[vc.Type]; n > 1 {
			vc.Name = fmt.Sprintf("%s#%d", vc.Type, n)
		}
		cfg.Validators = append(cfg.Validators, vc)
	}
	return cfg, nil
}

// parseValidatorEntry pulls the shared keys off one entry and keeps the rest for
// the per-type builder.
func parseValidatorEntry(entry map[string]any) (ValidatorConfig, error) {
	typ, _, err := optString(entry, keyType)
	if err != nil {
		return ValidatorConfig{}, err
	}
	if typ == "" {
		return ValidatorConfig{}, fmt.Errorf("type is required")
	}

	// Ordered rather than a map range so the reported error is deterministic
	// when more than one claim key is mistyped.
	var mapping ClaimMapping
	claimKeys := []struct {
		key string
		dst *string
	}{
		{keyPrincipalClaim, &mapping.PrincipalClaim},
		{keyTenantClaim, &mapping.TenantClaim},
		{keyScopesClaim, &mapping.ScopesClaim},
	}
	for _, ck := range claimKeys {
		v, _, err := optString(entry, ck.key)
		if err != nil {
			return ValidatorConfig{}, err
		}
		*ck.dst = v
	}

	options := make(map[string]any, len(entry))
	for k, v := range entry {
		switch k {
		case keyType, keyPrincipalClaim, keyTenantClaim, keyScopesClaim:
			continue
		default:
			options[k] = v
		}
	}
	return ValidatorConfig{Type: typ, ClaimMapping: mapping, Options: options}, nil
}

// BuildChain instantiates every configured validator and assembles them into a
// Chain, preserving config order. A Config with no validators yields a disabled
// Chain and no error: refusing to boot is the caller's call to make, not this
// package's.
func BuildChain(cfg Config) (*Chain, error) {
	if len(cfg.Validators) == 0 {
		return NewChain(), nil
	}
	named := make([]NamedValidator, 0, len(cfg.Validators))
	for i, vc := range cfg.Validators {
		v, err := buildValidator(vc)
		if err != nil {
			return nil, fmt.Errorf("nexusauth: config: validators[%d] (%s): %w", i, vc.Type, err)
		}
		named = append(named, NamedValidator{Name: vc.Name, Validator: v})
	}
	return NewChain(named...), nil
}

// ChainFromMap is the one call a host needs: parse an `auth:` block and build the
// chain from it.
func ChainFromMap(raw map[string]any) (*Chain, error) {
	cfg, err := ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	return BuildChain(cfg)
}

// buildValidator dispatches on the entry type. New validator types are added as
// cases here and to knownValidatorTypes.
func buildValidator(vc ValidatorConfig) (Validator, error) {
	switch vc.Type {
	case ValidatorTypeStatic:
		return buildStaticValidator(vc)
	default:
		return nil, fmt.Errorf("unknown validator type %q (known types: %s)",
			vc.Type, strings.Join(knownValidatorTypes(), ", "))
	}
}

// buildStaticValidator constructs a StaticValidator from a config entry.
//
// A claim mapping on a static entry is accepted and ignored: a static token has
// no claim set to map, and the mapping keys are parsed uniformly across every
// entry type.
func buildStaticValidator(vc ValidatorConfig) (Validator, error) {
	if err := rejectUnknownKeys(vc.Options, "tokens"); err != nil {
		return nil, err
	}
	rawTokens, present := vc.Options["tokens"]
	if !present || rawTokens == nil {
		return nil, fmt.Errorf("tokens is required")
	}
	list, ok := rawTokens.([]any)
	if !ok {
		return nil, fmt.Errorf("tokens: want a list, got %T", rawTokens)
	}

	tokens := make([]StaticToken, 0, len(list))
	for i, rawToken := range list {
		entry, ok := rawToken.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tokens[%d]: want a mapping, got %T", i, rawToken)
		}
		t, err := parseStaticToken(entry)
		if err != nil {
			return nil, fmt.Errorf("tokens[%d]: %w", i, err)
		}
		tokens = append(tokens, t)
	}
	return NewStaticValidator(tokens)
}

// parseStaticToken parses one `tokens:` entry.
func parseStaticToken(entry map[string]any) (StaticToken, error) {
	if err := rejectUnknownKeys(entry, "token", "principal", "tenant", "scopes"); err != nil {
		return StaticToken{}, err
	}
	token, _, err := optString(entry, "token")
	if err != nil {
		return StaticToken{}, err
	}
	principal, _, err := optString(entry, "principal")
	if err != nil {
		return StaticToken{}, err
	}
	tenant, _, err := optString(entry, "tenant")
	if err != nil {
		return StaticToken{}, err
	}
	scopes, err := ParseScopes(entry["scopes"])
	if err != nil {
		return StaticToken{}, fmt.Errorf("scopes: %w", err)
	}
	return StaticToken{
		Token:     token,
		Principal: Principal{ID: principal, Tenant: tenant, Scopes: scopes},
	}, nil
}

// optString reads an optional string key, trimming surrounding whitespace so a
// stray trailing space in YAML does not become part of a token or a claim name.
// The bool reports whether the key was present at all.
func optString(m map[string]any, key string) (string, bool, error) {
	v, present := m[key]
	if !present || v == nil {
		return "", present, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", true, fmt.Errorf("%s: want a string, got %T", key, v)
	}
	return strings.TrimSpace(s), true, nil
}

// rejectUnknownKeys fails on any key not in allowed, listing the offenders in a
// stable order so the error is reproducible.
func rejectUnknownKeys(m map[string]any, allowed ...string) error {
	set := make(map[string]struct{}, len(allowed))
	for _, k := range allowed {
		set[k] = struct{}{}
	}
	var unknown []string
	for k := range m {
		if _, ok := set[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("unknown key(s) %s (known: %s)",
		strings.Join(quoteAll(unknown), ", "), strings.Join(allowed, ", "))
}

// quoteAll quotes each element for an error message.
func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}
