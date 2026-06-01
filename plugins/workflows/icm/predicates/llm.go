package predicates

import (
	"context"
	"fmt"

	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// evalLLM dispatches to the configured Judge function. With no
// dispatcher set the evaluator returns a failure result so callers can
// surface "judge not configured" to the operator instead of silently
// passing. Step 6 wires the actual judge.
func (e *Evaluator) evalLLM(ctx context.Context, p *workspace.Predicate, artifact []byte, sc StageEvalContext, res Result) Result {
	if e.Judge == nil {
		res.Verdict = false
		res.Feedback = "llm judge not configured"
		return res
	}
	verdict, feedback, score, err := e.Judge(ctx, p, artifact, sc)
	if err != nil {
		res.Verdict = false
		res.Feedback = fmt.Sprintf("judge error: %v", err)
		return res
	}
	res.Verdict = verdict
	res.Feedback = feedback
	res.Score = score
	return res
}
