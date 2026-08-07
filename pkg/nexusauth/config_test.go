package nexusauth

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
			"unknown type",
			map[string]any{"type": "jwks", "issuer": "https://idp.example.com/"},
			`unknown validator type "jwks" (known types: static)`,
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
