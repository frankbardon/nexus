// Package internalflow centralises the predicate every memory plugin
// uses to skip recording assistant messages produced by internal sub-flows
// (planner, classifier router, summariser, compaction, subagent).
//
// The earlier predicate — "skip when LLMResponse.Metadata[\"_source\"] is
// non-empty" — was correct until Idea 09 (#83) made every agent main
// request tag itself with `_source = pluginID` for cost attribution.
// Provider plugins propagate request metadata onto the response, so under
// the old predicate every agent main response was silently dropped from
// history along with its tool_use blocks. Anthropic then rejected the
// next request with "unexpected tool_use_id found in tool_result blocks".
//
// The fix targets task_kind instead: each internal sub-flow has a stable
// task_kind value, and main agent loops do not appear in this set.
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
