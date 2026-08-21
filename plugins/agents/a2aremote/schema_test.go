package a2aremote_test

// These tests drive the plugin's config schema through the SAME code path Boot
// uses (engine.SmokeValidateConfig runs the schema validators without invoking
// Init), rather than compiling schema.json directly. The point of the schema is
// that a typo stops the process at boot, so the test has to prove the boot-time
// validator is the thing that rejects it — and that a config the plugin's own
// parser accepts is not rejected before it ever gets there.
//
// The package is a2aremote_test (external) so it can import pkg/engine and the
// plugin together without the plugin's own package participating in a cycle.

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/plugins/agents/a2aremote"
)

func validateYAML(t *testing.T, pluginBlock string) error {
	t.Helper()
	full := "core:\n  log_level: error\n\nplugins:\n  active:\n    - nexus.agent.a2a_remote\n\n" +
		"  nexus.agent.a2a_remote:\n" + pluginBlock
	eng, err := engine.NewFromBytes([]byte(full))
	if err != nil {
		t.Fatalf("NewFromBytes: %v\nconfig:\n%s", err, full)
	}
	eng.Registry.Register("nexus.agent.a2a_remote", a2aremote.New)
	return engine.SmokeValidateConfig(eng)
}

func mustValidate(t *testing.T, pluginBlock string) {
	t.Helper()
	if err := validateYAML(t, pluginBlock); err != nil {
		t.Fatalf("valid config rejected:\n%v", err)
	}
}

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

func TestSchemaAcceptsTheMinimumConfig(t *testing.T) {
	mustValidate(t, `    agents:
      - name: researcher
        base_url: https://research.internal
`)
}

// The full surface, at both levels, so a key the schema forgot is caught here
// rather than by an operator whose config stopped booting.
func TestSchemaAcceptsEveryKey(t *testing.T) {
	mustValidate(t, `    cache: true
    cache_size: 64
    max_depth: 2
    binding: jsonrpc
    validate_card: false
    stream: true
    timeout: 3m
    request_timeout: 45s
    message_timeout: 0s
    stream_open_timeout: 15s
    stream_idle_timeout: 20m
    extensions:
      - https://example.test/ext/v1
    retry:
      max_attempts: 5
      base_delay: 100ms
      max_delay: 2s
    agents:
      - name: researcher
        base_url: https://research.internal
        tool_name: ask_research
        description: a research agent
        posture: careful
        binding: http+json
        validate_card: true
        stream: false
        timeout: 90s
        request_timeout: 10s
        message_timeout: 5m
        stream_open_timeout: 5s
        stream_idle_timeout: 0s
        extensions: []
        retry:
          max_attempts: 1
      - name: pinned
        jsonrpc_endpoint: https://legacy.internal/a2a
      - name: pinned-rest
        rest_endpoint: https://legacy.internal/a2a/v1
`)
}

func TestSchemaAcceptsEveryCredentialBlock(t *testing.T) {
	mustValidate(t, `    agents:
      - name: open
        base_url: https://open.internal
        credentials:
          type: none
      - name: bearer
        base_url: https://bearer.internal
        credentials:
          type: bearer
          token_env: A2A_BEARER_TOKEN
          header: Authorization
          scheme: Bearer
      - name: oauth
        base_url: https://oauth.internal
        credentials:
          type: oauth2_client_credentials
          client_id_env: A2A_CLIENT_ID
          client_secret_env: A2A_CLIENT_SECRET
          token_url: https://issuer.internal/oauth2/token
          scopes:
            - a2a.invoke
          audience: https://oauth.internal
          auth_style: body
          refresh_leeway: 45s
      - name: mtls
        base_url: https://mtls.internal
        credentials:
          type: mtls
          cert_file: ~/.nexus/certs/client.pem
          key_file: ~/.nexus/certs/client-key.pem
          ca_file: ~/.nexus/certs/ca.pem
          server_name: mtls.internal
`)
}

func TestSchemaRejectsABadCredentialsBlock(t *testing.T) {
	mustReject(t, `    agents:
      - name: x
        base_url: https://a.internal
        credentials:
          type: kerberos
`, "credentials")

	mustReject(t, `    agents:
      - name: x
        base_url: https://a.internal
        credentials:
          token: hunter2
`, "type")

	mustReject(t, `    agents:
      - name: x
        base_url: https://a.internal
        credentials:
          type: bearer
          bearer_token: hunter2
`, "bearer_token")

	mustReject(t, `    agents:
      - name: x
        base_url: https://a.internal
        credentials:
          type: oauth2_client_credentials
          client_id: c
          client_secret: s
          auth_style: post
`, "auth_style")

	mustReject(t, `    agents:
      - name: x
        base_url: https://a.internal
        credentials:
          type: oauth2_client_credentials
          client_id: c
          client_secret: s
          refresh_leeway: 45
`, "refresh_leeway")
}

// Credentials are per remote. A plugin-level block would be a default silently
// applied to a remote it was never issued for, so the schema refuses one.
func TestSchemaRejectsPluginLevelCredentials(t *testing.T) {
	mustReject(t, `    credentials:
      type: bearer
      token: hunter2
    agents:
      - name: x
        base_url: https://a.internal
`, "credentials")
}

func TestSchemaRejectsUnknownKeys(t *testing.T) {
	mustReject(t, `    timeout_seconds: 90
    agents:
      - name: x
        base_url: https://a.internal
`, "timeout_seconds")

	mustReject(t, `    agents:
      - name: x
        base_url: https://a.internal
        bearer_token: hunter2
`, "bearer_token")
}

func TestSchemaRejectsBareNumberDurations(t *testing.T) {
	mustReject(t, `    timeout: 600
    agents:
      - name: x
        base_url: https://a.internal
`, "timeout")
}

func TestSchemaRejectsAnUnknownBinding(t *testing.T) {
	mustReject(t, `    binding: grpc
    agents:
      - name: x
        base_url: https://a.internal
`, "binding")
}

func TestSchemaRequiresAtLeastOneAgent(t *testing.T) {
	mustReject(t, `    agents: []
`, "agents")
	mustReject(t, `    cache: false
`, "agents")
}
