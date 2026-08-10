package nexusauth

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
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

// Keys a `jwks` validator entry accepts, consumed by buildJWKSValidator.
const (
	keyIssuer           = "issuer"
	keyJWKSURL          = "jwks_url"
	keyAudience         = "audience"
	keyAlgorithms       = "algorithms"
	keyCacheTTL         = "cache_ttl"
	keyNegativeCacheTTL = "negative_cache_ttl"
	keyHTTPTimeout      = "http_timeout"
	keyClockSkew        = "clock_skew"
)

// Keys an `introspect` validator entry accepts, consumed by
// buildIntrospectValidator. It shares keyCacheTTL, keyNegativeCacheTTL and
// keyHTTPTimeout with `jwks` — the same names mean the same things, so an
// operator does not have to learn two vocabularies for one concept.
const (
	keyIntrospectionURL = "introspection_url"
	keyClientID         = "client_id"
	keyClientSecret     = "client_secret"
	keyClientSecretEnv  = "client_secret_env"
	keyClientAuth       = "client_auth"
	keyTokenTypeHint    = "token_type_hint"
)

// knownValidatorTypes lists the types BuildChain can construct, for the
// unknown-type error message. Extend it alongside the switch in buildValidator.
func knownValidatorTypes() []string {
	return []string{ValidatorTypeStatic, ValidatorTypeJWKS, ValidatorTypeIntrospect}
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
	case ValidatorTypeJWKS:
		return buildJWKSValidator(vc)
	case ValidatorTypeIntrospect:
		return buildIntrospectValidator(vc)
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

// buildJWKSValidator constructs a JWKSValidator from a config entry.
//
// Nothing here reaches the network: the issuer's key set is fetched lazily on
// first use. A broker that cannot start because its identity provider is briefly
// unreachable would be a worse failure mode than one that starts and denies until
// the endpoint answers.
func buildJWKSValidator(vc ValidatorConfig) (Validator, error) {
	if err := rejectUnknownKeys(vc.Options,
		keyIssuer, keyJWKSURL, keyAudience, keyAlgorithms,
		keyCacheTTL, keyNegativeCacheTTL, keyHTTPTimeout, keyClockSkew,
	); err != nil {
		return nil, err
	}

	opts := jwksOptions{Mapping: vc.ClaimMapping}
	var err error
	if opts.Issuer, _, err = optString(vc.Options, keyIssuer); err != nil {
		return nil, err
	}
	if opts.JWKSURL, _, err = optString(vc.Options, keyJWKSURL); err != nil {
		return nil, err
	}
	if opts.Audiences, err = optStringList(vc.Options, keyAudience); err != nil {
		return nil, err
	}
	if opts.Algorithms, err = optStringList(vc.Options, keyAlgorithms); err != nil {
		return nil, err
	}
	if opts.CacheTTL, _, err = optDuration(vc.Options, keyCacheTTL); err != nil {
		return nil, err
	}
	if opts.NegativeCacheTTL, _, err = optDuration(vc.Options, keyNegativeCacheTTL); err != nil {
		return nil, err
	}
	if opts.HTTPTimeout, _, err = optDuration(vc.Options, keyHTTPTimeout); err != nil {
		return nil, err
	}
	if opts.ClockSkew, opts.ClockSkewSet, err = optDuration(vc.Options, keyClockSkew); err != nil {
		return nil, err
	}
	return newJWKSValidator(opts)
}

// buildIntrospectValidator constructs an IntrospectValidator from a config
// entry.
//
// Like the jwks builder, nothing here reaches the network: the first
// introspection happens on the first request, so a briefly unreachable identity
// provider cannot stop the broker booting.
func buildIntrospectValidator(vc ValidatorConfig) (Validator, error) {
	if err := rejectUnknownKeys(vc.Options,
		keyIntrospectionURL, keyClientID, keyClientSecret, keyClientSecretEnv,
		keyClientAuth, keyTokenTypeHint,
		keyCacheTTL, keyNegativeCacheTTL, keyHTTPTimeout,
	); err != nil {
		return nil, err
	}

	opts := introspectOptions{Mapping: vc.ClaimMapping}
	var err error
	if opts.IntrospectionURL, _, err = optString(vc.Options, keyIntrospectionURL); err != nil {
		return nil, err
	}
	if opts.ClientID, _, err = optString(vc.Options, keyClientID); err != nil {
		return nil, err
	}
	if opts.ClientSecret, err = resolveSecret(vc.Options, keyClientSecret, keyClientSecretEnv); err != nil {
		return nil, err
	}
	if opts.ClientAuth, _, err = optString(vc.Options, keyClientAuth); err != nil {
		return nil, err
	}
	if opts.TokenTypeHint, opts.TokenTypeHintSet, err = optString(vc.Options, keyTokenTypeHint); err != nil {
		return nil, err
	}
	if opts.CacheTTL, _, err = optDuration(vc.Options, keyCacheTTL); err != nil {
		return nil, err
	}
	if opts.NegativeCacheTTL, _, err = optDuration(vc.Options, keyNegativeCacheTTL); err != nil {
		return nil, err
	}
	if opts.HTTPTimeout, _, err = optDuration(vc.Options, keyHTTPTimeout); err != nil {
		return nil, err
	}
	return newIntrospectValidator(opts)
}

// resolveSecret reads a value that may be written inline (`key`) or sourced from
// the environment by name (`key_env`).
//
// THE SECRET-SOURCING CONVENTION for this package, and the one any future
// secret-bearing key should follow: `<key>` holds the literal value, `<key>_env`
// holds the NAME of an environment variable to read it from. It matches the
// `bearer_token` / `bearer_token_env` pair nexus.io.agui already ships, so
// operators meet one spelling rather than two.
//
// Three rules, each chosen so a mistake is loud rather than silent:
//
//   - Setting both is an error, not a precedence puzzle. Two sources for one
//     secret means one of them is stale, and quietly preferring either is how a
//     rotated credential appears not to have rotated.
//   - A named variable that is unset or empty is an error at load time. The
//     alternative — falling back to an empty secret — authenticates the broker
//     to the identity provider as an anonymous client and surfaces much later as
//     a confusing 401 from the endpoint.
//   - Errors name the variable, never its value, so a boot failure can be pasted
//     into an issue without leaking a credential.
func resolveSecret(m map[string]any, key, envKey string) (string, error) {
	inline, _, err := optString(m, key)
	if err != nil {
		return "", err
	}
	envName, _, err := optString(m, envKey)
	if err != nil {
		return "", err
	}
	if inline != "" && envName != "" {
		return "", fmt.Errorf("%s and %s are mutually exclusive: set one or the other", key, envKey)
	}
	if envName == "" {
		return inline, nil
	}
	// Trimmed because a secret injected from a file or a here-doc routinely
	// arrives with a trailing newline, and a credential that differs from the
	// configured one by an invisible byte is a miserable thing to debug.
	value := strings.TrimSpace(os.Getenv(envName))
	if value == "" {
		return "", fmt.Errorf("%s names environment variable %q, which is unset or empty", envKey, envName)
	}
	return value, nil
}

// optStringList reads a key that accepts either a single string or a list of
// strings, and returns it as a slice.
//
// A lone string is taken verbatim as one entry and is deliberately NOT split on
// whitespace, unlike ParseScopes. These lists hold audiences and algorithm names,
// which are single opaque identifiers: splitting `audience: "my service"` into
// two audiences would silently widen what the validator accepts, whereas leaving
// it whole produces a clean "no token matched" that an operator can diagnose.
func optStringList(m map[string]any, key string) ([]string, error) {
	v, present := m[key]
	if !present || v == nil {
		return nil, nil
	}
	switch value := v.(type) {
	case string:
		s := strings.TrimSpace(value)
		if s == "" {
			return nil, nil
		}
		return []string{s}, nil
	case []string:
		out := make([]string, 0, len(value))
		for _, s := range value {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	case []any:
		out := make([]string, 0, len(value))
		for i, elem := range value {
			s, ok := elem.(string)
			if !ok {
				return nil, fmt.Errorf("%s[%d]: want a string, got %T", key, i, elem)
			}
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s: want a string or a list of strings, got %T", key, v)
	}
}

// optDuration reads an optional duration key written as a Go duration string
// ("10m", "30s", "1h30m"). The bool reports whether the key was present, so a
// caller can tell an explicit zero from an absent key.
//
// A bare number is rejected rather than guessed at: "cache_ttl: 600" reads as ten
// minutes to one operator and six hundred nanoseconds to Go, and silently picking
// either would be worse than an error naming the key.
func optDuration(m map[string]any, key string) (time.Duration, bool, error) {
	v, present := m[key]
	if !present || v == nil {
		return 0, false, nil
	}
	s, ok := v.(string)
	if !ok {
		return 0, true, fmt.Errorf("%s: want a duration string such as \"10m\", got %T", key, v)
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0, true, fmt.Errorf("%s: %w", key, err)
	}
	return d, true, nil
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
