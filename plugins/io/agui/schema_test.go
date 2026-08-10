package agui_test

// These tests exercise the plugin's config schema through the SAME code path
// Boot uses (engine.SmokeValidateConfig runs validateConfigSchemas without
// invoking Init), rather than compiling schema.json directly. That matters:
// the point of the schema is that a typo stops the process at boot, so the
// test has to prove the boot-time validator is the thing that rejects it.
//
// The package is agui_test (external) so it can import pkg/engine and the
// plugin together without the plugin's own package participating in the cycle.

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/plugins/io/agui"
)

// validateYAML builds an engine from cfgYAML, registers only nexus.io.agui, and
// runs the boot-time schema pass over it. Returns the aggregated error, if any.
func validateYAML(t *testing.T, pluginBlock string) error {
	t.Helper()
	full := "core:\n  log_level: error\n\nplugins:\n  active:\n    - nexus.io.agui\n\n  nexus.io.agui:\n" + pluginBlock
	eng, err := engine.NewFromBytes([]byte(full))
	if err != nil {
		t.Fatalf("NewFromBytes: %v\nconfig:\n%s", err, full)
	}
	eng.Registry.Register("nexus.io.agui", agui.New)
	return engine.SmokeValidateConfig(eng)
}

func mustValidate(t *testing.T, pluginBlock string) {
	t.Helper()
	if err := validateYAML(t, pluginBlock); err != nil {
		t.Fatalf("valid config rejected:\n%v", err)
	}
}

// mustReject asserts the config fails and that the message names every needle,
// so an operator can find the offending key without re-reading the schema.
func mustReject(t *testing.T, pluginBlock string, needles ...string) {
	t.Helper()
	err := validateYAML(t, pluginBlock)
	if err == nil {
		t.Fatal("invalid config accepted, want a validation error")
	}
	for _, needle := range needles {
		if !strings.Contains(err.Error(), needle) {
			t.Errorf("error does not mention %q:\n%v", needle, err)
		}
	}
}

// TestSchemaRejectsMisspelledTopLevelKey is the whole reason this schema
// exists: before it, `bearer_tokn` produced an unauthenticated server and no
// warning at all.
func TestSchemaRejectsMisspelledTopLevelKey(t *testing.T) {
	mustReject(t, "    bearer_tokn: \"s3cret\"\n", "bearer_tokn", "bearer_token")
}

// TestSchemaRejectsMisspelledAuthKey covers the case E6-S1 made worse: a typo
// inside the auth block used to mean auth silently did not apply.
func TestSchemaRejectsMisspelledAuthKey(t *testing.T) {
	mustReject(t, `    auth:
      validatorz:
        - type: static
`, "validatorz")
}

// TestSchemaRejectsMisspelledValidatorKey proves additionalProperties reaches
// inside a validators[] entry, which only works because the per-type keys are
// described by if/then branches rather than one flat property list.
func TestSchemaRejectsMisspelledValidatorKey(t *testing.T) {
	mustReject(t, `    auth:
      validators:
        - type: jwks
          issuer: "https://id.example.com/"
          jwks_url: "https://id.example.com/.well-known/jwks.json"
          audience: "nexus-agui"
          principal_claim: sub
          clock_skewe: 1m
`, "clock_skewe")
}

// TestSchemaRejectsAdminScope pins the one deliberate difference from the
// broker's spelling of the same block: admin_scope is broker-only.
func TestSchemaRejectsAdminScope(t *testing.T) {
	mustReject(t, `    auth:
      admin_scope: "nexus.broker.admin"
      validators:
        - type: static
          tokens:
            - token: "t"
              principal: "p"
`, "admin_scope")
}

// TestSchemaRejectsCrossTypeValidatorKey asserts a key that is valid for one
// validator type is rejected on another — the flat-property-list failure mode.
func TestSchemaRejectsCrossTypeValidatorKey(t *testing.T) {
	mustReject(t, `    auth:
      validators:
        - type: static
          tokens:
            - token: "t"
              principal: "p"
          jwks_url: "https://id.example.com/.well-known/jwks.json"
`, "jwks_url")
}

// TestSchemaRejectsBareNumberDuration mirrors nexusauth's own rule: 600 reads
// as ten minutes to an operator and six hundred nanoseconds to Go.
func TestSchemaRejectsBareNumberDuration(t *testing.T) {
	mustReject(t, `    auth:
      validators:
        - type: jwks
          issuer: "https://id.example.com/"
          jwks_url: "https://id.example.com/.well-known/jwks.json"
          audience: "nexus-agui"
          principal_claim: sub
          cache_ttl: 600
`, "cache_ttl")
}

// TestSchemaRejectsWrongScalarTypes pins the types against what Init actually
// asserts: emit_state is read via a bool assertion, bind via a string one.
func TestSchemaRejectsWrongScalarTypes(t *testing.T) {
	mustReject(t, "    emit_state: \"yes\"\n", "emit_state")
	mustReject(t, "    bind: 8090\n", "bind")
}

// TestSchemaAcceptsShippedShapes covers the config surface the repo's own
// configs/ files use, plus every auth spelling the docs advertise.
func TestSchemaAcceptsShippedShapes(t *testing.T) {
	cases := map[string]string{
		"empty": "    {}\n",
		"bind and emit_state": `    bind: 127.0.0.1:18190
    emit_state: true
`,
		"bearer token": `    bearer_token: "s3cret"
    bearer_token_env: "AGUI_TOKEN"
`,
		// parseCORSOrigins accepts a YAML list...
		"cors list": `    cors_origins:
      - https://app.example.com
      - https://staging.example.com
`,
		// ...or a single comma-separated string.
		"cors comma string": "    cors_origins: \"https://app.example.com, https://staging.example.com\"\n",
		"auth null":         "    auth:\n",
		"auth empty validators": `    auth:
      validators: []
`,
		"auth full chain": `    auth:
      validators:
        - type: static
          tokens:
            - token: "ci-token"
              principal: "ci-runner"
              tenant: "acme"
              scopes: "a b"
        - type: jwks
          issuer: "https://id.example.com/"
          jwks_url: "https://id.example.com/.well-known/jwks.json"
          audience: ["nexus-agui", "nexus-agui-staging"]
          algorithms: ["RS256", "ES256"]
          principal_claim: sub
          tenant_claim: org_id
          scopes_claim: scope
          cache_ttl: 10m
          negative_cache_ttl: 1m
          http_timeout: 5s
          clock_skew: 1m
        - type: introspect
          introspection_url: "https://id.example.com/oauth2/introspect"
          client_id: "nexus-agui"
          client_secret_env: "NEXUS_AGUI_INTROSPECTION_SECRET"
          client_auth: basic
          token_type_hint: access_token
          principal_claim: sub
          cache_ttl: 1m
          negative_cache_ttl: 30s
          http_timeout: 5s
        - type: proxy_headers
          trusted_proxy_cidrs: ["10.4.0.0/16", "fd00:1ce::/64"]
          principal_header: X-Forwarded-User
          tenant_header: X-Auth-Request-Org
          scopes_header: X-Forwarded-Groups
`,
		// Single-string spellings of the list-shaped keys.
		"scalar list spellings": `    auth:
      validators:
        - type: jwks
          issuer: "https://id.example.com/"
          jwks_url: "https://id.example.com/.well-known/jwks.json"
          audience: "nexus-agui"
          algorithms: "RS256"
          principal_claim: sub
        - type: proxy_headers
          trusted_proxy_cidrs: "10.4.0.0/16"
          principal_header: X-Forwarded-User
`,
		// Setting both auth: and bearer_token is rejected by Init, which owns
		// the operator-facing message. The schema deliberately stays out of it,
		// so this shape must reach Init rather than being stopped here.
		"auth and bearer token reach Init": `    bearer_token: "s3cret"
    auth:
      validators:
        - type: static
          tokens:
            - token: "t"
              principal: "p"
`,
	}
	for name, block := range cases {
		t.Run(name, func(t *testing.T) { mustValidate(t, block) })
	}
}
