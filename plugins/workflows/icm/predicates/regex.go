package predicates

import (
	"fmt"
	"strings"

	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// evalRegex matches the artifact against the predicate's pre-compiled
// regex, scoped by anchor. The loader is responsible for the actual
// compile; absent CompiledRegex is treated as a fatal evaluation error
// (the workspace failed to load, but we got here anyway).
func (e *Evaluator) evalRegex(p *workspace.Predicate, artifact []byte, res Result) Result {
	rx := p.CompiledRegex()
	if rx == nil {
		res.Verdict = false
		res.Feedback = "regex was not compiled at load time"
		return res
	}

	text := string(artifact)
	var target string
	switch p.Anchor {
	case workspace.AnchorFirstLine:
		if i := strings.IndexAny(text, "\r\n"); i >= 0 {
			target = text[:i]
		} else {
			target = text
		}
	case workspace.AnchorLastLine:
		trimmed := strings.TrimRight(text, "\r\n")
		if i := strings.LastIndexAny(trimmed, "\r\n"); i >= 0 {
			target = trimmed[i+1:]
		} else {
			target = trimmed
		}
	case workspace.AnchorWhole, "":
		target = text
	default:
		res.Verdict = false
		res.Feedback = fmt.Sprintf("unknown anchor %q", p.Anchor)
		return res
	}

	if rx.MatchString(target) {
		res.Verdict = true
		return res
	}

	res.Verdict = false
	if p.Message != "" {
		res.Feedback = p.Message
	} else {
		res.Feedback = fmt.Sprintf("regex %q did not match (anchor=%s)", p.Pattern, p.Anchor)
	}
	return res
}
