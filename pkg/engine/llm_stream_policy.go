package engine

import (
	"log/slog"
	"sort"
	"strings"

	"github.com/frankbardon/nexus/pkg/events"
)

// Response-gate policy values for core.streaming.response_gate_policy.
const (
	// StreamPolicyDowngrade turns streaming off for a request whenever a
	// plugin that adjudicates whole responses, and only whole responses, is
	// active. The default.
	StreamPolicyDowngrade = "downgrade"
	// StreamPolicyAllow leaves Stream as the caller set it and accepts that
	// such a plugin's veto cannot prevent disclosure.
	StreamPolicyAllow = "allow"
)

// StreamUnsafePlugins returns the IDs of active plugins that handle
// before:llm.response but not before:llm.stream.chunk, sorted for stable
// logging.
//
// The distinction is the whole point of the streaming seam. A plugin holding
// only the response-level hook sees the model's text for the first time after
// the stream has finished, so its veto governs history and tool execution but
// arrives far too late to stop the text being shown. A plugin holding the
// chunk-level hook adjudicates before anything is released, so it keeps its
// guarantee with streaming on.
//
// Declared subscriptions are the source of truth rather than the live bus
// table, for two reasons: it can be computed at boot before any dispatch has
// happened, and Subscriptions() is already the contract the plugin harness
// enforces against runtime behavior, so a plugin cannot quietly disagree with
// what it declared.
//
// The consequence is a pleasant one: a gate acquires streaming support simply
// by growing a before:llm.stream.chunk handler, and streaming comes back on
// by itself. Nothing has to be re-registered, and no operator has to notice.
func StreamUnsafePlugins(plugins []Plugin) []string {
	var unsafe []string
	for _, p := range plugins {
		if p == nil {
			continue
		}
		var gatesResponse, gatesStream bool
		for _, sub := range p.Subscriptions() {
			switch sub.EventType {
			case "before:llm.response":
				gatesResponse = true
			case EventBeforeStreamChunk:
				gatesStream = true
			}
		}
		if gatesResponse && !gatesStream {
			unsafe = append(unsafe, p.ID())
		}
	}
	sort.Strings(unsafe)
	return unsafe
}

// installStreamPolicy wires the boot-time streaming downgrade and returns its
// unsubscribe func, or nil when no downgrade is warranted.
//
// This closes a hole rather than adding a feature. The response-veto contract
// told operators that a gate needing non-disclosure "requires stream: false",
// but no such knob was ever reachable: the ReAct, plan-execute and
// orchestrator loops all build their requests with Stream: true hardcoded. An
// operator running nexus.gate.content_safety in block mode was therefore
// protected against tool execution and against poisoned history, but not
// against the disclosure they had most likely deployed it for — with nothing
// in the logs to say so.
//
// So the engine decides instead of the operator. When a plugin that can only
// judge whole responses is active, outbound requests lose Stream and the
// provider makes a blocking call: strictly slower, and exactly as safe as the
// documentation always claimed. Operators who would rather have the latency
// than the guarantee set core.streaming.response_gate_policy to "allow".
//
// The handler sits on before:llm.request at a deliberately late priority so
// it is the final word on Stream, whatever earlier handlers decided. It never
// vetoes: downgrading a request is not a reason to refuse it.
func installStreamPolicy(bus EventBus, plugins []Plugin, policy string, logger *slog.Logger) func() {
	if bus == nil {
		return nil
	}
	if policy == "" {
		policy = StreamPolicyDowngrade
	}

	unsafe := StreamUnsafePlugins(plugins)
	if len(unsafe) == 0 {
		return nil
	}

	if policy != StreamPolicyDowngrade {
		if logger != nil {
			logger.Warn("streaming left on for plugins that can only gate whole responses",
				"plugins", strings.Join(unsafe, ", "),
				"policy", policy,
				"consequence", "a before:llm.response veto cannot prevent disclosure of text already streamed")
		}
		return nil
	}

	if logger != nil {
		logger.Info("streaming disabled for LLM requests",
			"reason", "active plugins gate before:llm.response but not before:llm.stream.chunk",
			"plugins", strings.Join(unsafe, ", "),
			"policy", policy,
			"override", "core.streaming.response_gate_policy: allow")
	}

	return bus.Subscribe("before:llm.request", func(event Event[any]) {
		vp, ok := event.Payload.(*VetoablePayload)
		if !ok {
			return
		}
		req, ok := vp.Original.(*events.LLMRequest)
		if !ok || !req.Stream {
			return
		}
		req.Stream = false
	}, WithPriority(streamPolicyPriority), WithSource("nexus.core"))
}

// streamPolicyPriority runs the downgrade after every plugin handler on
// before:llm.request. Plugins conventionally use priorities in the tens; this
// sits far enough above that a gate re-enabling Stream cannot outrank the
// engine's decision by accident.
const streamPolicyPriority = 10000
