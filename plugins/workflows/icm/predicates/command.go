package predicates

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/sandbox"
	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// defaultCommandTimeout is the floor applied to command predicates
// when neither the predicate, the stage budget, nor the plugin-config
// default provide a non-zero value.
const defaultCommandTimeout = 30 * time.Second

// evalCommand runs the predicate's `run` script through the configured
// sandbox. The script receives the artifact bytes on stdin. Exit 0 is
// pass; any non-zero exit is fail with trimmed stdout (or stderr when
// stdout is empty) as the failure feedback.
func (e *Evaluator) evalCommand(ctx context.Context, p *workspace.Predicate, artifact []byte, sc StageEvalContext, res Result) Result {
	if p.Run == "" {
		res.Verdict = false
		res.Feedback = "command predicate missing 'run' path"
		return res
	}
	if e.Sandbox == nil {
		res.Verdict = false
		res.Feedback = "sandbox not configured"
		return res
	}

	script := p.Run
	if !filepath.IsAbs(script) {
		if sc.WorkspaceRoot == "" {
			res.Verdict = false
			res.Feedback = fmt.Sprintf("cannot resolve relative script %q: no workspace root", script)
			return res
		}
		script = filepath.Join(sc.WorkspaceRoot, script)
	}

	timeout := e.resolveCommandTimeout(p, sc)
	result, err := e.Sandbox.Exec(ctx, sandbox.ExecRequest{
		Kind:    sandbox.KindShell,
		Source:  []byte(script),
		Stdin:   artifact,
		Timeout: timeout,
	})
	if err != nil {
		res.Verdict = false
		res.Feedback = fmt.Sprintf("command %q failed to run: %v", p.Run, err)
		return res
	}
	if result.TimedOut {
		res.Verdict = false
		res.Feedback = fmt.Sprintf("command %q timed out after %s", p.Run, timeout)
		return res
	}
	if result.Exit == 0 {
		res.Verdict = true
		return res
	}

	feedback := strings.TrimSpace(string(bytes.TrimRight(result.Stdout, "\x00")))
	if feedback == "" {
		feedback = strings.TrimSpace(string(bytes.TrimRight(result.Stderr, "\x00")))
	}
	if feedback == "" {
		feedback = fmt.Sprintf("command %q exited %d", p.Run, result.Exit)
	}
	res.Verdict = false
	res.Feedback = feedback
	return res
}

// resolveCommandTimeout picks the most-specific non-zero timeout in
// the order: predicate → stage budget → plugin-config default → 30s.
func (e *Evaluator) resolveCommandTimeout(p *workspace.Predicate, sc StageEvalContext) time.Duration {
	if p.TimeoutSeconds > 0 {
		return time.Duration(p.TimeoutSeconds) * time.Second
	}
	if sc.StageBudgetTimeoutSec > 0 {
		return time.Duration(sc.StageBudgetTimeoutSec) * time.Second
	}
	if e.CommandTimeoutSecs > 0 {
		return time.Duration(e.CommandTimeoutSecs) * time.Second
	}
	return defaultCommandTimeout
}
