// Package internalflow centralises the predicate every memory plugin
// uses to skip recording assistant messages produced by internal sub-flows
// (planner, classifier router, summariser, compaction, subagent).
//
// The predicate keys on task_kind: each internal sub-flow stamps a stable
// task_kind value, and main agent loops do not appear in that set. Keying on
// task_kind rather than on a non-empty _source is deliberate — every agent
// main request also tags itself with `_source = pluginID` for cost
// attribution, and provider plugins propagate that onto the response, so an
// empty-source test would drop main agent responses (and their tool_use
// blocks) from history.
package internalflow

// internalTaskKinds enumerates the task_kind values produced by sub-flows
// memory plugins should not record in the main conversation history.
// Agent main loops (react_main, planexec_step, orchestrator_decompose,
// orchestrator_synthesize) are deliberately excluded — they ARE the
// conversation.
var internalTaskKinds = map[string]bool{
	"plan":      true, // dynamic / static planner
	"classify":  true, // classifier router probe
	"summarise": true, // summary_buffer / compaction summary call
	"compact":   true, // explicit compaction
	"subagent":  true, // subagent has its own scratch history
}

// SkipForHistory returns true when the response metadata indicates an
// internal sub-flow whose output must not be recorded as part of the
// user-facing conversation history.
func SkipForHistory(meta map[string]any) bool {
	if meta == nil {
		return false
	}
	kind, _ := meta["task_kind"].(string)
	return internalTaskKinds[kind]
}

// SkipForCuration returns true when the request metadata indicates an
// internal sub-flow whose outgoing LLM request a curation layer should
// leave alone. Mirrors SkipForHistory but is intended for the
// before:llm.request side: tool_result_clear, tool_def_pruner, and any
// other curator should bail when the request originates from a planner,
// classifier, summariser, compaction, or subagent flow rather than the
// main agent loop.
func SkipForCuration(meta map[string]any) bool {
	return SkipForHistory(meta)
}
