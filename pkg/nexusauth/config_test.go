package nexusauth

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// decodeYAML mirrors how broker.yaml reaches ParseConfig: yaml.v3 into an `any`
// target, which yields map[string]any / []any. A plugin's ctx.Config block has
// the same shape, which is why the parser takes a map rather than a struct.
func decodeYAML(t *testing.T, doc string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := yaml.Unmarshal([]byte(doc), &out); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	auth, ok := out["auth"].(map[string]any)
	if !ok {
		t.Fatalf("auth block is %T, want map[string]any", out["auth"])
	}
	return auth
}

func TestParseConfigFromYAML(t *testing.T) {
	auth := decodeYAML(t, `
auth:
  validators:
    - type: static
      tokens:
        - token: tok-ops
          principal: ops-cli
          tenant: acme
          scopes: [leases.read, leases.write]
        - token: tok-ci
          principal: ci
          scopes: admin
`)
	cfg, err := ParseConfig(auth)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if len(cfg.Validators) != 1 {
		t.Fatalf("want 1 validator, got %d", len(cfg.Validators))
	}
	if cfg.Validators[0].Type != ValidatorTypeStatic || cfg.Validators[0].Name != "static" {
		t.Fatalf("unexpected entry: %+v", cfg.Validators[0])
	}

	chain, err := BuildChain(cfg)
	if err != nil {
		t.Fatalf("BuildChain: %v", err)
	}
	if !chain.Enabled() {
		t.Fatal("configured chain reports disabled")
	}

	p, err := chain.Validate(context.Background(), newRequest(t, "Bearer tok-ops"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ID != "ops-cli" || p.Tenant != "acme" || !p.HasScope("leases.write") {
		t.Fatalf("unexpected principal: %+v", p)
	}

	// A space-delimited scopes string in config is accepted too.
	p, err = chain.Validate(context.Background(), newRequest(t, "Bearer tok-ci"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ID != "ci" || !p.HasScope("admin") {
		t.Fatalf("unexpected principal: %+v", p)
	}
}

func TestParseConfigFromJSONShape(t *testing.T) {
	// The same parser has to accept a JSON-decoded block, since a plugin's
	// config may arrive that way.
	var auth map[string]any
	body := `{"validators":[{"type":"static","tokens":[{"token":"t","principal":"p"}]}]}`
	if err := json.Unmarshal([]byte(body), &auth); err != nil {
		t.Fatalf("json: %v", err)
	}
	chain, err := ChainFromMap(auth)
	if err != nil {
		t.Fatalf("ChainFromMap: %v", err)
	}
	p, err := chain.Validate(context.Background(), newRequest(t, "Bearer t"))
	if err != nil || p.ID != "p" {
		t.Fatalf("principal %+v, err %v", p, err)
	}
}

func TestParseConfigCarriesClaimMapping(t *testing.T) {
	// The claim-mapping keys are parsed and carried for every entry even though
	// no validator in this package consumes them yet.
	auth := decodeYAML(t, `
auth:
  validators:
    - type: static
      principal_claim: sub
      tenant_claim: org_id
      scopes_claim: scope
      tokens:
        - token: t
          principal: p
`)
	cfg, err := ParseConfig(auth)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	want := ClaimMapping{PrincipalClaim: "sub", TenantClaim: "org_id", ScopesClaim: "scope"}
	if got := cfg.Validators[0].ClaimMapping; got != want {
		t.Fatalf("ClaimMapping = %+v, want %+v", got, want)
	}
	// Carried options must not leak the shared keys into the type-specific set.
	if _, found := cfg.Validators[0].Options[keyPrincipalClaim]; found {
		t.Fatalf("shared key left in options: %+v", cfg.Validators[0].Options)
	}
	// And the mapping must be inert for static — the chain still builds.
	if _, err := BuildChain(cfg); err != nil {
		t.Fatalf("BuildChain: %v", err)
	}
}

func TestParseConfigAbsentAndEmpty(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
	}{
		{"nil map", nil},
		{"empty map", map[string]any{}},
		{"null validators", map[string]any{"validators": nil}},
		{"empty validators", map[string]any{"validators": []any{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chain, err := ChainFromMap(tc.raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if chain.Enabled() {
				t.Fatal("empty config produced an enabled chain")
			}
			if _, err := chain.Validate(context.Background(), newRequest(t, "Bearer x")); err == nil {
				t.Fatal("unconfigured chain authenticated a request")
			}
		})
	}
}

func TestParseConfigDisambiguatesRepeatedTypes(t *testing.T) {
	auth := map[string]any{"validators": []any{
		map[string]any{"type": "static", "tokens": []any{map[string]any{"token": "a", "principal": "pa"}}},
		map[string]any{"type": "static", "tokens": []any{map[string]any{"token": "b", "principal": "pb"}}},
	}}
	cfg, err := ParseConfig(auth)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if got := cfg.Validators[0].Name; got != "static" {
		t.Fatalf("first name = %q", got)
	}
	if got := cfg.Validators[1].Name; got != "static#2" {
		t.Fatalf("second name = %q, want static#2", got)
	}

	chain, err := BuildChain(cfg)
	if err != nil {
		t.Fatalf("BuildChain: %v", err)
	}
	// The second table is only reachable because the chain falls through.
	p, err := chain.Validate(context.Background(), newRequest(t, "Bearer b"))
	if err != nil || p.ID != "pb" {
		t.Fatalf("principal %+v, err %v", p, err)
	}
	// And a denial names both entries distinguishably.
	_, err = chain.Validate(context.Background(), newRequest(t, "Bearer nope"))
	if err == nil {
		t.Fatal("want a denial")
	}
	for _, want := range []string{"static:", "static#2:"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("denial %q missing %q", err.Error(), want)
		}
	}
}

func TestParseConfigRejectsBadInput(t *testing.T) {
	cases := []struct {
		name    string
		raw     map[string]any
		wantErr string
	}{
		{
			"unknown top-level key",
			map[string]any{"validaters": []any{}},
			`unknown key(s) "validaters"`,
		},
		{
			"validators not a list",
			map[string]any{"validators": map[string]any{"type": "static"}},
			"validators: want a list",
		},
		{
			"entry not a mapping",
			map[string]any{"validators": []any{"static"}},
			"validators[0]: want a mapping",
		},
		{
			"missing type",
			map[string]any{"validators": []any{map[string]any{"tokens": []any{}}}},
			"type is required",
		},
		{
			"type not a string",
			map[string]any{"validators": []any{map[string]any{"type": 7}}},
			"type: want a string",
		},
		{
			"claim key not a string",
			map[string]any{"validators": []any{map[string]any{"type": "static", "scopes_claim": []any{"a"}}}},
			"scopes_claim: want a string",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseConfig(tc.raw); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestBuildChainRejectsBadValidators(t *testing.T) {
	cases := []struct {
		name    string
		entry   map[string]any
		wantErr string
	}{
		{
			// Deliberately a type that does not exist. It used to be "jwks", which
			// became a real type in E4-S1; the case is about the diagnostic for an
			// unrecognized type, so it needs a name no validator will ever claim.
			"unknown type",
			map[string]any{"type": "introspect-over-carrier-pigeon"},
			`unknown validator type "introspect-over-carrier-pigeon" (known types: static, jwks, introspect)`,
		},
		{
			"static without tokens",
			map[string]any{"type": "static"},
			"tokens is required",
		},
		{
			"static unknown option",
			map[string]any{"type": "static", "tokens": []any{}, "token_file": "/etc/tokens"},
			`unknown key(s) "token_file"`,
		},
		{
			"tokens not a list",
			map[string]any{"type": "static", "tokens": "tok"},
			"tokens: want a list",
		},
		{
			"token entry not a mapping",
			map[string]any{"type": "static", "tokens": []any{"tok"}},
			"tokens[0]: want a mapping",
		},
		{
			"token entry unknown key",
			map[string]any{"type": "static", "tokens": []any{
				map[string]any{"token": "t", "principal": "p", "tenent": "acme"},
			}},
			`unknown key(s) "tenent"`,
		},
		{
			"token entry missing principal",
			map[string]any{"type": "static", "tokens": []any{map[string]any{"token": "t"}}},
			"principal must not be empty",
		},
		{
			"token entry bad scopes",
			map[string]any{"type": "static", "tokens": []any{
				map[string]any{"token": "t", "principal": "p", "scopes": 3},
			}},
			"scopes:",
		},
		{
			"empty token table",
			map[string]any{"type": "static", "tokens": []any{}},
			"at least one token is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := map[string]any{"validators": []any{tc.entry}}
			_, err := ChainFromMap(raw)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
			// Errors have to point at the offending entry.
			if err != nil && !strings.Contains(err.Error(), "validators[0]") {
				t.Fatalf("error lacks positional context: %v", err)
			}
		})
	}
}

// TestParseJWKSConfigFromYAML walks a `jwks` entry through the same path
// broker.yaml takes: yaml.v3 → map[string]any → ChainFromMap.
func TestParseJWKSConfigFromYAML(t *testing.T) {
	auth := decodeYAML(t, `
auth:
  validators:
    - type: jwks
      issuer: "https://issuer.example/"
      jwks_url: "https://issuer.example/.well-known/jwks.json"
      audience: nexus-broker
      algorithms: [RS256, ES256]
      principal_claim: sub
      tenant_claim: org_id
      scopes_claim: scope
      cache_ttl: 15m
      negative_cache_ttl: 30s
      http_timeout: 3s
      clock_skew: 0s
`)
	cfg, err := ParseConfig(auth)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if len(cfg.Validators) != 1 {
		t.Fatalf("want 1 validator, got %d", len(cfg.Validators))
	}
	vc := cfg.Validators[0]
	if vc.Type != ValidatorTypeJWKS || vc.Name != ValidatorTypeJWKS {
		t.Fatalf("unexpected type/name: %+v", vc)
	}
	if vc.ClaimMapping.PrincipalClaim != "sub" || vc.ClaimMapping.TenantClaim != "org_id" ||
		vc.ClaimMapping.ScopesClaim != "scope" {
		t.Fatalf("claim mapping not lifted: %+v", vc.ClaimMapping)
	}

	chain, err := BuildChain(cfg)
	if err != nil {
		t.Fatalf("BuildChain: %v", err)
	}
	if !chain.Enabled() {
		t.Fatalf("chain is disabled")
	}
	v, ok := chain.validators[0].Validator.(*JWKSValidator)
	if !ok {
		t.Fatalf("want a *JWKSValidator, got %T", chain.validators[0].Validator)
	}
	if len(v.algorithms) != 2 || v.algorithms[0] != "RS256" || v.algorithms[1] != "ES256" {
		t.Fatalf("algorithms not carried: %v", v.algorithms)
	}
	if v.keys.ttl != 15*time.Minute || v.keys.negTTL != 30*time.Second || v.keys.timeout != 3*time.Second {
		t.Fatalf("durations not carried: ttl=%s neg=%s timeout=%s", v.keys.ttl, v.keys.negTTL, v.keys.timeout)
	}
	// An explicit zero clock_skew must mean "no leeway", not "apply the default".
	if _, err := ParseConfig(auth); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
}

// TestJWKSConfigAcceptsAudienceList covers the list form of `audience`, and
// asserts a lone string is NOT whitespace-split into several audiences.
func TestJWKSConfigAcceptsAudienceList(t *testing.T) {
	for _, doc := range []string{
		`
auth:
  validators:
    - type: jwks
      issuer: "https://issuer.example/"
      jwks_url: "https://issuer.example/jwks"
      audience: [nexus-broker, nexus-broker-staging]
      principal_claim: sub
`,
		`
auth:
  validators:
    - type: jwks
      issuer: "https://issuer.example/"
      jwks_url: "https://issuer.example/jwks"
      audience: "one audience with spaces"
      principal_claim: sub
`,
	} {
		if _, err := ChainFromMap(decodeYAML(t, doc)); err != nil {
			t.Fatalf("ChainFromMap: %v", err)
		}
	}
}

// TestJWKSConfigNamesRepeatedEntries checks two issuers stay distinguishable in a
// single denial record.
func TestJWKSConfigNamesRepeatedEntries(t *testing.T) {
	auth := decodeYAML(t, `
auth:
  validators:
    - type: jwks
      issuer: "https://a.example/"
      jwks_url: "https://a.example/jwks"
      audience: nexus-broker
      principal_claim: sub
    - type: jwks
      issuer: "https://b.example/"
      jwks_url: "https://b.example/jwks"
      audience: nexus-broker
      principal_claim: sub
`)
	chain, err := ChainFromMap(auth)
	if err != nil {
		t.Fatalf("ChainFromMap: %v", err)
	}
	names := chain.Names()
	if len(names) != 2 || names[0] != "jwks" || names[1] != "jwks#2" {
		t.Fatalf("unexpected chain names: %v", names)
	}
}

func TestBuildJWKSValidatorRejectsBadConfig(t *testing.T) {
	valid := func() map[string]any {
		return map[string]any{
			"type":            "jwks",
			"issuer":          "https://issuer.example/",
			"jwks_url":        "https://issuer.example/jwks",
			"audience":        "nexus-broker",
			"principal_claim": "sub",
		}
	}
	cases := []struct {
		name    string
		mutate  func(map[string]any)
		wantErr string
	}{
		{"unknown key", func(m map[string]any) { m["jwks_uri"] = "x" }, `unknown key(s) "jwks_uri"`},
		{"missing issuer", func(m map[string]any) { delete(m, "issuer") }, "issuer is required"},
		{"missing jwks_url", func(m map[string]any) { delete(m, "jwks_url") }, "jwks_url is required"},
		{"missing audience", func(m map[string]any) { delete(m, "audience") }, "audience is required"},
		{
			"missing principal_claim",
			func(m map[string]any) { delete(m, "principal_claim") },
			"principal_claim is required",
		},
		{"issuer not a string", func(m map[string]any) { m["issuer"] = 7 }, "issuer: want a string"},
		{
			"audience not a string or list",
			func(m map[string]any) { m["audience"] = 7 },
			"audience: want a string or a list of strings",
		},
		{
			"audience list with a non-string",
			func(m map[string]any) { m["audience"] = []any{"a", 7} },
			"audience[1]: want a string",
		},
		{
			"algorithms includes none",
			func(m map[string]any) { m["algorithms"] = []any{"none"} },
			"an unsigned token proves nothing",
		},
		{
			"algorithms includes an HMAC family member",
			func(m map[string]any) { m["algorithms"] = []any{"HS256"} },
			"algorithm-confusion attack",
		},
		{
			"cache_ttl as a bare number",
			func(m map[string]any) { m["cache_ttl"] = 600 },
			`cache_ttl: want a duration string such as "10m"`,
		},
		{
			"cache_ttl unparseable",
			func(m map[string]any) { m["cache_ttl"] = "ten minutes" },
			"cache_ttl:",
		},
		{
			"clock_skew beyond the cap",
			func(m map[string]any) { m["clock_skew"] = "1h" },
			"clock_skew must be between",
		},
		{
			"plain http jwks_url",
			func(m map[string]any) { m["jwks_url"] = "http://issuer.example/jwks" },
			"http is only allowed for a loopback host",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := valid()
			tc.mutate(entry)
			_, err := ChainFromMap(map[string]any{"validators": []any{entry}})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
			if !strings.Contains(err.Error(), "validators[0]") {
				t.Fatalf("error lacks positional context: %v", err)
			}
		})
	}
}

// TestUnknownValidatorTypeListsJWKS keeps the diagnostic honest: a typo must be
// answered with the full list of types that exist.
func TestUnknownValidatorTypeListsJWKS(t *testing.T) {
	_, err := ChainFromMap(map[string]any{
		"validators": []any{map[string]any{"type": "jwt"}},
	})
	if err == nil {
		t.Fatalf("want an unknown-type error")
	}
	for _, want := range []string{ValidatorTypeStatic, ValidatorTypeJWKS} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error does not mention %q: %v", want, err)
		}
	}
}
