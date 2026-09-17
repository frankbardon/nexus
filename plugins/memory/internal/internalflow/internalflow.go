// Package internalflow centralises the predicate every memory plugin
// uses to skip recording assistant messages produced by internal sub-flows
// (planner, classifier router, summariser, compaction, subagent, delegate).
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
	"delegate":  true, // delegated sub-session keeps its own history
}

// The delegate entry was missing until a topology of
// parent -> delegate -> sub-agent shipped: pkg/delegate runs its own
// in-process history and stamps task_kind "delegate", but the capped
// memory recorded those responses into the TOP-LEVEL history anyway. The
// parent's history then read user -> assistant(delegate call) ->
// assistant(sub-agent tool call), and Gemini rejects a model turn carrying
// a functionCall that immediately follows another model turn with
// 400 INVALID_ARGUMENT. nexus.agent.react never saw it because react
// filters on _source instead; this set is the asymmetry.

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
