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
