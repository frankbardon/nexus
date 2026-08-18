package a2a_test

// These tests exercise the plugin's config schema through the SAME code path
// Boot uses (engine.SmokeValidateConfig runs validateConfigSchemas without
// invoking Init), rather than compiling schema.json directly. The point of the
// schema is that a typo stops the process at boot, so the test has to prove the
// boot-time validator is the thing that rejects it.
//
// The package is a2a_test (external) so it can import pkg/engine and the plugin
// together without the plugin's own package participating in the cycle.

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	a2aplugin "github.com/frankbardon/nexus/plugins/io/a2a"
)

// validCard is the minimum card block every schema case needs, since the schema
// requires one.
const validCard = `    card:
      name: nexus
      description: A Nexus agent.
      version: "0.1.0"
      skills:
        - id: chat
          name: Chat
          description: Run a turn.
`

func validateYAML(t *testing.T, pluginBlock string) error {
	t.Helper()
	full := "core:\n  log_level: error\n\nplugins:\n  active:\n    - nexus.io.a2a\n\n  nexus.io.a2a:\n" + pluginBlock
	eng, err := engine.NewFromBytes([]byte(full))
	if err != nil {
		t.Fatalf("NewFromBytes: %v\nconfig:\n%s", err, full)
	}
	eng.Registry.Register("nexus.io.a2a", a2aplugin.New)
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

// TestSchemaAcceptsTheDocumentedShape walks every key the reference documents.
func TestSchemaAcceptsTheDocumentedShape(t *testing.T) {
	mustValidate(t, `    bind: 127.0.0.1:8091
    public_url: https://agent.example.test
    jsonrpc_path: /a2a
    rest_prefix: /a2a/v1
    strict_version_header: false
    card_requires_auth: false
    cors_origins:
      - https://ui.example.test
    bearer_token_env: NEXUS_A2A_TOKEN
    card:
      name: nexus
      description: A Nexus agent.
      version: "0.1.0"
      documentation_url: https://example.test/docs
      icon_url: https://example.test/icon.png
      provider:
        organization: Nexus
        url: https://example.test
      default_input_modes: ["text/plain"]
      default_output_modes: ["text/plain"]
      skills:
        - id: chat
          name: Chat
          description: Run a turn.
          tags: ["chat"]
          examples: ["Summarize the README."]
          input_modes: ["text/plain"]
          output_modes: ["text/plain"]
`)
}

// TestSchemaAcceptsTheValidatorChain proves the shared nexusauth block is
// reachable here with the same spelling nexus.io.agui uses.
func TestSchemaAcceptsTheValidatorChain(t *testing.T) {
	mustValidate(t, `    auth:
      validators:
        - type: static
          tokens:
            - token: s3cret
              principal: partner-a
        - type: jwks
          issuer: https://idp.example.test/
          jwks_url: https://idp.example.test/.well-known/jwks.json
          audience: nexus
          principal_claim: sub
`+validCard)
}

// TestSchemaRejectsMisspelledTopLevelKey is the whole reason this schema
// exists: a misspelled auth key would otherwise produce an unauthenticated
// server and no warning at all.
func TestSchemaRejectsMisspelledTopLevelKey(t *testing.T) {
	mustReject(t, "    bearer_tokn: \"s3cret\"\n"+validCard, "bearer_tokn", "bearer_token")
}

// TestSchemaRejectsMisspelledCardKey guards the card block, which is where an
// operator does the most hand-editing.
func TestSchemaRejectsMisspelledCardKey(t *testing.T) {
	mustReject(t, `    card:
      name: nexus
      description: A Nexus agent.
      version: "0.1.0"
      documentaion_url: https://typo.example.test
      skills:
        - id: chat
          name: Chat
          description: Run a turn.
`, "documentaion_url")
}

// TestSchemaRequiresACard pins the deliberate refusal to synthesize one.
func TestSchemaRequiresACard(t *testing.T) {
	mustReject(t, "    bind: 127.0.0.1:8091\n", "card")
}

// TestSchemaRequiresANonEmptySkillList pins the A2A requirement.
func TestSchemaRequiresANonEmptySkillList(t *testing.T) {
	mustReject(t, `    card:
      name: nexus
      description: A Nexus agent.
      version: "0.1.0"
      skills: []
`, "skills")
}

// TestSchemaRejectsAnUnknownCapabilityKey pins the derived-not-configured rule
// at the schema level: there is no key for it, so stating one fails the boot.
func TestSchemaRejectsAnUnknownCapabilityKey(t *testing.T) {
	mustReject(t, `    card:
      name: nexus
      description: A Nexus agent.
      version: "0.1.0"
      capabilities:
        streaming: true
      skills:
        - id: chat
          name: Chat
          description: Run a turn.
`, "capabilities")
}
