package client_test

// These tests exercise the plugin's config schema through the SAME code path
// Boot uses (engine.SmokeValidateConfig runs validateConfigSchemas without
// invoking Init), rather than compiling schema.json directly. That matters:
// the whole point of the schema is that a bad config stops the process at
// boot — before parseServer ever runs — so the test has to prove the
// boot-time validator is the thing that rejects it.
//
// The package is client_test (external) so it can import pkg/engine and the
// plugin together without the plugin's own package participating in a cycle.

import (
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	client "github.com/frankbardon/nexus/plugins/mcp/client"
)

// validateYAML builds an engine from pluginBlock, registers only
// nexus.mcp.client, and runs the boot-time schema pass over it. pluginBlock is
// indented four spaces (it sits under `plugins: nexus.mcp.client:`).
func validateYAML(t *testing.T, pluginBlock string) error {
	t.Helper()
	full := "core:\n  log_level: error\n\nplugins:\n  active:\n    - nexus.mcp.client\n\n  nexus.mcp.client:\n" + pluginBlock
	eng, err := engine.NewFromBytes([]byte(full))
	if err != nil {
		t.Fatalf("NewFromBytes: %v\nconfig:\n%s", err, full)
	}
	eng.Registry.Register("nexus.mcp.client", client.New)
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

// TestSchemaRejectsMisspelledTopLevelKey is the headline typo case: before the
// schema, `serverz:` meant the plugin booted with zero servers and said nothing.
func TestSchemaRejectsMisspelledTopLevelKey(t *testing.T) {
	mustReject(t, `    serverz:
      - name: fake
        command: /bin/echo
`, "serverz")
}

// TestSchemaRejectsMisspelledServerKey proves additionalProperties reaches
// inside a servers[] entry, not just the top level.
func TestSchemaRejectsMisspelledServerKey(t *testing.T) {
	mustReject(t, `    servers:
      - name: fake
        command: /bin/echo
        lifecyle: engine
`, "lifecyle")
}

// TestSchemaRejectsMisspelledNestedKey walks all the way down into
// servers[].resources, the deepest object level in the schema.
func TestSchemaRejectsMisspelledNestedKey(t *testing.T) {
	mustReject(t, `    servers:
      - name: fake
        command: /bin/echo
        resources:
          auto_register_statik: true
`, "auto_register_statik")
	mustReject(t, `    defaults:
      resources:
        subscribe_update: true
`, "subscribe_update")
	mustReject(t, `    servers:
      - name: fake
        command: /bin/echo
        tools:
          allowed: ["echo"]
`, "allowed")
}

// TestSchemaRejectsInprocessWithoutServer is the reason the if/then branches
// exist: the missing `server` key must be named at boot, not surface later as
// "inprocess transport requires server" from parseServer's connect phase.
func TestSchemaRejectsInprocessWithoutServer(t *testing.T) {
	err := validateYAML(t, `    servers:
      - name: host
        transport: inprocess
`)
	if err == nil {
		t.Fatal("inprocess server without `server` accepted, want a boot-time validation error")
	}
	if !strings.Contains(err.Error(), "missing property 'server'") {
		t.Errorf("error does not name the missing key:\n%v", err)
	}
	// Pin that this came from the schema pass, not from parseServer — the
	// latter never runs here, but the message shape is the operator-facing
	// difference worth asserting.
	if !strings.Contains(err.Error(), "config validation failed") {
		t.Errorf("error is not the aggregated boot-time validation error:\n%v", err)
	}
	if strings.Contains(err.Error(), "host-injected server key") {
		t.Errorf("error came from parseServer, not the schema:\n%v", err)
	}
}

// TestSchemaRejectsStdioWithoutCommand covers both spellings of stdio: named
// explicitly, and left absent so parseServer's default applies.
func TestSchemaRejectsStdioWithoutCommand(t *testing.T) {
	mustReject(t, `    servers:
      - name: fake
        transport: stdio
`, "command")
	mustReject(t, `    servers:
      - name: fake
`, "command")
}

func TestSchemaRejectsHTTPWithoutURL(t *testing.T) {
	mustReject(t, `    servers:
      - name: remote
        transport: http
`, "url")
}

// TestSchemaRejectsNumericTimeout pins the type against what parseServer
// actually asserts: `m["timeout"].(string)`. A YAML number is silently ignored
// today, so it must not be schema-valid.
func TestSchemaRejectsNumericTimeout(t *testing.T) {
	mustReject(t, `    servers:
      - name: fake
        command: /bin/echo
        timeout: 30
`, "timeout")
	mustReject(t, `    defaults:
      timeout: 30
`, "timeout")
}

// TestSchemaRejectsBadEnums / patterns pin the constraints parseServer enforces
// by hand so the same configs fail earlier and more legibly.
func TestSchemaRejectsBadEnumsAndPatterns(t *testing.T) {
	mustReject(t, `    servers:
      - name: fake
        transport: grpc
        command: /bin/echo
`, "transport")
	mustReject(t, `    servers:
      - name: fake
        command: /bin/echo
        lifecycle: forever
`, "lifecycle")
	mustReject(t, `    servers:
      - name: Fake_Server
        command: /bin/echo
`, "name")
	mustReject(t, `    servers:
      - command: /bin/echo
`, "name")
}

// TestSchemaRejectsWrongScalarTypes covers the readers that use bool/int type
// assertions: a wrong type is dropped on the floor by applyResourceConfig.
func TestSchemaRejectsWrongScalarTypes(t *testing.T) {
	mustReject(t, `    servers:
      - name: fake
        command: /bin/echo
        resources:
          enabled: "yes"
`, "enabled")
	mustReject(t, `    servers:
      - name: fake
        command: /bin/echo
        resources:
          auto_register_max: "50"
`, "auto_register_max")
	mustReject(t, `    servers:
      - name: fake
        command: /bin/echo
        prompts:
          enabled: 1
`, "enabled")
	mustReject(t, `    aliases:
      review: 7
`, "review")
}

// TestSchemaAcceptsShippedShapes mirrors the two configs/ files that use the
// plugin, plus every transport and key the reference documents.
func TestSchemaAcceptsShippedShapes(t *testing.T) {
	cases := map[string]string{
		"empty": "    {}\n",
		// Verbatim shape of configs/local-pulse-mcp-oneshot.yaml and
		// configs/local-pulse-mcp-browser.yaml.
		"shipped pulse config": `    servers:
      - name: pulse
        transport: stdio
        command: /Users/example/Work/nexus/bin/pulse
        args: ["mcp", "--data-dir", "/Users/example/.nexus/pulse-data"]
        lifecycle: engine
        timeout: 30s
        resources:
          enabled: true
          auto_register_static: true
          auto_register_max: 50
          subscribe_updates: false
        prompts:
          enabled: true
`,
		"defaults block": `    defaults:
      lifecycle: session
      timeout: 10s
      command_prefix: tools
      resources:
        enabled: true
        auto_register_static: false
        auto_register_template: false
        auto_register_max: 0
        subscribe_updates: false
      prompts:
        enabled: false
`,
		"aliases": `    aliases:
      review: gh.review_pr
      plan: pulse.plan
`,
		"stdio with env": `    servers:
      - name: gh
        transport: stdio
        command: gh-mcp
        env:
          GITHUB_TOKEN: "${GITHUB_TOKEN}"
        env_passthrough: ["HOME", "PATH"]
        tools:
          allow: ["review_pr"]
          deny: ["delete_repo"]
`,
		"http": `    servers:
      - name: remote
        transport: http
        url: https://mcp.example.com/mcp
        headers:
          Authorization: "Bearer ${MCP_TOKEN}"
        timeout: 45s
`,
		"inprocess": `    servers:
      - name: host
        transport: inprocess
        server: host-tools
        lifecycle: session
`,
		// The flat key set is shared across transports: parseServer reads
		// command, url and server unconditionally, so an http server carrying a
		// leftover command must still reach Init rather than being stopped here.
		"cross-transport keys are not exclusive": `    servers:
      - name: remote
        transport: http
        url: https://mcp.example.com/mcp
        command: /bin/echo
`,
		// Duplicate names are a runtime check in parseConfig, deliberately not
		// expressed here. This shape must pass the schema.
		"duplicate names reach parseConfig": `    servers:
      - name: dup
        command: /bin/echo
      - name: dup
        command: /bin/true
`,
		"empty servers list": "    servers: []\n",
	}
	for name, block := range cases {
		t.Run(name, func(t *testing.T) { mustValidate(t, block) })
	}
}
