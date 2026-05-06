//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestCodeExec_Boot validates the code_exec plugin loads cleanly and registers
// the run_code tool without surprising side effects on neighbouring plugins.
func TestCodeExec_Boot(t *testing.T) {
	h := testharness.New(t, "configs/test-code-exec.yaml", testharness.WithTimeout(30*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.tool.code_exec",
		"nexus.tool.shell",
		"nexus.agent.react",
	)
	h.AssertEventEmitted("tool.register")
}

// TestCodeExec_ScriptInvokesShell validates the full round-trip: mocked LLM
// dispatches a run_code tool call whose script invokes tools.Shell, which
// rides the real bus back through the shell plugin, whose result is surfaced
// to the script, which returns it as the run_code payload.
func TestCodeExec_ScriptInvokesShell(t *testing.T) {
	h := testharness.New(t, "configs/test-code-exec.yaml", testharness.WithTimeout(30*time.Second))
	h.Run()

	// Outer tool call — what the LLM asked for.
	h.AssertToolCalled("run_code")
	// Inner tool call — dispatched by the script through the bus shim.
	h.AssertToolCalled("shell")

	// Plugin-specific events.
	h.AssertEventEmitted("code.exec.request")
	h.AssertEventEmitted("code.exec.result")

	// Final turn output must come from the second mock response.
	h.AssertOutputContains("Done. The script ran")
}

// TestCodeExec_RejectsScriptWithGoStmt validates the AST-level sandbox. The
// override replaces the valid script with one containing a goroutine; the
// plugin should fail fast with a structured error and no shell call should
// fire.
func TestCodeExec_RejectsScriptWithGoStmt(t *testing.T) {
	cfg := copyConfig(t, "configs/test-code-exec.yaml", map[string]any{
		"nexus.io.test": map[string]any{
			"inputs":        []string{"Please run a script with goroutines."},
			"input_delay":   "200ms",
			"approval_mode": "approve",
			"timeout":       "20s",
			"mock_responses": []map[string]any{
				{
					"tool_calls": []map[string]any{
						{
							"name":      "run_code",
							"arguments": `{"script":"package main\n\nimport \"context\"\n\nfunc Run(ctx context.Context) (any, error) {\n\tgo func() {}()\n\treturn nil, nil\n}\n"}`,
						},
					},
				},
				{"content": "Script was rejected, as expected."},
			},
		},
	})

	h := testharness.New(t, cfg, testharness.WithTimeout(30*time.Second))
	h.Run()

	h.AssertToolCalled("run_code")
	// Script must NOT make it past the AST gate — only the outer run_code
	// invocation fires, never the inner shell.
	h.AssertEventCount("tool.invoke", 1, 1)
	h.AssertEventEmitted("code.exec.result")
}
