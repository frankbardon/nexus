package codeexec

import (
	"context"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/sandbox"
	"github.com/frankbardon/nexus/pkg/events"
)

// runScriptWasm dispatches a run_code invocation through ctx.Sandbox to the
// Wasm backend. v1 capability surface inside Wasm is intentionally narrow:
// no `tools.*`, no `parallel.*`, no skill helpers — only the configured
// stdlib whitelist. The bridge SDK (`nexus_sdk/{http,fs,exec,env,tools}`)
// arrives in Phase 3 and reopens those affordances.
func (p *Plugin) runScriptWasm(tc events.ToolCall, script string) {
	started := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	res, err := p.sandbox.Exec(ctx, sandbox.ExecRequest{
		Kind:            sandbox.KindGoWasm,
		Source:          []byte(script),
		AllowedPackages: p.allowedPackages,
		Timeout:         p.timeout,
	})
	durationMs := time.Since(started).Milliseconds()
	if err != nil {
		p.emitResult(tc, "", "", "wasm sandbox: "+err.Error(), durationMs, false)
		return
	}

	stdout := string(res.Stdout)
	stderr := string(res.Stderr)
	if res.TimedOut {
		p.emitResult(tc, stdout, "", "script timed out after "+p.timeout.String(), durationMs, res.Truncated)
		return
	}
	if stderr != "" {
		p.emitResult(tc, stdout, "", stderr, durationMs, res.Truncated)
		return
	}
	p.emitResult(tc, stdout, "", "", durationMs, res.Truncated)
}
