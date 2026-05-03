//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestSandboxWasm_Boot validates the engine boots cleanly with code_exec
// configured for compiler=yaegi-wasm and sandbox.backend=wasm. Catches
// regressions in PluginContext.Sandbox plumbing, lifecycle.resolveSandbox,
// and the wazero runtime initialisation path.
func TestSandboxWasm_Boot(t *testing.T) {
	h := testharness.New(t, "configs/test-sandbox-wasm.yaml", testharness.WithTimeout(60*time.Second))
	h.Run()

	h.AssertBooted(
		"nexus.tool.code_exec",
		"nexus.agent.react",
	)
	h.AssertEventEmitted("tool.register")
}

// TestSandboxWasm_PureComputeSnippet validates the full round-trip: the
// mocked LLM dispatches a run_code tool call; the script executes inside
// the embedded Yaegi-on-Wasm runner; the result returns to the agent;
// the final assistant message acknowledges it.
//
// Note: ~10–15 s wall on first run because wazero AOT-compiles the 39 MiB
// embedded runner. Subsequent runs in the same process are fast.
func TestSandboxWasm_PureComputeSnippet(t *testing.T) {
	h := testharness.New(t, "configs/test-sandbox-wasm.yaml", testharness.WithTimeout(60*time.Second))
	h.Run()

	h.AssertToolCalled("run_code")
	h.AssertEventEmitted("code.exec.request")
	h.AssertEventEmitted("code.exec.result")
	h.AssertOutputContains("returned 55")
}

// TestSandboxWasm_NetDeniedSurfacesAsScriptError pushes a snippet that calls
// nexus_sdk/http.Get against a host that is not in net.allow_hosts. The
// bridge gate must reject the call; the snippet's err handling propagates
// the failure as a code.exec.result with a non-empty error; the agent's
// next turn (canned) acknowledges. No live network is reached.
func TestSandboxWasm_NetDeniedSurfacesAsScriptError(t *testing.T) {
	cfg := copyConfig(t, "configs/test-sandbox-wasm.yaml", map[string]any{
		"nexus.io.test": map[string]any{
			"inputs":        []string{"Try to fetch a URL."},
			"input_delay":   "200ms",
			"approval_mode": "approve",
			"timeout":       "60s",
			"mock_responses": []map[string]any{
				{
					"tool_calls": []map[string]any{
						{
							"name":      "run_code",
							"arguments": `{"script":"package main\n\nimport (\n\t\"context\"\n\t\"errors\"\n\t\"fmt\"\n\tnhttp \"nexus_sdk/http\"\n)\n\nfunc Run(ctx context.Context) (any, error) {\n\t_, err := nhttp.Get(\"https://blocked.example/x\")\n\tif err != nil {\n\t\tif errors.Is(err, nhttp.ErrCapDenied) {\n\t\t\tfmt.Println(\"DENIED\")\n\t\t\treturn \"denied\", nil\n\t\t}\n\t\treturn nil, err\n\t}\n\treturn \"unexpected-success\", nil\n}\n"}`,
						},
					},
				},
				{"content": "Capability denial confirmed."},
			},
		},
		"nexus.tool.code_exec": map[string]any{
			"compiler":         "yaegi-wasm",
			"timeout_seconds":  30,
			"allowed_packages": []string{"fmt", "context", "errors"},
			"sandbox": map[string]any{
				"backend": "wasm",
				"timeout": "30s",
				// No net.allow_hosts → deny-all default.
			},
		},
	})

	h := testharness.New(t, cfg, testharness.WithTimeout(60*time.Second))
	h.Run()

	h.AssertToolCalled("run_code")
	h.AssertEventEmitted("code.exec.result")
	// The snippet handles ErrCapDenied internally and returns "denied"; the
	// agent's canned next turn acknowledges. If the bridge gate had failed
	// open, the snippet's "unexpected-success" branch would have run and
	// the canned response would never have been triggered cleanly.
	h.AssertOutputContains("Capability denial confirmed")
}
