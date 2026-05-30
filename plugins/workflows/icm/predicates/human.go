package predicates

import (
	"context"
	"fmt"

	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// evalHuman dispatches to the configured Human function. With no
// dispatcher set the evaluator returns a failure result so callers can
// surface "human handler not configured" to the operator instead of
// silently passing. Step 6 wires the actual handler.
func (e *Evaluator) evalHuman(ctx context.Context, p *workspace.Predicate, artifact []byte, sc StageEvalContext, res Result) Result {
	if e.Human == nil {
		res.Verdict = false
		res.Feedback = "human handler not configured"
		return res
	}
	verdict, feedback, err := e.Human(ctx, p, artifact, sc)
	if err != nil {
		res.Verdict = false
		res.Feedback = fmt.Sprintf("human handler error: %v", err)
		return res
	}
	res.Verdict = verdict
	res.Feedback = feedback
	return res
}
