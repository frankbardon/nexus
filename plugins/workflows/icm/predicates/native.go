package predicates

import (
	"context"
	"fmt"

	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// evalNative looks up the predicate's handler in the in-process
// registry and dispatches to it. Unknown handlers and missing handler
// names return failure results rather than panicking — the orchestrator
// surfaces them to the LLM as ordinary validator feedback.
func (e *Evaluator) evalNative(ctx context.Context, p *workspace.Predicate, artifact []byte, res Result) Result {
	if p.Handler == "" {
		res.Verdict = false
		res.Feedback = "native predicate missing handler name"
		return res
	}
	h, ok := e.LookupNative(p.Handler)
	if !ok {
		res.Verdict = false
		res.Feedback = fmt.Sprintf("native handler %q is not registered", p.Handler)
		return res
	}
	nr := h.Evaluate(ctx, p.Args, artifact)
	res.Verdict = nr.Verdict
	res.Feedback = nr.Feedback
	res.Score = nr.Score
	// Name and Type were set by the caller; preserve them across the
	// handler boundary so handlers cannot accidentally rename
	// themselves in event payloads.
	res.Name = p.Name
	res.Type = workspace.PredNative
	return res
}
